// Package service 是业务编排层：缓存 + 请求合并 + 超时预算 + 平台路由。
//
// 它刻意不关心 HTTP：这样同一套逻辑既可以被 HTTP handler 用，
// 也可以被 CLI / 测试 / 未来的 MCP 服务直接调用。
package service

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"vidlink/internal/cache"
	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/extract"
	"vidlink/internal/urlx"
)

// Options 是服务层可调参数。
type Options struct {
	// ParseTimeout 是单次解析（含所有上游请求）的时间预算。
	ParseTimeout time.Duration
	// CacheTTL 是解析结果的缓存时长。
	//
	// 取值需要权衡：平台的取流直链通常有 1~2 小时有效期
	// （B 站文档明确写 120min），而元信息基本不变。
	// 默认 10 分钟——足够吸收热点流量，又不会返回临期链接。
	CacheTTL time.Duration
	// NegativeTTL 是失败结果的缓存时长，避免对已删除内容反复打上游。
	NegativeTTL time.Duration
	// BatchConcurrency 是批量解析的并发上限。
	//
	// 单次批量允许的条数上限**不在这里**：那是接口层的参数
	// （config.BatchMin / BatchMax），因为它同时决定路由契约与报错文案。
	BatchConcurrency int
}

// DefaultOptions 返回面向"高并发 + 保护上游"的默认值。
func DefaultOptions() Options {
	return Options{
		ParseTimeout:     20 * time.Second,
		CacheTTL:         10 * time.Minute,
		NegativeTTL:      45 * time.Second,
		BatchConcurrency: 6,
	}
}

// Service 是解析服务。
type Service struct {
	deps *deps.Deps
	reg  *extract.Registry
	opts Options

	cache *cache.Cache[*core.Video]
	// searchCache 与解析缓存同参（TTL/负缓存/抖动）：搜索结果只含元信息，
	// 不含任何会过期的直链，所以按同样的时长缓存是安全的。
	searchCache *cache.Cache[*core.SearchResult]

	sem chan struct{} // 全局解析并发闸门，防止上游被自己打爆
}

// New 构造服务。
func New(d *deps.Deps, reg *extract.Registry, opts Options) *Service {
	if opts.ParseTimeout <= 0 {
		opts = DefaultOptions()
	}
	s := &Service{
		deps: d,
		reg:  reg,
		opts: opts,
		cache: cache.New[*core.Video](opts.CacheTTL,
			cache.WithNegativeTTL(opts.NegativeTTL),
			cache.WithNegativeFilter(cacheableNegative),
			cache.WithJitter(0.1)),
		searchCache: cache.New[*core.SearchResult](opts.CacheTTL,
			cache.WithNegativeTTL(opts.NegativeTTL),
			cache.WithNegativeFilter(cacheableNegative),
			cache.WithJitter(0.1)),
	}
	// 闸门容量取批量并发的一定倍数：既限流又不至于让单请求饿死
	s.sem = make(chan struct{}, max(16, opts.BatchConcurrency*4))
	return s
}

// Registry 暴露注册表（/platforms 用）。
func (s *Service) Registry() *extract.Registry { return s.reg }

// CacheStats 返回缓存统计。
func (s *Service) CacheStats() cache.Stats { return s.cache.Stats() }

// PurgeCache 主动清空缓存（Cookie 变更或排障时用）。
func (s *Service) PurgeCache() { s.cache.Purge() }

// Parse 解析一段用户输入（可以是分享文案、链接，或"平台:ID"形式）。
func (s *Service) Parse(ctx context.Context, raw string) (*core.Video, error) {
	u, err := urlx.Parse(raw)
	if err != nil {
		return nil, err
	}

	ext, err := s.reg.Route(u)
	if err != nil {
		return nil, err
	}

	key := cacheKey(string(ext.Name()), "url", u.Href)
	if key == "" {
		key = cacheKey(string(ext.Name()), "raw", raw)
	}
	return s.cached(ctx, key, func(ctx context.Context) (*core.Video, error) {
		return s.callParse(ctx, ext, u)
	})
}

