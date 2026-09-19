package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"vidlink/internal/core"
	"vidlink/internal/quota"
	"vidlink/internal/tier"
)

// 搜索参数的上限。刻意与上游一致（B 站 page_size ≤ 50、page ≤ 50），
// 不做成环境变量：这些是上游的硬限制，不是运营旋钮。
const (
	searchDefaultLimit = 20
	searchMaxLimit     = 50
	searchMaxPage      = 50
	searchMaxKeyword   = 64 // rune 数
)

// handleSearch 是 search 档：按关键词搜索，返回"元信息 + id"，不含任何直链。
//
// 计价是**按返回条数**（默认 0.25/条）：没有结果就一分不扣，
// 扣多少与"拿到几条"严格对应。因为条数在请求时还不确定（上游可能少于 limit），
// 这里的做法是：先按 limit 做一次上限预授权，拿到结果后按实际条数结算。
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	platform := strings.ToLower(strings.TrimSpace(q.Get("platform")))
	keyword := strings.TrimSpace(q.Get("keyword"))
	if keyword == "" {
		// `q` 是给"顺手敲一条命令"的别名；文档里以 keyword 为准。
		keyword = strings.TrimSpace(q.Get("q"))
	}

	if platform == "" {
		writeError(w, core.Unsupported("", "缺少 platform 参数；当前可搜索：%s",
			strings.Join(s.svc.SearchablePlatforms(), ", ")))
		return
	}
	plat := core.Platform(platform)
	if !s.svc.Searchable(plat) {
		reason := quota.SearchUnsupportedReason(plat)
		if reason == "" {
			writeError(w, core.Unsupported(plat, "未知平台 %q；当前可搜索：%s",
				platform, strings.Join(s.svc.SearchablePlatforms(), ", ")))
			return
		}
		writeError(w, core.Unsupported(plat, "平台 %s 不支持搜索：%s", platform, reason))
		return
	}
	if keyword == "" {
		writeError(w, core.BadInput(plat, "缺少 keyword 参数（要搜什么）"))
		return
	}
	if n := len([]rune(keyword)); n > searchMaxKeyword {
		writeError(w, core.BadInput(plat, "关键词过长：%d 个字符（上限 %d）", n, searchMaxKeyword))
		return
	}

	page, err := s.intParam(q.Get("page"), 1, 1, searchMaxPage, "page")
	if err != nil {
		writeError(w, err)
		return
	}
	limit, err := s.intParam(q.Get("limit"), searchDefaultLimit, 1, searchMaxLimit, "limit")
	if err != nil {
		writeError(w, err)
		return
	}

	// 上限预授权：真正扣多少等结果出来再按条数结算。
	if need := s.quotaTable.MaxCoefficient(quota.EndpointSearch) * float64(limit); need > 0 {
		if !s.preauthQuota(w, r, need) {
			return
		}
	}

	res, err := s.svc.Search(r.Context(), platform, keyword, page, limit)
	if err != nil {
		s.writeServiceError(w, err)
		return
	}
	out := tier.NewSearch(res)
	if units, ok := s.consumeQuotaFor(w, r, plat, quota.EndpointSearch, len(out.Items)); ok {
		out.Cost = units
	}
	writeJSON(w, http.StatusOK, out)
}

// intParam 解析一个带范围与默认值的整数 query 参数。
func (s *Server) intParam(raw string, def, min, max int, name string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, core.BadInput("", "%s 必须是整数：%q", name, raw)
	}
	if n < min || n > max {
		return 0, core.BadInput("", "%s 超出范围：%d（允许 %d~%d）", name, n, min, max)
	}
	return n, nil
}

// preauthQuota 按"这次最多可能扣多少"做一次上限检查（不够就直接 429）。
//
// 与中间件里的预授权是同一件事，区别只在"需要多少"要等 handler 解析完参数
// 才知道（搜索按条计价，条数写在 limit 里）。返回 false 表示已经写了错误响应。
func (s *Server) preauthQuota(w http.ResponseWriter, r *http.Request, need float64) bool {
	if s.cfg.IsEase() {
		return true
	}
	acct, ok := accountFrom(r.Context())
	if !ok {
		return true
	}
	var err error
	if acct.PublicAccount {
		err = s.publicQ.Check(s.clientIP(r), need)
	} else {
		err = s.accounts.Check(acct.Key, need)
	}
	if err != nil {
		s.writeQuotaError(w, err)
		return false
	}
	return true
}

// searchHint 是给搜索类接口用的统一提示（当前可搜索平台）。
func (s *Server) searchHint() string {
	return fmt.Sprintf("当前可搜索：%s", strings.Join(s.svc.SearchablePlatforms(), ", "))
}
