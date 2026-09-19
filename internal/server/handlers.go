package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"vidlink/internal/account"
	"vidlink/internal/core"
	"vidlink/internal/gate"
	"vidlink/internal/publicq"
	"vidlink/internal/quota"
	"vidlink/internal/tier"
	"vidlink/internal/urlx"
)

// --- 账号与配额计量的中间件 ---

// ctxKeyAccount 把已认证的账号放进 context。
type ctxKeyAccount struct{}

// accountFrom 取回当前请求的账号。
func accountFrom(ctx context.Context) (account.Account, bool) {
	a, ok := ctx.Value(ctxKeyAccount{}).(account.Account)
	return a, ok
}

// 凭据解析统一在 auth.go：头部与 Bearer 是明文 Key，?key= 只接受签名凭据。

// withAccount 完成认证、并发闸门与预授权，并把账号放进 context。
//
// 旧版是"配了一组静态 Key，命中即放行"；现在 Key 对应一个**有配额和
// 账号倍率的账号**，所以认证与配额计量是同一件事的两面。
//
// 管理面（/v1/admin/*）走的是**另一条**鉴权路径，见 withAdminKey：
// 管理凭据是固定 Key，不是账本里的账号，因此这里必须先把它分流出去，
// 否则会出现"管理 Key 不是账号 → 403"这种自相矛盾的结果。
//
// 三件事都放在中间件里而不是各 handler 里，是为了让四个配额端点
// **不可能漏掉**其中任何一件——漏掉预授权就是未计入消耗，漏掉闸门就是没有上限。
func (s *Server) withAccount(next http.Handler, specs []routeSpec) http.Handler {
	// 收集成路径模板而不是精确路径：管理端点里有 /v1/admin/accounts/{key}
	// 这类带通配的路由，用 map[路径] 查会在"带句柄的具体请求"上漏掉，
	// 症状是管理 Key 被当成普通账号 Key 去查账本、然后 403。
	var publicPaths, adminPaths []string
	for _, rt := range specs {
		if rt.public {
			publicPaths = append(publicPaths, rt.path)
		}
		if rt.admin {
			adminPaths = append(adminPaths, rt.path)
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if matchAnyPath(publicPaths, r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// 管理端点：用固定管理 Key 鉴权，与账本无关。
		if matchAnyPath(adminPaths, r.URL.Path) {
			s.serveAdminKey(next, w, r)
			return
		}
		// 根路径在开启 WebUI 时也要公开：页面本身不含任何数据（数据全靠
		// 页面里带的 Key 去请求），若不公开，浏览器只会拿到一个 403 JSON，
		// 用户连填 Key 的地方都没有。
		if s.cfg.WebUI && (r.URL.Path == "/" || r.URL.Path == adminUIPath) &&
			(r.Method == http.MethodGet || r.Method == http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}

		value, src := credentialOf(r)
		if value == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vidlink"`)
			// 没带 Key 的人最可能就是想试一下：直接把公共 Key 告诉他。
			// 这比"缺少 API Key"有用得多——后者会让人以为必须先注册。
			msg := missingKeyHint
			if k := strings.TrimSpace(s.cfg.PublicKey); k != "" {
				msg += fmt.Sprintf("；也可以直接用公共 Key「%s」免费试用（每 IP 每日 %.4g 配额）",
					k, s.publicQ.Limit())
			}
			writeError(w, core.Errf(core.KindForbidden, "", "auth", "%s", msg))
			return
		}
		acct, credErr := s.accountForCredential(value, src)
		if credErr != nil {
			credErr.write(w)
			return
		}
		if acct.Disabled {
			writeError(w, core.Errf(core.KindForbidden, "", "auth",
				"账号已停用，请联系管理员"))
			return
		}

		// 按 Key 的串行闸门**只作用于计量端点**。
		//
		// 它的目的是防止单个账号打满全局解析槽位，只有真正解析的请求才会
		// 占用槽位。如果连 /v1/usage、/v1/admin/* 这类读接口也一起串行化，
		// 客户端在解析进行中查一次用量就会拿到 429——那是纯粹的误伤。
		// 媒体代理同理：一次下载会长时间占着这个槽位。
		ep, metered := s.endpointOf(specs, r.Method, r.URL.Path)
		if metered {
			// 公共账号的闸门必须按 **IP** 而不是按 Key：Key 是共享的，
			// 按 Key 串行会让"同时只有一个免费用户能解析"——那显然不对。
			gateKey := acct.Key
			if acct.PublicAccount {
				gateKey = "pub:" + s.clientIP(r)
			}
			release, err := s.gate.AcquireKey(gateKey)
			if err != nil {
				w.Header().Set("Retry-After", "1")
				writeErrorStatus(w, http.StatusTooManyRequests, "concurrency_limited",
					"同一个 API Key 同时只允许一个解析请求；请串行调用，或联系管理员提高上限")
				return
			}
			defer release()
		}

		// 预授权：解析前我们还不知道是哪家平台，先按该端点**最贵**的
		// 平台档位检查一次。这样配额不足的账号不会让我们白跑上游，
		// 也不会出现"解析完了才发现配额不够"。
		// 真正扣减配额按实际平台结算，所以这里只是上限检查，不会多扣。
		if metered {
			if need := s.quotaTable.MaxCoefficient(ep); need > 0 {
				var err error
				if acct.PublicAccount {
					// 公共账号的额度在每 IP 的日限额里，与账本余额无关
					err = s.publicQ.Check(s.clientIP(r), need)
				} else {
					err = s.accounts.Check(acct.Key, need)
				}
				if err != nil {
					s.writeQuotaError(w, err)
					return
				}
			}
		}

		ctx := context.WithValue(r.Context(), ctxKeyAccount{}, acct)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ctxKeyAdmin 标记"这次请求的管理 Key 校验已通过"。
//
// 用上下文标记而不是把管理 Key 放进 context：handler 只需要知道
// "验过了"，不需要再拿到那个密钥——拿到就多一处可能被打印/落日志的地方。
type ctxKeyAdmin struct{}

// adminAuthed 报告本次请求是否已通过管理 Key 校验。
func adminAuthed(ctx context.Context) bool {
	ok, _ := ctx.Value(ctxKeyAdmin{}).(bool)
	return ok
}

// serveAdminKey 是管理面的鉴权：明文管理 Key，或管理签名（adm_…）。
//
// 四条设计约束：
//
//   - **管理 Key 不是账号。** 它不查账本、不看账本的停用状态、不扣配额、
//     不占按 Key 的解析闸门。它唯一的能力就是调用 /v1/admin/*；
//     反过来，账本里的账号无论怎么改都拿不到管理权限（账号没有权限位）。
//   - **没配就永远失败。** 未设置 VIDLINK_ADMIN_KEY 时这里恒返回 403 并
//     说明原因。刻意不用 404：管理路由确实存在，装作不存在只会让运维
//     在"是不是路径写错了"上白花时间。
//   - **常量时间比较。** 管理 Key 是服务级凭据，普通字符串比较会因为
//     提前返回而泄漏前缀信息，长期看是可被逐字节试探的旁路。
//   - **URL 里只认签名。** 管理面板要把接口链接给出去（右键复制、新标签页
//     打开），URL 里放明文管理 Key 等于把服务级凭据写进浏览器历史；
//     所以这里同样只接受 adm_ 签名，明文只能走请求头。
func (s *Server) serveAdminKey(next http.Handler, w http.ResponseWriter, r *http.Request) {
	want := s.adminKey()
	if want == "" {
		writeError(w, core.Errf(core.KindForbidden, "", "auth",
			"管理接口未启用：未配置管理 Key（环境变量或 .vl 里的 VIDLINK_ADMIN_KEY）"))
		return
	}
	value, src := credentialOf(r)
	if value == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="vidlink-admin"`)
		writeError(w, core.Errf(core.KindForbidden, "", "auth",
			"缺少管理 Key：请通过 X-API-Key 头或 Authorization: Bearer 头提供管理凭据"+
				"（明文管理 Key，或 GET /v1/admin/sign 签发的 adm_ 签名）"))
		return
	}
	switch {
	case src == credQuerySigned:
		// URL 里只收签名：明文管理 Key 进了 URL 就会长期留在浏览器历史、
		// 隧道与反代日志里——而它是服务级凭据。
		if !looksSigned(value) {
			forbidden("key_format", queryKeyHint).write(w)
			return
		}
		if credErr := s.adminFromSigned(value); credErr != nil {
			credErr.write(w)
			return
		}
	case constantTimeEqual(value, want):
		// 明文管理 Key：只在请求头里接受
	case looksSigned(value):
		if credErr := s.adminFromSigned(value); credErr != nil {
			credErr.write(w)
			return
		}
	default:
		writeError(w, core.Errf(core.KindForbidden, "", "auth", "管理 Key 无效"))
		return
	}
	next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyAdmin{}, true)))
}

// gateErr 把并发闸门的错误翻译成带分类的领域错误。
//
// 不翻译的话它们会落进 KindOf 的 default 分支变成 500，
// 让"主动卸载"看起来像"服务坏了"——监控会误报，运维会白查。
// 排队满/排队超时是 503（服务是好的，只是现在不接），
// 客户端自己要走了则是 499 语义，这里给 503 即可（响应多半也没人读）。
func gateErr(err error) error {
	switch {
	case errors.Is(err, gate.ErrQueueFull), errors.Is(err, gate.ErrQueueTimeout),
		errors.Is(err, context.Canceled):
		return core.E(core.KindUnavailable, "", "gate", err.Error(), err)
	case errors.Is(err, context.DeadlineExceeded):
		return core.E(core.KindTimeout, "", "gate", err.Error(), err)
	default:
		return err
	}
}

// endpointOf 找出请求对应的配额端点。
//
// **方法与路径都要匹配**：只看路径会把 `POST /v1/links` 也当成计量端点，
// 于是"方法不对"的请求先撞上配额预授权，返回 429 而不是 405——
// 客户端会以为要等配额，实际是它自己用错了方法。
func (s *Server) endpointOf(specs []routeSpec, method, path string) (quota.Endpoint, bool) {
	for _, rt := range specs {
		if rt.endpoint != "" && rt.method == method && matchPath(rt.path, path) {
			return rt.endpoint, true
		}
	}
	return "", false
}

// writeQuotaError 把配额错误映射成合适的状态码。
//
// 刻意**不用 402 Payment Required**：那个状态码字面上就是"需要付款"，
// 会把一个"配额用完"的内部状态描述成商业交易。配额是一种用量上限，
// 与金额无关，用 429 语义准确得多（配额只是用量额度）。
// consumePublicQuota 结算一次公共账号的消耗：扣每 IP 日额度 + 只记账不扣余额。
//
// 记账（RecordUsage）是为了让管理面能看到"公共入口被用了多少"；
// 不扣账本余额是因为公共 Key 共享，余额对它没有意义。
func (s *Server) consumePublicQuota(w http.ResponseWriter, r *http.Request,
	acct account.Account, units float64, detail string) {
	ip := s.clientIP(r)
	snap, err := s.publicQ.Charge(ip, units)
	if err != nil {
		// 预授权已经查过额度，这里是并发下的兜底
		s.log.Warn("公共账号额度扣减失败", "request_id", requestID(r.Context()),
			"ip", ip, "units", units, "err", err)
		s.writeQuotaError(w, err)
		return
	}
	if err := s.accounts.RecordUsage(acct.Key, units, "public@"+ip+" "+detail); err != nil {
		// 记账失败不影响已经成功的解析：额度已经扣了，用户不该因此拿不到结果
		s.log.Error("公共账号用量记账失败", "request_id", requestID(r.Context()),
			"key", acct.Masked(), "units", units, "err", err)
	}
	setQuotaHeaders(w, units, snap.Remaining)
}

func (s *Server) writeQuotaError(w http.ResponseWriter, err error) {
	var insuf account.ErrQuotaExhausted
	var pub publicq.ErrDailyExhausted
	switch {
	case errors.As(err, &pub):
		writeErrorStatus(w, http.StatusTooManyRequests, "public_quota_exhausted",
			fmt.Sprintf("今日免费额度已用完（每 IP 每日 %.4g）：本次需要 %.2f，剩余 %.2f；"+
				"额度过期后自动重置。需要更多配额请向管理员申请独立 Key。"+
				"另外：与其换 IP 刷额度，不如自己部署一个——单个静态二进制约 7 MB、"+
				"常驻内存约 8 MB，树莓派/旧手机都能跑，见 %s",
				s.publicQ.Limit(), pub.Need, pub.Have, repoURL))
	case errors.As(err, &insuf):
		writeErrorStatus(w, http.StatusTooManyRequests, "quota_exhausted",
			fmt.Sprintf("配额已用尽：本次最多需要 %.2f，当前剩余 %.2f；"+
				"配额由管理员分配，请联系管理员调整", insuf.Need, insuf.Have))
	case errors.Is(err, account.ErrDisabled):
		writeError(w, core.Errf(core.KindForbidden, "", "auth", "账号已停用"))
	default:
		writeError(w, core.E(core.KindInternal, "", "quota", "配额处理失败", err))
	}
}

// consumeQuota 在解析成功后结算，并把消耗与剩余回写在响应头里。
//
// **只有成功才扣减配额**：上游超时、内容不存在、限流一律不扣。
// 上游抖动不是客户的问题，计入用量会直接变成投诉。
//
// 结算本身失败**不能**把已经成功的响应改成 500——结果客户已经拿到了。
// 记 error 日志让运维介入，比让客户白等一次更合理。
//
// 免校验模式下直接返回：不扣配额，也不回写 X-Quota-* 头。
// 头里写 0 会让人以为"还有配额这回事，只是这次没用掉"，干脆不写。
func (s *Server) consumeQuota(w http.ResponseWriter, r *http.Request, v *core.Video, ep quota.Endpoint, items int) {
	s.consumeQuotaFor(w, r, v.Platform, ep, items)
}

// consumeQuotaFor 按"平台 + 端點 + 条数"结算，返回实际扣减的 units。
//
// 第二个返回值 ok 表示"确实发生了一次扣减"：免校验模式与缺少账号上下文时
// 返回 false——调用方据此决定要不要在响应里写 cost 字段
// （写 0 会让人以为"有配额这回事、只是这次没花钱"）。
func (s *Server) consumeQuotaFor(w http.ResponseWriter, r *http.Request,
	platform core.Platform, ep quota.Endpoint, items int) (float64, bool) {
	if s.cfg.IsEase() {
		return 0, false
	}
	// 按条计价的端点（search/batch）在"一条都没成功"时不结算：
	// quota.Consume 会把 items<1 夹成 1，这里必须先挡住，
	// 否则"搜不到"也要扣一份钱。
	if items <= 0 {
		return 0, false
	}
	acct, ok := accountFrom(r.Context())
	if !ok {
		return 0, false
	}
	units, err := s.quotaTable.Consume(ep, platform, items, acct.Multiplier)
	if err != nil {
		s.log.Error("配额计量失败：无法计算系数乘积", "request_id", requestID(r.Context()),
			"endpoint", string(ep), "platform", string(platform), "err", err)
		return 0, false
	}
	// 流水里的说明要能让人看懂"这一笔花在哪"：端点 + 平台 + 条数
	detail := fmt.Sprintf("%s/%s ×%d", ep, platform, items)

	if acct.PublicAccount {
		s.consumePublicQuota(w, r, acct, units, detail)
		return units, true
	}

	rc, err := s.accounts.Consume(acct.Key, units, detail)
	if err != nil {
		s.log.Error("配额计量失败：扣减未成功", "request_id", requestID(r.Context()),
			"key", acct.Masked(), "units", units, "err", err)
		return units, false
	}
	setQuotaHeaders(w, rc.Units, rc.Quota)
	return rc.Units, true
}

func setQuotaHeaders(w http.ResponseWriter, cost, balance float64) {
	w.Header().Set("X-Quota-Consumed", strconv.FormatFloat(cost, 'f', -1, 64))
	w.Header().Set("X-Quota-Remaining", strconv.FormatFloat(balance, 'f', -1, 64))
}

// --- 业务端点 ---

// parseInput 从 query 里取出待解析的输入：url 优先，其次 platform + id。
func parseInput(r *http.Request) (target, platform, id string, err error) {
	if u := strings.TrimSpace(r.URL.Query().Get("url")); u != "" {
		return u, "", "", nil
	}
	platform = strings.TrimSpace(r.URL.Query().Get("platform"))
	id = strings.TrimSpace(r.URL.Query().Get("id"))
	if platform != "" || id != "" {
		if platform == "" || id == "" {
			return "", "", "", fmt.Errorf("platform 与 id 需要同时提供")
		}
		return "", platform, id, nil
	}
	return "", "", "", fmt.Errorf("缺少 url 参数（或 platform + id）")
}

// resolve 执行一次解析，并占用一个**全局解析槽位**。
//
// 槽位在每次解析时获取，而不是整个 HTTP 请求只获取一次——
// 否则批量的 20 条会绕过全局上限。
func (s *Server) resolve(r *http.Request, target, platform, id string) (*core.Video, error) {
	release, err := s.gate.AcquireTask(r.Context())
	if err != nil {
		return nil, gateErr(err)
	}
	defer release()

	if platform != "" {
		return s.svc.ParseByID(r.Context(), platform, id)
	}
	return s.svc.Parse(r.Context(), target)
}

// handleInfo 是 info 档：元信息 + 档位列表，不含任何直链。
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	target, platform, id, err := parseInput(r)
	if err != nil {
		writeError(w, core.BadInput("", "%v", err))
		return
	}
	v, err := s.resolve(r, target, platform, id)
	if err != nil {
		s.writeServiceError(w, err)
		return
	}
	s.consumeQuota(w, r, v, quota.EndpointInfo, 1)
	writeJSON(w, http.StatusOK, tier.NewInfoFor(v, r.URL.Query().Get("quality")))
}

// handleLinks 是 links 档：只给直链，不给任何内容元信息。
//
// 取流前先让平台校验 ID 是否"足够具体"（core.LinkIDChecker）：
// 荐片一部剧有很多集，只给影片 ID 不知道该给哪一集——这种请求必须在
// 发上游之前就失败，否则用户会以为拿到的第 1 集就是他点的那一集。
func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	target, platform, id, err := parseInput(r)
	if err != nil {
		writeError(w, core.BadInput("", "%v", err))
		return
	}
	if platform != "" {
		if err := s.svc.CheckLinkID(platform, id); err != nil {
			writeError(w, err)
			return
		}
	}
	v, err := s.resolve(r, target, platform, id)
	if err != nil {
		s.writeServiceError(w, err)
		return
	}
	out, err := tier.NewLinks(v, r.URL.Query().Get("quality"))
	if err != nil {
		// 清晰度不存在属于**输入问题**，而且此刻还没扣减配额——
		// 客户改个参数重试即可，不花配额。
		writeError(w, core.BadInput(v.Platform, "%v", err))
		return
	}
	s.consumeQuota(w, r, v, quota.EndpointLinks, 1)
	writeJSON(w, http.StatusOK, out)
}

// handleDetail 是 detail 档：元信息 + 全部档位的直链。
func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	target, platform, id, err := parseInput(r)
	if err != nil {
		writeError(w, core.BadInput("", "%v", err))
		return
	}
	v, err := s.resolve(r, target, platform, id)
	if err != nil {
		s.writeServiceError(w, err)
		return
	}
	s.consumeQuota(w, r, v, quota.EndpointDetail, 1)
	writeJSON(w, http.StatusOK, tier.NewDetailFor(v, r.URL.Query().Get("quality")))
}

// --- 批量 ---

type batchLinksRequest struct {
	URLs    []string `json:"urls"`
	Text    string   `json:"text"`
	Quality string   `json:"quality"`
}

type batchItem struct {
	Input string      `json:"input"`
	Links *tier.Links `json:"links,omitempty"`
	Error *itemError  `json:"error,omitempty"`

	// platform 不导出：只用于内部计量，不污染响应
	platform core.Platform
	ok       bool
}

type itemError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type batchLinksResponse struct {
	Total   int         `json:"total"`
	Success int         `json:"success"`
	Failed  int         `json:"failed"`
	Cost    float64     `json:"cost"`
	Results []batchItem `json:"results"`
}

// handleBatchLinks 是批量档：只给直链，条数受限，**不支持抖音**。
//
// 配额计量按**成功条数**结算。失败的那几条不扣配额——这与"逐条调 links"的
// 预期一致，也避免把上游抖动转嫁成客户的配额消耗。
//
// 抖音在**解析前**就被挡掉：先用注册表路由出平台（纯本地判断，无上游调用），
// 不支持批量的直接生成错误，不浪费上游配额，也不浪费 IP 维度的风控额度。
func (s *Server) handleBatchLinks(w http.ResponseWriter, r *http.Request) {
	// 免校验模式没有账号上下文，也就没有倍率与结算；其余流程完全一致。
	ease := s.cfg.IsEase()
	var acct account.Account
	if !ease {
		a, ok := accountFrom(r.Context())
		if !ok {
			writeError(w, core.E(core.KindInternal, "", "quota", "缺少账号上下文", nil))
			return
		}
		acct = a
	}

	var req batchLinksRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, core.BadInput("", "请求体不是合法 JSON: %v", err))
		return
	}

	urls := req.URLs
	if len(urls) == 0 && strings.TrimSpace(req.Text) != "" {
		for _, line := range strings.Split(req.Text, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				urls = append(urls, line)
			}
		}
	}
	if len(urls) == 0 {
		writeError(w, core.BadInput("", "urls 与 text 不能同时为空"))
		return
	}
	// 最少 5 条：让"批量更省配额"只给真正的批量。1~4 条应走单条 links，
	// 否则一个客户用 1 条也能省 25% 配额，这个下调就失去意义。
	if len(urls) < s.cfg.BatchMin {
		writeError(w, core.BadInput("", "批量最少 %d 条（当前 %d 条）；"+
			"少于 %d 条请改用 GET /v1/links", s.cfg.BatchMin, len(urls), s.cfg.BatchMin))
		return
	}
	if len(urls) > s.cfg.BatchMax {
		writeError(w, core.BadInput("", "批量最多 %d 条（当前 %d 条）", s.cfg.BatchMax, len(urls)))
		return
	}

	resp := batchLinksResponse{Total: len(urls), Results: make([]batchItem, len(urls))}

	// 每条各自抢一个全局槽位，但整个请求只占**一个** Key 槽位——
	// 这正是"批量"的语义（免校验模式下没有 Key 槽位这一层）。
	conc := s.cfg.Service.BatchConcurrency
	if conc > len(urls) {
		conc = len(urls)
	}
	if conc < 1 {
		conc = 1
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, conc)
	for i := range urls {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			resp.Results[i] = s.batchOne(r, urls[i], req.Quality)
		}(i)
	}
	wg.Wait()

	// 汇总 + 配额计量
	var total float64
	for i := range resp.Results {
		it := &resp.Results[i]
		if it.Error != nil {
			resp.Failed++
			continue
		}
		if ease {
			// 不计量，但仍要区分"成功/失败"，否则总数对不上
			resp.Success++
			continue
		}
		units, err := s.quotaTable.Consume(quota.EndpointBatchLinks, it.platform, 1, acct.Multiplier)
		if err != nil {
			// 平台不支持批量（抖音）：把这条转成明确错误，且不消耗配额
			it.Error = &itemError{Kind: "unsupported", Message: err.Error()}
			it.Links = nil
			it.ok = false
			resp.Failed++
			continue
		}
		resp.Success++
		total += units
	}
	resp.Cost = total

	if !ease && total > 0 {
		if acct.PublicAccount {
			s.consumePublicQuota(w, r, acct, total,
				fmt.Sprintf("batch_links ×%d", resp.Success))
		} else if rc, err := s.accounts.Consume(acct.Key, total,
			fmt.Sprintf("batch_links ×%d", resp.Success)); err == nil {
			setQuotaHeaders(w, rc.Units, rc.Quota)
		} else {
			s.log.Error("批量配额计量失败", "request_id", requestID(r.Context()),
				"key", acct.Masked(), "units", total, "err", err)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// batchOne 处理批量里的一条。
func (s *Server) batchOne(r *http.Request, input, quality string) batchItem {
	it := batchItem{Input: input}

	// 先做纯本地的平台判定：不支持批量的平台不必浪费一次上游请求
	if u, err := urlx.Parse(input); err == nil {
		if ext, rerr := s.svc.Registry().Route(u); rerr == nil {
			if reason := quota.BatchUnsupportedReason(ext.Name()); reason != "" {
				it.Error = &itemError{Kind: "unsupported", Message: reason}
				return it
			}
		}
	}

	release, err := s.gate.AcquireTask(r.Context())
	if err != nil {
		gerr := gateErr(err)
		it.Error = &itemError{Kind: kindName(core.KindOf(gerr)), Message: gerr.Error()}
		return it
	}
	defer release()

	v, err := s.svc.Parse(r.Context(), input)
	if err != nil {
		it.Error = &itemError{Kind: kindName(core.KindOf(err)), Message: err.Error()}
		return it
	}
	links, lerr := tier.NewLinks(v, quality)
	if lerr != nil {
		it.Error = &itemError{Kind: "bad_input", Message: lerr.Error()}
		return it
	}
	it.Links = &links
	it.platform = v.Platform
	it.ok = true
	return it
}

// --- 不计配额的端点 ---

// handleUsage 返回本 Key 的配额、用量、账号倍率与各端点系数。不消耗配额。
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	acct, ok := accountFrom(r.Context())
	if !ok {
		writeError(w, core.Errf(core.KindForbidden, "", "auth", "缺少 API Key"))
		return
	}
	// 公共账号：没有"我的余额"这回事，返回的是**这个 IP 今天**的额度。
	// 形状保持兼容（quota/used 仍在），客户端不必为它写特例；
	// 另外给一个 public 段说明口径。
	if acct.PublicAccount {
		snap := s.publicQ.Snapshot(s.clientIP(r))
		writeJSON(w, http.StatusOK, map[string]any{
			"name":       acct.Name,
			"public":     true,
			"unit":       unitName,
			"quota":      snap.Remaining, // 兼容字段：本 IP 今日剩余
			"used":       snap.Used,
			"calls":      acct.Calls, // 公共账号的累计调用（所有人合计）
			"multiplier": acct.Multiplier,
			"daily":      snap,
			"rates":      s.allRates(),
			"rates_note": ratesNote,
			"proxy": map[string]any{
				"rate":            fmt.Sprintf("%.4g 配额/MiB", s.proxyRate()),
				"rate_per_mib":    s.proxyRate(),
				"unit_bytes":      quota.ProxyUnitBytes,
				"platform_factor": false,
				"note": fmt.Sprintf("媒体代理按传输体积计费：实扣 = 体积(MiB) × %.4g × 账号倍率"+
					"（所有账号同一费率）", s.proxyRate()),
			},
			"limits": map[string]any{
				"per_key_concurrency":  s.gate.Options().PerKey,
				"global_concurrency":   s.gate.Options().Global,
				"batch_min":            s.cfg.BatchMin,
				"batch_max":            s.cfg.BatchMax,
				"search_limit_max":     searchMaxLimit,
				"search_page_max":      searchMaxPage,
				"searchable_platforms": s.svc.SearchablePlatforms(),
			},
			"note": "公共账号：Key 公开，额度按「每 IP 每日」计算，不使用账本余额；" +
				"不提供账单，需要账单请申请独立 Key",
		})
		return
	}

	usage := map[string]any{
		"name":       acct.Name,
		"unit":       unitName,
		"quota":      acct.Quota,
		"used":       acct.Used,
		"calls":      acct.Calls,
		"multiplier": acct.Multiplier,
		"rates":      s.allRates(),
		"rates_note": ratesNote,
		// 代理按体积计费，与上面的平台系数表不同源，单独给一条
		"proxy": map[string]any{
			"rate":            fmt.Sprintf("%.4g 配额/MiB", s.proxyRate()),
			"rate_per_mib":    s.proxyRate(),
			"unit_bytes":      quota.ProxyUnitBytes,
			"platform_factor": false,
			"note": fmt.Sprintf("媒体代理按传输体积计费：实扣 = 体积(MiB) × %.4g × 你的账号倍率"+
				"（所有账号同一费率）", s.proxyRate()),
		},
		"limits": map[string]any{
			"per_key_concurrency":  s.gate.Options().PerKey,
			"global_concurrency":   s.gate.Options().Global,
			"batch_min":            s.cfg.BatchMin,
			"batch_max":            s.cfg.BatchMax,
			"search_limit_max":     searchMaxLimit,
			"search_page_max":      searchMaxPage,
			"searchable_platforms": s.svc.SearchablePlatforms(),
		},
	}
	// 每日签到：**始终**给出这一段，用 enabled 表明这个账号能不能签。
	//
	// 用"字段不存在"表示"不能签"会让客户端只能靠猜：拿到 0 也不知道是
	// 没开放还是额度恰好为 0。宁可多一个布尔。
	usage["checkin"] = map[string]any{
		"enabled":          acct.DailyGrant > 0,
		"daily":            acct.DailyGrant,
		"cap":              acct.GrantCap, // 0 = 不封顶
		"checked_in_today": acct.DailyGrant > 0 && acct.GrantDay == account.DayKey(time.Now()),
		"last_checkin_day": acct.GrantDay,
		"endpoint":         "POST /v1/checkin",
		"formula":          "余额 = min(余额 + 每日签到额度, 停止增加界限)，每天一次",
		"note": "签到是**主动**的：服务不会在每天第一次调用时自动补额，" +
			"只有调 /v1/checkin 才会加",
	}
	writeJSON(w, http.StatusOK, usage)
}

// unitName 是配额单位的对外名称。
//
// 刻意只在**文案**里出现，API 字段一律用单位无关的 quota/used/cost。
// 这样将来改名（配额→积分→品牌名）只改这一个常量，
// 不需要动 API 契约，也不要求所有客户改代码。
const unitName = "配额"

// repoURL 是项目仓库；文案里用它指向"自己部署"的说明。
const repoURL = "https://github.com/wzmwayne/vidlink"

// ratesNote 说明系数是**当前部署**的实时值。
//
// 用户文档里印的是仓库默认值，而部署者可以随时改价——不写这句话，
// 用户会拿文档里的数字来对账，然后以为被多扣了。
const ratesNote = "系数由部署者配置，这里是当前部署的生效值；" +
	"仓库里的默认值可能与之不同。管理接口见 " + repoURL + "/blob/master/docs/API.md"

// allRates 返回四个平台各端点的完整系数。
func (s *Server) allRates() map[string]any {
	out := map[string]any{}
	for _, p := range quota.AllPlatforms {
		m := map[string]any{}
		for k, v := range s.quotaTable.Coefficients(p) {
			m[string(k)] = v
		}
		if reason := quota.BatchUnsupportedReason(p); reason != "" {
			m[string(quota.EndpointBatchLinks)] = map[string]any{
				"unsupported": true, "reason": reason,
			}
		}
		if reason := quota.SearchUnsupportedReason(p); reason != "" {
			m[string(quota.EndpointSearch)] = map[string]any{
				"unsupported": true, "reason": reason,
			}
		}
		out[string(p)] = m
	}
	return out
}

// handlePlatforms 返回平台清单与各端点系数。不消耗配额。
//
// 免校验模式下**不给 rates/unit**：那时不存在配额这回事，
// 继续返回一份"每次调用扣多少"的表只会让人以为还有计量。
func (s *Server) handlePlatforms(w http.ResponseWriter, r *http.Request) {
	ease := s.cfg.IsEase()
	pf := s.svc.Platforms()
	out := make([]map[string]any, 0, len(pf))
	for _, p := range pf {
		plat := core.Platform(p.Name)
		m := map[string]any{
			"name":          p.Name,
			"hosts":         p.Hosts,
			"supports_id":   p.ByID,
			"has_cookie":    p.HasAuth,
			"batch":         quota.BatchUnsupportedReason(plat) == "",
			"batch_reason":  quota.BatchUnsupportedReason(plat),
			"search":        p.Search,
			"search_reason": quota.SearchUnsupportedReason(plat),
		}
		if !ease {
			m["rates"] = s.quotaTable.Coefficients(plat)
		}
		out = append(out, m)
	}
	body := map[string]any{
		"platforms": out,
		"mode":      s.modeName(),
		"limits": map[string]any{
			"batch_min":            s.cfg.BatchMin,
			"batch_max":            s.cfg.BatchMax,
			"search_default_limit": searchDefaultLimit,
			"search_limit_max":     searchMaxLimit,
			"search_page_max":      searchMaxPage,
			"searchable_platforms": s.svc.SearchablePlatforms(),
		},
	}
	if !ease {
		body["unit"] = unitName
	}
	writeJSON(w, http.StatusOK, body)
}

// modeName 返回当前运行模式，给 /v1/health、/v1/platforms 与导航页共用。
//
// 让"是否在免校验模式"成为一个可观测的事实：客户端可以据此决定要不要带 Key，
// 运维也能一眼看出这台机器有没有身份校验。
func (s *Server) modeName() string {
	if s.cfg.IsEase() {
		return "ease"
	}
	return "account"
}