// ParseByID 按平台 + 内容 ID 解析。
//
// ID 的形态由各平台自己解释（荐片支持 `<影片ID>_<单集ID>` 这种复合 ID），
// 服务层把它当不透明字符串——这样新增平台不需要动缓存键或路由。
func (s *Service) ParseByID(ctx context.Context, platform, id string) (*core.Video, error) {
	ext, ok := s.reg.ByName(core.Platform(platform))
	if !ok {
		return nil, core.Unsupported(core.Platform(platform), "未知平台 %q", platform)
	}

	key := cacheKey(platform, "id", id)
	return s.cached(ctx, key, func(ctx context.Context) (*core.Video, error) {
		// 优先用平台原生 ID 解析能力，缺失时退化为构造 URL 走通用流程
		if byID, ok := ext.(core.ByIDExtractor); ok {
			return withBudget(s, ctx, "解析超时", "解析失败", func(ctx context.Context) (*core.Video, error) {
				return byID.ParseID(ctx, id)
			})
		}
		return s.callParse(ctx, ext, &core.URL{Raw: id, ID: id})
	})
}

// CheckLinkID 让平台校验"取流用的 ID"是否合法（未实现该能力的平台一律放行）。
//
// 放在服务层而不是 handler 里：这样任何调用方（HTTP、CLI、测试）都走同一份规则。
func (s *Service) CheckLinkID(platform, id string) error {
	ext, ok := s.reg.ByName(core.Platform(platform))
	if !ok {
		return nil // 未知平台由后续解析路径给出更准确的报错
	}
	if chk, ok := ext.(core.LinkIDChecker); ok {
		return chk.CheckLinkID(id)
	}
	return nil
}

