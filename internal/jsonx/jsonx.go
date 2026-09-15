// Package jsonx 提供「容错 + 多路径回退」的 JSON 读取工具。
//
// 为什么需要它：各平台的响应结构会随前端发版而变化（字段改驼峰/下划线、
// 层级调整、同名数组搬到别处）。如果代码里到处写死单一取值路径，
// 平台一改版就整条链路失败。
//
// 本包的做法是让调用方声明**一组候选路径**，按优先级取第一个非空值：
//
//	title := jsonx.Pick(node, "desc", "aweme_detail.desc", "item.title")
//
// 这样改版时通常只需在候选列表里补一条，而不是重写解析逻辑。
//
// 另外它容忍两种现实中很常见的"非法 JSON"：
//   - JS 字面量 undefined（小红书 __INITIAL_STATE__ 里大量出现）
//   - 尾随分号（`window.X = {...};`）
package jsonx

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Node 是解析后的 JSON 节点：map[string]any / []any / string / float64 / bool / nil。
type Node = any

// Unmarshal 解析 JSON，并做 JS 兼容的容错预处理。
func Unmarshal(b []byte) (Node, error) {
	b = Sanitize(b)
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return normalize(v), nil
}

// Sanitize 把 Web 端常见的"伪 JSON"修补成合法 JSON。
//
// 已处理的形态（都是实战中真实出现过的）：
//   - `undefined` → `null`（小红书 __INITIAL_STATE__）
//   - 尾随分号
//   - XSSI 前缀 `)]}',`
func Sanitize(b []byte) []byte {
	b = bytes.TrimSpace(b)
	// XSSI 前缀
	if bytes.HasPrefix(b, []byte(")]}'")) {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = bytes.TrimSpace(b[i+1:])
		}
	}
	// 尾随分号
	b = bytes.TrimRight(b, "; \t\r\n")
	// undefined → null
	if bytes.Contains(b, []byte("undefined")) {
		b = replaceToken(b, "undefined", "null")
	}
	// 单引号包裹的 key 也偶有出现；这里只处理最外层的简单情况，
	// 复杂情形交由调用方用正则预处理。
	return b
}

// replaceToken 只替换作为独立 token 出现的 old，避免误伤字符串内的同名子串。
func replaceToken(b []byte, old, new string) []byte {
	var out bytes.Buffer
	out.Grow(len(b))
	for i := 0; i < len(b); {
		// 字符串字面量整体拷贝，不替换其中内容
		if b[i] == '"' {
			j := i + 1
			for j < len(b) {
				if b[j] == '\\' {
					j += 2
					continue
				}
				if b[j] == '"' {
					j++
					break
				}
				j++
			}
			out.Write(b[i:min(j, len(b))])
			i = j
			continue
		}
		if bytes.HasPrefix(b[i:], []byte(old)) && boundaryOK(b, i, len(old)) {
			out.WriteString(new)
			i += len(old)
			continue
		}
		out.WriteByte(b[i])
		i++
	}
	return out.Bytes()
}

func boundaryOK(b []byte, i, n int) bool {
	isWord := func(c byte) bool {
		return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
	}
	if i > 0 && isWord(b[i-1]) {
		return false
	}
	if i+n < len(b) && isWord(b[i+n]) {
		return false
	}
	return true
}

// normalize 把 json.Number 转成 int64/float64，便于后续统一断言。
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = normalize(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalize(val)
		}
		return t
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	default:
		return v
	}
}

// Get 按点路径取值，支持数组下标，例如：
//
//	Get(node, "loaderData.video_(id)/page.videoInfoRes.item_list.0")
//	Get(node, "data.items[0].note_card.title")
//
// 路径中若含 `.` 或 `[` 的键名（如 `video_(id)/page`），请用方括号形式：
//
//	Get(node, "loaderData[video_(id)/page].videoInfoRes")
func Get(root Node, path string) Node {
	if root == nil || path == "" {
		return nil
	}
	cur := root
	for _, seg := range splitPath(path) {
		if cur == nil {
			return nil
		}
		switch c := cur.(type) {
		case map[string]any:
			cur = c[seg]
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(c) {
				return nil
			}
			cur = c[idx]
		default:
			return nil
		}
	}
	return cur
}

// splitPath 把 "a.b[0].c" 拆成 ["a","b","0","c"]。
func splitPath(path string) []string {
	var segs []string
	var buf strings.Builder
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '.':
			if buf.Len() > 0 {
				segs = append(segs, buf.String())
				buf.Reset()
			}
		case '[':
			if buf.Len() > 0 {
				segs = append(segs, buf.String())
				buf.Reset()
			}
			j := strings.IndexByte(path[i:], ']')
			if j < 0 {
				buf.WriteString(path[i:])
				i = len(path)
				break
			}
			segs = append(segs, path[i+1:i+j])
			i += j
		default:
			buf.WriteByte(path[i])
		}
	}
	if buf.Len() > 0 {
		segs = append(segs, buf.String())
	}
	return segs
}

// GetAny 按候选路径列表依次尝试，返回第一个存在（非 nil）的值。
func GetAny(root Node, paths ...string) Node {
	for _, p := range paths {
		if v := Get(root, p); v != nil {
			return v
		}
	}
	return nil
}

// String 按候选路径取第一个非空字符串。
func String(root Node, paths ...string) string {
	for _, p := range paths {
		if s := asString(Get(root, p)); s != "" {
			return s
		}
	}
	return ""
}

// Int 按候选路径取第一个非零整数。
func Int(root Node, paths ...string) int64 {
	for _, p := range paths {
		if n, ok := asInt(Get(root, p)); ok {
			return n
		}
	}
	return 0
}

// Float 按候选路径取第一个非零浮配额。
func Float(root Node, paths ...string) float64 {
	for _, p := range paths {
		if f, ok := asFloat(Get(root, p)); ok {
			return f
		}
	}
	return 0
}

// Bool 按候选路径取第一个为 true 的布尔值。
func Bool(root Node, paths ...string) bool {
	for _, p := range paths {
		switch v := Get(root, p).(type) {
		case bool:
			if v {
				return true
			}
		case string:
			if v == "true" || v == "1" {
				return true
			}
		case int64:
			if v != 0 {
				return true
			}
		}
	}
	return false
}

// Slice 按候选路径取第一个非空数组。
func Slice(root Node, paths ...string) []Node {
	for _, p := range paths {
		switch v := Get(root, p).(type) {
		case []any:
			if len(v) > 0 {
				return v
			}
		case map[string]any:
			// 有时平台把数组包在 {list: [...]} 里
			if inner, ok := v["list"].([]any); ok && len(inner) > 0 {
				return inner
			}
		}
	}
	return nil
}

// StringList 按候选路径取第一个非空字符串数组。
func StringList(root Node, paths ...string) []string {
	for _, p := range paths {
		switch v := Get(root, p).(type) {
		case []any:
			out := make([]string, 0, len(v))
			for _, item := range v {
				if s := asString(item); s != "" {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		case string:
			if v != "" {
				return []string{v}
			}
		}
	}
	return nil
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func asInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case float64:
		return int64(t), true
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, true
		}
		if f, err := t.Float64(); err == nil {
			return int64(f), true
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return i, true
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f, true
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
