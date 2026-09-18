package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"vidlink/internal/account"
	"vidlink/internal/core"
	"vidlink/internal/quota"
	"vidlink/internal/rates"
)

// adminRoutes 是管理面路由。
//
// 为什么单独一张表：管理端点用的是**固定管理 Key**（配置项
// VIDLINK_ADMIN_KEY），而不是账本里的账号，鉴权路径与业务端点完全不同；
// 而它们**同样不能**被 404/405 判定逻辑漏掉，所以必须与业务路由一起
// 交给同一套 fallback 处理。
//
// 管理 Key 与账号体系互不蕴含：
//   - 管理 Key 不能用来解析（它不是账号，计量端点会回 403）；
//   - 账号也不是管理员（账号结构里没有权限位，改配额/改倍率都改不出管理权）；
//   - 未配置管理 Key 时，这里注册的每条路由都恒返回 403。
func (s *Server) adminRoutes() []routeSpec {
	return []routeSpec{
		{method: http.MethodGet, path: "/v1/admin/accounts", admin: true,
			handler: s.handleAdminListAccounts},
		{method: http.MethodPost, path: "/v1/admin/accounts", admin: true,
			handler: s.handleAdminCreateAccount},
		{method: http.MethodGet, path: "/v1/admin/accounts/{key}", admin: true,
			handler: s.handleAdminGetAccount},
		{method: http.MethodPatch, path: "/v1/admin/accounts/{key}", admin: true,
			handler: s.handleAdminPatchAccount},
		{method: http.MethodDelete, path: "/v1/admin/accounts/{key}", admin: true,
			handler: s.handleAdminDeleteAccount},
		{method: http.MethodGet, path: "/v1/admin/stats", admin: true,
			handler: s.handleAdminStats},
		{method: http.MethodGet, path: "/v1/admin/quota", admin: true,
			handler: s.handleAdminQuota},
		// 改价与复位：见 handleAdminQuotaUpdate / handleAdminQuotaReset
		{method: http.MethodPut, path: "/v1/admin/quota", admin: true,
			handler: s.handleAdminQuotaUpdate},
		{method: http.MethodDelete, path: "/v1/admin/quota", admin: true,
			handler: s.handleAdminQuotaReset},
		{method: http.MethodGet, path: "/v1/admin/ledger", admin: true,
			handler: s.handleAdminLedger},
	}
}

// handleAdminQuota 返回计费倍率的全貌（生效值 + 覆盖层 + 审计）。
//
// 可读也可写：GET 看现状，PUT 改格子/代理费率，DELETE 复位。
// 单个账号的临时调整仍走该账号的 multiplier（那是按账号隔离的）；
// 这里改的是**共享的价目表**，影响所有账号的后续调用。
//
// 这个端点的意义是"让管理员能自己核对一次调用到底扣多少"，
// 而不用去读源码或翻文档。
func (s *Server) handleAdminQuota(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.quotaSnapshot())
}

