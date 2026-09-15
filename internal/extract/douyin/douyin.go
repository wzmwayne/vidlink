// Package douyin 实现抖音（含图文/图集）解析。
//
// 链路取舍（2026-09 实网验证）
//
// 主链路是 **web detail API + 完整签名**：
//
//	GET /aweme/v1/web/aweme/detail/?<业务参数>&a_bogus=..&verifyFp=..&fp=..&uifid=..&timestamp=..&x-secsdk-web-signature=..
//
// 三个签名件都是纯计算，可以完全在 Go 里算出来，无需 JS 引擎或浏览器：
//
//	a_bogus                  —— 见 internal/sign/abogus
//	x-secsdk-web-signature   —— 见 internal/sign/secsdk（纯 MD5 + 公开盐）
//	timestamp                —— time.Now()
//
// 唯一算不出来的是 uifid：那是服务端签发给浏览器的**匿名访客标识**
// （cookie 名通常为 UIFID_TEMP，160 字符）。任何算法都产生不了它，
// 必须先铸造一次，再通过 VIDLINK_COOKIE_DOUYIN 注入。铸造流程见
// tools/douyin-mint/。没有它时，接口会回 403 Uifid Not Found。
//
// 降级链路是 iesdouyin 分享页（window._ROUTER_DATA）。它在 2026 年已改为
// 客户端渲染，服务端不再下发作品数据，因此实际上只是**历史兼容**，
// 不能当作主力。保留它是因为改回 SSR 的成本对平台来说很低，
// 而多一条链路不花什么钱。
//
// 风控的正确读法
//
// 平台限流时会返回 **HTTP 200 + 0 字节**，而不是 4xx。这类"静默拒绝"
// 必须当成限流处理，否则调用方会误判为成功、再在 JSON 解析处报一个
// 找不着北的错。本包把它映射为 KindRateLimit。
//
// 另外：同 IP 的浏览器与程序是**同命相连**的。实测中，触发限流后连
// 真实 Chrome 发出的、带完整签名的 detail 请求也同样得到 200 + 0 字节。
// 所以这不是"指纹被识破"或"缺了参数"，而是 IP 维度的配额问题。
package douyin

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/jsonx"
	"vidlink/internal/netx"
	"vidlink/internal/sign/secsdk"
	"vidlink/internal/urlx"
	"vidlink/internal/webx"
)

// Endpoints 集中管理地址。
type Endpoints struct {
	SharePage string // 分享页模板，%s 为作品 ID
	DetailAPI string // web detail 接口（增强链路）
}

// DefaultEndpoints 返回官方地址。
func DefaultEndpoints() Endpoints {
	return Endpoints{
		SharePage: "https://www.iesdouyin.com/share/video/%s",
		DetailAPI: "https://www.douyin.com/aweme/v1/web/aweme/detail/",
	}
}

// Extractor 是抖音提取器。
type Extractor struct {
	d  *deps.Deps
	ep Endpoints
}

// New 构造抖音提取器。
func New(d *deps.Deps) *Extractor {
	return &Extractor{d: d, ep: DefaultEndpoints()}
}

func (e *Extractor) Name() core.Platform { return core.PlatformDouyin }

func (e *Extractor) Hosts() []string {
	return []string{"douyin.com", "iesdouyin.com", "amemv.com"}
}

func (e *Extractor) Match(u *core.URL) bool {
	if u == nil || u.Href == "" {
		return false
	}
	segs := urlx.PathSegments(u.Path)
	for i, s := range segs {
		switch s {
		case "video", "note", "slides":
			if i+1 < len(segs) {
				return true
			}
		}
	}
	// 短链与精选页（?modal_id=）都放行
	if urlx.HostHasSuffix(u.Host, "v.douyin.com") || urlx.HostHasSuffix(u.Host, "v.iesdouyin.com") {
		return true
	}
	return urlx.Query(u, "modal_id") != "" || urlx.Query(u, "vid") != ""
}

// Parse 解析一个抖音作品。
func (e *Extractor) Parse(ctx context.Context, u *core.URL) (*core.Video, error) {
	href := u.Href
	if href == "" {
		// 裸 ID 输入
		return e.ParseID(ctx, u.ID)
	}

	expanded, err := e.expand(ctx, href)
	if err != nil {
		return nil, err
	}
	awemeID, err := e.extractID(expanded)
	if err != nil {
		return nil, err
	}
	return e.ParseID(ctx, awemeID)
}

