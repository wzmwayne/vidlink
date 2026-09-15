package server

import (
	"io"
	"net/http"
	"net/url"
	"strings"

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
//   - 开启时必须配置域名白名单，否则启动即报错；
//   - 不跟随重定向（避免白名单被 302 绕过）；
//   - 只允许 GET/HEAD。
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

	// 白名单校验：防止该接口被当作开放代理使用
	if !s.proxyHostAllowed(target.Hostname()) {
		writeError(w, core.Errf(core.KindForbidden, "", "proxy",
			"目标域名 %s 不在 VIDLINK_PROXY_ALLOW_HOSTS 白名单内", target.Hostname()))
		return
	}

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

func (s *Server) proxyHostAllowed(host string) bool {
	if len(s.cfg.ProxySrv.AllowHosts) == 0 {
		return false
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
func defaultReferer(host string) string {
	switch {
	case urlx.HostHasSuffix(host, "bilibili.com"), urlx.HostHasSuffix(host, "bilivideo.com"),
		urlx.HostHasSuffix(host, "hdslb.com"), urlx.HostHasSuffix(host, "akamaized.net"):
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
