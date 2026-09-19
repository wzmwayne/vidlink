package core

import "context"

// Extractor 是平台提取器契约。
//
// 一个平台实现一个 Extractor；服务层只依赖这个接口，
// 因此新增平台不需要改动服务层与路由层（策略模式 + 注册表）。
type Extractor interface {
	// Name 返回平台标识，必须与 Platform 常量一致。
	Name() Platform

	// Hosts 返回该平台可识别的域名后缀（小写，不含 scheme）。
	// 服务层据此把 URL 路由到正确的提取器，避免逐个试错。
	Hosts() []string

	// Match 在 Hosts 命中后做更精细的判断（例如区分视频页与用户主页）。
	// 返回 false 时路由会继续尝试其他提取器。
	Match(u *URL) bool

	// Parse 解析一个已经归一化的页面 URL，返回统一模型。
	Parse(ctx context.Context, u *URL) (*Video, error)
}

// ByIDExtractor 是可选能力：支持直接用平台内容 ID 解析（不经过分享链接）。
// 提取器按需实现，服务层用类型断言探测。
type ByIDExtractor interface {
	ParseID(ctx context.Context, id string) (*Video, error)
}

// BatchExtractor 是可选能力：平台原生批量接口。
// 未实现时，服务层退化为并发单条解析。
type BatchExtractor interface {
	ParseBatch(ctx context.Context, ids []string) ([]*Video, []error)
}

// SearchExtractor 是可选能力：按关键词搜索内容。
//
// 只有"搜索结果里能拿到可用 ID、且该 ID 能被本提取器解析"的平台才实现它——
// 否则用户搜到了也拿不到直链，等于一个死接口。
//
// 返回的每条结果只带元信息与 ID（见 SearchResult 的说明），**不含直链**。
type SearchExtractor interface {
	Search(ctx context.Context, q SearchQuery) (*SearchResult, error)
}

// URL 是归一化后的输入地址。
//
// 之所以不直接用 *url.URL：解析前需要先"展开短链 + 提取文案中的链接"，
// 这些动作会改变 URL，用一个可变结构承载更清晰。
type URL struct {
	// Raw 是用户原始输入（可能包含分享文案、emoji）。
	Raw string
	// Href 是当前生效的绝对地址。
	Href string
	// Host 是 Href 的小写主机名（不含端口）。
	Host string
	// Path 是 Href 的路径部分。
	Path string
	// Query 是原始查询串（不含 '?'）。
	Query string
	// ID 是路由阶段能直接识别出的平台内容 ID，可能为空。
	ID string
}

// String 实现 fmt.Stringer，便于日志。
func (u *URL) String() string {
	if u == nil {
		return ""
	}
	return u.Href
}
