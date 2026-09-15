// Package kuaishou 实现快手解析。
//
// 好消息：快手的移动端作品页把完整数据内嵌在 window.INIT_STATE 中，
// **既不需要签名，也不需要 Cookie**。因此本包不实现任何签名逻辑
// （社区资料里常见的 __NS_sig3 / graphql / visionVideoDetail 在本项目的
// 实测路径中都不需要）。
//
// 唯一的技巧在跳转链：v.kuaishou.com 短链会经过 www.kuaishou.com/short-video/{id}
// 再跳到移动端 .../fw/photo/{id}。中间那跳返回的是 PC 版页面（结构不同），
// 所以必须精确控制在哪一跳停下。
package kuaishou

import (
	"context"
	"net/url"
	"regexp"
	"strings"

	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/jsonx"
	"vidlink/internal/netx"
	"vidlink/internal/urlx"
	"vidlink/internal/webx"
)

// shortVideoPath 匹配「中间落地页」的路径。命中则继续跟随跳转。
var shortVideoPath = regexp.MustCompile(`^/short-video/[^/]+/?$`)

// photoIDPattern 从任意形态的快手链接里抠出作品 ID。
var photoIDPattern = regexp.MustCompile(`(?:short-video|photo|fw/photo|long-video)/([A-Za-z0-9_-]+)`)

// Endpoints 集中管理地址模板。
type Endpoints struct {
	// PhotoPage 是移动端作品页模板，%s 为 photoId。
	// 留空表示"使用跳转链最后落地的地址"（推荐，避免域名变更）。
	PhotoPage string
}

// Extractor 是快手提取器。
type Extractor struct {
	d  *deps.Deps
	ep Endpoints
}

// New 构造快手提取器。
func New(d *deps.Deps) *Extractor { return &Extractor{d: d} }

func (e *Extractor) Name() core.Platform { return core.PlatformKuaishou }

func (e *Extractor) Hosts() []string {
	return []string{"kuaishou.com", "gifshow.com", "chenzhongtech.com"}
}

func (e *Extractor) Match(u *core.URL) bool {
	if u == nil || u.Href == "" {
		return false
	}
	if u.Path == "" || u.Path == "/" {
		// 短链形态
		return urlx.HostHasSuffix(u.Host, "v.kuaishou.com")
	}
	return photoIDPattern.MatchString(u.Path) || strings.Contains(u.Path, "/fw/")
}

// Parse 解析一个快手作品。
func (e *Extractor) Parse(ctx context.Context, u *core.URL) (*core.Video, error) {
	href := u.Href
	if href == "" {
		return nil, core.BadInput(core.PlatformKuaishou,
			"快手暂不支持按 ID 解析（缺少稳定的 ID→页面映射），请提供分享链接")
	}

	landing, err := e.followToPhotoPage(ctx, href)
	if err != nil {
		return nil, err
	}

	headers := netx.Headers{
		"User-Agent": e.d.Params.Mobile(),
		"Accept":     "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	}
	body, _, err := e.d.Client.GetText(ctx, landing, headers)
	if err != nil {
		return nil, core.Upstream(core.PlatformKuaishou, "photopage", err)
	}
	return e.parseHTML(body, landing)
}

// ParseID 用 photoId 直接解析（先尝试移动端页面）。
func (e *Extractor) ParseID(ctx context.Context, id string) (*core.Video, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, core.BadInput(core.PlatformKuaishou, "作品 ID 为空")
	}
	if e.ep.PhotoPage != "" {
		landing := strings.Replace(e.ep.PhotoPage, "%s", id, 1)
		headers := netx.Headers{"User-Agent": e.d.Params.Mobile()}
		body, _, err := e.d.Client.GetText(ctx, landing, headers)
		if err != nil {
			return nil, core.Upstream(core.PlatformKuaishou, "photopage", err)
		}
		return e.parseHTML(body, landing)
	}
	// 没有配置模板时，用 PC 短链入口走一遍跳转
	return e.Parse(ctx, &core.URL{Href: "https://www.kuaishou.com/short-video/" + id, Host: "www.kuaishou.com"})
}

