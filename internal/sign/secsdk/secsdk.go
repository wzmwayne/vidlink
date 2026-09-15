// Package secsdk 实现抖音 web 端的 x-secsdk-web-signature 签名。
//
// 它是什么
//
//	抖音对一部分接口（见「受保护路径」清单）要求请求同时携带三个东西：
//	a_bogus、timestamp、x-secsdk-web-signature，外加 uifid。
//	本包负责后两者中的签名与 uifid 的读取。
//
// 算法
//
//	query = URLSearchParams(业务参数 + a_bogus + verifyFp + fp + uifid + timestamp)
//	sig   = md5(f"{uifid}_{timestamp}_{SALT}_{query}")
//
// 名字里有 "secsdk"、叫 "signature"，但它是**纯 MD5 + 一个写死在客户端里的
// 公开盐**，不含任何密钥。也就是说它挡不住任何有心人——它的真实作用是
// 「格式约束 + 减速带」：强制调用方把参数序列化成和官方 JS 完全一致的字节。
//
// 正因为盐是公开的，**真正的门是 uifid**：签名覆盖了 uifid，而 uifid 是
// 服务端签发给浏览器的访客标识，任何算法都算不出来（需由浏览器或等价流程铸造）。
//
// 序列化必须逐字节一致
//
// 同一个字符串既被哈希、又被发到线上。若在预像里按一种方式转义、在 URL 里
// 按另一种方式转义，签的就是平台从未见过的字节。平台用它自己的
// URLSearchParams 序列化，规则为：
//
//	保留    : A-Z a-z 0-9 * - . _
//	空格    : '%20'（不是 '+'，见 URLSearchParams 的说明）
//	其余    : '%XX'（大写十六进制）
//
// 注意 Go 标准库的 url.QueryEscape 与它有两点不同：把 '~' 当作保留字符
// （不转义），且把空格转成 '+'。因此这里自己实现，不直接用 url.QueryEscape。
package secsdk

