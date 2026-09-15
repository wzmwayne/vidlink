// Package extract 提供平台提取器的注册与路由。
//
// 这是"新增平台不改服务层"的关键：服务层只认 core.Extractor 接口，
// 平台实现通过这里注册进来。路由按域名索引，避免对每个请求逐个试错。
package extract

import (
	"sort"
	"strings"

	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/extract/bilibili"
	"vidlink/internal/extract/douyin"
	"vidlink/internal/extract/kuaishou"
	"vidlink/internal/extract/xiaohongshu"
	"vidlink/internal/urlx"
)

// Registry 是提取器注册表。
type Registry struct {
	all    []core.Extractor
	byHost map[string][]core.Extractor
	byName map[core.Platform]core.Extractor
}

// NewRegistry 构造内置平台的注册表。
func NewRegistry(d *deps.Deps) *Registry {
	return NewRegistryWith(
		douyin.New(d),
		bilibili.New(d),
		kuaishou.New(d),
		xiaohongshu.New(d),
	)
}

// NewRegistryWith 用指定提取器构造注册表（便于测试与裁剪）。
func NewRegistryWith(exts ...core.Extractor) *Registry {
	r := &Registry{
		all:    make([]core.Extractor, 0, len(exts)),
		byHost: make(map[string][]core.Extractor, len(exts)*3),
		byName: make(map[core.Platform]core.Extractor, len(exts)),
	}
	for _, e := range exts {
		r.all = append(r.all, e)
		r.byName[e.Name()] = e
		for _, h := range e.Hosts() {
			h = strings.ToLower(h)
			r.byHost[h] = append(r.byHost[h], e)
		}
	}
	return r
}

// All 返回所有提取器（顺序稳定，便于 /platforms 输出）。
func (r *Registry) All() []core.Extractor {
	out := make([]core.Extractor, len(r.all))
	copy(out, r.all)
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// ByName 按平台名查找提取器。
func (r *Registry) ByName(p core.Platform) (core.Extractor, bool) {
	e, ok := r.byName[p]
	return e, ok
}

// Route 把一个归一化 URL 路由到具体提取器。
//
// 匹配顺序：先按域名后缀缩小候选，再用 Extractor.Match 做精细判断。
// 域名匹配用 HostHasSuffix 而不是 Contains，避免 evil-bilibili.com 之类
// 被误判为 bilibili.com。
func (r *Registry) Route(u *core.URL) (core.Extractor, error) {
	if u == nil {
		return nil, core.BadInput("", "URL 为空")
	}

	// 裸 ID 输入：没有 host，交给调用方按平台指定
	if u.Host == "" {
		return nil, core.BadInput("", "无法从输入中识别链接；如需按 ID 解析请指定平台")
	}

	var candidates []core.Extractor
	seen := make(map[core.Platform]bool, len(r.all))
	for host, exts := range r.byHost {
		if !urlx.HostHasSuffix(u.Host, host) {
			continue
		}
		for _, e := range exts {
			if !seen[e.Name()] {
				seen[e.Name()] = true
				candidates = append(candidates, e)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, core.Unsupported("", "暂不支持的平台或链接：%s", u.Href)
	}

	for _, e := range candidates {
		if e.Match(u) {
			return e, nil
		}
	}

	// 域名命中但形态不匹配时，给出更具体的错误（而不是笼统的"不支持"）
	names := make([]string, 0, len(candidates))
	for _, e := range candidates {
		names = append(names, string(e.Name()))
	}
	return nil, core.Unsupported("", "链接属于 %s，但形态不是可解析的作品页：%s",
		strings.Join(names, "/"), u.Href)
}
