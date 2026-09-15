package server

import (
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"vidlink/internal/core"
	"vidlink/internal/urlx"
)

// handleProxy 是一个**受限的**流式媒体代理。
//
// 为什么需要它：平台的取流 CDN 大多做了 Referer 防盗链，浏览器直接
// 用 <video src="直链"> 会 403。代理会把正确的 Referer/UA 补上，
// 并原样透传 Range，于是播放器可以拖动进度条、服务端也不缓存整段视频
// （纯流式转发，内存占用与文件大小无关）。
//
// 安全上它是本项目风险最高的接口，因此：
//   - 默认关闭（VIDLINK_PROXY_ENDPOINT=false）；
//   - 域名白名单**可选**：填了就按后缀收紧，留空则放行任意 http/https 目标
//     （留空等于对外提供一个 HTTP 代理，别暴露到公网，启动日志会警告）；
//   - 不跟随重定向（避免白名单被 302 绕过）；
//   - 只允许 GET/HEAD。
//
// 另外支持两个可选参数，用来把"直链"变成一个名字正确的下载：
//
//	filename=<名字>   设置 Content-Disposition: attachment; filename=...
//	type=video/mp4    覆盖上游的 Content-Type（CDN 常返回 octet-stream）
//
// 没有它们的话，浏览器只能按 URL 最后一段命名——实测就是下载到一个
// 名叫 `proxy`、没有后缀的文件。
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("url"))
	if raw == "" {
		writeError(w, core.BadInput("", "缺少 url 参数"))
		return
	}

	target, err := url.Parse(raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		writeError(w, core.BadInput("", "url 必须是 http/https 绝对地址"))
		return
	}

	// 白名单校验（留空即不限制）
	if !s.proxyHostAllowed(target.Hostname()) {
		writeError(w, core.Errf(core.KindForbidden, "", "proxy",
			"目标域名 %s 不在 VIDLINK_PROXY_ALLOW_HOSTS 白名单内", target.Hostname()))
		return
	}

	// 下载文件名与 Content-Type 覆盖（都可选）。文件名会进响应头，
	// 必须消毒——它来自查询参数，放任就等于给了响应头注入的口子。
	filename := sanitizeFilename(r.URL.Query().Get("filename"))
	ctype := safeContentType(r.URL.Query().Get("type"))

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), nil)
	if err != nil {
		writeError(w, core.BadInput("", "构造上游请求失败: %v", err))
		return
	}

	// 请求头：Range 必须透传，否则播放器无法 seek
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	if im := r.Header.Get("If-Range"); im != "" {
		req.Header.Set("If-Range", im)
	}

	// Referer/UA：可由调用方覆盖，否则用默认值
	referer := r.URL.Query().Get("referer")
	if referer == "" {
		referer = defaultReferer(target.Hostname())
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	ua := r.URL.Query().Get("ua")
	if ua == "" {
		ua = s.cfg.Net.DefaultUserAgent()
	}
	req.Header.Set("User-Agent", ua)

	// 不跟随重定向：避免攻击者用一个受信域名 302 到任意地址
	client := *s.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := client.Do(req)
	if err != nil {
		writeError(w, core.E(core.KindUpstream, "", "proxy", "上游请求失败", err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// 透传与播放相关的响应头
	for _, h := range []string{
		"Content-Type", "Content-Length", "Content-Range",
		"Accept-Ranges", "ETag", "Last-Modified", "Cache-Control",
	} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if ctype != "" {
		// 上游对 .m4s 常回 application/octet-stream，浏览器据此无法判断类型；
		// 调用方明确知道这是什么时允许覆盖。
		w.Header().Set("Content-Type", ctype)
	}
	if filename != "" {
		// mime.FormatMediaType 会按 RFC 2231 处理非 ASCII（中文标题），
		// 手写 filename="..." 在中文名上会被浏览器截断或乱码。
		w.Header().Set("Content-Disposition",
			mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	}
	if w.Header().Get("Accept-Ranges") == "" {
		w.Header().Set("Accept-Ranges", "bytes")
	}
	// 让浏览器知道这是可缓存的媒体
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}

	w.WriteHeader(resp.StatusCode)

	if r.Method == http.MethodHead {
		return
	}

	var src io.Reader = resp.Body
	// 限制单次代理字节数（防止被用来刷流量）
	if s.cfg.ProxySrv.MaxBytes > 0 && resp.ContentLength < 0 {
		src = io.LimitReader(resp.Body, s.cfg.ProxySrv.MaxBytes)
	}

	// 32KB 缓冲区：流式转发的吞吐与内存占用的平衡点
	buf := make([]byte, 32<<10)
	if _, err := io.CopyBuffer(w, src, buf); err != nil {
		// 客户端中断是常态（用户拖进度条），不值得记为错误
		if !isClientGone(err) {
			s.log.Debug("代理传输中断", "url", target.String(), "err", err)
		}
	}
}

// proxyHostAllowed 判断目标域名是否放行。
//
// 空白名单 = **不限制**：配置了 VIDLINK_PROXY_ENDPOINT=true 就是明确要用它，
// 此时再要求一份域名清单只会逼人写个 `*`。要收紧就填具体后缀。
// 注意这条把口子开得很大：这个接口等同于一个 HTTP 代理，
// 别把它暴露到公网（启动日志会就此给出警告）。
func (s *Server) proxyHostAllowed(host string) bool {
	if len(s.cfg.ProxySrv.AllowHosts) == 0 {
		return true
	}
	for _, suffix := range s.cfg.ProxySrv.AllowHosts {
		if urlx.HostHasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// defaultReferer 依据目标域名给出合适的 Referer。
//
// 平台的 CDN 会校验 Referer 是否属于自家域，因此这里按域名映射，
// 而不是统一用一个固定值。
// sanitizeFilename 把调用方给的文件名消毒到"可以安全放进响应头"。
//
// 三件事必须做，少一件都有真实后果：
//
//  1. 去掉控制字符与引号、反斜杠 —— 否则可以闭合 Content-Disposition 的
//     引号往里塞新头（响应头注入），或让某些客户端解析错乱；
//  2. 去掉路径分隔符 —— 名字里带 `/` 或 `..` 时，部分浏览器会把它当路径，
//     变成"写到别的目录"（目录穿越）；
//  3. 限长 —— 标题可以很长，某些系统对文件名有 255 字节上限，
//     超长会被截断成乱码甚至保存失败。
//
// 保留中文与空格：现在的浏览器都支持 RFC 2231 编码，没有必要为了兼容
// 把中文标题抹掉。名字为空时返回空串，调用方据此不设 Content-Disposition。
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == '"' || r == '\'' || unicode.IsControl(r):
			// 丢弃这些字符而不是替换成下划线：替换会让 "a/b" 变成 "a_b"，
			// 看起来像用户原意，实际上已经不是一个路径了。
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	// 去掉纯点号的名字（"." ".." 这类在文件系统里有特殊含义）
	if strings.Trim(out, ".") == "" {
		return ""
	}
	const maxRunes = 120 // 约 360 字节 UTF-8，留出扩展名与 .crdownload 的余量
	if rs := []rune(out); len(rs) > maxRunes {
		out = strings.TrimSpace(string(rs[:maxRunes]))
	}
	return out
}

// safeContentType 只放行媒体类与 octet-stream，避免这个参数被用来
// 让代理回 text/html（那等于给自己开一个 XSS 落点）。
func safeContentType(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return ""
	}
	if i := strings.IndexByte(v, ';'); i >= 0 { // 去掉 charset 之类的参数
		v = strings.TrimSpace(v[:i])
	}
	switch v {
	case "video/mp4", "audio/mp4", "video/webm", "audio/webm",
		"video/x-matroska", "audio/mpeg", "video/mp2t",
		"application/octet-stream":
		return v
	default:
		return ""
	}
}

func defaultReferer(host string) string {
	switch {
	case urlx.HostHasSuffix(host, "bilibili.com"), urlx.HostHasSuffix(host, "bilivideo.com"),
		urlx.HostHasSuffix(host, "hdslb.com"), urlx.HostHasSuffix(host, "akamaized.net"),
		// B 站的 P2P CDN：实测 *.edge.mountaintoys.cn 同时要求 Referer 与 UA，
		// 缺任一个都返回 403（浏览器无法自行携带 Referer，只能靠代理补）。
		urlx.HostHasSuffix(host, "mountaintoys.cn"), urlx.HostHasSuffix(host, "p2pcdn.com"):
		return "https://www.bilibili.com/"
	case urlx.HostHasSuffix(host, "douyin.com"), urlx.HostHasSuffix(host, "douyinvod.com"),
		urlx.HostHasSuffix(host, "zjcdn.com"), urlx.HostHasSuffix(host, "ixigua.com"):
		return "https://www.douyin.com/"
	case urlx.HostHasSuffix(host, "xiaohongshu.com"), urlx.HostHasSuffix(host, "xhscdn.com"):
		return "https://www.xiaohongshu.com/"
	case urlx.HostHasSuffix(host, "kuaishou.com"), urlx.HostHasSuffix(host, "kwimgs.com"),
		urlx.HostHasSuffix(host, "kscdn.com"):
		return "https://www.kuaishou.com/"
	}
	return ""
}

func isClientGone(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "context canceled")
}