import (
	"crypto/md5"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Salt 是 douyin_web 这个 project 的盐。
//
// 它来自 lf-security.bytegoofy.com 的 runtime_bundler_34.js
// （@byted/secsdk-strategy，www.douyin.com 以 project-id="34" 加载）。
// 盐是**按 project 区分**的：字节系其它站点加载别的 bundle，可能用别的盐。
const Salt = "A96D855A08C0A9707F8BEF0D9A527E4E"

// 请求参数与头部名。
const (
	// SignatureParam 既可能出现在 query 里，也作为同名头部发送。
	SignatureParam = "x-secsdk-web-signature"
	// UIFIDParam 同理：既是 query 参数，也是头部。
	UIFIDParam = "uifid"
	// TimestampParam 只出现在 query 里。
	TimestampParam = "timestamp"
	// ExpireHeader 随头部一起发送。平台不带它也接受，但官方页面会带——
	// 而「看起来像官方页面」正是这套东西存在的意义。
	ExpireHeader = "x-secsdk-web-expire"
)

// UIFIDCookieNames 是 SDK 查找访客标识时依次尝试的 cookie 名，先命中先用。
//
// 浏览器铸造出来的通常叫 UIFID_TEMP；其余是同一值在不同上下文里的拼写。
var UIFIDCookieNames = []string{
	"uifid",
	"uifid_temp",
	"uifidtemp",
	"UIFID",
	"UIFID_TEMP",
	"UIFIDTEMP",
}

// VerifyFPCookie 这个 cookie 的值原样就是 verifyFp / fp 两个查询参数。
const VerifyFPCookie = "s_v_web_id"

// VerifyFPParams 是同一份 s_v_web_id 要以两个名字重复发送。
var VerifyFPParams = []string{"verifyFp", "fp"}

// PickUIFID 从 Cookie 头里取出访客标识；取不到返回空串。
//
// 返回空串是**正常结果**而不是错误：调用方据此判断"这个身份不够，
// 受保护接口走不通"，而不是拿一个伪造值去撞墙。
func PickUIFID(cookieHeader string) string {
	for _, name := range UIFIDCookieNames {
		if v := CookieValue(cookieHeader, name); v != "" {
			return v
		}
	}
	return ""
}

// CookieValue 从 "k=v; k=v" 形式的 Cookie 头里取某个键的值。
//
// 不区分大小写匹配 cookie 名：浏览器与 SDK 对大小写的处理并不统一。
func CookieValue(cookieHeader, name string) string {
	if cookieHeader == "" || name == "" {
		return ""
	}
	for _, part := range strings.Split(cookieHeader, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// URLSearchParams 按官方 SDK 的规则序列化参数。
//
// 转义表（由 golden vector 反推并逐字节验证）：
//
//	保留 : A-Z a-z 0-9 * - . _
//	空格 : '%20'
//	其余 : '%XX'（大写十六进制）
//
// 两处容易踩的坑，都是实测出来的：
//
//	空格 → '%20'，**不是** '+'。两者在概念上等价，但签名是对字节取的
//	  md5，写 '+' 就与平台看到的字节不同。所谓 "URLSearchParams" 这个名字
//	  在这里是误导：真正的序列化是 encodeURIComponent 风格。
//	'~'  → '%7E'。Go 的 url.QueryEscape 把 '~' 当保留字符不转义，不一致。
func URLSearchParams(pairs [][2]string) string {
	var b strings.Builder
	b.Grow(len(pairs) * 24)
	for i, kv := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		writeEncoded(&b, kv[0])
		b.WriteByte('=')
		writeEncoded(&b, kv[1])
	}
	return b.String()
}

func writeEncoded(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		default:
			const hexDigits = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
}

// Signature 计算签名的裸值（32 位小写十六进制）。
//
// query 必须是**最终要发出去的**那条 query，且已包含 uifid 与 timestamp。
func Signature(uifid string, timestamp int64, query string) string {
	h := md5.New()
	// 逐段写入，不把预像拼成一个字符串——它可能有几 KB，能省一次分配。
	// 四段之间各插一个 '_'：uifid _ timestamp _ Salt _ query
	h.Write([]byte(uifid))
	h.Write([]byte{'_'})
	var buf [20]byte
	h.Write(strconv.AppendInt(buf[:0], timestamp, 10))
	h.Write([]byte{'_'})
	h.Write([]byte(Salt))
	h.Write([]byte{'_'})
	h.Write([]byte(query))
	return hex.EncodeToString(h.Sum(nil))
}

// Sign 追加访客参数并签名，返回三段结果。
//
// pairs 是签名前的全部参数，**顺序即发送顺序**：业务参数在前，随后是
// a_bogus 层加的内容。uifid 与 timestamp 由本函数追加在末尾，因为签名覆盖它们。
//
// 若 pairs 里已经存在 uifid，则保留其原位、不再追加第二个：多一个 uifid
// 会改变预像，平台会拒绝。这与官方 SDK 的行为一致。
//
// timestamp 传 0 表示取当前时间（秒）。
func Sign(pairs [][2]string, uifid string, timestamp int64) (query, signature string, headers map[string]string) {
	if timestamp == 0 {
		timestamp = time.Now().Unix()
	}
	stamp := formatSeconds(timestamp)

	covered := make([][2]string, 0, len(pairs)+2)
	covered = append(covered, pairs...)
	hasUIFID := false
	for _, kv := range covered {
		if kv[0] == UIFIDParam {
			hasUIFID = true
			break
		}
	}
	if !hasUIFID {
		covered = append(covered, [2]string{UIFIDParam, uifid})
	}
	covered = append(covered, [2]string{TimestampParam, stamp})

	query = URLSearchParams(covered)
	signature = Signature(uifid, timestamp, query)
	query = query + "&" + SignatureParam + "=" + signature

	headers = map[string]string{
		UIFIDParam:     uifid,
		SignatureParam: signature,
		ExpireHeader:   stamp,
	}
	return query, signature, headers
}

func formatSeconds(ts int64) string {
	var buf [20]byte
	return string(strconv.AppendInt(buf[:0], ts, 10))
}

// SplitPairs 把一条 query string 拆成键值对，并**解码**。
//
// 解码是必须的：query 已经被 a_bogus 层百分号编码过，而签名要对整个
// 重新序列化的结果取哈希——每个值都要先还原、再统一编码一次，
// 否则"被哈希的字节"和"被发送的字节"会不一致。
func SplitPairs(query string) [][2]string {
	if query == "" {
		return nil
	}
	parts := strings.Split(query, "&")
	out := make([][2]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		out = append(out, [2]string{unescape(k), unescape(v)})
	}
	return out
}

// unescape 还原 URLSearchParams 的转义（含 '+' → 空格）。
//
// 先用 url.QueryUnescape；它遇到非法转义会报错，此时退回原串——
// 宁可签一个"未还原"的值，也不要因为一个畸形片段让整个解析失败。
func unescape(s string) string {
	if s == "" {
		return ""
	}
	if v, err := url.QueryUnescape(s); err == nil {
		return v
	}
	return s
}
