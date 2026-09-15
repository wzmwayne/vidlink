// Package xiaohongshu 实现小红书（RedNote）笔记解析。
//
// 链路：xhslink 短链展开 → 笔记页 HTML → window.__INITIAL_STATE__ → 提取。
// 这条链路**不需要任何签名**（x-s / x-s-common / x-rap-param 等只在走
// edith.xiaohongshu.com 的官方 API 时才需要）。
//
// 关于"无水印"，小红书是四个平台里唯一需要**主动改写 URL** 的：
//
//   - 视频：笔记 JSON 里的 masterUrl / originVideoKey 本身就是无水印源，
//     直接取即可；
//   - 图片：urlDefault 指向带尺寸后缀（`!nd_dft_wlteh_webp_3`）的处理图，
//     改写为 `https://ci.xiaohongshu.com/notes_pre_post/{imgId}?imageView2/format/jpg`
//     可拿到原图。若原地址不含 notes_pre_post（老笔记/特殊场景），
//     改写会失效，因此代码里保留了回退到原地址的分支。
package xiaohongshu

import (
	"context"
	"net/url"
	"path"
	"strings"

	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/jsonx"
	"vidlink/internal/netx"
	"vidlink/internal/urlx"
	"vidlink/internal/webx"
)

// Endpoints 集中管理地址模板。
type Endpoints struct {
	// ImageCDN 是无水印原图的 CDN 前缀。
	ImageCDN string
	// VideoCDN 是 originVideoKey 拼接用的 CDN。
	VideoCDN string
}

// DefaultEndpoints 返回官方地址。
func DefaultEndpoints() Endpoints {
	return Endpoints{
		ImageCDN: "https://ci.xiaohongshu.com/notes_pre_post/",
		VideoCDN: "https://sns-video-bd.xhscdn.com/",
	}
}

// Extractor 是小红书提取器。
type Extractor struct {
	d  *deps.Deps
	ep Endpoints
}

// New 构造小红书提取器。
func New(d *deps.Deps) *Extractor {
	return &Extractor{d: d, ep: DefaultEndpoints()}
}

func (e *Extractor) Name() core.Platform { return core.PlatformXiaohongshu }

func (e *Extractor) Hosts() []string {
	return []string{"xiaohongshu.com", "xhslink.com", "xhslink.cn"}
}

func (e *Extractor) Match(u *core.URL) bool {
	if u == nil || u.Href == "" {
		return false
	}
	if urlx.HostHasSuffix(u.Host, "xhslink.com") || urlx.HostHasSuffix(u.Host, "xhslink.cn") {
		return true
	}
	segs := urlx.PathSegments(u.Path)
	for i, s := range segs {
		switch s {
		case "explore", "discovery":
			if i+1 < len(segs) {
				return true
			}
		}
	}
	return false
}

// Parse 解析一条小红书笔记。
func (e *Extractor) Parse(ctx context.Context, u *core.URL) (*core.Video, error) {
	href := u.Href
	if href == "" {
		return nil, core.BadInput(core.PlatformXiaohongshu, "需要笔记链接（小红书没有稳定的裸 ID 入口）")
	}

	// 展开短链。注意：跳转后的地址必须**完整保留 query**，
	// 因为 xsec_token / xsec_source 是访问笔记的必要凭据。
	final, err := e.expand(ctx, href)
	if err != nil {
		return nil, err
	}
	noteID := extractNoteID(final)

	headers := netx.Headers{
		"User-Agent":      e.d.Params.Desktop(),
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "zh-CN,zh;q=0.9",
		"Referer":         "https://www.xiaohongshu.com/",
	}
	if ck := e.d.Cookies.Get(core.PlatformXiaohongshu); ck != "" {
		headers["Cookie"] = ck
	}

	body, status, err := e.d.Client.GetText(ctx, final, headers)
	if err != nil {
		return nil, core.Upstream(core.PlatformXiaohongshu, "notepage", err)
	}
	if status == 461 || status == 471 || status == 406 {
		return nil, core.Errf(core.KindForbidden, core.PlatformXiaohongshu, "notepage",
			"触发小红书风控（HTTP %d），需要人工验证或更换出口 IP", status)
	}

	return e.parseHTML(body, final, noteID)
}

