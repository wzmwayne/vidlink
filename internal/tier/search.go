package tier

import "vidlink/internal/core"

// SearchResult 是 search 档的响应。
//
// 口径与 core.SearchResult 一致：**只给元信息 + id，不含任何直链**。
// 拿到 items[].id 之后要再用 info/detail/links 取播放地址——
// 这也是 search 能按"条"计价（0.25/条）的前提。
type SearchResult struct {
	Platform string       `json:"platform"`
	Keyword  string       `json:"keyword"`
	Page     int          `json:"page"`
	Limit    int          `json:"limit"`
	Count    int          `json:"count"`
	Total    int          `json:"total"`
	HasMore  bool         `json:"has_more"`
	Items    []SearchItem `json:"items"`
	// Cost 是本次实际扣减的配额（按返回条数结算），免校验模式下省略。
	Cost    float64 `json:"cost,omitempty"`
	Warning string  `json:"warning,omitempty"`
	Cached  bool    `json:"cached,omitempty"`
	// Next 是下一页的现成参数（方便客户端直接翻页；没有下一页时省略）。
	Next string `json:"next,omitempty"`
}

// SearchItem 是一条搜索结果（等价 info 档的元信息部分 + id）。
type SearchItem struct {
	Platform string `json:"platform"`
	Type     string `json:"type,omitempty"`
	ID       string `json:"id"`
	Title    string `json:"title"`
	Desc     string `json:"desc,omitempty"`
	Cover    string `json:"cover,omitempty"`
	// URL 是可直接用于 /v1/detail?url= 的页面地址（平台没有稳定页面时省略）。
	URL         string  `json:"url,omitempty"`
	Author      Author  `json:"author"`
	Stats       *Stats  `json:"stats,omitempty"`
	Duration    float64 `json:"duration,omitempty"`
	PublishedAt string  `json:"published_at,omitempty"`

	// 影视向字段（荐片），短视频平台为空。
	Latest   string   `json:"latest,omitempty"`
	Finished bool     `json:"finished,omitempty"`
	Score    float64  `json:"score,omitempty"`
	Year     int      `json:"year,omitempty"`
	Category string   `json:"category,omitempty"`
	Actors   []string `json:"actors,omitempty"`
	Episodes int      `json:"episodes,omitempty"`

	// Detail 是取得播放地址/线路/选集的入口提示，避免客户端猜路径。
	Detail string `json:"detail,omitempty"`
}

// NewSearch 把领域结果投影成 search 档。
func NewSearch(r *core.SearchResult) SearchResult {
	if r == nil {
		return SearchResult{Items: []SearchItem{}}
	}
	out := SearchResult{
		Platform: string(r.Platform),
		Keyword:  r.Keyword,
		Page:     r.Page,
		Limit:    r.Limit,
		Total:    r.Total,
		Warning:  r.Warning,
		Cached:   r.Cached,
	}
	for _, it := range r.Items {
		item := SearchItem{
			Platform:    string(it.Platform),
			Type:        it.Type,
			ID:          it.ID,
			Title:       it.Title,
			Desc:        it.Desc,
			Cover:       it.Cover,
			URL:         it.URL,
			Author:      Author{ID: it.Author.ID, Name: it.Author.Name, Avatar: it.Author.Avatar},
			Duration:    it.Duration,
			PublishedAt: it.PublishedAt,
			Latest:      it.Latest,
			Finished:    it.Finished,
			Score:       it.Score,
			Year:        it.Year,
			Category:    it.Category,
			Actors:      it.Actors,
			Episodes:    it.Episodes,
			Detail:      "GET /v1/detail?platform=" + string(it.Platform) + "&id=" + it.ID,
		}
		if it.Stats != nil {
			item.Stats = &Stats{
				View: it.Stats.View, Like: it.Stats.Like, Comment: it.Stats.Comment,
				Collect: it.Stats.Collect, Share: it.Stats.Share,
				Danmaku: it.Stats.Danmaku, Duration: it.Stats.Duration,
			}
		}
		out.Items = append(out.Items, item)
	}
	if out.Items == nil {
		out.Items = []SearchItem{}
	}
	out.Count = len(out.Items)
	// has_more：只要能凑够一页，就认为后面可能还有（上游的 total 常被截断，
	// 拿它当唯一判据会让"第 50 页"这种边界直接少一页）。
	out.HasMore = out.Count >= out.Limit && out.Limit > 0
	if out.HasMore {
		out.Next = "page=" + itoa(out.Page+1) + "&limit=" + itoa(out.Limit)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