// quotaSnapshot 组装 /v1/admin/quota 的响应（GET/PUT/DELETE 共用一套形状，
// 面板改完一次往返就能重渲染）。
func (s *Server) quotaSnapshot() map[string]any {
	preauth := map[string]float64{}
	for _, ep := range quota.AllEndpoints {
		preauth[string(ep)] = s.quotaTable.MaxCoefficient(ep)
	}
	view := s.rates.View()
	defaults := map[string]any{}
	for k, v := range s.quotaTable.Coefficients("") {
		defaults[string(k)] = v
	}
	history := make([]map[string]any, 0, len(view.History))
	for _, h := range view.History {
		history = append(history, map[string]any{
			"at": h.At, "action": h.Action, "actor": h.Actor, "detail": h.Detail,
		})
	}
	out := map[string]any{
		"unit":    unitName,
		"formula": "消耗 = 端点系数(端点, 平台) × 条数 × 账号倍率",
		"endpoints": map[string]any{
			"info":        "仅元信息与可用档位列表，不含任何直链",
			"links":       "仅直链，不含元信息",
			"detail":      "元信息 + 全部档位直链 + 字幕/图集",
			"batch_links": "批量只取直链，按成功条数计量",
		},
		// 预授权上限：解析开始前按该端点在所有平台中的最高系数检查一次配额。
		// 客户端据此可以预判"最少要留多少配额才能调这个端点"。改价后立刻跟着变。
		"preauth_max":      preauth,
		"platforms":        s.allRates(),
		"default_platform": defaults,
		// 覆盖层（管理员改过的格子）与出厂默认，便于面板算出"哪一格被改过"
		"overrides":  view.Overrides,
		"defaults":   view.Defaults,
		"source":     view.Source,
		"path":       view.Path,
		"updated_at": view.UpdatedAt,
		"limits": map[string]any{
			"min_rate": quota.MinRate,
			"max_rate": quota.MaxRate,
		},
		"warnings": s.quotaTable.Warnings(),
		"history":  history,
		// 代理是唯一不按"端点 × 平台"计费的配额出口，必须单独说明，
		// 否则管理员核对用量时会以为它漏记了。
		"proxy": map[string]any{
			"rate":            fmt.Sprintf("%.4g 配额/MiB", s.proxyRate()),
			"rate_per_mib":    s.proxyRate(),
			"unit_bytes":      quota.ProxyUnitBytes,
			"platform_factor": false,
			"formula":         "实扣 = 传输体积(MiB) × 费率 × 账号倍率（费率对所有账号相同）",
			"note": "媒体代理按传输体积计费，**不乘平台系数**（它与上游解析成本无关）。" +
				"上游声明了长度时先扣后传，长度未知时传完按实际字节扣；提前中断不退。",
		},
		// 公共入口：Key 公开，配额按每 IP 每日限额，与账本余额无关
		"public": map[string]any{
			"enabled":          s.cfg.PublicKey != "",
			"key":              s.cfg.PublicKey,
			"daily_per_ip":     s.publicQ.Limit(),
			"accounting":       "不使用账本余额；用量与调用次数仍累计到公共账号上",
			"ledger_available": false,
			"note": "公共 Key 是共享的，额度按「每 IP 每日」计算，过期自动重置；" +
				"停用公共账号即可关闭公共入口",
		},
		"note": "系数只影响后续调用，已发生的用量不重算；" +
			"单个账号的调整请改该账号的 multiplier",
	}
	return out
}

// updateQuotaRequest 是改价的请求体。
//
// 语义是**逐格合并**：只提交要动的格子，其余保持不变；
// 值为 null 表示清除该格的自定义（回到内置默认）。
//
//	{"rates": {"links": {"douyin": 1.5, "kuaishou": null}}, "proxy_rate": 0.6}
type updateQuotaRequest struct {
	Rates     map[string]map[string]*float64 `json:"rates"`
	ProxyRate *float64                       `json:"proxy_rate"`
}

// handleAdminQuotaUpdate 修改计费倍率（PUT /v1/admin/quota）。
func (s *Server) handleAdminQuotaUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req updateQuotaRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, core.BadInput("", "请求体不合法: %v", err))
		return
	}
	if len(req.Rates) == 0 && req.ProxyRate == nil {
		writeError(w, core.BadInput("", "请求体里没有任何要改的内容："+
			"rates（平台 → 端点 → 倍率，null 表示恢复默认）与 proxy_rate 至少给一个"))
		return
	}
	view, err := s.rates.Apply(req.Rates, req.ProxyRate)
	if err != nil {
		writeError(w, core.BadInput("", "%v", err))
		return
	}
	s.log.Info("计费倍率已更新", "request_id", requestID(r.Context()),
		"path", view.Path, "proxy_rate", view.ProxyRate,
		"detail", lastHistoryDetail(view))
	writeJSON(w, http.StatusOK, s.quotaSnapshot())
}

// handleAdminQuotaReset 清空全部自定义，回到内置默认（DELETE /v1/admin/quota）。
func (s *Server) handleAdminQuotaReset(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	view, err := s.rates.Reset()
	if err != nil {
		writeError(w, core.E(core.KindInternal, "", "rates", "恢复默认失败", err))
		return
	}
	s.log.Warn("计费倍率已恢复内置默认", "request_id", requestID(r.Context()),
		"path", view.Path)
	writeJSON(w, http.StatusOK, s.quotaSnapshot())
}

