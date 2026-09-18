package server

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"vidlink/internal/account"
	"vidlink/internal/core"
	"vidlink/internal/quota"
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
	// 长度上限：URL 本身没有理由超过 4KB（超过的多半是构造出来的），
	// 提前拒掉可以省下解析与日志上的开销。
	if len(raw) > 4096 {
		writeError(w, core.BadInput("", "url 过长（上限 4096 字节）"))
		return
	}

	target, err := url.Parse(raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		writeError(w, core.BadInput("", "url 必须是 http/https 绝对地址"))
		return
	}
	// 带 userinfo 的 URL（http://user:pass@host/）会把这串凭据转发给上游，
	// 也会让日志里出现不该出现的东西。合法媒体直链不需要它。
	if target.User != nil {
		writeError(w, core.BadInput("", "url 不允许携带用户名/密码"))
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

	// Referer/UA：可由调用方覆盖，否则用默认值。
	//
	// 这两个值都来自查询参数并会被写进**请求头**，所以按"外来字符串"处理：
	// 只接受 http/https 绝对地址（Referer），去掉控制字符并限长。
	// Go 的 transport 本身会拒掉含 CR/LF 的头值（防响应/请求头注入），
	// 这里再收一道是纵深防御，也顺手把超长值挡在日志之外。
	referer := strings.TrimSpace(r.URL.Query().Get("referer"))
	if referer != "" {
		u, err := url.Parse(referer)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeError(w, core.BadInput("", "referer 必须是 http/https 绝对地址"))
			return
		}
		referer = sanitizeHeaderValue(referer, 1024)
	} else {
		referer = defaultReferer(target.Hostname())
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	ua := sanitizeHeaderValue(r.URL.Query().Get("ua"), 256)
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

	// 配额结算**必须在写任何响应头之前**：一来配额不足要在发出字节前就拒绝，
	// 二来一旦先把上游的 Content-Length 写进响应头，再改写成错误体就会
	// 让客户端按错误的长度等待（连接被挂住直到超时）。
	declared := proxyDeclaredBytes(resp)
	if !s.chargeProxyDeclared(w, r, declared) {
		return
	}

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

	// 上游没声明长度时（chunked）才需要限流：声明了长度的那条路径
	// 已经按声明值扣过费，多传就等于多送。
	var src io.Reader = resp.Body
	if declared <= 0 {
		limit := s.proxyBudgetBytes(r)
		if s.cfg.ProxySrv.MaxBytes > 0 {
			limit = minInt64(limit, s.cfg.ProxySrv.MaxBytes)
		}
		src = io.LimitReader(resp.Body, limit)
	}

	// 32KB 缓冲区：流式转发的吞吐与内存占用的平衡点
	buf := make([]byte, 32<<10)
	written, err := io.CopyBuffer(w, src, buf)
	if err != nil {
		// 客户端中断是常态（用户拖进度条），不值得记为错误
		if !isClientGone(err) {
			s.log.Debug("代理传输中断", "url", target.String(), "err", err)
		}
	}
	// 长度未知的那条路径只能事后按实结算（此时响应体已经发出，
	// X-Quota-* 来不及写，失败也只能记日志——所以它只是兜底路径）
	if declared <= 0 {
		s.chargeProxyActual(r, written)
	}
}

// --- 媒体代理的配额计量 ---
//
// 公式：实扣 = 传输体积(MiB) × 1 × 账号倍率（quota.ProxyCost）。
// **不乘平台系数**：代理搬的是任意 CDN 的字节，与"哪家平台解析更贵"无关。
//
// 计费时机分两条路径：
//
//   - **上游声明了长度**（Content-Length，Range 请求时它就是这一段的长度）：
//     先扣后传。配额不足时一个字节都不发就回 429，且 X-Quota-* 能如实写进响应。
//   - **长度未知**（chunked）：按账户余额折算出一个字节上限边传边限，
//     传完再按实际字节扣。这是兜底，不是主路径。
//
// 提前中断（用户拖进度条、客户端断开）**不退**：按声明长度计费是"这次
// 请求占用了多少出口带宽"的度量，且退配额需要往账本里写补偿记录，
// 复杂度远大于它解决的问题。

// proxyDeclaredBytes 返回上游声明的本次传输字节数；-1 表示未知。
func proxyDeclaredBytes(resp *http.Response) int64 {
	if resp.ContentLength > 0 {
		// Range 请求时上游回 206，Content-Length 就是这一段的长度，
		// 正是我们要计费的口径（不能拿 Content-Range 里的**总长**去扣，
		// 那样一次 1KB 的 seek 会被按整部片子收费）。
		return resp.ContentLength
	}
	return -1
}

