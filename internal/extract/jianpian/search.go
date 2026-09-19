package jianpian

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"vidlink/internal/core"
	"vidlink/internal/jsonx"
)

// Search 按关键词搜索影视内容。
//
// 上游固定 **10 条/页**，所以"一次给 20/50 条"要自己拼窗口：
// 请求 (page, limit) → 换算成窗口 [start, start+limit) →
// 取窗口覆盖到的上游页（最多 5 页）→ 合并去重 → 切片。
//
// 合并时按页序、按 id 去重；某一页失败只让结果少几条并给出 Warning，
// 首屏页失败才整单报错（否则用户会因为第 5 页抖动而整单失败）。
func (e *Extractor) Search(ctx context.Context, q core.SearchQuery) (*core.SearchResult, error) {
	limit := clampInt(q.Limit, searchMinLimit, searchMaxLimit)
	page := clampInt(q.Page, 1, searchMaxPage)
	start := (page - 1) * limit

	firstUp := start/pageSize + 1
	lastUp := (start+limit-1)/pageSize + 1

	type pageResult struct {
		items []core.SearchItem
		total int
		err   error
	}
	results := make([]pageResult, lastUp-firstUp+1)

	// 有界并发：一次搜索最多 5 个上游请求，同时也不再对其他请求形成尖峰。
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i := range results {
		up := firstUp + i
		wg.Add(1)
		sem <- struct{}{}
		go func(i, up int) {
			defer wg.Done()
			defer func() { <-sem }()
			items, total, err := e.searchPage(ctx, q.Keyword, up)
			results[i] = pageResult{items: items, total: total, err: err}
		}(i, up)
	}
	wg.Wait()

	if results[0].err != nil {
		return nil, results[0].err
	}

	out := &core.SearchResult{
		Platform: core.PlatformJianpian,
		Keyword:  q.Keyword,
		Page:     page,
		Limit:    limit,
		Total:    results[0].total,
	}
	var all []core.SearchItem
	seen := make(map[string]bool, limit)
	for _, r := range results {
		if r.err != nil {
			out.Warning = "部分结果页拉取失败，本次结果可能少几条：" + r.err.Error()
			continue
		}
		for _, it := range r.items {
			if it.ID == "" || seen[it.ID] {
				continue
			}
			seen[it.ID] = true
			all = append(all, it)
		}
	}

	// 切出请求的窗口：丢掉该页之前的条目，再截断到 limit。
	if off := start % pageSize; off > 0 {
		if off >= len(all) {
			all = nil
		} else {
			all = all[off:]
		}
	}
	if len(all) > limit {
		all = all[:limit]
	}
	out.Items = all
	if out.Items == nil {
		out.Items = []core.SearchItem{}
	}
	return out, nil
}

// searchPage 拉取上游的某一页（10 条）。
func (e *Extractor) searchPage(ctx context.Context, keyword string, page int) ([]core.SearchItem, int, error) {
	q := url.Values{}
	q.Set("key", keyword)
	q.Set("page", strconv.Itoa(page))

	body, code, err := e.d.Client.GetBytes(ctx, e.ep.Search+"?"+q.Encode(), e.headers(ctx), 4<<20)
	if err != nil {
		return nil, 0, core.Upstream(core.PlatformJianpian, "search", err)
	}
	if code < 200 || code >= 300 {
		return nil, 0, core.Errf(core.KindUpstream, core.PlatformJianpian, "search", "HTTP %d", code)
	}
	node, jerr := jsonx.Unmarshal(body)
	if jerr != nil {
		return nil, 0, core.E(core.KindUpstream, core.PlatformJianpian, "search",
			"响应不是合法 JSON", jerr)
	}
	if c := jsonx.Int(node, "code"); c != 1 {
		return nil, 0, classifyAPIError(c, jsonx.String(node, "msg"))
	}

	var items []core.SearchItem
	for _, it := range jsonx.Slice(node, "data") {
		id := strconv.FormatInt(jsonx.Int(it, "id"), 10)
		if id == "0" {
			continue
		}
		items = append(items, e.searchItem(ctx, it, id))
	}
	return items, int(jsonx.Int(node, "total")), nil
}

func (e *Extractor) searchItem(ctx context.Context, it jsonx.Node, id string) core.SearchItem {
	category := jsonx.String(it, "top_category.name")
	item := core.SearchItem{
		Platform: core.PlatformJianpian,
		Type:     contentType(category),
		ID:       id,
		Title:    jsonx.String(it, "title"),
		Desc:     jsonx.String(it, "description"),
		Cover:    e.imageURL(ctx, firstNonEmpty(jsonx.String(it, "thumbnail"), jsonx.String(it, "tvimg"))),
		Author:   core.Author{Name: strings.Join(jsonx.StringList(it, "directors"), " / ")},
		Latest:   jsonx.String(it, "mask"),
		Score:    jsonx.Float(it, "score"),
		Category: category,
		Actors:   jsonx.StringList(it, "actors"),
	}
	// years 是 [{"year":"1999"}] 这种形状，不是数字。
	if years := jsonx.Slice(it, "years"); len(years) > 0 {
		if y, err := strconv.Atoi(strings.TrimSpace(jsonx.String(years[0], "year"))); err == nil {
			item.Year = y
		}
	}
	return item
}

// contentType 判断是电影还是剧集：搜索结果的 top_category.name 是中文分类名。
func contentType(category string) string {
	switch strings.TrimSpace(category) {
	case "电影":
		return "movie"
	case "":
		return "video"
	default:
		return "series"
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