func (e *Extractor) parseHTML(html, srcURL, hintID string) (*core.Video, error) {
	raw, ok := webx.ExtractWindowJSON(html, "__INITIAL_STATE__")
	if !ok {
		return nil, core.E(core.KindUpstream, core.PlatformXiaohongshu, "parsehtml",
			"页面中未找到 __INITIAL_STATE__（可能被风控、链接失效或需要 xsec_token）", nil)
	}

	// 这一步是关键：__INITIAL_STATE__ 里含大量 JS 字面量 undefined，
	// 用严格 JSON 解析器会直接报错。jsonx.Sanitize 会先做替换。
	node, err := jsonx.Unmarshal(raw)
	if err != nil {
		return nil, core.E(core.KindUpstream, core.PlatformXiaohongshu, "parsehtml",
			"内嵌数据不是合法 JSON", err)
	}

	note := e.pickNote(node, hintID)
	if note == nil {
		return nil, core.NotFound(core.PlatformXiaohongshu,
			"未找到笔记数据（可能是私密笔记、已删除，或 xsec_token 已过期）")
	}

	v := &core.Video{
		Platform: core.PlatformXiaohongshu,
		ID: firstNonEmpty(
			jsonx.String(note, "noteId", "id"),
			jsonx.String(node, "note.currentNoteId", "note.firstNoteId"),
			hintID,
		),
		Title:     jsonx.String(note, "title", "desc"),
		Desc:      jsonx.String(note, "desc", "title"),
		SourceURL: srcURL,
		Author: core.Author{
			ID:     jsonx.String(note, "user.userId", "user.user_id"),
			Name:   jsonx.String(note, "user.nickname", "user.nickName"),
			Avatar: jsonx.String(note, "user.avatar", "user.image"),
		},
		Stats: &core.Stats{
			Like:    jsonx.Int(note, "interactInfo.likedCount", "interact_info.liked_count"),
			Comment: jsonx.Int(note, "interactInfo.commentCount"),
			Collect: jsonx.Int(note, "interactInfo.collectedCount"),
			Share:   jsonx.Int(note, "interactInfo.shareCount"),
		},
	}

	// 视频：优先多编码流，缺失时用 originVideoKey 拼 CDN
	if streams := e.videoStreams(note); len(streams) > 0 {
		core.SortStreams(streams)
		v.Videos = streams
	}

	// 图片（图文笔记 / 视频封面）
	v.Images = e.images(note)
	if v.Cover == "" && len(v.Images) > 0 {
		v.Cover = v.Images[0].URL
	}

	if len(v.Videos) == 0 && len(v.Images) == 0 {
		return nil, core.NotFound(core.PlatformXiaohongshu, "笔记中没有可用的视频或图片")
	}
	return v, nil
}

// pickNote 定位笔记对象。
//
// noteDetailMap 的键在不同版本里是 currentNoteId 或 firstNoteId，
// 也可能与 URL 中的 ID 不一致（推荐流场景），因此按优先级多路尝试，
// 最后兜底取 map 中的第一个值。
func (e *Extractor) pickNote(root jsonx.Node, hintID string) jsonx.Node {
	candidates := []string{
		jsonx.String(root, "note.currentNoteId"),
		jsonx.String(root, "note.firstNoteId"),
		hintID,
	}
	for _, id := range candidates {
		if id == "" {
			continue
		}
		if v := jsonx.Get(root, "note.noteDetailMap."+id+".note"); v != nil {
			if m, ok := v.(map[string]any); ok {
				return m
			}
		}
	}
	// 兜底：遍历 noteDetailMap
	if m, ok := jsonx.Get(root, "note.noteDetailMap").(map[string]any); ok {
		for _, item := range m {
			if n := jsonx.Get(item, "note"); n != nil {
				if mm, ok := n.(map[string]any); ok {
					return mm
				}
			}
		}
	}
	return nil
}

// videoStreams 汇总视频流。
//
// 小红书的字段路径在不同版本间差异较大，这里把已知的几种都列上：
// media.stream.{h264,h265,av1,h266}[] 以及 consumer.originVideoKey。
func (e *Extractor) videoStreams(note jsonx.Node) []core.Stream {
	var out []core.Stream

	type codecPath struct{ path, codec string }
	paths := []codecPath{
		{"video.media.stream.h264", "avc1"},
		{"video.media.stream.h265", "hevc"},
		{"video.media.stream.av1", "av01"},
		{"video.media.stream.h266", "hevc"},
		{"video.media.stream", ""},
	}
	for _, cp := range paths {
		for _, item := range jsonx.Slice(note, cp.path) {
			u := jsonx.String(item, "masterUrl", "master_url", "url", "backupUrls.0")
			if u == "" {
				continue
			}
			quality := jsonx.String(item, "qualityType", "quality_type", "videoCodec")
			out = append(out, core.Stream{
				URL:       u,
				Quality:   qualityLabel(quality),
				Codec:     firstNonEmpty(jsonx.String(item, "videoCodec"), cp.codec),
				Width:     int(jsonx.Int(item, "width")),
				Height:    int(jsonx.Int(item, "height")),
				Bandwidth: int(jsonx.Int(item, "avgBitrate", "avg_bitrate", "videoBitrate")),
				Duration:  jsonx.Float(item, "duration"),
				Size:      jsonx.Int(item, "size"),
				MimeType:  "video/mp4",
			})
		}
	}

	// 兜底：originVideoKey 拼接 CDN
	if len(out) == 0 {
		key := jsonx.String(note,
			"video.consumer.originVideoKey", "video.consumer.origin_video_key",
			"video.media.videoId")
		key = strings.ReplaceAll(key, `\u002F`, "/")
		if key != "" {
			if !strings.HasPrefix(key, "http") {
				key = e.ep.VideoCDN + strings.TrimPrefix(key, "/")
			}
			out = append(out, core.Stream{URL: key, Quality: "原画", MimeType: "video/mp4"})
		}
	}
	return out
}