func lastHistoryDetail(v rates.View) string {
	if len(v.History) == 0 {
		return ""
	}
	return v.History[len(v.History)-1].Detail
}

// requireAdmin 确认本次请求已经过了管理 Key 校验。
//
// 校验本身在中间件里完成（serveAdminKey），这里只读上下文标记：
// handler 再拿一次管理 Key 既没必要，也多一处可能被打印进日志的地方。
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !adminAuthed(r.Context()) {
		// 正常路径下中间件已经拦住了，走到这里是编程错误（漏挂中间件）；
		// 给 403 而不是 500：口径必须是"没通过管理 Key 校验"。
		writeError(w, core.Errf(core.KindForbidden, "", "auth",
			"该端点需要管理 Key（X-API-Key 头、Authorization: Bearer 或 ?key=）"))
		return false
	}
	return true
}

// accountView 是管理面的账号形状：账号本身 + 派生出来的可读字段。
//
// 派生字段（如 can_check_in）不入账本，只在这里算：写进结构体就得考虑
// 它与 daily_grant 的一致性，而它本来就只是"从属性推出来的一个布尔"。
type accountView struct {
	account.Account
	// CanCheckIn 表示这个账号当前是否开放每日签到。
	CanCheckIn bool `json:"can_check_in"`
}

func accountViewOf(a account.Account) accountView {
	return accountView{Account: a, CanCheckIn: a.DailyGrant > 0 && !a.Disabled}
}

func (s *Server) handleAdminListAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	list := s.accounts.List()
	views := make([]accountView, 0, len(list))
	for _, a := range list {
		views = append(views, accountViewOf(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": views,
		"stats":    s.accounts.Stats(),
	})
}

// createAccountRequest 是创建账号的请求体。
//
// 倍率用**指针**：nil 表示"没提"，此时按 1.0（标准）；
// 而显式的 0 表示"不扣配额"，是有意义的取值。不用指针就无法区分这两者，
// 结果是每个新建账号默认不扣配额——那会让用量统计失去意义。
type createAccountRequest struct {
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	Quota      *float64 `json:"quota"`
	Multiplier *float64 `json:"multiplier"`
	// DailyGrant / GrantCap：每日自动补额与它的封顶（0 = 关闭 / 不封顶）
	DailyGrant *float64 `json:"daily_grant"`
	GrantCap   *float64 `json:"grant_cap"`
	Note       string   `json:"note"`
}

func (s *Server) handleAdminCreateAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req createAccountRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, core.BadInput("", "请求体不合法: %v", err))
		return
	}

	key := strings.TrimSpace(req.Key)
	generated := false
	if key == "" {
		// 不填就自动生成，避免管理员用弱 Key
		var err error
		key, err = newAPIKey()
		if err != nil {
			writeError(w, core.E(core.KindInternal, "", "admin", "生成 Key 失败", err))
			return
		}
		generated = true
	}

	mult := 1.0 // 默认标准倍率；0 是有含义的取值（不扣配额），不能当默认值
	if req.Multiplier != nil {
		mult = *req.Multiplier
	}
	q := 0.0
	if req.Quota != nil {
		q = *req.Quota
	}

	grant, cap := 0.0, 0.0
	if req.DailyGrant != nil {
		grant = *req.DailyGrant
	}
	if req.GrantCap != nil {
		cap = *req.GrantCap
	}
	if grant < 0 || cap < 0 {
		writeError(w, core.BadInput("", "daily_grant 与 grant_cap 不能为负"))
		return
	}
	a, err := s.accounts.Create(account.Account{
		Key: key, Name: req.Name, Quota: q, Multiplier: mult,
		DailyGrant: grant, GrantCap: cap, Note: req.Note,
	})
	if err != nil {
		if errors.Is(err, account.ErrDuplicate) {
			writeError(w, core.BadInput("", "该 Key 已存在"))
			return
		}
		writeError(w, core.E(core.KindInternal, "", "admin", "创建账号失败", err))
		return
	}

	// 明文 Key 只在**创建这一次**返回。此后所有接口都只回掩码，
	// 避免 Key 出现在日志、浏览器历史和运维截屏里。
	resp := map[string]any{"account": accountViewOf(a)}
	if generated {
		resp["key"] = a.Key
		resp["notice"] = "请立即保存这个 Key，它只会出现这一次"
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleAdminGetAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	a, ok := s.accounts.Resolve(r.PathValue("key"))
	if !ok {
		writeError(w, core.NotFound("", "账号不存在"))
		return
	}
	// Public() 掩码 Key：明文只在创建时返回一次，
	// 否则 Key 会出现在浏览器历史、代理日志、运维截屏里。
	writeJSON(w, http.StatusOK, map[string]any{
		"account": accountViewOf(a.Public()),
	})
}

