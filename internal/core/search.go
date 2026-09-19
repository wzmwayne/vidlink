package core

// 搜索的领域模型。
//
// 口径（与配额档位对齐）：**搜索结果只给"元信息 + id"，不含任何下载链接**。
// 一条搜索结果等价于 info 档能给出的那部分元信息：标题、简介、封面、作者、
// 统计、时长/进度等——线路/档位列表与直链必须再调 info / detail / links。
//
// 这样切分的理由有两条：
//
//  1. 档位边界即计价边界：search(0.5) 只是入口，links(1.0)/detail(1.2)
//     才是"给直链"的那一档；搜索里顺手带直链会让后者形同虚设。
//  2. 搜索一次最多 50 条。若每条都要"完整 detail"，就得对 50 条各打一次
//     上游（50 次 RPC），代价与收益完全不成比例。

// SearchQuery 是一次搜索的入参。
type SearchQuery struct {
	// Keyword 是关键词（调用方已 trim）。
	Keyword string
	// Page 是页码（1 基）。
	Page int
	// Limit 是期望条数（1..50）。平台可能返回更少——调用方不应把它当承诺。
	Limit int
}

// SearchResult 是搜索的领域结果。
type SearchResult struct {
	Platform Platform
	Keyword  string
	// Page 是本次结果对应的页码（1 基）。
	Page int
	// Limit 是**实际**每批条数（荐片固定 10 条/页，与请求值可能不同）。
	Limit int
	// Total 是平台声明的匹配总数（可能被平台截断，如 B 站上限 1000）。
	Total int
	Items []SearchItem
	// Cached 标记本次结果是否直接来自服务端缓存（由服务层填充）。
	Cached bool
	// Warning 承载"部分成功"的提示（例如某一页上游失败，结果少了几条）。
	Warning string
}

// SearchItem 是一条搜索结果。
//
// 字段是"通用元信息"：不同平台能提供的子集不同，用 omitempty 表达缺失。
type SearchItem struct {
	Platform Platform

	// Type 是内容类型：video（短视频/稿件）/ series（影视剧集）。
	Type string
	// ID 是平台内的内容 ID（B 站 bvid、荐片影片 ID）——就是拿去 detail/links 的那个。
	ID    string
	Title string
	Desc  string
	Cover string
	// URL 是可直接喂给 /v1/detail?url= 的页面地址（可能为空，荐片就是空）。
	URL    string
	Author Author
	Stats  *Stats
	// Duration 是时长（秒），未知为 0。
	Duration float64
	// PublishedAt 是发布时间（RFC3339，可能为空）。
	PublishedAt string

	// 以下字段偏影视向（荐片），短视频平台通常为空。
	Latest   string   // 更新进度，如 "第10集"
	Finished bool     // 已完结
	Score    float64  // 评分，如 8.0
	Year     int      // 年份
	Category string   // 分类，如 "欧美剧"
	Actors   []string // 主演
	Episodes int      // 集数
}