// followToPhotoPage 精确控制跳转链，停在移动端作品页。
//
// 每一步只前进一跳，并检查"新地址是否还是中间落地页"：
//   - 是  → 继续跟
//   - 否  → 停下，这一跳的 Location 就是目标页
func (e *Extractor) followToPhotoPage(ctx context.Context, href string) (string, error) {
	const maxHops = 6
	headers := netx.Headers{
		"User-Agent": e.d.Params.Mobile(),
		"Accept":     "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	}

	cur := href
	for i := 0; i < maxHops; i++ {
		loc, err := e.d.Client.ResolveLocation(ctx, cur, headers)
		if err != nil {
			return "", core.Upstream(core.PlatformKuaishou, "redirect", err)
		}
		if loc == cur {
			// 没有更多跳转
			return cur, nil
		}
		cur = loc

		pu, perr := url.Parse(cur)
		if perr != nil {
			return "", core.BadInput(core.PlatformKuaishou, "跳转地址无效: %s", cur)
		}
		if shortVideoPath.MatchString(pu.Path) {
			continue // 仍是中间落地页，继续跟
		}
		// 已经离开中间页。长视频页的结构与图片页不同，统一改写成 photo 路径。
		return strings.Replace(cur, "/fw/long-video/", "/fw/photo/", 1), nil
	}
	return cur, nil
}

// parseHTML 从内嵌状态里取作品数据。
//
// INIT_STATE 的顶层键是动态的（形如 {"VisionVideoDetailPhoto:xxx": {...}}），
// 所以不能按固定键取值。这里用"同时包含 result 与 photo 两个字段"来定位，
// 这比猜键名鲁棒得多——前端改键名不影响。
func (e *Extractor) parseHTML(html, srcURL string) (*core.Video, error) {
	raw, ok := webx.ExtractWindowJSON(html, "INIT_STATE", "__APOLLO_STATE__")
	if !ok {
		return nil, core.E(core.KindUpstream, core.PlatformKuaishou, "parsehtml",
			"页面中未找到 INIT_STATE/__APOLLO_STATE__（可能被风控或链接已失效）", nil)
	}
	node, err := jsonx.Unmarshal(raw)
	if err != nil {
		return nil, core.E(core.KindUpstream, core.PlatformKuaishou, "parsehtml", "内嵌数据不是合法 JSON", err)
	}

	photo, resultCode := findPhoto(node)
	if photo == nil {
		// 再兜一层：__APOLLO_STATE__ 的扁平结构
		photo = findApolloPhoto(node)
	}
	if photo == nil {
		return nil, core.NotFound(core.PlatformKuaishou, "未在页面数据中找到作品（result=%d）", resultCode)
	}

	v := &core.Video{
		Platform:  core.PlatformKuaishou,
		ID:        jsonx.String(photo, "photoId", "id"),
		Title:     jsonx.String(photo, "caption", "title"),
		Cover:     jsonx.String(photo, "coverUrls.0.url", "coverUrl", "webCoverUrl"),
		SourceURL: srcURL,
		Author: core.Author{
			ID:     jsonx.String(photo, "userId", "user.userId"),
			Name:   jsonx.String(photo, "userName", "user.userName"),
			Avatar: jsonx.String(photo, "headUrl", "user.headUrl"),
		},
		Stats: &core.Stats{
			Like:    jsonx.Int(photo, "likeCount", "realLikeCount"),
			Comment: jsonx.Int(photo, "commentCount"),
			View:    jsonx.Int(photo, "viewCount"),
		},
	}

	// 视频流：mainMvUrls 是各清晰度列表，同时兼容 manifest 结构
	streams := parseStreams(photo)
	if len(streams) > 0 {
		core.SortStreams(streams)
		v.Videos = streams
	}

	// 图集：CDN 主机名 + 相对路径拼接
	if cdn := jsonx.String(photo, "ext_params.atlas.cdn.0", "extParams.atlas.cdn.0"); cdn != "" {
		for _, item := range jsonx.Slice(photo, "ext_params.atlas.list", "extParams.atlas.list") {
			key := asString(item)
			if key == "" {
				continue
			}
			if strings.HasPrefix(key, "http") {
				v.Images = append(v.Images, core.Image{URL: key})
			} else {
				v.Images = append(v.Images, core.Image{URL: "https://" + cdn + "/" + strings.TrimPrefix(key, "/")})
			}
		}
	}

	if len(v.Videos) == 0 && len(v.Images) == 0 {
		return nil, core.NotFound(core.PlatformKuaishou, "作品中没有可用的视频或图片")
	}
	return v, nil
}

// findPhoto 在任意层级的 map/slice 中寻找同时含 result 与 photo 的对象。
func findPhoto(node jsonx.Node) (jsonx.Node, int64) {
	switch t := node.(type) {
	case map[string]any:
		if p, ok := t["photo"]; ok {
			if _, hasResult := t["result"]; hasResult {
				return p, toInt(t["result"])
			}
			return p, 0
		}
		for _, v := range t {
			if p, code := findPhoto(v); p != nil {
				return p, code
			}
		}
	case []any:
		for _, v := range t {
			if p, code := findPhoto(v); p != nil {
				return p, code
			}
		}
	}
	return nil, 0
}

// findApolloPhoto 处理 PC 版 __APOLLO_STATE__ 的扁平结构：
// 键形如 "VisionVideoDetailPhoto:123"，值是作品对象。
func findApolloPhoto(node jsonx.Node) jsonx.Node {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	for k, v := range m {
		if strings.HasPrefix(k, "VisionVideoDetailPhoto:") {
			if obj, ok := v.(map[string]any); ok {
				return obj
			}
		}
	}
	return nil
}

// parseStreams 汇总各清晰度。
func parseStreams(photo jsonx.Node) []core.Stream {
	var out []core.Stream

	// mainMvUrls 是数组，每项带 url + 元信息
	for _, item := range jsonx.Slice(photo, "mainMvUrls", "mainMvUrl") {
		u := jsonx.String(item, "url")
		if u == "" {
			continue
		}
		out = append(out, core.Stream{
			URL:       u,
			Quality:   jsonx.String(item, "qualityType", "qualityLabel", "type"),
			Codec:     jsonx.String(item, "codec"),
			Width:     int(jsonx.Int(item, "width")),
			Height:    int(jsonx.Int(item, "height")),
			Bandwidth: int(jsonx.Int(item, "bitrate", "bitRate")),
			MimeType:  "video/mp4",
		})
	}

	// manifest.adaptationSet[].representation[]：标准 DASH 式结构
	for _, as := range jsonx.Slice(photo, "manifest.adaptationSet", "manifest.adaptationSets") {
		for _, rep := range jsonx.Slice(as, "representation", "representations") {
			u := jsonx.String(rep, "url", "baseUrl")
			if u == "" {
				continue
			}
			out = append(out, core.Stream{
				URL:       u,
				QualityID: int(jsonx.Int(rep, "qualityType", "id")),
				Codec:     jsonx.String(rep, "codec", "mimeType"),
				Width:     int(jsonx.Int(rep, "width")),
				Height:    int(jsonx.Int(rep, "height")),
				Bandwidth: int(jsonx.Int(rep, "bitrate", "bandwidth")),
				MimeType:  jsonx.String(rep, "mimeType"),
			})
		}
	}

	// 单地址兜底
	if len(out) == 0 {
		if u := jsonx.String(photo, "srcNoMark", "photoUrl", "playUrl"); u != "" {
			out = append(out, core.Stream{URL: u, Quality: "默认", MimeType: "video/mp4"})
		}
	}
	return out
}

func asString(v jsonx.Node) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		// 少数版本把每张图包成 {url: "..."}
		if u, ok := t["url"].(string); ok {
			return u
		}
	}
	return ""
}

func toInt(v jsonx.Node) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case float64:
		return int64(t)
	}
	return 0
}