// Search 按关键词搜索某个平台的内容。
//
// 只有实现了 core.SearchExtractor 的平台可搜；未实现时返回 Unsupported，
// 报错里带上当前可搜索的平台清单（用户至少知道该换哪个平台）。
func (s *Service) Search(ctx context.Context, platform, keyword string, page, limit int) (*core.SearchResult, error) {
	plat := core.Platform(platform)
	ext, ok := s.reg.ByName(plat)
	if !ok {
		return nil, core.Unsupported(plat, "未知平台 %q", platform)
	}
	searcher, ok := ext.(core.SearchExtractor)
	if !ok {
		return nil, core.Unsupported(plat, "平台 %s 不支持搜索；当前可搜索：%s",
			platform, strings.Join(s.SearchablePlatforms(), ", "))
	}

	key := cacheKey(platform, "search", keyword, strconv.Itoa(page), strconv.Itoa(limit))
	res, hit, err := s.searchCache.GetOrLoad(ctx, key, func(ctx context.Context) (*core.SearchResult, error) {
		return withBudget(s, ctx, "搜索超时", "搜索失败", func(ctx context.Context) (*core.SearchResult, error) {
			return searcher.Search(ctx, core.SearchQuery{Keyword: keyword, Page: page, Limit: limit})
		})
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, core.E(core.KindInternal, plat, "search", "提取器返回了空结果且未报告错误", nil)
	}
	if hit {
		cp := *res
		cp.Cached = true
		return &cp, nil
	}
	return res, nil
}

// Searchable 报告某平台是否支持搜索。
func (s *Service) Searchable(p core.Platform) bool {
	ext, ok := s.reg.ByName(p)
	if !ok {
		return false
	}
	_, ok = ext.(core.SearchExtractor)
	return ok
}

// SearchablePlatforms 返回支持搜索的平台名（有序，便于报错文案与文档）。
func (s *Service) SearchablePlatforms() []string {
	var out []string
	for _, e := range s.reg.All() {
		if _, ok := e.(core.SearchExtractor); ok {
			out = append(out, string(e.Name()))
		}
	}
	return out
}

// PlatformInfo 描述一个受支持平台。
type PlatformInfo struct {
	Name    string   `json:"name"`
	Hosts   []string `json:"hosts"`
	ByID    bool     `json:"supports_id"`
	HasAuth bool     `json:"has_cookie"`
	// Search 表示该平台的提取器实现了搜索能力（**是否可搜的唯一事实来源**）。
	// 不支持的原因文案由接口层从 quota 的能力表里取。
	Search bool `json:"search"`
}

// Platforms 返回受支持平台清单。
func (s *Service) Platforms() []PlatformInfo {
	exts := s.reg.All()
	out := make([]PlatformInfo, 0, len(exts))
	for _, e := range exts {
		_, byID := e.(core.ByIDExtractor)
		_, search := e.(core.SearchExtractor)
		out = append(out, PlatformInfo{
			Name:    string(e.Name()),
			Hosts:   e.Hosts(),
			ByID:    byID,
			HasAuth: s.deps.Cookies.Has(e.Name()),
			Search:  search,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// --- 内部 ---

// cacheableNegative 决定一个失败是否值得被负缓存一段时间。
//
// 判据是"这个结论会不会变"：
//
//	值得缓存（确定性）：内容不存在 / 已删除、需要登录态或权限不足。
//	  重试一百次也是同样结果，缓存能省下大量无效上游请求。
//	不值得缓存（瞬时的）：超时、上游 5xx、请求格式错。
//	  DNS 抖一下、上游 502 一下，下一次就可能成功。若把它也缓存
//	  45 秒，等于把一次网络抖动放大成"该条目 45 秒内必失败"——
//	  实测中就出现过一次 DNS 超时导致后续请求全部秒回同一个错误。
func cacheableNegative(err error) bool {
	switch core.KindOf(err) {
	case core.KindNotFound, core.KindForbidden:
		return true
	default:
		return false
	}
}

// cached 走 缓存 + singleflight。
func (s *Service) cached(ctx context.Context, key string, load func(context.Context) (*core.Video, error)) (*core.Video, error) {
	v, hit, err := s.cache.GetOrLoad(ctx, key, load)
	if err != nil {
		// 命中负缓存时，这里拿到的就是上次失败的真实原因（见 cache.entry.err），
		// 而不是一个语焉不详的内部错误。
		return nil, err
	}
	if v == nil {
		// 走到这里只可能是 loader 自己返回了 (nil, nil)——那是提取器的 bug，
		// 不是缓存的问题。错误信息要说清楚，否则会被误读成缓存故障。
		return nil, core.E(core.KindInternal, "", "parse",
			"提取器返回了空结果且未报告错误", nil)
	}
	if hit {
		// 复制一份再标记，避免污染缓存中的对象
		cp := *v
		cp.Cached = true
		return &cp, nil
	}
	return v, nil
}

// callParse 在超时预算与并发闸门下调用提取器。
func (s *Service) callParse(ctx context.Context, ext core.Extractor, u *core.URL) (*core.Video, error) {
	return withBudget(s, ctx, "解析超时", "解析失败", func(ctx context.Context) (*core.Video, error) {
		return ext.Parse(ctx, u)
	})
}

// withBudget 给一次上游调用加上全局并发闸门与超时预算。
//
// 泛型是为了让"解析（*core.Video）"与"搜索（*core.SearchResult）"共用同一套
// 闸门、超时与错误分类逻辑。Go 的方法不能带类型参数，所以它是包级函数。
func withBudget[T any](s *Service, ctx context.Context, timeoutMsg, failMsg string,
	fn func(context.Context) (T, error)) (T, error) {
	var zero T

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return zero, ctx.Err()
	}

	// 单次调用的超时预算独立于客户端 ctx：
	// 客户端断开不应让缓存里的加载半途而废（否则会产生"空缓存"抖动）。
	callCtx, cancel := context.WithTimeout(ctx, s.opts.ParseTimeout)
	defer cancel()

	out, err := fn(callCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return zero, core.E(core.KindTimeout, "", "parse", timeoutMsg, err)
		}
		if errors.Is(err, context.Canceled) {
			return zero, err
		}
		// 分类未知的 error 统一归为上游问题，避免把内部细节暴露给客户端
		if core.KindOf(err) == core.KindInternal {
			if _, ok := err.(*core.Error); !ok {
				return zero, core.E(core.KindUpstream, "", "parse", failMsg, err)
			}
		}
		return zero, err
	}
	return out, nil
}

// cacheKey 组装缓存键。用 '|' 分隔字段，避免不同字段拼接产生歧义。
func cacheKey(parts ...string) string {
	var b []byte
	for i, p := range parts {
		if p == "" {
			return ""
		}
		if i > 0 {
			b = append(b, '|')
		}
		b = append(b, p...)
	}
	return string(b)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