// chargeProxyDeclared 按声明长度结算。返回 false 表示已经写了错误响应。
func (s *Server) chargeProxyDeclared(w http.ResponseWriter, r *http.Request, declared int64) bool {
	if declared <= 0 || r.Method == http.MethodHead {
		return true // 未知长度走到传完再扣；HEAD 没有响应体，不计费
	}
	acct, ok := s.proxyAccount(r)
	if !ok {
		return true // 免校验模式：没有账户，也就没有配额
	}
	units := quota.ProxyCostAt(declared, s.proxyRate(), acct.Multiplier)
	detail := fmt.Sprintf("proxy %.4g MiB（按体积）", float64(declared)/float64(quota.ProxyUnitBytes))

	// 公共账号：额度在每 IP 日限额里，账本只记用量（见 consumePublicQuota）
	if acct.PublicAccount {
		snap, err := s.publicQ.Charge(s.clientIP(r), units)
		if err != nil {
			s.writeQuotaError(w, err)
			return false
		}
		if err := s.accounts.RecordUsage(acct.Key, units,
			"public@"+s.clientIP(r)+" "+detail); err != nil {
			s.log.Error("公共账号代理记账失败", "request_id", requestID(r.Context()),
				"key", acct.Masked(), "units", units, "err", err)
		}
		setQuotaHeaders(w, units, snap.Remaining)
		return true
	}

	rc, err := s.accounts.Consume(acct.Key, units, detail)
	if err != nil {
		var insuf account.ErrQuotaExhausted
		if errors.As(err, &insuf) {
			writeErrorStatus(w, http.StatusTooManyRequests, "quota_exhausted",
				fmt.Sprintf("配额不足：本次代理需要 %.2f（%.1f MiB，按 %.4g 配额/MiB × 账号倍率 %.2f），"+
					"当前剩余 %.2f；配额由管理员分配，请联系管理员调整",
					insuf.Need, float64(declared)/float64(quota.ProxyUnitBytes),
					s.proxyRate(), acct.Multiplier, insuf.Have))
			return false
		}
		s.log.Error("代理配额扣减失败", "request_id", requestID(r.Context()),
			"key", acct.Masked(), "units", units, "err", err)
		writeError(w, core.E(core.KindInternal, "", "quota", "配额处理失败", err))
		return false
	}
	setQuotaHeaders(w, rc.Units, rc.Quota)
	return true
}

// chargeProxyActual 是长度未知时的兜底结算：按实际发出的字节补扣。
func (s *Server) chargeProxyActual(r *http.Request, written int64) {
	if written <= 0 {
		return
	}
	acct, ok := s.proxyAccount(r)
	if !ok {
		return
	}
	units := quota.ProxyCostAt(written, s.proxyRate(), acct.Multiplier)
	if units <= 0 {
		return
	}
	detail := fmt.Sprintf("proxy %.4g MiB（上游未给长度，按实际传输）",
		float64(written)/float64(quota.ProxyUnitBytes))
	if acct.PublicAccount {
		if _, err := s.publicQ.Charge(s.clientIP(r), units); err != nil {
			s.log.Warn("公共账号代理额度补扣未成功（流量已发出）",
				"request_id", requestID(r.Context()), "ip", s.clientIP(r), "err", err)
			return
		}
		if err := s.accounts.RecordUsage(acct.Key, units,
			"public@"+s.clientIP(r)+" "+detail); err != nil {
			s.log.Error("公共账号代理记账失败", "err", err)
		}
		return
	}
	if _, err := s.accounts.Consume(acct.Key, units, detail); err != nil {
		// 字节已经发出去了，只能记账：这是"上游不给长度"这条兜底路径的已知代价
		s.log.Warn("代理按实际字节补扣未成功（流量已发出）",
			"request_id", requestID(r.Context()), "key", acct.Masked(),
			"bytes", written, "units", units, "err", err)
	}
}

// proxyAccount 取当前请求的账号；免校验模式或没有账号时返回 false。
func (s *Server) proxyAccount(r *http.Request) (account.Account, bool) {
	if s.cfg.IsEase() {
		return account.Account{}, false
	}
	return accountFrom(r.Context())
}

// proxyRate 返回媒体代理的费率（配额/MiB）。
//
// **所有账号一个价**：代理的成本是服务端出口带宽，与账号身份无关；
// 分档只会让"这次要花多少"变成需要查表的题。
func (s *Server) proxyRate() float64 {
	if r := s.cfg.ProxyRate; r > 0 {
		return r
	}
	return quota.ProxyRate
}

// proxyBudgetBytes 按账户剩余配额折算出本次最多能传的字节数。
//
// 上限存在的意义是防止"上游不给长度 + 配额不足"变成无限免费流量：
// 余额能付多少就传多少，传完就断。倍率为 0 的免费账号与免校验模式
// 返回一个足够大的值（它们本来就不计费）。
func (s *Server) proxyBudgetBytes(r *http.Request) int64 {
	const noLimit = int64(1) << 62
	acct, ok := s.proxyAccount(r)
	if !ok || acct.Multiplier <= 0 {
		return noLimit
	}
	// 公共账号的可付额度来自每 IP 日限额，而不是账本余额
	allowance := acct.Quota
	if acct.PublicAccount {
		allowance = s.publicQ.Snapshot(s.clientIP(r)).Remaining
	}
	if allowance <= 0 {
		return 0
	}
	rate := s.proxyRate()
	affordable := int64(allowance / (acct.Multiplier * rate) * float64(quota.ProxyUnitBytes))
	if affordable < 0 {
		return 0
	}
	// 多给 1 MiB 余量：否则"刚好付得起"的那一次会在最后一字节前被截断
	if affordable > noLimit-quota.ProxyUnitBytes {
		return noLimit
	}
	return affordable + quota.ProxyUnitBytes
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
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

// sanitizeHeaderValue 把一个外来字符串收拾成"可以安全放进请求头"的值。
//
// 去控制字符（含 CR/LF/NUL）、去掉首尾空白、按 rune 截断到 max 字节以内。
// 返回空串表示这个值不可用，调用方据此回退到默认值。
func sanitizeHeaderValue(v string, max int) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range v {
		if unicode.IsControl(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
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
