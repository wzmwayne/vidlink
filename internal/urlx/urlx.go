// Package urlx 负责从"分享文案"里提取链接并归一化。
//
// 现实中的输入几乎从不是干净的 URL：
//
//	7.87 Pjm:/ 复制打开抖音，看看【xxx】的作品 https://v.douyin.com/iRxxxx/
//	【标题 - 哔哩哔哩】 https://b23.tv/abc123
//	99 复制打开小红书，看看【xxx】 https://xhslink.com/a/xxxx
//
// 因此提取逻辑必须对文案、emoji、全角字符鲁棒，且不能把文案里的
// 非链接文本误当成 URL。
package urlx

import (
	"net/url"
	"strings"

	"vidlink/internal/core"
)

// Extract 从任意文本中提取第一个 http(s) 链接。
//
// 返回的链接保持原样（不做跳转），由提取器决定是否展开短链。
func Extract(text string) (string, bool) {
	s := strings.TrimSpace(text)
	if s == "" {
		return "", false
	}

	// 优先按空白切分后再判断：能避免把后随的中文标点吞进 URL。
	for _, field := range strings.Fields(s) {
		if u := trimURL(field); isHTTP(u) {
			return u, true
		}
	}

	// 没有空白分隔时（整段就是一条 URL，或文案与 URL 粘连），
	// 退化为扫描 "http" 起始位置。
	idx := strings.Index(s, "http")
	if idx < 0 {
		return "", false
	}
	tail := s[idx:]
	// 在 URL 中截断常见的中文/全角标点与空白
	if end := strings.IndexAny(tail, " \t\r\n\u3000，。！？；：、（）【】《》“”‘’"); end >= 0 {
		tail = tail[:end]
	}
	tail = trimURL(tail)
	if !isHTTP(tail) {
		return "", false
	}
	return tail, true
}

// trimURL 去掉 URL 两侧常见的包裹字符与尾随标点。
func trimURL(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'<>()[]{}，。！？；：、`)
	// 尾随的英文标点通常是句子成分，不属于 URL
	s = strings.TrimRight(s, ".,;:!?")
	// 仅在路径里出现过 '/' 时才保留尾斜杠，否则去掉（如 https://a.com/ → https://a.com）
	return s
}

func isHTTP(s string) bool {
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Host != ""
}

// Parse 把文本归一化为 core.URL。
//
// 若文本中找不到链接，会退化为"把整段文本当作平台内 ID"，
// 以满足「按 ID 直接解析」的调用方式。
func Parse(text string) (*core.URL, error) {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return nil, core.BadInput("", "输入为空")
	}

	href, ok := Extract(raw)
	if !ok {
		// 不是链接：当作裸 ID 处理，交给 ByIDExtractor。
		return &core.URL{Raw: raw, ID: raw}, nil
	}

	u, err := url.Parse(href)
	if err != nil {
		return nil, core.BadInput("", "链接格式无效: %s", href)
	}
	return &core.URL{
		Raw:   raw,
		Href:  href,
		Host:  strings.ToLower(u.Hostname()),
		Path:  u.Path,
		Query: u.RawQuery,
	}, nil
}

// HostHasSuffix 判断 host 是否等于 suffix 或以 "."+suffix 结尾。
//
// 刻意不用 strings.Contains：`Contains("evil-bilibili.com", "bilibili.com")`
// 会误判，这是一个真实的跨站劫持隐患。
func HostHasSuffix(host, suffix string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	suffix = strings.ToLower(suffix)
	if host == suffix {
		return true
	}
	return strings.HasSuffix(host, "."+suffix)
}

// PathSegments 返回去掉空段后的路径分段。
func PathSegments(p string) []string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	out := parts[:0]
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// LastPathSegment 返回路径最后一段（常用于取内容 ID）。
func LastPathSegment(p string) string {
	segs := PathSegments(p)
	if len(segs) == 0 {
		return ""
	}
	return segs[len(segs)-1]
}

// Query 从 core.URL 的原始查询串里取参数。
func Query(u *core.URL, key string) string {
	if u == nil || u.Query == "" {
		return ""
	}
	v, err := url.ParseQuery(u.Query)
	if err != nil {
		return ""
	}
	return v.Get(key)
}