// images 提取图片并做去水印改写。
func (e *Extractor) images(note jsonx.Node) []core.Image {
	items := jsonx.Slice(note, "imageList", "image_list", "noteCard.imageList")
	out := make([]core.Image, 0, len(items))
	for _, it := range items {
		orig := jsonx.String(it, "urlDefault", "url_default", "url", "urlPre")
		if orig == "" {
			continue
		}
		img := core.Image{
			URL:    e.rewriteImageURL(orig),
			Width:  int(jsonx.Int(it, "width")),
			Height: int(jsonx.Int(it, "height")),
		}
		// 实况照片：动态部分是短视频
		if jsonx.Bool(it, "livePhoto", "live_photo") {
			for _, s := range jsonx.Slice(it, "stream.h264", "stream.h265") {
				if u := jsonx.String(s, "masterUrl", "master_url"); u != "" {
					img.LivePhotoURL = u
					break
				}
			}
		}
		out = append(out, img)
	}
	return out
}

// rewriteImageURL 把带尺寸后缀的图片地址改写为无水印原图地址。
//
// 原地址形如：
//
//	https://sns-webpic-qc.xhscdn.com/202401/abc/notes_pre_post/1040g2sg31xxxx!nd_dft_wlteh_webp_3
//
// 目标形如：
//
//	https://ci.xiaohongshu.com/notes_pre_post/1040g2sg31xxxx?imageView2/format/jpg
//
// 注意 imgId 的取法：取路径最后一段并按 '!' 截断（'!' 之后是平台的处理参数）。
func (e *Extractor) rewriteImageURL(orig string) string {
	u, err := url.Parse(orig)
	if err != nil {
		return orig
	}
	seg := path.Base(u.Path)
	if i := strings.IndexByte(seg, '!'); i >= 0 {
		seg = seg[:i]
	}
	if seg == "" || seg == "." || seg == "/" {
		return orig
	}

	prefix := ""
	// spectrum 目录需要一起带上，否则 CDN 找不到对象
	if strings.Contains(orig, "spectrum") {
		prefix = "spectrum/"
	}
	// 只对确认支持该改写路径的笔记生效；否则回退原地址（这是实测得出的约束）
	if !strings.Contains(orig, "notes_pre_post") {
		return orig
	}
	return e.ep.ImageCDN + prefix + seg + "?imageView2/format/jpg"
}

func (e *Extractor) expand(ctx context.Context, href string) (string, error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", core.BadInput(core.PlatformXiaohongshu, "链接无效: %s", href)
	}
	if !urlx.HostHasSuffix(u.Hostname(), "xhslink.com") && !urlx.HostHasSuffix(u.Hostname(), "xhslink.cn") {
		return href, nil
	}
	loc, err := e.d.Client.ResolveLocation(ctx, href, netx.Headers{
		"User-Agent": e.d.Params.Desktop(),
	})
	if err != nil {
		return "", core.Upstream(core.PlatformXiaohongshu, "expand", err)
	}
	// 短链跳转通常已经带上 xsec_token；若上一跳是相对地址则补全
	if strings.HasPrefix(loc, "/") {
		loc = "https://www.xiaohongshu.com" + loc
	}
	return loc, nil
}

// extractNoteID 从 /explore/{id} 或 /discovery/item/{id} 取 ID。
func extractNoteID(href string) string {
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	segs := urlx.PathSegments(u.Path)
	for i, s := range segs {
		switch s {
		case "explore", "item", "note":
			if i+1 < len(segs) {
				return segs[i+1]
			}
		}
	}
	return urlx.LastPathSegment(u.Path)
}

func qualityLabel(q string) string {
	switch q {
	case "1":
		return "标清"
	case "2":
		return "高清"
	case "3":
		return "超清"
	}
	return q
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
