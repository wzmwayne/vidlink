package server

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"vidlink/internal/account"
)

// 本文件是**凭据解析**的唯一入口。
//
// 三种传法（请求头 / Bearer / 查询参数）以前是等价的；现在前两种仍然是
// 明文 Key，第三种**只接受签名凭据**。理由是明文 Key 进了 URL 就会留在
// 浏览器历史、隧道与反向代理日志、聊天记录和截屏里，而它长期有效；
// 签名凭据把"能用"压缩到几十秒，且签名本身不含任何秘密。
//
// 两条路径汇合在同一个账号上，因此下游（停用检查、并发闸门、配额预授权、
// 计量结算）只有一份实现——不会出现"某个接口忘了处理签名"这种漏洞。

// credSource 说明凭据来自请求的哪个部位。
type credSource int

const (
	credNone credSource = iota
	credHeader
	credBearer
	credQuerySigned
)

// String 返回适合放进日志的短标识。
func (c credSource) String() string {
	switch c {
	case credHeader:
		return "header"
	case credBearer:
		return "bearer"
	case credQuerySigned:
		return "signed-url"
	default:
		return "none"
	}
}

// credentialOf 从请求里取出凭据。
//
// 顺序改了：头部 → Bearer → 查询参数。以前 ?key= 排在第二位，而它现在
// 只能承载签名——若还让 URL 优先于调用方真正的身份（头部），
// 转发一条带签名的 URL 就会把请求者的身份换掉。
func credentialOf(r *http.Request) (string, credSource) {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k, credHeader
	}
	if k := bearer(r.Header.Get("Authorization")); k != "" {
		return k, credBearer
	}
	if k := r.URL.Query().Get("key"); k != "" {
		return k, credQuerySigned
	}
	return "", credNone
}

// authLogSource 只从请求本身判断凭据来源（不依赖中间件传值），供访问日志用。
func authLogSource(r *http.Request) string {
	_, src := credentialOf(r)
	return src.String()
}

// credError 是一次认证失败：带状态码、稳定错误码与人话说明。
type credError struct {
	status int
	code   string
	msg    string
}

func (e *credError) Error() string { return e.msg }

func (e *credError) write(w http.ResponseWriter) {
	writeErrorStatus(w, e.status, e.code, e.msg)
}

func forbidden(code, msg string) *credError {
	return &credError{status: http.StatusForbidden, code: code, msg: msg}
}

// 缺少凭据时的提示。签名是唯一 URL 传法，所以这里必须把"怎么换签名"
// 一并说清楚，否则调用方只会看到 403 却不知道 URL 里该放什么。
const missingKeyHint = "缺少 API Key：请通过 X-API-Key 头或 Authorization: Bearer 头提供明文 Key；" +
	"若要把凭据放进 URL（代理下载、<video> 等无法自定义请求头的场景），" +
	"请使用签名凭据 ?key=acc_<16位句柄>.<8位时间戳hex>.<64位签名hex>（用 GET /v1/sign 换取）"

// queryKeyHint 是 ?key= 形态不对时的提示。
const queryKeyHint = "URL 里的 key 只能是签名凭据 " +
	"acc_<16位句柄>.<8位时间戳hex>.<64位签名hex>（用 GET /v1/sign 换取）；" +
	"明文 Key 请放在 X-API-Key 头里，不要写进 URL"

// accountForCredential 把一次请求的凭据解析成**同一个**账号对象。
//
//	明文 Key  → 按 Key 直取（句柄由 Handle 派生，两者必然一致）
//	acc_ 签名 → 按句柄取账号，再用该账号的 Key 验签
//
// 两条路径的结论完全相同，所以调用方（withAccount）之后无需区分它们。
//
// **请求头按形态识别，URL 只认签名**：
//
//	头 / Bearer → 明文 Key 或签名都可以（前端用的就是"头里放签名"，
//	              这样明文 Key 根本不进网络，只留在本机浏览器里）
//	?key=       → 只接受签名。明文 Key 进 URL 是本次改动要杜绝的那件事：
//	              它会留在浏览器历史、隧道与反代日志、聊天记录与截屏里。
func (s *Server) accountForCredential(value string, src credSource) (account.Account, *credError) {
	if src == credQuerySigned {
		if !looksSigned(value) {
			return account.Account{}, forbidden("key_format", queryKeyHint)
		}
		return s.accountFromSigned(value)
	}
	// 明文优先：极端情况下账本里可能存在一个"长得像签名"的历史 Key，
	// 精确按键命中永远该赢；而伪造的签名不可能同时是某个账号的 Key。
	if a, ok := s.accounts.Get(value); ok {
		return a, nil
	}
	if looksSigned(value) {
		return s.accountFromSigned(value)
	}
	// 刻意不区分"Key 不存在"与"Key 错误"，避免被用来枚举有效 Key
	return account.Account{}, forbidden("key_invalid", "API Key 无效")
}

// looksSigned 报告一个串是否是签名凭据的形态（不含任何验签）。
func looksSigned(v string) bool {
	_, _, _, ok := account.ParseCredential(v)
	return ok
}

