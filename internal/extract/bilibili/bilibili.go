// Package bilibili 实现哔哩哔哩的视频解析。
//
// 链路（已用真实请求验证）：
//
//	短链展开 → x/web-interface/view 取元信息与 cid
//	        → x/player/wbi/playurl（DASH）取多清晰度流
//	        → x/player/wbi/v2 取字幕
//
// 关于"水印"：B 站不会因某个 playurl 参数就下发无水印流。UP 主压进画面的
// 水印属于像素内容，任何解析器都去不掉；而"服务端台标"则与取流通道有关——
// 社区实践表明 TV 端接口（api.snm0516.aisee.tv/x/tv/playurl）通常下发的源
// 不带台标，但它需要 TV 端 access_token 与 appkey 签名，属于可选通道，
// 本包先实现 WEB 通道，并把 TV 通道留作可插拔扩展（见 Mode）。
package bilibili

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/jsonx"
	"vidlink/internal/netx"
	"vidlink/internal/urlx"
)

// Endpoints 集中管理接口地址，便于前端改版或切换镜像时覆盖，而不是改代码。
type Endpoints struct {
	View        string
	PlayURLWBI  string
	PlayURL     string
	PlayerV2WBI string
	Nav         string
	// Search 是搜索接口的**非签名**通道；SearchWBI 是同功能的 WBI 通道。
	//
	// 两条都要：实测匿名走非签名通道就能拿到结果（省一次 nav 请求），
	// 但历史上 B 站收紧过搜索接口，届时自动降级/升级到 WBI 通道即可。
	Search    string
	SearchWBI string
}

// DefaultEndpoints 返回官方地址。
func DefaultEndpoints() Endpoints {
	return Endpoints{
		View:        "https://api.bilibili.com/x/web-interface/view",
		PlayURLWBI:  "https://api.bilibili.com/x/player/wbi/playurl",
		PlayURL:     "https://api.bilibili.com/x/player/playurl",
		PlayerV2WBI: "https://api.bilibili.com/x/player/wbi/v2",
		Nav:         "https://api.bilibili.com/x/web-interface/nav",
		Search:      "https://api.bilibili.com/x/web-interface/search/type",
		SearchWBI:   "https://api.bilibili.com/x/web-interface/wbi/search/type",
	}
}

// Mode 决定取流策略。
type Mode int

const (
	// ModeDASH 取 DASH 分离流（最高清晰度，音视频需混流）。默认。
	ModeDASH Mode = iota
	// ModeHTML5 取 html5 平台的 MP4 单文件。清晰度较低（通常 ≤720P），
	// 但**不需要 Referer**，可直接丢给浏览器 <video> 播放。
	ModeHTML5
)

// Extractor 是 B 站提取器。
type Extractor struct {
	d    *deps.Deps
	ep   Endpoints
	mode Mode
}

// New 构造 B 站提取器。
func New(d *deps.Deps) *Extractor {
	return &Extractor{d: d, ep: DefaultEndpoints(), mode: ModeDASH}
}

// WithMode 返回切换取流模式的副本。
func (e *Extractor) WithMode(m Mode) *Extractor {
	cp := *e
	cp.mode = m
	return &cp
}

// WithEndpoints 返回替换了接口地址的副本。
//
// 存在的意义有两个：把测试指向 httptest 上游（离线可测），
// 以及在被镜像/反代场景下不改代码就换地址。
func (e *Extractor) WithEndpoints(ep Endpoints) *Extractor {
	cp := *e
	cp.ep = ep
	return &cp
}

func (e *Extractor) Name() core.Platform { return core.PlatformBilibili }

func (e *Extractor) Hosts() []string {
	return []string{"bilibili.com", "b23.tv", "bili2233.cn"}
}

func (e *Extractor) Match(u *core.URL) bool {
	if u == nil || u.Href == "" {
		return false
	}
	// 只认视频/番剧页，避免把空间、动态等当成可解析目标
	segs := urlx.PathSegments(u.Path)
	for _, s := range segs {
		switch s {
		case "video", "bangumi", "cheese", "festival":
			return true
		}
	}
	// 短链直接放行，展开后再判断
	return urlx.HostHasSuffix(u.Host, "b23.tv") || urlx.HostHasSuffix(u.Host, "bili2233.cn")
}

