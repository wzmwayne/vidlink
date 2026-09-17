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

	"vidlink/internal/account"
	"vidlink/internal/core"
	"vidlink/internal/gate"
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

// resolveKey 从请求里取出 API Key。三种传法等价。
func resolveKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if k := r.URL.Query().Get("key"); k != "" {
		return k
	}
	return bearer(r.Header.Get("Authorization"))
}

// withAccount 完成认证、并发闸门与预授权，并把账号放进 context。
//
// 旧版是"配了一组静态 Key，命中即放行"；现在 Key 对应一个**有配额和
// 账号倍率的账号**，所以认证与配额计量是同一件事的两面。
//
// 三件事都放在中间件里而不是各 handler 里，是为了让四个配额端点
// **不可能漏掉**其中任何一件——漏掉预授权就是未计入消耗，漏掉闸门就是没有上限。
func (s *Server) withAccount(next http.Handler, specs []routeSpec) http.Handler {
	public := make(map[string]bool, len(specs))
	for _, rt := range specs {
		if rt.public {
			public[rt.path] = true
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if public[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		// 根路径在开启 WebUI 时也要公开：页面本身不含任何数据（数据全靠
		// 页面里带的 Key 去请求），若不公开，浏览器只会拿到一个 403 JSON，
		// 用户连填 Key 的地方都没有。
		if s.cfg.WebUI && r.URL.Path == "/" &&
			(r.Method == http.MethodGet || r.Method == http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}

		key := resolveKey(r)
		if key == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vidlink"`)
			writeError(w, core.Errf(core.KindForbidden, "", "auth",
				"缺少 API Key：请通过 X-API-Key 头、Authorization: Bearer 头，"+
					"或 ?key= 查询参数提供"))
			return
		}
		acct, ok := s.accounts.Get(key)
		if !ok {
			// 刻意不区分"Key 不存在"与"Key 错误"，避免被用来枚举有效 Key
			writeError(w, core.Errf(core.KindForbidden, "", "auth", "API Key 无效"))
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
			release, err := s.gate.AcquireKey(acct.Key)
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
				if err := s.accounts.Check(acct.Key, need); err != nil {
					s.writeQuotaError(w, err)
					return
				}
			}
		}

		ctx := context.WithValue(r.Context(), ctxKeyAccount{}, acct)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
func (s *Server) writeQuotaError(w http.ResponseWriter, err error) {
	var insuf account.ErrQuotaExhausted
	switch {
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
	if s.cfg.IsEase() {
		return
	}
	acct, ok := accountFrom(r.Context())
	if !ok {
		return
	}
	units, err := s.quotaTable.Consume(ep, v.Platform, items, acct.Multiplier)
	if err != nil {
		s.log.Error("配额计量失败：无法计算系数乘积", "request_id", requestID(r.Context()),
			"endpoint", string(ep), "platform", string(v.Platform), "err", err)
		return
	}
	rc, err := s.accounts.Consume(acct.Key, units)
	if err != nil {
		s.log.Error("配额计量失败：扣减未成功", "request_id", requestID(r.Context()),
			"key", acct.Masked(), "units", units, "err", err)
		return
	}
	setQuotaHeaders(w, rc.Units, rc.Quota)
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
	writeJSON(w, http.StatusOK, tier.NewInfo(v))
}

// handleLinks 是 links 档：只给直链，不给任何内容元信息。
func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, tier.NewDetail(v))
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
		if rc, err := s.accounts.Consume(acct.Key, total); err == nil {
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
	writeJSON(w, http.StatusOK, map[string]any{
		"name":       acct.Name,
		"unit":       unitName,
		"quota":      acct.Quota,
		"used":       acct.Used,
		"calls":      acct.Calls,
		"multiplier": acct.Multiplier,
		"rates":      s.allRates(),
		"limits": map[string]any{
			"per_key_concurrency": s.gate.Options().PerKey,
			"global_concurrency":  s.gate.Options().Global,
			"batch_min":           s.cfg.BatchMin,
			"batch_max":           s.cfg.BatchMax,
		},
	})
}

// unitName 是配额单位的对外名称。
//
// 刻意只在**文案**里出现，API 字段一律用单位无关的 quota/used/cost。
// 这样将来改名（配额→积分→品牌名）只改这一个常量，
// 不需要动 API 契约，也不要求所有客户改代码。
const unitName = "配额"

// allRates 返回四个平台各端点的完整系数。
func (s *Server) allRates() map[string]any {
	out := map[string]any{}
	for _, p := range []core.Platform{
		core.PlatformBilibili, core.PlatformDouyin,
		core.PlatformKuaishou, core.PlatformXiaohongshu,
	} {
		m := map[string]any{}
		for k, v := range s.quotaTable.Coefficients(p) {
			m[string(k)] = v
		}
		if reason := quota.BatchUnsupportedReason(p); reason != "" {
			m[string(quota.EndpointBatchLinks)] = map[string]any{
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
			"name":         p.Name,
			"hosts":        p.Hosts,
			"supports_id":  p.ByID,
			"has_cookie":   p.HasAuth,
			"batch":        quota.BatchUnsupportedReason(plat) == "",
			"batch_reason": quota.BatchUnsupportedReason(plat),
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
			"batch_min": s.cfg.BatchMin,
			"batch_max": s.cfg.BatchMax,
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
