// Package wbi 实现哔哩哔哩 Web 端的 WBI 签名（w_rid / wts）。
//
// 背景
//
//	自 2023-03 起，B 站 web 端大量查询接口（x/player/wbi/playurl、
//	x/web-interface/wbi/view、x/space/wbi/arc/search 等）要求 query 中
//	带 w_rid 与 wts；缺失或错误会返回 v_voucher（而不是直接 403），
//	表现为"能通但拿不到数据"。
//
// 算法（纯计算，无 JS）
//
//  1. 从 https://api.bilibili.com/x/web-interface/nav 取
//     data.wbi_img.img_url / sub_url，截取文件名得 img_key / sub_key；
//  2. raw = img_key + sub_key，按 MIXIN_KEY_ENC_TAB 重排后取前 32 位 → mixin_key；
//  3. params 加 wts（秒级时间戳），按 key 升序排序，剔除值中的 !'()* 字符，
//     以 encodeURIComponent 规则百分号编码，拼成 query；
//  4. w_rid = md5(query + mixin_key)。
//
// img_key/sub_key 全站统一，每日轮换，因此这里做了带 TTL 的缓存 + 并发合并。
package wbi

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"vidlink/internal/core"
	"vidlink/internal/netx"
)

// mixinKeyEncTab 是官方的字符重排表（长 64）。
var mixinKeyEncTab = [64]int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35, 27, 43, 5, 49,
	33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13, 37, 48, 7, 16, 24, 55, 40,
	61, 26, 17, 0, 1, 60, 51, 30, 4, 22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11,
	36, 20, 34, 44, 52,
}

// navEndpoint 是获取实时口令的接口。未登录时返回 code=-101，但 data 里仍有 wbi_img。
const navEndpoint = "https://api.bilibili.com/x/web-interface/nav"

// Keys 是一次签名所需的全部密钥材料。
type Keys struct {
	Img   string
	Sub   string
	Mixin string
}

// MixinKey 由 img_key 与 sub_key 计算 mixin_key（纯函数，便于单测）。
//
// 注意 MIXIN_KEY_ENC_TAB 的索引可能超过字符串长度，调用方需保证
// len(img+sub) > 52；B 站返回值恒为 32+32=64 位十六进制，满足要求。
func MixinKey(img, sub string) string {
	raw := img + sub
	var b [32]byte
	for i := range b {
		idx := mixinKeyEncTab[i]
		if idx >= len(raw) {
			// 防御：异常短的 key 直接返回空，由调用方判错。
			return ""
		}
		b[i] = raw[idx]
	}
	return string(b[:])
}

// SignQuery 对参数做 WBI 签名，返回可直接拼到 URL 的 query 字符串。
//
// 返回的字符串形如：
//
//	bar=514&foo=114&wts=1702204169&zab=1919810&w_rid=8f6f2b5b3d485fe1886cec6a0be8c5d4
//
// 注意：这里刻意返回字符串而不是 url.Values，因为 url.Values.Encode()
// 使用 QueryEscape（空格→'+'、'~'→"%7E"），与签名时使用的
// encodeURIComponent 口径不同。虽然两者在服务端解码后等价，
// 但直接发出签名时用的那一份 query 可以完全排除歧义。
func SignQuery(params url.Values, mixinKey string, now time.Time) string {
	out := cleanValues(params)
	out.Set("wts", fmt.Sprintf("%d", now.Unix()))

	query := canonicalQuery(out) // 此时还不含 w_rid
	sum := md5.Sum([]byte(query + mixinKey))
	return query + "&w_rid=" + hex.EncodeToString(sum[:])
}

// SignValues 与 SignQuery 相同，但返回 url.Values（含 w_rid/wts），
// 便于调用方继续合并其它参数。
func SignValues(params url.Values, mixinKey string, now time.Time) url.Values {
	out := cleanValues(params)
	out.Set("wts", fmt.Sprintf("%d", now.Unix()))
	query := canonicalQuery(out)
	sum := md5.Sum([]byte(query + mixinKey))
	out.Set("w_rid", hex.EncodeToString(sum[:]))
	return out
}

// cleanValues 复制参数并剔除值中的 !'()* 字符。
func cleanValues(params url.Values) url.Values {
	out := make(url.Values, len(params)+2)
	for k, vs := range params {
		cleaned := make([]string, 0, len(vs))
		for _, v := range vs {
			cleaned = append(cleaned, stripIllegal(v))
		}
		out[k] = cleaned
	}
	return out
}

// canonicalQuery 按 key 升序 + encodeURIComponent 规则拼 query。
func canonicalQuery(v url.Values) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		vals := v[k]
		sort.Strings(vals)
		for j, val := range vals {
			if j > 0 {
				b.WriteByte('&')
			}
			b.WriteString(encodeURIComponent(k))
			b.WriteByte('=')
			b.WriteString(encodeURIComponent(val))
		}
	}
	return b.String()
}

// stripIllegal 剔除 B 站签名前要求去掉的 5 个字符。
func stripIllegal(s string) string {
	if !strings.ContainsAny(s, "!'()*") {
		return s
	}
	return strings.NewReplacer("!", "", "'", "", "(", "", ")", "", "*", "").Replace(s)
}