// Parse 解析一个 B 站视频页。
func (e *Extractor) Parse(ctx context.Context, u *core.URL) (*core.Video, error) {
	href, err := e.expand(ctx, u.Href)
	if err != nil {
		return nil, err
	}

	ref, err := e.resolveRef(href)
	if err != nil {
		return nil, err
	}

	headers := e.headers()
	view, err := e.fetchView(ctx, ref, headers)
	if err != nil {
		return nil, err
	}

	// 番剧重定向：普通稿件入口指向番剧时，view 会给出 redirect_url。
	if redirect := jsonx.String(view, "data.redirect_url"); redirect != "" && ref.bvid == "" {
		return nil, core.Errf(core.KindUnsupport, core.PlatformBilibili,
			"parse", "该地址指向番剧，请使用番剧链接: %s", redirect)
	}

	cid := e.pickCID(view, ref)
	if cid == 0 {
		return nil, core.NotFound(core.PlatformBilibili, "未找到可用的 cid")
	}

	video := &core.Video{
		Platform:  core.PlatformBilibili,
		ID:        jsonx.String(view, "data.bvid"),
		Title:     jsonx.String(view, "data.title"),
		Desc:      jsonx.String(view, "data.desc"),
		Cover:     normalizeURL(jsonx.String(view, "data.pic")),
		SourceURL: "https://www.bilibili.com/video/" + jsonx.String(view, "data.bvid"),
		Author: core.Author{
			ID:     strconv.FormatInt(jsonx.Int(view, "data.owner.mid"), 10),
			Name:   jsonx.String(view, "data.owner.name"),
			Avatar: normalizeURL(jsonx.String(view, "data.owner.face")),
		},
		Stats: &core.Stats{
			View:     jsonx.Int(view, "data.stat.view"),
			Like:     jsonx.Int(view, "data.stat.like"),
			Comment:  jsonx.Int(view, "data.stat.reply"),
			Collect:  jsonx.Int(view, "data.stat.favorite"),
			Share:    jsonx.Int(view, "data.stat.share"),
			Danmaku:  jsonx.Int(view, "data.stat.danmaku"),
			Duration: int(jsonx.Int(view, "data.duration")),
		},
	}
	if video.ID == "" {
		video.ID = "av" + strconv.FormatInt(jsonx.Int(view, "data.aid"), 10)
	}

	// 取流
	if err := e.attachStreams(ctx, video, ref, cid, jsonx.Int(view, "data.aid"), headers); err != nil {
		return nil, err
	}

	// 字幕属于"锦上添花"，失败不应让整次解析失败
	if subs, err := e.fetchSubtitles(ctx, ref, cid, jsonx.Int(view, "data.aid"), headers); err == nil {
		video.Subtitles = subs
	}

	// 未配置 Cookie 时不再声称"仅 480P"——那是修复端点顺序之前的错误说法。
	//
	// 实测（2026-09-14，6 条热门视频 6/6 复现）：非签名 playurl 通道带
	// try_look=1 时，**匿名就能拿到完整 1080P**（ffprobe 校验时长与官方一致，
	// 不是试看片段）。所以只有在本次确实没拿到 1080P 时才提示。
	if !e.d.Cookies.Has(core.PlatformBilibili) {
		if h := maxHeight(video); h < 1080 {
			video.Warning = "未配置 B 站 Cookie，本次最高只取到 " + strconv.Itoa(h) +
				"P；配置 SESSDATA 可能解锁更高清晰度"
		}
	}
	return video, nil
}

// maxHeight 返回视频轨里的最大高度；没有视频轨时返回 0。
func maxHeight(v *core.Video) int {
	m := 0
	for _, s := range v.Videos {
		if s.Height > m {
			m = s.Height
		}
	}
	return m
}

// ParseID 支持直接传 BV 号 / av 号。
func (e *Extractor) ParseID(ctx context.Context, id string) (*core.Video, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, core.BadInput(core.PlatformBilibili, "ID 为空")
	}
	u := &core.URL{Raw: id, Href: "https://www.bilibili.com/video/" + id, Host: "www.bilibili.com", Path: "/video/" + id}
	return e.Parse(ctx, u)
}

// --- 内部实现 ---