// accountFromSigned 处理 URL 里的签名凭据。
func (s *Server) accountFromSigned(value string) (account.Account, *credError) {
	handle, ts, sig, ok := account.ParseCredential(value)
	if !ok {
		return account.Account{}, forbidden("key_format", queryKeyHint)
	}
	if strings.HasPrefix(handle, account.AdminPrefix) {
		// 管理签名不是账号，也绝不因此获得账号身份
		return account.Account{}, forbidden("key_scope",
			"这是管理签名（adm_），只能用于 /v1/admin/* 接口；解析接口请用账号的 acc_ 签名")
	}
	acct, ok := s.accounts.ByHandle(handle)
	if !ok {
		// 与"Key 无效"同一个口径：不泄漏该句柄是否真实存在
		return account.Account{}, forbidden("key_invalid", "API Key 无效")
	}
	switch st := account.VerifyCredential(acct.Key, handle, sig, ts, time.Now(), s.sigTTL); st {
	case account.SigOK:
		return acct, nil
	case account.SigExpired, account.SigFuture:
		return account.Account{}, forbidden(st.Code(), s.sigTimeHint(ts))
	default:
		return account.Account{}, forbidden(st.Code(), "签名无效：请重新获取签名（GET /v1/sign）")
	}
}

// sigTimeHint 把"偏了多少秒"直接写进错误里：这是排查时钟问题最快的线索。
func (s *Server) sigTimeHint(ts int64) string {
	now := time.Now().Unix()
	delta := now - ts
	who := "已过期"
	if delta < 0 {
		who = "时间戳在未来"
		delta = -delta
	}
	return fmt.Sprintf("签名%s：票面时间戳 %d，服务器时间 %d，偏了 %d 秒（容差 %d 秒）。"+
		"请重新签发（GET /v1/sign）；若是本机自行签发，请先用 /v1/health 的 time 对表",
		who, ts, now, delta, int64(s.sigTTL/time.Second))
}

// adminKey 返回生效的管理 Key（与启动时读取的一致，去掉首尾空白）。
func (s *Server) adminKey() string { return strings.TrimSpace(s.cfg.AdminKey) }

// adminFromSigned 校验管理面的签名凭据（adm_ 前缀，签名密钥是管理 Key）。
func (s *Server) adminFromSigned(value string) *credError {
	want := s.adminKey()
	handle, ts, sig, ok := account.ParseCredential(value)
	if !ok {
		return forbidden("key_format", queryKeyHint)
	}
	if !strings.HasPrefix(handle, account.AdminPrefix) {
		return forbidden("key_scope",
			"这是账号签名（acc_），不能用于 /v1/admin/* 接口")
	}
	// 句柄必须严格等于由管理 Key 派生出来的那个。
	//
	// 签名原料里已经含了句柄，所以"拿管理 Key 签另一个 adm_ 句柄"本来就
	// 验不过；这里再显式比一次，是为了让这种情况得到明确的 key_invalid，
	// 而不是一句含糊的"签名无效"——也挡住将来有人把句柄从签名原料里改掉。
	if handle != s.adminHandle {
		return forbidden("key_invalid", "管理签名无效")
	}
	switch st := account.VerifyCredential(want, handle, sig, ts, time.Now(), s.sigTTL); st {
	case account.SigOK:
		return nil
	case account.SigExpired, account.SigFuture:
		return forbidden(st.Code(), s.sigTimeHint(ts))
	default:
		return forbidden(st.Code(), "管理签名无效：请重新获取签名（GET /v1/admin/sign）")
	}
}

// signPayload 组装签发响应（账号面与管理面同形，前端与脚本一份代码）。
//
// "query" 直接给可拼接的形式，是因为调用方要的就是"塞进 URL"，
// 让它自己拼字符串只会多一处可能拼错的地方。
func (s *Server) signPayload(prefix, secret, typ string) map[string]any {
	now := time.Now()
	ts := now.Unix()
	cred := account.SignAt(prefix, secret, ts)
	ttl := int64(s.sigTTL / time.Second)
	return map[string]any{
		"key":        cred,
		"query":      "key=" + url.QueryEscape(cred),
		"type":       typ,
		"ttl":        ttl,
		"issued_at":  ts,
		"expires_at": ts + ttl,
		"note": fmt.Sprintf("签名有效期 %d 秒，且不绑定具体请求（拿到的人在有效期内可调用同一账号的任意接口，"+
			"消耗记在该账号账上）。长期使用请把明文 Key 放在 X-API-Key 头里。", ttl),
	}
}

// handleSign 是账号面的签发端点：GET /v1/sign
//
// 为什么要有服务端签发：页面在**局域网明文 HTTP** 下没有安全上下文，
// 而秘密绝不能下发给页面之外的东西；同时让时间戳取服务器时钟，
// 调用方自己的钟准不准就无关紧要了。
func (s *Server) handleSign(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountFrom(r.Context())
	if !ok {
		// 走不到这里（/v1/sign 在账户中间件之内），但真走到了也必须回 403
		// 而不是 500：credError 不是 core.Error，交给 writeError 会被归类成
		// "内部错误"——那会让"少带凭据"看起来像服务端坏了。
		forbidden("key_missing", missingKeyHint).write(w)
		return
	}
	writeJSON(w, http.StatusOK, s.signPayload(account.HandlePrefix, acct.Key, "account"))
}

// handleAdminSign 是管理面的签发端点：GET /v1/admin/sign
func (s *Server) handleAdminSign(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.adminKey() == "" {
		forbidden("admin_disabled",
			"管理接口未启用：未配置管理 Key（环境变量或 .vl 里的 VIDLINK_ADMIN_KEY）").write(w)
		return
	}
	writeJSON(w, http.StatusOK, s.signPayload(account.AdminPrefix, s.adminKey(), "admin"))
}

// constantTimeEqual 比较两个服务级凭据。
//
// 普通字符串比较会因为提前返回而泄漏前缀信息，长期看是可被逐字节试探的
// 旁路。Key 是定长随机串，长度本身不是秘密。
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