// ParseID 用作品 ID 解析。
//
// 先走签名 detail 链路（唯一能拿到多档清晰度与无水印 play_addr 的路），
// 失败再降级到分享页。两条都失败时，返回**信息量更大的那个错误**——
// detail 的错误通常解释了"为什么走不通"，而分享页的错误只是
// "分享页本身不行"，对排障没有帮助。
func (e *Extractor) ParseID(ctx context.Context, id string) (*core.Video, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, core.BadInput(core.PlatformDouyin, "作品 ID 为空")
	}
	if !isNumericID(id) {
		return nil, core.BadInput(core.PlatformDouyin, "作品 ID 应为纯数字，得到 %q", id)
	}

	cookie := e.d.Cookies.Get(core.PlatformDouyin)

	var detailErr error
	if cookie == "" {
		detailErr = core.Errf(core.KindUnsupport, core.PlatformDouyin, "detail",
			"未配置抖音 Cookie：受保护接口需要匿名访客标识 UIFID_TEMP。"+
				"用 tools/douyin-mint/ 铸造一次后写入 VIDLINK_COOKIE_DOUYIN，"+
				"详见 docs/平台调研报告.md 第 1 节")
	} else if v, err := e.parseViaDetailAPI(ctx, id, cookie); err == nil {
		return v, nil
	} else {
		detailErr = err
	}

	v, shareErr := e.parseViaSharePage(ctx, id)
	if shareErr == nil {
		return v, nil
	}
	if detailErr != nil {
		return nil, detailErr
	}
	return nil, shareErr
}

// detailParams 返回 detail 接口的**业务参数**，顺序即发送顺序。
//
// 顺序不能动：a_bogus 是对这条 query 逐字节取的哈希，顺序变了签名就废。
// 这里的取值全部来自 2026-09 实网通过的一组参数。
//
// 刻意让所有值都不含空格：a_bogus 层与 secsdk 层对空格的转义规则不同
// （'+' vs '%20'），而官方请求里两层是一致的。避免空格就绕开了这个歧义。
// 若将来必须加带空格的参数，两层都要改用 secsdk.URLSearchParams。
func detailParams(awemeID string) [][2]string {
	return [][2]string{
		{"device_platform", "webapp"},
		{"aid", "6383"},
		{"channel", "channel_pc_web"},
		{"pc_client_type", "1"},
		{"version_code", "290100"},
		{"version_name", "29.1.0"},
		{"cookie_enabled", "true"},
		{"screen_width", "1920"},
		{"screen_height", "1080"},
		{"browser_language", "zh-CN"},
		{"browser_platform", "Win32"},
		{"browser_name", "Chrome"},
		{"browser_version", "130.0.0.0"},
		{"browser_online", "true"},
		{"engine_name", "Blink"},
		{"engine_version", "130.0.0.0"},
		{"os_name", "Windows"},
		{"os_version", "10"},
		{"cpu_core_num", "12"},
		{"device_memory", "8"},
		{"platform", "PC"},
		{"downlink", "10"},
		{"effective_type", "4g"},
		{"round_trip_time", "0"},
		{"update_version_code", "170400"},
		{"aweme_id", awemeID},
	}
}

// parseViaDetailAPI 走带完整签名的 web detail 接口。
func (e *Extractor) parseViaDetailAPI(ctx context.Context, id, cookie string) (*core.Video, error) {
	uifid := secsdk.PickUIFID(cookie)
	if uifid == "" {
		return nil, core.Errf(core.KindUnsupport, core.PlatformDouyin, "detail",
			"Cookie 中没有 UIFID_TEMP（或等价的访客标识）：签名覆盖该值，"+
				"缺失时受保护接口必然返回 403 Uifid Not Found")
	}
	if e.d.ABogus == nil {
		return nil, core.Errf(core.KindInternal, core.PlatformDouyin, "detail",
			"a_bogus 签名器未注入")
	}

	params := detailParams(id)

	// 第一层：a_bogus 覆盖业务参数。注意此时 query 里还没有
	// uifid/timestamp/signature —— 顺序搞反签名就对不上。
	bogus := e.d.ABogus.Sign(secsdk.URLSearchParams(params), "GET")
	params = append(params, [2]string{"a_bogus", bogus})

	// verifyFp 与 fp 是同一个 s_v_web_id 值，以两个名字重复发送。
	if fp := secsdk.CookieValue(cookie, secsdk.VerifyFPCookie); fp != "" {
		for _, name := range secsdk.VerifyFPParams {
			params = append(params, [2]string{name, fp})
		}
	}

	// 第二层：追加 uifid/timestamp 并计算 x-secsdk-web-signature。
	query, _, sigHeaders := secsdk.Sign(params, uifid, 0)

	headers := netx.Headers{
		"User-Agent":      e.d.Params.Desktop(),
		"Referer":         "https://www.douyin.com/",
		"Accept":          "application/json, text/plain, */*",
		"Accept-Language": "zh-CN,zh;q=0.9",
		"Cookie":          cookie,
	}
	for k, v := range sigHeaders {
		headers[k] = v
	}

	// detail 响应实测约 100KB，给它 4MiB 足够；超出说明上游返回了别的东西。
	raw, status, err := e.d.Client.GetText(ctx, e.ep.DetailAPI+"?"+query, headers)
	if err != nil {
		return nil, core.Upstream(core.PlatformDouyin, "detail", err)
	}
	return e.classifyDetail(raw, status, id)
}

