package webx

import (
	"bytes"
	"regexp"
	"strings"
)

// 各平台把首屏数据内嵌在 HTML 的 `window.XXX = {...}` 里。
// 这里集中管理与"从 HTML 里挖出那段 JSON"有关的知识，避免散落在各提取器。
//
// 之所以用正则而不是 HTML 解析器：目标是一段 script 文本，
// 用 DOM 解析反而更脆弱（脚本可能被 CDN 注入、标签可能自闭合异常）。
var (
	// 抖音：window._ROUTER_DATA = {...}</script>
	reDouyinRouterData = regexp.MustCompile(`(?s)window\._ROUTER_DATA\s*=\s*(\{.*?\})\s*</script>`)
	// 快手：window.INIT_STATE = {...}</script>
	reKuaishouInitState = regexp.MustCompile(`(?s)window\.INIT_STATE\s*=\s*(\{.*?\})\s*</script>`)
	// 快手 PC：window.__APOLLO_STATE__ = {...};
	reKuaishouApollo = regexp.MustCompile(`(?s)window\.__APOLLO_STATE__\s*=\s*(\{.*?\})\s*;?\s*</script>`)
	// 小红书：window.__INITIAL_STATE__ = {...}</script>
	reXhsInitialState = regexp.MustCompile(`(?s)window\.__INITIAL_STATE__\s*=\s*(\{.*?\})\s*</script>`)

	// 通用兜底：任意 window.<name> = {...}
	reGenericWindowVar = regexp.MustCompile(`(?s)window\.([A-Za-z_$][\w$]*)\s*=\s*(\{.*?\})\s*(?:;|</script>)`)
)

// ExtractWindowJSON 从 HTML 中提取 `window.<name> = {...}` 的 JSON 主体。
//
// 返回的字节已做基础清洗（去尾随分号）。多个候选变量名按顺序尝试，
// 这样前端改变量名时只需补一个候选。
func ExtractWindowJSON(html string, names ...string) ([]byte, bool) {
	for _, name := range names {
		if b, ok := extractNamed(html, name); ok {
			return b, true
		}
	}
	return nil, false
}

func extractNamed(html, name string) ([]byte, bool) {
	// 先尝试专用正则（更精确，能正确处理 `</script>` 边界）
	switch name {
	case "_ROUTER_DATA":
		if m := reDouyinRouterData.FindStringSubmatch(html); len(m) > 1 {
			return clean(m[1]), true
		}
	case "INIT_STATE":
		if m := reKuaishouInitState.FindStringSubmatch(html); len(m) > 1 {
			return clean(m[1]), true
		}
	case "__APOLLO_STATE__":
		if m := reKuaishouApollo.FindStringSubmatch(html); len(m) > 1 {
			return clean(m[1]), true
		}
	case "__INITIAL_STATE__":
		if m := reXhsInitialState.FindStringSubmatch(html); len(m) > 1 {
			return clean(m[1]), true
		}
	}
	// 通用兜底
	for _, m := range reGenericWindowVar.FindAllStringSubmatch(html, -1) {
		if len(m) > 2 && m[1] == name {
			return clean(m[2]), true
		}
	}
	return nil, false
}

func clean(s string) []byte {
	return bytes.TrimRight(bytes.TrimSpace([]byte(s)), "; \t\r\n")
}

// ExtractAllWindowJSON 返回 HTML 中所有 `window.X = {...}` 的 (变量名, JSON)。
//
// 用于"不知道变量名"的场景：例如快手初始化状态是按作品 ID 动态命名的键，
// 我们只关心结构而不是名字。
func ExtractAllWindowJSON(html string) map[string][]byte {
	out := make(map[string][]byte, 8)
	for _, m := range reGenericWindowVar.FindAllStringSubmatch(html, -1) {
		if len(m) > 2 {
			out[m[1]] = clean(m[2])
		}
	}
	// 专用正则命中的优先级更高，覆盖同名结果
	for name, b := range map[string][]byte{
		"_ROUTER_DATA":      firstOf(reDouyinRouterData, html),
		"INIT_STATE":        firstOf(reKuaishouInitState, html),
		"__APOLLO_STATE__":  firstOf(reKuaishouApollo, html),
		"__INITIAL_STATE__": firstOf(reXhsInitialState, html),
	} {
		if b != nil {
			out[name] = b
		}
	}
	return out
}

func firstOf(re *regexp.Regexp, html string) []byte {
	if m := re.FindStringSubmatch(html); len(m) > 1 {
		return clean(m[1])
	}
	return nil
}

// CanonicalHref 从 HTML 中取 <link rel="canonical"> 的地址。
// 抖音用它区分视频页与图集页（/video/ vs /note/），比猜 URL 形态可靠。
func CanonicalHref(html string) string {
	lower := strings.ToLower(html)
	i := strings.Index(lower, `rel="canonical"`)
	if i < 0 {
		// 属性顺序可能相反
		i = strings.Index(lower, `rel='canonical'`)
		if i < 0 {
			return ""
		}
	}
	// 在附近找 href
	seg := html[max(0, i-400):min(len(html), i+400)]
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)href\s*=\s*"([^"]+)"`),
		regexp.MustCompile(`(?i)href\s*=\s*'([^']+)'`),
	} {
		if m := re.FindStringSubmatch(seg); len(m) > 1 {
			return m[1]
		}
	}
	return ""
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