// patchAccountRequest 是局部修改。所有字段用指针以区分"没提"与"设为零"。
type patchAccountRequest struct {
	Name       *string  `json:"name"`
	Note       *string  `json:"note"`
	Quota      *float64 `json:"quota"`
	AddQuota   *float64 `json:"add_quota"`
	Multiplier *float64 `json:"multiplier"`
	Disabled   *bool    `json:"disabled"`
	// DailyGrant / GrantCap：每日自动补额与封顶（0 = 关闭 / 不封顶）
	DailyGrant *float64 `json:"daily_grant"`
	GrantCap   *float64 `json:"grant_cap"`
}

func (s *Server) handleAdminPatchAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	acct, ok := s.accounts.Resolve(r.PathValue("key"))
	if !ok {
		writeError(w, core.NotFound("", "账号不存在"))
		return
	}
	var req patchAccountRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, core.BadInput("", "请求体不合法: %v", err))
		return
	}
	if req.Quota != nil && req.AddQuota != nil {
		writeError(w, core.BadInput("", "quota 与 add_quota 不能同时使用："+
			"前者是设为该值，后者是在现有值上加减"))
		return
	}

	a, err := s.accounts.Update(acct.Key, account.Patch{
		Name: req.Name, Note: req.Note,
		Quota: req.Quota, AddQuota: req.AddQuota,
		Multiplier: req.Multiplier, Disabled: req.Disabled,
		DailyGrant: req.DailyGrant, GrantCap: req.GrantCap,
	})
	if err != nil {
		if errors.Is(err, account.ErrNotFound) {
			writeError(w, core.NotFound("", "账号不存在"))
			return
		}
		writeError(w, core.BadInput("", "%v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account": accountViewOf(a.Public()),
	})
}

func (s *Server) handleAdminDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	// 不需要"不能删自己"的保护：管理 Key 不是账号，删哪个账号
	// 都不会影响管理面本身的可达性（这正是把 admin 移出账本换来的性质）。
	acct, ok := s.accounts.Resolve(r.PathValue("key"))
	if !ok {
		writeError(w, core.NotFound("", "账号不存在"))
		return
	}
	if err := s.accounts.Delete(acct.Key); err != nil {
		if errors.Is(err, account.ErrNotFound) {
			writeError(w, core.NotFound("", "账号不存在"))
			return
		}
		writeError(w, core.E(core.KindInternal, "", "admin", "删除失败", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": s.accounts.Stats(),
		"gate":     s.gate.Stats(),
		"public":   s.publicQ.Stats(),
		"cache":    s.svc.CacheStats(),
		"traffic": map[string]any{
			"requests":   s.reqCount.Load(),
			"errors":     s.errCount.Load(),
			"uptime_sec": int(time.Since(s.startedAt).Seconds()),
		},
	})
}

// decodeStrictJSON 解析管理面的请求体，**拒绝未知字段**。
//
// 为什么比默认行为更严：账号结构里已经不存在 admin 这类字段了，
// 而静默忽略未知字段会让调用方以为"已经设置成功"——比如
// {"admin":true} 会返回 200 却什么也没发生，接着就是拿着一把
// 永远 403 的 Key 排查半天。宁可现在报 400 并说清哪个字段不认识。
func decodeStrictJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// newAPIKey 生成一个 256 位随机 Key。
//
// 用 crypto/rand 而不是 math/rand：Key 就是账号本身，
// 可预测等于别人的配额能被随便花。
func newAPIKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("熵源不可用: %w", err)
	}
	return "vl_" + hex.EncodeToString(b[:]), nil
}