// classifyDetail 把 detail 接口的响应判成"成功 / 限流 / 身份不足 / 其它"。
//
// 这个函数存在的唯一理由是那个 200 + 0 字节：平台限流时不报错，
// 只回一个空体。若不显式识别，调用方会当作成功，然后在 JSON 解析处
// 得到一个和真实原因毫无关系的报错。
func (e *Extractor) classifyDetail(raw string, status int, id string) (*core.Video, error) {
	switch {
	case status == 444:
		// 平台的显式限流档位；比空体更明确。
		return nil, core.Errf(core.KindRateLimit, core.PlatformDouyin, "detail",
			"上游返回 444 Access Denied（IP 维度限流）。退避后重试，不要换身份——"+
				"实测换新身份同样被拒")
	case status == 403:
		return nil, core.Forbidden(core.PlatformDouyin,
			"detail 接口拒绝访问（403）：%s", shorten(raw, 120))
	case status < 200 || status >= 300:
		return nil, core.Errf(core.KindUpstream, core.PlatformDouyin, "detail",
			"detail 接口返回 %d: %s", status, shorten(raw, 120))
	case strings.TrimSpace(raw) == "":
		// 最阴险的一种：HTTP 200 但没有 body。
		// 同 IP 的真实浏览器在限流窗口内也会得到同样的响应，
		// 所以这不是"参数不对"或"指纹被识破"。
		return nil, core.Errf(core.KindRateLimit, core.PlatformDouyin, "detail",
			"上游返回 200 但响应体为空——这是抖音的静默限流（IP 维度）。"+
				"退避后用同一身份重试即可；实测惩罚窗口约 20 分钟后自行恢复")
	}

	data, err := jsonx.Unmarshal([]byte(raw))
	if err != nil {
		return nil, core.E(core.KindUpstream, core.PlatformDouyin, "detail",
			"响应不是合法 JSON", err)
	}

	item := jsonx.GetAny(data,
		"aweme_detail",
		"aweme_details.0",
		"item_list.0",
	)
	if item == nil {
		if msg := jsonx.String(data, "status_msg", "message"); msg != "" {
			return nil, core.Errf(core.KindUpstream, core.PlatformDouyin, "detail",
				"接口返回 status_code=%d: %s",
				jsonx.Int(data, "status_code"), msg)
		}
		return nil, core.NotFound(core.PlatformDouyin, "detail 未返回作品数据（ID=%s）", id)
	}
	return e.build(item, id), nil
}