// encodeURIComponent 等价于 JavaScript 的 encodeURIComponent。
//
// 不能用 url.QueryEscape：它把空格编码为 '+'，而 B 站要求 %20，
// 且不允许大小写混淆（十六进制必须大写）。
func encodeURIComponent(s string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperhex[c>>4])
		b.WriteByte(upperhex[c&0x0f])
	}
	return b.String()
}

// isUnreserved 对应 encodeURIComponent 不做转义的字符集。
func isUnreserved(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	switch c {
	case '-', '_', '.', '!', '~', '*', '\'', '(', ')':
		return true
	}
	return false
}

// Manager 负责实时口令的获取、缓存与并发合并。
type Manager struct {
	client   *netx.Client
	endpoint string
	ttl      time.Duration

	mu        sync.RWMutex
	cached    Keys
	fetchedAt time.Time

	// inflight 用于并发合并：同一时刻只允许一个刷新在跑。
	inflight *sync.Mutex
}

// Option 用于调整 Manager。
type Option func(*Manager)

// WithTTL 设置口令缓存时长（默认 6 小时；B 站每日轮换，留足余量）。
func WithTTL(d time.Duration) Option {
	return func(m *Manager) { m.ttl = d }
}

// WithEndpoint 覆盖 nav 接口地址（测试用）。
func WithEndpoint(e string) Option {
	return func(m *Manager) { m.endpoint = e }
}

// NewManager 构造 WBI 口令管理器。
func NewManager(c *netx.Client, opts ...Option) *Manager {
	m := &Manager{
		client:   c,
		endpoint: navEndpoint,
		ttl:      6 * time.Hour,
		inflight: &sync.Mutex{},
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Keys 返回当前有效的密钥，必要时刷新。
func (m *Manager) Keys(ctx context.Context) (Keys, error) {
	m.mu.RLock()
	k, at := m.cached, m.fetchedAt
	m.mu.RUnlock()
	if k.Mixin != "" && time.Since(at) < m.ttl {
		return k, nil
	}

	// 串行化刷新，避免每日零点前后的惊群。
	m.inflight.Lock()
	defer m.inflight.Unlock()

	m.mu.RLock()
	k, at = m.cached, m.fetchedAt
	m.mu.RUnlock()
	if k.Mixin != "" && time.Since(at) < m.ttl {
		return k, nil
	}

	fresh, err := m.fetch(ctx)
	if err != nil {
		// 刷新失败但有旧钥匙时降级使用旧钥匙：WBI key 轮换后有重叠期，
		// 用旧 key 仍可能成功，比直接失败体验好得多。
		if k.Mixin != "" {
			return k, nil
		}
		return Keys{}, err
	}

	m.mu.Lock()
	m.cached, m.fetchedAt = fresh, time.Now()
	m.mu.Unlock()
	return fresh, nil
}

// Invalidate 主动失效缓存（例如收到 v_voucher 时调用后重试）。
func (m *Manager) Invalidate() {
	m.mu.Lock()
	m.fetchedAt = time.Time{}
	m.mu.Unlock()
}

func (m *Manager) fetch(ctx context.Context) (Keys, error) {
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			WbiImg struct {
				ImgURL string `json:"img_url"`
				SubURL string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}

	headers := netx.Headers{
		"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Referer":    "https://www.bilibili.com/",
	}
	if err := m.client.GetJSON(ctx, m.endpoint, headers, &resp); err != nil {
		return Keys{}, core.E(core.KindUpstream, core.PlatformBilibili, "wbi.nav", "获取 WBI 口令失败", err)
	}
	// code=-101（未登录）也会返回两个 key，因此不作为错误。
	img := filenameKey(resp.Data.WbiImg.ImgURL)
	sub := filenameKey(resp.Data.WbiImg.SubURL)
	if img == "" || sub == "" {
		return Keys{}, core.Errf(core.KindUpstream, core.PlatformBilibili, "wbi.nav",
			"nav 未返回有效 wbi_img（code=%d msg=%s）", resp.Code, resp.Message)
	}
	mixin := MixinKey(img, sub)
	if mixin == "" {
		return Keys{}, core.E(core.KindInternal, core.PlatformBilibili, "wbi.mixin", "mixin key 计算失败", nil)
	}
	return Keys{Img: img, Sub: sub, Mixin: mixin}, nil
}

// filenameKey 从
// https://i0.hdslb.com/bfs/wbi/7cd084941338484aae1ad9425b84077c.png
// 提取 7cd084941338484aae1ad9425b84077c。
func filenameKey(raw string) string {
	if raw == "" {
		return ""
	}
	i := strings.LastIndexByte(raw, '/')
	if i >= 0 {
		raw = raw[i+1:]
	}
	if j := strings.IndexByte(raw, '.'); j >= 0 {
		raw = raw[:j]
	}
	return raw
}

// Sign 是一个便捷封装：取钥匙 + 签名，返回可用的 query 字符串。
func (m *Manager) Sign(ctx context.Context, params url.Values) (string, error) {
	keys, err := m.Keys(ctx)
	if err != nil {
		return "", err
	}
	return SignQuery(params, keys.Mixin, time.Now()), nil
}