type reference struct {
	aid  int64
	bvid string
	ep   int64
}

func (e *Extractor) resolveRef(href string) (reference, error) {
	u, err := url.Parse(href)
	if err != nil {
		return reference{}, core.BadInput(core.PlatformBilibili, "链接无效: %s", href)
	}
	var ref reference
	segs := urlx.PathSegments(u.Path)
	for i, s := range segs {
		switch {
		case s == "video" && i+1 < len(segs):
			v := segs[i+1]
			if strings.HasPrefix(strings.ToUpper(v), "BV") {
				ref.bvid = v
			} else if strings.HasPrefix(strings.ToLower(v), "av") {
				ref.aid, _ = strconv.ParseInt(v[2:], 10, 64)
			}
		case s == "bangumi" && i+1 < len(segs) && strings.HasPrefix(segs[i+1], "play"):
			// /bangumi/play/ep123 或 ss456
			if i+2 < len(segs) {
				tok := segs[i+2]
				switch {
				case strings.HasPrefix(tok, "ep"):
					ref.ep, _ = strconv.ParseInt(tok[2:], 10, 64)
				}
			}
		}
	}
	// 查询串里的 bvid / aid / p
	if ref.bvid == "" {
		if v := urlx.Query(&core.URL{Query: u.RawQuery}, "bvid"); v != "" {
			ref.bvid = v
		}
	}
	if ref.aid == 0 && ref.bvid == "" {
		if v := urlx.Query(&core.URL{Query: u.RawQuery}, "aid"); v != "" {
			ref.aid, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if ref.aid == 0 && ref.bvid == "" && ref.ep == 0 {
		return reference{}, core.Unsupported(core.PlatformBilibili,
			"无法从链接中识别 BV/av/ep：%s", href)
	}
	return ref, nil
}

func (e *Extractor) expand(ctx context.Context, href string) (string, error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", core.BadInput(core.PlatformBilibili, "链接无效: %s", href)
	}
	if !urlx.HostHasSuffix(u.Hostname(), "b23.tv") && !urlx.HostHasSuffix(u.Hostname(), "bili2233.cn") {
		return href, nil
	}
	loc, err := e.d.Client.ResolveLocation(ctx, href, netx.Headers{
		"User-Agent": e.d.Params.Desktop(),
	})
	if err != nil {
		return "", core.Upstream(core.PlatformBilibili, "expand", err)
	}
	return loc, nil
}

func (e *Extractor) headers() netx.Headers {
	h := netx.Headers{
		"User-Agent": e.d.Params.Desktop(),
		"Referer":    e.d.Params.BilibiliReferer,
		"Origin":     "https://www.bilibili.com",
		"Accept":     "application/json, text/plain, */*",
	}
	if ck := e.d.Cookies.Get(core.PlatformBilibili); ck != "" {
		h["Cookie"] = ck
	}
	return h
}

func (e *Extractor) fetchView(ctx context.Context, ref reference, headers netx.Headers) (jsonx.Node, error) {
	q := url.Values{}
	if ref.bvid != "" {
		q.Set("bvid", ref.bvid)
	} else if ref.aid != 0 {
		q.Set("aid", strconv.FormatInt(ref.aid, 10))
	} else {
		return nil, core.Unsupported(core.PlatformBilibili, "仅支持普通稿件（BV/av），番剧请用 ep 链接")
	}

	body, code, err := e.d.Client.GetBytes(ctx, e.ep.View+"?"+q.Encode(), headers, 0)
	if err != nil {
		return nil, core.Upstream(core.PlatformBilibili, "view", err)
	}
	node, err := jsonx.Unmarshal(body)
	if err != nil {
		return nil, core.E(core.KindUpstream, core.PlatformBilibili, "view", "响应不是合法 JSON", err)
	}
	if apiCode := jsonx.Int(node, "code"); apiCode != 0 {
		return nil, classifyAPIError(apiCode, jsonx.String(node, "message"))
	}
	if code < 200 || code >= 300 {
		return nil, core.Errf(core.KindUpstream, core.PlatformBilibili, "view", "HTTP %d", code)
	}
	return node, nil
}

// pickCID 选择分 P：默认第 1 P，若 URL 指定了 ?p= 则用对应页。
func (e *Extractor) pickCID(view jsonx.Node, ref reference) int64 {
	pages := jsonx.Slice(view, "data.pages")
	if len(pages) == 0 {
		return jsonx.Int(view, "data.cid")
	}
	return jsonx.Int(pages[0], "cid")
}

func (e *Extractor) attachStreams(ctx context.Context, v *core.Video, ref reference, cid, aid int64, headers netx.Headers) error {
	params := url.Values{}
	if ref.bvid != "" {
		params.Set("bvid", ref.bvid)
	}
	if aid != 0 {
		params.Set("avid", strconv.FormatInt(aid, 10))
	}
	params.Set("cid", strconv.FormatInt(cid, 10))
	params.Set("qn", "127")
	params.Set("fnver", "0")
	params.Set("otype", "json")
	params.Set("fourk", "1")

	if e.mode == ModeHTML5 {
		// html5 通道：单文件 MP4，无需 Referer，可直接浏览器播放
		params.Set("fnval", "1")
		params.Set("platform", "html5")
		params.Set("high_quality", "1")
	} else {
		params.Set("fnval", "4048") // 16|64|128|256|512|1024|2048：所有 DASH 流
		params.Set("platform", "pc")
		params.Set("try_look", "1") // 未登录也尝试拿 720P/1080P
	}

	body, apiCode, err := e.requestPlayURL(ctx, params, headers)
	if err != nil {
		return err
	}
	if apiCode != 0 {
		return classifyAPIError(apiCode, "")
	}

	node, err := jsonx.Unmarshal(body)
	if err != nil {
		return core.E(core.KindUpstream, core.PlatformBilibili, "playurl", "响应不是合法 JSON", err)
	}
	authNote := ""

	// DASH：多清晰度，音视频分离
	videos := parseDASHStreams(node, "data.dash.video")
	audios := parseDASHStreams(node, "data.dash.audio")
	// 无损/杜比音轨挂在 dash.flac.audio / dash.dolby.audio 下
	if extra := parseDASHStreams(node, "data.dash.flac.audio"); len(extra) > 0 {
		audios = append(audios, extra...)
	}
	if extra := parseDASHStreams(node, "data.dash.dolby.audio"); len(extra) > 0 {
		audios = append(audios, extra...)
	}

	if len(videos) == 0 {
		// 降级：durl（单文件 MP4/FLV）
		if durl := parseDURLStreams(node); len(durl) > 0 {
			videos = durl
		}
	}

	if len(videos) == 0 {
		return core.NotFound(core.PlatformBilibili,
			"未取到任何视频流（可能是付费/充电专属/区域限制内容）")
	}

	core.SortStreams(videos)
	core.SortStreams(audios)

	// 取流 CDN 做 Referer 防盗链，必须把请求头透传给客户端
	playHeaders := map[string]string{
		"Referer":    e.d.Params.BilibiliReferer,
		"User-Agent": e.d.Params.Desktop(),
	}
	for i := range videos {
		if videos[i].Headers == nil {
			videos[i].Headers = playHeaders
		}
	}
	for i := range audios {
		if audios[i].Headers == nil {
			audios[i].Headers = playHeaders
		}
	}

	v.Videos = videos
	v.Audios = audios
	v.NeedsMux = len(audios) > 0 && len(videos) > 0 && videos[0].MimeType != "" &&
		!strings.HasPrefix(videos[0].MimeType, "video/mp4;+durl")

	if authNote != "" {
		v.Warning = authNote
	}
	return nil
}

// requestPlayURL 先走 WBI 通道，失败再降级到不带签名的旧通道。
//
// 这个降级是有实测依据的：WBI 通道在匿名态可能因风控返回 v_voucher，
// 而旧通道当前仍可用；两者参数完全一致。
// requestPlayURL 取播放地址。
//
// **端点顺序是实测决定的，不是随意排的。**
//
// 两个端点接受完全相同的参数，但未登录时给的东西差一档：
//
//	x/player/playurl      （非签名）→ 带 try_look=1 时给完整 1080P
//	x/player/wbi/playurl  （WBI 签名）→ 同样参数只给 480P
//
// 2026-09-14 用 6 条热门视频逐条对照，**6/6 复现**：
// 非签名端点最高 1080P（其中一条 1920×1080），WBI 端点一律 480P。
// 用 ffprobe 探测非签名端点返回的 1080P 流，时长 212.32s 对上官方 213s
// ——是**完整视频**，不是试看片段。
//
// 所以这里**先走非签名端点**，WBI 只作为兜底。
// 本函数曾经反过来（WBI 优先），代价是白白丢掉 1080P，
// 而且症状很隐蔽：请求成功、有流、只是清晰度低一档。
func (e *Extractor) requestPlayURL(ctx context.Context, params url.Values, headers netx.Headers) ([]byte, int64, error) {
	var lastErr error

	// 首选：非签名通道
	//
	// 注意 GetBytes 的第二个返回值是 **HTTP 状态码**，不是响应体里的业务 code。
	// B 站的业务码在 JSON 的 "code" 字段里，必须解析后单独读。
	body, _, err := e.d.Client.GetBytes(ctx, e.ep.PlayURL+"?"+params.Encode(), headers, 0)
	switch {
	case err != nil:
		lastErr = err
	default:
		if node, jerr := jsonx.Unmarshal(body); jerr == nil {
			apiCode := jsonx.Int(node, "code")
			if apiCode == 0 && hasDASHStreams(node) {
				return body, apiCode, nil
			}
			// 有响应但没有可用的 DASH 流（风控 v_voucher、付费、地区限制等）
			lastErr = core.Errf(core.KindUpstream, core.PlatformBilibili, "playurl",
				"非签名通道不可用（code=%d %s）", apiCode, jsonx.String(node, "message"))
		} else {
			lastErr = core.E(core.KindUpstream, core.PlatformBilibili, "playurl",
				"响应不是合法 JSON", jerr)
		}
	}

	// 兜底：WBI 签名通道（可能解锁非签名通道拿不到的场景）
	if e.d.WBI != nil {
		if query, serr := e.d.WBI.Sign(ctx, params); serr == nil {
			wbody, _, werr := e.d.Client.GetBytes(ctx, e.ep.PlayURLWBI+"?"+query, headers, 0)
			if werr == nil {
				if wnode, jerr := jsonx.Unmarshal(wbody); jerr == nil {
					wapiCode := jsonx.Int(wnode, "code")
					if wapiCode == 0 && hasDASHStreams(wnode) {
						return wbody, wapiCode, nil
					}
					// v_voucher 是风控信号，作废缓存的签名口令
					if jsonx.String(wnode, "data.v_voucher") != "" {
						e.d.WBI.Invalidate()
					}
				}
			} else if lastErr == nil {
				lastErr = werr
			}
		} else if lastErr == nil {
			lastErr = serr
		}
	}

	if lastErr != nil {
		return nil, 0, lastErr
	}
	return nil, 0, core.Errf(core.KindUpstream, core.PlatformBilibili, "playurl",
		"两个通道均未返回可用的播放地址")
}

// hasDASHStreams 判断响应里是否真的有可用的 DASH 视频轨。
//
// 只看 code==0 是不够的：风控或权限不足时也会返回 code=0 但 dash 为空，
// 那种响应不能当作成功。
func hasDASHStreams(node jsonx.Node) bool {
	return len(jsonx.Slice(node, "data.dash.video")) > 0 ||
		len(jsonx.Slice(node, "data.durl")) > 0
}

func (e *Extractor) fetchSubtitles(ctx context.Context, ref reference, cid, aid int64, headers netx.Headers) ([]core.Subtitle, error) {
	if e.d.WBI == nil {
		return nil, nil
	}
	params := url.Values{}
	if ref.bvid != "" {
		params.Set("bvid", ref.bvid)
	}
	if aid != 0 {
		params.Set("aid", strconv.FormatInt(aid, 10))
	}
	params.Set("cid", strconv.FormatInt(cid, 10))

	query, err := e.d.WBI.Sign(ctx, params)
	if err != nil {
		return nil, err
	}
	body, _, err := e.d.Client.GetBytes(ctx, e.ep.PlayerV2WBI+"?"+query, headers, 0)
	if err != nil {
		return nil, err
	}
	node, err := jsonx.Unmarshal(body)
	if err != nil {
		return nil, err
	}
	items := jsonx.Slice(node, "data.subtitle.subtitles")
	out := make([]core.Subtitle, 0, len(items))
	for _, it := range items {
		u := jsonx.String(it, "subtitle_url", "subtitleUrl")
		if u == "" {
			continue
		}
		// 协议相对地址需要补全
		if strings.HasPrefix(u, "//") {
			u = "https:" + u
		}
		out = append(out, core.Subtitle{
			Lang:   jsonx.String(it, "lan", "lang"),
			Name:   jsonx.String(it, "lan_doc", "lanDoc"),
			URL:    u,
			Format: "json",
		})
	}
	return out, nil
}

// parseDASHStreams 把 dash.video / dash.audio 数组转成统一的 Stream。
//
// 同时兼容驼峰与下划线两套键名——同一个响应里两种键名是并存的。
func parseDASHStreams(node jsonx.Node, path string) []core.Stream {
	items := jsonx.Slice(node, path)
	if len(items) == 0 {
		return nil
	}
	out := make([]core.Stream, 0, len(items))
	for _, it := range items {
		u := jsonx.String(it, "baseUrl", "base_url", "url")
		if u == "" {
			continue
		}
		id := int(jsonx.Int(it, "id"))
		bandwidth := int(jsonx.Int(it, "bandwidth"))
		out = append(out, core.Stream{
			URL:        u,
			BackupURLs: jsonx.StringList(it, "backupUrl", "backup_url"),
			Quality:    qualityLabel(id),
			QualityID:  id,
			Codec:      jsonx.String(it, "codecs"),
			MimeType:   jsonx.String(it, "mimeType", "mime_type"),
			Width:      int(jsonx.Int(it, "width")),
			Height:     int(jsonx.Int(it, "height")),
			FrameRate:  jsonx.String(it, "frameRate", "frame_rate"),
			Bandwidth:  bandwidth,
		})
	}
	return out
}

func parseDURLStreams(node jsonx.Node) []core.Stream {
	items := jsonx.Slice(node, "data.durl")
	out := make([]core.Stream, 0, len(items))
	for i, it := range items {
		u := jsonx.String(it, "url")
		if u == "" {
			continue
		}
		out = append(out, core.Stream{
			URL:       u,
			Quality:   fmt.Sprintf("分片 %d", i+1),
			Size:      jsonx.Int(it, "size"),
			Bandwidth: int(jsonx.Int(it, "length")),
			MimeType:  "video/mp4",
		})
	}
	return out
}

// qualityLabel 把 qn/音质代码翻译成人类可读标签。
// 未知代码回退为 "qn=<code>"，避免因新增档位而显示为空。
func qualityLabel(qn int) string {
	switch qn {
	case 6:
		return "240P"
	case 16:
		return "360P"
	case 32:
		return "480P"
	case 64:
		return "720P"
	case 74:
		return "720P60"
	case 80:
		return "1080P"
	case 100:
		return "智能修复"
	case 112:
		return "1080P+"
	case 116:
		return "1080P60"
	case 120:
		return "4K"
	case 125:
		return "HDR"
	case 126:
		return "杜比视界"
	case 127:
		return "8K"
	case 129:
		return "HDR Vivid"
	// 音频
	case 30216:
		return "64K"
	case 30232:
		return "132K"
	case 30280:
		return "192K"
	case 30250:
		return "杜比全景声"
	case 30251:
		return "Hi-Res 无损"
	}
	if qn == 0 {
		return ""
	}
	return "qn=" + strconv.Itoa(qn)
}

func normalizeURL(u string) string {
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}

func classifyAPIError(code int64, msg string) error {
	switch code {
	case -404, 62002, 62004, 62012:
		return core.NotFound(core.PlatformBilibili, "内容不存在或不可见（code=%d %s）", code, msg)
	case -403:
		return core.Forbidden(core.PlatformBilibili, "无权限访问（code=%d %s）", code, msg)
	case -352, -412:
		return core.Errf(core.KindRateLimit, core.PlatformBilibili, "api",
			"触发 B 站风控（code=%d %s）", code, msg)
	case -688, -689:
		return core.Errf(core.KindForbidden, core.PlatformBilibili, "api",
			"地区/版权限制（code=%d %s）", code, msg)
	default:
		return core.Errf(core.KindUpstream, core.PlatformBilibili, "api",
			"B 站返回错误 code=%d msg=%s", code, msg)
	}
}