// parseViaSharePage 走 iesdouyin 分享页（历史兼容链路）。
//
// 2026 年实测：分享页已改为客户端渲染，_ROUTER_DATA 里只剩页面配置，
// 不再下发 item_list / videoInfoRes。因此这条链路通常只会返回
// 「链路失效」而不是数据；保留它作为兜底。
func (e *Extractor) parseViaSharePage(ctx context.Context, id string) (*core.Video, error) {
	headers := netx.Headers{
		"User-Agent":      e.d.Params.Mobile(),
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "zh-CN,zh;q=0.9",
	}
	page := fmt.Sprintf(e.ep.SharePage, id)
	body, _, err := e.d.Client.GetText(ctx, page, headers)
	if err != nil {
		return nil, core.Upstream(core.PlatformDouyin, "sharepage", err)
	}

	node, ok := webx.ExtractWindowJSON(body, "_ROUTER_DATA")
	if !ok {
		return nil, core.E(core.KindUpstream, core.PlatformDouyin, "sharepage",
			"页面中未找到 _ROUTER_DATA（可能被风控或需要验证码）", nil)
	}
	data, err := jsonx.Unmarshal(node)
	if err != nil {
		return nil, core.E(core.KindUpstream, core.PlatformDouyin, "sharepage", "内嵌数据不是合法 JSON", err)
	}

	item := e.pickItem(data, id)
	if item == nil {
		// 平台会给出结构化的拒绝原因，直接透传比自造文案更有用
		if reason := jsonx.String(data,
			"loaderData.video_(id)/page.videoInfoRes.filter_list.0.filter_reason",
			"loaderData.video_(id)/page.videoInfoRes.filter_list.0.detail_msg"); reason != "" {
			return nil, core.NotFound(core.PlatformDouyin, "作品不可用: %s", reason)
		}
		// 2026 年实测：分享页已改为客户端渲染——`renderInSSR` 仍为 1，
		// 但 `item_list`/`videoInfoRes` 不再下发，只剩页面配置。
		// 这是"链路失效"而不是"作品不存在"，必须区分开，
		// 否则用户会误以为视频被删了。
		if IsClientRendered(data) {
			return nil, core.E(core.KindUnsupport, core.PlatformDouyin, "sharepage",
				"抖音分享页已改为客户端渲染，服务端不再下发作品数据；"+
					"主力链路是签名 detail 接口，需要 UIFID_TEMP，"+
					"详见 docs/平台调研报告.md 第 1 节", nil)
		}
		return nil, core.NotFound(core.PlatformDouyin, "分享页未返回作品数据（ID=%s）", id)
	}
	return e.build(item, id), nil
}

