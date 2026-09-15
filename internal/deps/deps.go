// Package deps 汇集各平台提取器共享的基础设施。
//
// 它存在的意义是打破循环依赖：提取器实现（internal/extract/<platform>）
// 需要 HTTP 客户端、签名器、Cookie 仓库，而注册表又要导入这些实现。
// 把这些共享物件放进一个叶子包，双方都只依赖它，就不会成环。
package deps

import (
	"sync"
	"time"

	"vidlink/internal/cache"
	"vidlink/internal/core"
	"vidlink/internal/netx"
	"vidlink/internal/sign/abogus"
	"vidlink/internal/sign/wbi"
)

// Cookies 是按平台隔离的 Cookie 仓库，支持运行期热更新。
//
// 设计取舍：Cookie 属于"运维输入"而非代码常量，因此绝不在代码里写死。
// 来源优先级由上层决定（环境变量 / 配置文件 / 管理接口），
// 这里只保证并发安全与按平台隔离。
type Cookies struct {
	mu sync.RWMutex
	m  map[core.Platform]string
}

// NewCookies 构造空的 Cookie 仓库。
func NewCookies() *Cookies {
	return &Cookies{m: make(map[core.Platform]string, 4)}
}

// Get 返回某平台的 Cookie，未配置时返回空串（调用方走匿名降级路径）。
func (c *Cookies) Get(p core.Platform) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.m[p]
}

// Set 热更新某平台的 Cookie；空串表示清除。
func (c *Cookies) Set(p core.Platform, v string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if v == "" {
		delete(c.m, p)
		return
	}
	c.m[p] = v
}

// Has 报告某平台是否已配置 Cookie（用于决定是否提示"可登录获取更高清晰度"）。
func (c *Cookies) Has(p core.Platform) bool { return c.Get(p) != "" }

// Deps 是注入给所有提取器的共享依赖。
type Deps struct {
	Client  *netx.Client
	WBI     *wbi.Manager
	ABogus  *abogus.Signer
	Cookies *Cookies

	// Params 承载运行期可调参数（UA、超时等）。
	Params *Params

	// ResultCache 供提取器自行缓存"取流结果"这类短时效数据。
	// （解析结果的缓存由服务层统一负责，这里只给平台内部用。）
	ResultCache *cache.Cache[*core.Video]

	Now func() time.Time
}

// Params 是运行期可调参数。
//
// 刻意做成结构体而非散落的常量：平台的 UA 白名单、页面路径都会变，
// 集中一处便于按需覆盖（配置/环境变量/热更新），而不是改代码重新编译。
type Params struct {
	// DesktopUA / MobileUA 分别用于桌面与移动端页面。
	DesktopUA string
	MobileUA  string

	// BilibiliReferer 等按平台区分的 Referer，取流 CDN 会校验。
	BilibiliReferer string

	// BilibiliCookie 优先级低于 Cookies 仓库；保留字段用于兼容旧配置。
	_ struct{}
}

// DefaultParams 返回一组合适的默认参数。
func DefaultParams() *Params {
	return &Params{
		DesktopUA: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
		MobileUA: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 " +
			"(KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
		BilibiliReferer: "https://www.bilibili.com/",
	}
}

// Desktop 返回桌面 UA，空则回退默认。
func (p *Params) Desktop() string {
	if p == nil || p.DesktopUA == "" {
		return DefaultParams().DesktopUA
	}
	return p.DesktopUA
}

// Mobile 返回移动 UA，空则回退默认。
func (p *Params) Mobile() string {
	if p == nil || p.MobileUA == "" {
		return DefaultParams().MobileUA
	}
	return p.MobileUA
}
