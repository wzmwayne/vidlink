package bilibili

import (
	"context"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"vidlink/internal/core"
	"vidlink/internal/jsonx"
)

// 搜索的条数上限：B 站 page_size 最大 50，端点层已按 1..50 校验，
// 这里再夹一次是为了"提取器自己拿出去用"（CLI/测试）时也安全。
const (
	searchMinLimit = 1
	searchMaxLimit = 50
	searchMaxPage  = 50
)

// Search 按关键词搜索稿件（search_type=video）。
//
// 通道策略：**先用非签名通道**。2026-09 实测匿名（UA + Referer）即可返回
// code:0 与完整结果；若上游收紧到强制 WBI（历史上发生过），
// 用 wbi.Manager 重签一次走 WBI 通道再试，两条通道共用同一套解析逻辑。
//
// 结果只含元信息 + bvid，**不含任何直链**：拿链接要再调 /v1/links。
func (e *Extractor) Search(ctx context.Context, q core.SearchQuery) (*core.SearchResult, error) {
	limit := q.Limit
	if limit < searchMinLimit {
		limit = searchMinLimit
	}
	if limit > searchMaxLimit {
		limit = searchMaxLimit
	}
	page := q.Page
	if page < 1 {
		page = 1
	}
	if page > searchMaxPage {
		page = searchMaxPage
	}

	params := url.Values{}
	params.Set("search_type", "video")
	params.Set("keyword", q.Keyword)
	params.Set("page", strconv.Itoa(page))
	params.Set("page_size", strconv.Itoa(limit))
	// 显式写死排序，避免上游改默认值后"同样的关键词、不同的结果"。
	params.Set("order", "totalrank")

	headers := e.headers()
	body, _, err := e.d.Client.GetBytes(ctx, e.ep.Search+"?"+params.Encode(), headers, 0)
	if err != nil {
		return nil, core.Upstream(core.PlatformBilibili, "search", err)
	}
	res, perr := parseSearchResponse(body, q.Keyword, page, limit)
	if perr == nil {
		return res, nil
	}

	// 风控才值得换通道重试：其它错误（参数错、上游 5xx）换通道也一样。
	switch core.KindOf(perr) {
	case core.KindRateLimit, core.KindForbidden:
	default:
		return nil, perr
	}

	query, serr := e.d.WBI.Sign(ctx, params)
	if serr != nil {
		return nil, perr
	}
	body, _, err = e.d.Client.GetBytes(ctx, e.ep.SearchWBI+"?"+query, headers, 0)
	if err != nil {
		return nil, perr
	}
	if res, werr := parseSearchResponse(body, q.Keyword, page, limit); werr == nil {
		return res, nil
	}
	return nil, perr
}

// searchTagRe 匹配搜索结果标题里的高亮标签（`<em class="keyword">…</em>`）。
var searchTagRe = regexp.MustCompile(`<[^>]*>`)

// cleanSearchText 去掉高亮标签并反转义 HTML 实体。
//
// B 站返回的标题形如 `【全748集】<em class="keyword">Python</em>教程 &amp; …`，
// 直接透传会让客户端渲染出标签或显示 `&amp;`。
func cleanSearchText(s string) string {
	if s == "" {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(searchTagRe.ReplaceAllString(s, "")))
}

// parseClock 把 "12:34" / "1:02:03" 转成秒。
func parseClock(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var total float64
	for _, part := range strings.Split(s, ":") {
		n, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return total
}

// parseSearchResponse 解析搜索响应（纯函数，便于离线单测）。
func parseSearchResponse(body []byte, keyword string, page, limit int) (*core.SearchResult, error) {
	node, err := jsonx.Unmarshal(body)
	if err != nil {
		return nil, core.E(core.KindUpstream, core.PlatformBilibili, "search",
			"响应不是合法 JSON", err)
	}
	if code := jsonx.Int(node, "code"); code != 0 {
		return nil, classifyAPIError(code, jsonx.String(node, "message"))
	}

	out := &core.SearchResult{
		Platform: core.PlatformBilibili,
		Keyword:  keyword,
		Page:     page,
		Limit:    limit,
		Total:    int(jsonx.Int(node, "data.numResults")),
	}
	// 零结果时 result 可能是 null / 空对象，Slice 会安全返回 nil。
	for _, it := range jsonx.Slice(node, "data.result") {
		id := jsonx.String(it, "bvid")
		if id == "" {
			continue
		}
		item := core.SearchItem{
			Platform: core.PlatformBilibili,
			Type:     "video",
			ID:       id,
			Title:    cleanSearchText(jsonx.String(it, "title")),
			Desc:     cleanSearchText(jsonx.String(it, "description")),
			Cover:    normalizeURL(jsonx.String(it, "pic")),
			URL:      "https://www.bilibili.com/video/" + id,
			Author: core.Author{
				ID:   strconv.FormatInt(jsonx.Int(it, "mid"), 10),
				Name: jsonx.String(it, "author"),
			},
			Duration: parseClock(jsonx.String(it, "duration")),
		}
		if ts := jsonx.Int(it, "pubdate"); ts > 0 {
			item.PublishedAt = time.Unix(ts, 0).UTC().Format(time.RFC3339)
		}
		view, danmaku := jsonx.Int(it, "play"), jsonx.Int(it, "danmaku")
		if view > 0 || danmaku > 0 {
			item.Stats = &core.Stats{
				View: view, Danmaku: danmaku, Duration: int(item.Duration),
			}
		}
		out.Items = append(out.Items, item)
	}
	if out.Items == nil {
		out.Items = []core.SearchItem{}
	}
	return out, nil
}