// shorten 截断过长的上游文本，避免把整个 HTML 错误页塞进错误信息。
func shorten(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// IsClientRendered 判断页面是否退化成了"只有配置、没有数据"的客户端渲染壳。
//
// 判据用的是**结构特征**而不是某个固定字段名：page 节点存在、
// 但不含任何作品数据字段，却带着渲染/页面配置类字段。
// 这样前端改字段名时不会误判。
func IsClientRendered(data jsonx.Node) bool {
	page := jsonx.GetAny(data,
		"loaderData.video_(id)/page",
		"loaderData.note_(id)/page",
	)
	if page == nil {
		return false
	}
	if jsonx.GetAny(page, "videoInfoRes", "item_list", "aweme_detail") != nil {
		return false // 有数据，是正常的 SSR 页
	}
	// 只有配置类字段 ⇒ 客户端渲染壳
	return jsonx.GetAny(page, "itemId", "webId", "abParams", "renderInSSR", "commonContext") != nil
}

// pickItem 在 _ROUTER_DATA 里定位作品对象。
//
// 路径前缀可能随前端版本变化，因此给出多组候选；再兜底做一次
// "任意 page 下的 item_list[0]" 的宽松搜索。
func (e *Extractor) pickItem(root jsonx.Node, id string) jsonx.Node {
	if v := jsonx.GetAny(root,
		"loaderData.video_(id)/page.videoInfoRes.item_list.0",
		"loaderData.video_(id)/page.videoInfoRes.aweme_detail",
		"loaderData.note_(id)/page.videoInfoRes.item_list.0",
		"videoInfoRes.item_list.0",
	); v != nil {
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	// 宽松兜底：遍历 loaderData 下所有 page，找第一个含 item_list 的
	if ld, ok := jsonx.Get(root, "loaderData").(map[string]any); ok {
		for _, page := range ld {
			if v := jsonx.GetAny(page, "videoInfoRes.item_list.0", "videoInfoRes.aweme_detail"); v != nil {
				if m, ok := v.(map[string]any); ok {
					return m
				}
			}
		}
	}
	return nil
}

// build 把抖音的作品对象映射为统一模型。
//
// 这里刻意对图片/视频两套结构都做尝试，而不是先用 canonical URL 判断类型：
// 判断依据本身也会变，而"有 images 就是图集"是稳定的事实。
func (e *Extractor) build(item jsonx.Node, id string) *core.Video {
	v := &core.Video{
		Platform:  core.PlatformDouyin,
		ID:        firstNonEmpty(jsonx.String(item, "aweme_id"), id),
		Title:     jsonx.String(item, "desc", "title"),
		Cover:     pickImage(item, "video.cover.url_list", "video.origin_cover.url_list", "video.dynamic_cover.url_list"),
		SourceURL: "https://www.douyin.com/video/" + id,
		Author: core.Author{
			ID:     jsonx.String(item, "author.sec_uid", "author.uid"),
			Name:   jsonx.String(item, "author.nickname", "author.unique_id"),
			Avatar: pickImage(item, "author.avatar_thumb.url_list", "author.avatar_168x168.url_list"),
		},
		Stats: &core.Stats{
			Like:    jsonx.Int(item, "statistics.digg_count"),
			Comment: jsonx.Int(item, "statistics.comment_count"),
			Share:   jsonx.Int(item, "statistics.share_count"),
			Collect: jsonx.Int(item, "statistics.collect_count"),
			// 注意：抖音 Web 接口的 play_count 恒为 0，不要用它当真实播放量
			View: jsonx.Int(item, "statistics.play_count"),
		},
	}

	// 图集
	for _, img := range jsonx.Slice(item, "images") {
		urls := jsonx.StringList(img, "url_list", "download_url_list")
		u := preferNonWebP(urls)
		if u == "" {
			continue
		}
		v.Images = append(v.Images, core.Image{
			URL:          u,
			LivePhotoURL: jsonx.String(img, "video.play_addr.url_list.0"),
			Width:        int(jsonx.Int(img, "width")),
			Height:       int(jsonx.Int(img, "height")),
		})
	}

	// 视频流：优先取无水印的 play_addr。
	//
	// 抖音的字段语义：play_addr 是**无水印**流，download_addr 才是带水印的
	// （其 URL 上带 &watermark=1）。另外分享页可能仍返回老的 playwm 地址，
	// 因此这里同时做 playwm→play 的替换作为兜底。
	streams := e.collectStreams(item)
	if len(streams) > 0 && len(v.Images) == 0 {
		core.SortStreams(streams)
		v.Videos = streams
	} else if len(v.Images) > 0 {
		// 图集的 music 字段实际是背景音乐
		if mu := jsonx.String(item, "music.play_url.url_list.0"); mu != "" {
			v.Music = &core.Stream{URL: mu, Quality: jsonx.String(item, "music.title")}
		}
	}
	if len(v.Videos) == 0 && len(v.Images) == 0 {
		return &core.Video{
			Platform: core.PlatformDouyin, ID: v.ID,
			Warning: "未取到视频或图集（可能已删除、仅作者可见或需要登录）",
		}
	}
	return v
}

// collectStreams 汇总所有可用视频流，并明确排除带水印的 download_addr。
func (e *Extractor) collectStreams(item jsonx.Node) []core.Stream {
	var out []core.Stream

	// add 登记一条"只给 URL、没有 bit_rate 元数据"的流。
	//
	// codec 必须显式传进来：SortStreams 按 codec 排序（avc1 > hev1 > av01），
	// 留空会让这条流在排序里失去身份——实测表现为 4K 的 H.264 流排在
	// 4K 的 HEVC 之后，最终选中一个对树莓派不友好的编码。
	add := func(path, quality, codec string) {
		urls := jsonx.StringList(item, path)
		u := preferNonWebP(urls)
		if u == "" {
			return
		}
		out = append(out, core.Stream{
			URL:       cleanPlayURL(u),
			Quality:   quality,
			Codec:     codec,
			MimeType:  "video/mp4",
			Width:     int(jsonx.Int(item, "video.width")),
			Height:    int(jsonx.Int(item, "video.height")),
			FrameRate: fmt.Sprintf("%d", jsonx.Int(item, "video.ratio")),
		})
	}

	// 多清晰度档位（web detail 接口才有：bit_rate[].play_addr）
	for _, br := range jsonx.Slice(item, "video.bit_rate") {
		u := preferNonWebP(jsonx.StringList(br, "play_addr.url_list"))
		if u == "" {
			continue
		}
		out = append(out, core.Stream{
			URL:       cleanPlayURL(u),
			Quality:   jsonx.String(br, "gear_name"),
			QualityID: int(jsonx.Int(br, "quality_type")),
			Codec:     codecName(jsonx.Bool(br, "is_h265")),
			Bandwidth: int(jsonx.Int(br, "bit_rate")),
			Width:     int(jsonx.Int(br, "play_addr.width")),
			Height:    int(jsonx.Int(br, "play_addr.height")),
			Size:      jsonx.Int(br, "play_addr.data_size"),
			MimeType:  "video/mp4",
		})
	}

	// 主地址。实测 detail 接口的 video.play_addr 与 video.play_addr_h264
	// 是**逐字节相同的 URL**，所以它就是 H.264，codec 标 avc1。
	// 无水印：而 download_addr 是带水印的降级版（实测 720p），见下面的兜底分支。
	add("video.play_addr.url_list", "默认", "avc1")
	add("video.play_addr_h264.url_list", "H.264", "avc1")
	// HEVC 变体（detail 接口叫 play_addr_265；分享页时代叫 play_addr_bytevc1）
	add("video.play_addr_265.url_list", "H.265", "hev1")
	// 最差兜底：带水印地址。只在前面都没拿到时才用，并显式标注
	if len(out) == 0 {
		if u := jsonx.String(item, "video.download_addr.url_list.0"); u != "" {
			out = append(out, core.Stream{
				URL:      cleanPlayURL(u),
				Quality:  "带水印（唯一可用源）",
				MimeType: "video/mp4",
			})
		}
	}
	return dedupeByURL(out)
}

// dedupeByURL 去掉 URL 完全相同的重复流，保留第一次出现的那个。
//
// detail 接口会从多个字段给出同一个文件（play_addr 与 play_addr_h264
// 就是逐字节相同），bit_rate 的多个档位之间也常共用 URL。同一个 URL
// 就是同一个文件，重复登记只会让响应变长、让客户端多一次无意义的选择。
//
// 保留"先出现的"是有意的：bit_rate 档位排在前面，带着 codec/带宽/档位名，
// 信息比后面那两条裸 URL 丰富。
func dedupeByURL(in []core.Stream) []core.Stream {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if _, dup := seen[s.URL]; dup {
			continue
		}
		seen[s.URL] = struct{}{}
		out = append(out, s)
	}
	return out
}

func (e *Extractor) expand(ctx context.Context, href string) (string, error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", core.BadInput(core.PlatformDouyin, "链接无效: %s", href)
	}
	host := strings.ToLower(u.Hostname())
	// 只有 v.douyin.com 这类短链需要展开
	if host == "v.douyin.com" || host == "v.iesdouyin.com" {
		loc, err := e.d.Client.ResolveLocation(ctx, href, netx.Headers{
			"User-Agent": e.d.Params.Mobile(),
		})
		if err != nil {
			return "", core.Upstream(core.PlatformDouyin, "expand", err)
		}
		return loc, nil
	}
	return href, nil
}

