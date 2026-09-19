package account

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 签名凭据：URL 里唯一允许出现的凭据形态。
//
// 为什么要有它：明文 Key 一旦进了 URL，就会留在浏览器历史、隧道/反代日志、
// 聊天记录与截屏里，而它是**长期有效**的。签名凭据把"能用"压缩到几十秒，
// 且签名本身不含秘密——泄露一张过期的签名，什么都换不来。
//
// 形态（定长，三段）：
//
//	acc_<16位小写hex句柄>.<8位小写hex时间戳>.<64位小写hex签名>
//	└──────── 20 ───────┘ │ └── 8 ──┘ │ └────── 64 ──────┘
//	                     分隔符        分隔符            总长 94
//
// 句柄是公开信息（由 Key 派生，不可逆），时间戳明文可见，签名是
// HMAC-SHA256 的**完整**输出（64 位十六进制，不截断）。
//
// 校验为什么不需要反推、也不需要遍历：
//
//	签名不是"把时间戳藏起来的密文"，它是"有钥匙者可以复算的函数"。
//	服务器读出票面的时间戳（纯减法判时效），再用自己手里的 Key 把
//	「句柄|时间戳」重新算一遍 HMAC 比对即可 —— 全部是正向计算。
//
// 防伪靠三个不变量：
//   - 算不出：没有 Key 得不到这个 HMAC，造不出票；
//   - 改不动：句柄与时间戳都在签名原料里，改期、换人都会对不上；
//   - 不用猜：正向复算一次即可，没有候选集合。
const (
	// HandlePrefix 是账号句柄前缀。
	HandlePrefix = "acc_"
	// AdminPrefix 是管理凭据句柄前缀（同一套数学，另一个命名空间）。
	AdminPrefix = "adm_"

	// DefaultTTL 是签名允许的时间偏差（也是它的有效期）。
	DefaultTTL = 30 * time.Second

	credHandleLen = 20 // acc_/adm_ + 16 位 hex
	credTSLen     = 8  // unix 秒的 %08x，够用到 2106 年
	credSigLen    = 64 // HMAC-SHA256 完整输出
	credSep       = '.'
	// CredLen 是一条凭据的确定长度。
	CredLen = credHandleLen + 1 + credTSLen + 1 + credSigLen
)

// HandleID 由秘密派生公开句柄：prefix + SHA-256(secret) 的前 8 字节。
//
// 取前 8 字节（16 位十六进制）：几百到几千个账号下碰撞概率可以忽略，
// 而长度足够短，能放进 URL 路径、日志和运维对话里。句柄不是秘密——
// 别人拿到它只能当"我是谁"用（还得配上签名），冒充不了任何身份。
func HandleID(prefix, secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return prefix + hex.EncodeToString(sum[:8])
}

// SignAt 为某个时刻生成一条完整凭据（句柄由 Key 派生）。
//
// ts 由调用方给出而不是内部取 time.Now()：这样测试可以构造任意时刻，
// 边界用例（恰好 30 秒、31 秒）不需要假时钟，也就能精确断言。
func SignAt(prefix, secret string, ts int64) string {
	return SignAtWith(HandleID(prefix, secret), secret, ts)
}

// SignAtWith 用已知句柄生成凭据。句柄与秘密必须匹配（否则签名验不过），
// 拆开是为了让服务端在"已经按句柄查到账号"后少算一次 SHA-256。
func SignAtWith(handle, secret string, ts int64) string {
	tsHex := FormatTS(ts)
	return handle + string(credSep) + tsHex + string(credSep) + macHex(secret, handle, tsHex)
}

// FormatTS 把 unix 秒格式化成凭据里的那 8 位十六进制。
func FormatTS(ts int64) string { return fmt.Sprintf("%08x", ts) }

// macHex 计算 hex(HMAC-SHA256(secret, handle + "|" + tsHex))。
//
// 消息里的时间戳用的是**票面那 8 位十六进制原样**（不是十进制秒数）：
// 实现方零转换、抄下来就能算，少一类"格式不对但对不上"的排查。
// 分隔符是单个 '|'（0x7C），没有换行、没有空格。
func macHex(secret, handle, tsHex string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(handle + "|" + tsHex))
	return hex.EncodeToString(m.Sum(nil))
}

// ParseCredential 定长拆解一条凭据。
//
// 用定长切而不是 strings.Split：句柄恒 20 字符（见 HandleID），
// 长度与分隔符位置都写死，格式不对的串在动 HMAC 之前就被拒掉。
// 只接受小写：'A1' 与 'a1' 是同一段字节，允许多种写法只会制造歧义。
func ParseCredential(v string) (handle string, ts int64, sig string, ok bool) {
	if len(v) != CredLen ||
		v[credHandleLen] != credSep ||
		v[credHandleLen+1+credTSLen] != credSep {
		return "", 0, "", false
	}
	handle = v[:credHandleLen]
	tsHex := v[credHandleLen+1 : credHandleLen+1+credTSLen]
	sig = v[credHandleLen+1+credTSLen+1:]
	if !ValidHandle(handle) || !isLowerHex(tsHex) || !isLowerHex(sig) {
		return "", 0, "", false
	}
	u, err := strconv.ParseUint(tsHex, 16, 32)
	if err != nil {
		return "", 0, "", false
	}
	return handle, int64(u), sig, true
}

// ValidHandle 报告一个句柄是否是合法形态（acc_/adm_ + 16 位小写 hex）。
func ValidHandle(h string) bool {
	if len(h) != credHandleLen {
		return false
	}
	switch {
	case strings.HasPrefix(h, HandlePrefix), strings.HasPrefix(h, AdminPrefix):
		return isLowerHex(h[len(HandlePrefix):])
	default:
		return false
	}
}

// SigStatus 是一次验签的结论。
//
// 把"过期"与"伪造"分开，是为了让报错可操作：前者提示重新签发，
// 后者提示 Key 不对。两者都不会泄漏账号是否存在。
type SigStatus int

const (
	// SigOK 签名有效且在时间容差内。
	SigOK SigStatus = iota
	// SigExpired 时间戳太旧（超期）。
	SigExpired
	// SigFuture 时间戳太新（签发机时钟走快了）。
	SigFuture
	// SigInvalid 签名对不上（Key 不对，或内容被改过）。
	SigInvalid
)

// Code 返回适合放进错误响应的稳定标识。
func (s SigStatus) Code() string {
	switch s {
	case SigOK:
		return "ok"
	case SigExpired:
		return "signature_expired"
	case SigFuture:
		return "signature_future"
	default:
		return "signature_invalid"
	}
}

// VerifyCredential 校验一条凭据。
//
// 三步，全是正向计算：
//  1. 时间戳已经在手（明文），比较 |now-ts| ≤ ttl —— 纯减法；
//  2. 用秘密复算一次 HMAC —— 没有反推，也没有候选集合；
//  3. 常数时间比较，避免逐字节试探。
func VerifyCredential(secret, handle, sig string, ts int64, now time.Time, ttl time.Duration) SigStatus {
	limit := int64(ttl / time.Second)
	if limit < 0 {
		limit = 0
	}
	delta := now.Unix() - ts
	switch {
	case delta > limit:
		return SigExpired
	case delta < -limit:
		return SigFuture
	}
	want := macHex(secret, handle, FormatTS(ts))
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) != 1 {
		return SigInvalid
	}
	return SigOK
}

func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