// extractID 从展开后的链接里取出作品 ID。
func (e *Extractor) extractID(href string) (string, error) {
	u, err := url.Parse(href)
	if err != nil {
		return "", core.BadInput(core.PlatformDouyin, "链接无效: %s", href)
	}
	// 精选页：?modal_id=xxx
	for _, k := range []string{"modal_id", "vid", "aweme_id"} {
		if v := u.Query().Get(k); isNumericID(v) {
			return v, nil
		}
	}
	segs := urlx.PathSegments(u.Path)
	for i, s := range segs {
		switch s {
		case "video", "note", "slides":
			if i+1 < len(segs) && isNumericID(segs[i+1]) {
				return segs[i+1], nil
			}
		}
	}
	// 兜底：最后一段是数字就用它
	if last := urlx.LastPathSegment(u.Path); isNumericID(last) {
		return last, nil
	}
	return "", core.Unsupported(core.PlatformDouyin, "无法从链接中识别作品 ID: %s", href)
}

// --- 小工具 ---

func isNumericID(s string) bool {
	if len(s) < 10 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// cleanPlayURL 去掉水印标记。
//
// 分享页常见 `playwm`（watermark）路径，替换为 `play` 即为无水印版本；
// 同时移除 URL 上残留的 watermark 查询参数。
func cleanPlayURL(u string) string {
	u = strings.ReplaceAll(u, "/playwm/", "/play/")
	u = strings.ReplaceAll(u, "playwm", "play")
	u = strings.ReplaceAll(u, "&watermark=1", "")
	u = strings.ReplaceAll(u, "watermark=1&", "")
	u = strings.ReplaceAll(u, "?watermark=1", "")
	return u
}

// preferNonWebP 优先选非 webp 的地址：webp 在多数播放器/图库里兼容性更差。
func preferNonWebP(urls []string) string {
	if len(urls) == 0 {
		return ""
	}
	for _, u := range urls {
		if !strings.Contains(u, ".webp") {
			return u
		}
	}
	return urls[0]
}

func pickImage(node jsonx.Node, paths ...string) string {
	for _, p := range paths {
		if u := preferNonWebP(jsonx.StringList(node, p)); u != "" {
			return u
		}
	}
	return ""
}

func codecName(isH265 bool) string {
	if isH265 {
		return "hevc"
	}
	return "avc1"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
