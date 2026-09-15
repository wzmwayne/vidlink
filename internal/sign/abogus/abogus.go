// Package abogus 实现抖音 Web 端的 a_bogus 风控签名。
//
// 背景
//
//	自 2024 年起，抖音 Web 端 aweme/v1/web/* 系列接口要求 query 中携带
//	a_bogus 参数；缺失或错误会返回 status_code=8（"请稍后再试"）或直接
//	返回空数据。a_bogus 的生成完全由前端 JS 完成，但算法是确定的纯计算：
//	SM3 双重哈希 + RC4 流加密 + 自定义字母表编码，没有任何环境相关的
//	不可复现随机源（时间戳与随机数只影响长度，不影响服务端校验通过与否）。
//
//	因此这里用纯 Go 复现，不引入 JS 引擎（goja/v8）或 headless 浏览器——
//	这是本项目能做到"极轻量"的关键之一：一个签名调用约 3~5 µs，
//	无 CGO、无外部进程、可无限并发。
//
// 算法结构
//
//  1. paramsHash = SM3(SM3(query + "cus"))
//  2. methodHash = SM3(SM3(method + "cus"))
//  3. uaCode     = reverse(SM3(userAgent))       // 32 字节指纹
//  4. 按固定模板拼出 payload 字节数组（含时间戳、三个随机数、UA/参数哈希片段）
//  5. checksum   = XOR(payload)                    // 追加在浏览器指纹之后
//  6. prefix     = 三个随机数编码出的 12 字节
//  7. cipher     = RC4(key='y', payload)
//  8. result     = customBase64(prefix + cipher)
//
// 参考实现：wujunwei928/parse-video 的 parser/douyin_detail.go（MIT），
// 其确定性测试向量被本项目用作回归用例（见 abogus_test.go）。
package abogus

import (
	"math/rand/v2"
	"strings"
	"time"

	"vidlink/internal/sign/sm3"
)

const (
	// salt 是抖音前端硬编码的固定盐。
	salt = "cus"

	// browserFingerprint 是前端上报的窗口/屏幕指纹串。
	// 服务端并不强校验其真实性，但长度参与了 payload 构造，必须固定。
	browserFingerprint = "1536|742|1536|864|0|0|0|0|1536|864|1536|864|1536|742|24|24|MacIntel"

	// alphabet 是自定义 Base64 字母表（非标准表）。
	alphabet = "Dkdpgh2ZmsQB80/MfvV36XI1R45-WUAlEixNLwoqYTOPuzKFjJnry79HbGcaStCe"

	// defaultUserAgent 是配套的桌面 Chrome UA。
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/90.0.4430.212 Safari/537.36"
)

// rc4Key 是 RC4 KSA 使用的单字节密钥 'y'。
const rc4Key = 'y'

// Variant 标识 a_bogus 载荷模板的一个版本。
//
// 为什么需要它：参考实现与它自己给出的黄金向量**互相矛盾**——模板第 34 号
// 位置（一个固定标志位）在密文里必须是 2，但校验和却要按 3 计算。这意味着
// 至少有一处是陈旧的，而离线无法判定哪个对。
//
// 与其把某个魔法数字写死，这里把两种解释都保留为**数据**，
// 并提供一个联调测试在真实接口上判定（见 live_test.go）。
type Variant int

const (
	// VariantSourceFaithful 完全照抄参考实现源码：标志位=3，校验和覆盖模板自身。
	// 自洽，但与参考实现的黄金向量差 1 个字符。
	VariantSourceFaithful Variant = iota
	// VariantVectorFaithful 以参考实现的黄金向量为准：密文里标志位=2，
	// 校验和仍按标志位=3 计算。自洽性差，但能逐字节复现已知正确的输出。
	VariantVectorFaithful
)

// defaultVariant 是当前默认版本。
//
// 选择 VectorFaithful 的理由：黄金向量是唯一的外部证据（它来自一次真实抓包），
// 而源码与它矛盾时，证据优先于源码。若线上实测失败，切到 SourceFaithful 即可。
const defaultVariant = VariantVectorFaithful

// flagValues 返回某变体下「密文标志位」与「参与校验和的标志位」。
func (v Variant) flagValues() (inPayload, inChecksum byte) {
	switch v {
	case VariantSourceFaithful:
		return 3, 3
	default:
		return 2, 3
	}
}

// Fingerprint 是参与载荷构造的 32 字节指纹。
//
// 逆向资料普遍认为它等于 reverse(SM3(userAgent))，但本项目的实测结论是：
// 参考实现里该常量**无法**由它同时使用的 UA 推导出来——对 Chrome 80–135
// 与 iPhone Safari 的 UA，以及 UTF-16 编码 / RC4(0,1,14) / 自定义 Base64
// 等十余种组合做穷举，均不匹配。
//
// 换言之，指纹是**与算法版本绑定的常量**，而不是 UA 的函数。
// 因此这里把「已验证可用」的常量作为默认，另提供由 UA 派生的实验性通道。
type Fingerprint [sm3.Size]byte

// VerifiedFingerprint 是经黄金向量逐字节验证的指纹。
var VerifiedFingerprint = Fingerprint{
	76, 98, 15, 131, 97, 245, 224, 133,
	122, 199, 241, 166, 79, 34, 90, 191,
	128, 126, 122, 98, 66, 11, 14, 40,
	49, 110, 110, 173, 67, 96, 138, 252,
}

// DefaultUserAgent 返回内置的默认 UA。
//
// 注意：指纹常量与 UA 在参考实现中并不自洽（见 Fingerprint 的说明），
// 但请求头仍建议使用该 UA，以免引入其它维度的指纹矛盾。
func DefaultUserAgent() string { return defaultUserAgent }

// Signer 是并发安全的签名器。
//
// 签名器不持有可变状态（随机源用 math/rand/v2 的无锁实现），
// 因此可以安全地被任意多个 goroutine 共享。
type Signer struct {
	fp      Fingerprint
	variant Variant
	now     func() time.Time
}

// New 构造使用「已验证指纹 + 默认模板变体」的签名器。这是生产路径。
func New() *Signer {
	return &Signer{fp: VerifiedFingerprint, variant: defaultVariant, now: time.Now}
}

// NewWithFingerprint 用指定指纹构造签名器，供算法版本切换时使用。
func NewWithFingerprint(fp Fingerprint) *Signer {
	return &Signer{fp: fp, variant: defaultVariant, now: time.Now}
}

// WithVariant 返回切换了模板变体的副本（签名器本身不可变）。
func (s *Signer) WithVariant(v Variant) *Signer {
	cp := *s
	cp.variant = v
	return &cp
}

// Variant 返回当前使用的模板变体。
func (s *Signer) Variant() Variant { return s.variant }

// NewWithUserAgent 由 UA 派生指纹（reverse(SM3(ua)))。
//
// ⚠️ 该路径**未经在线验证**：派生出的指纹可能与当前线上算法版本不一致，
// 结果是签名被服务端拒绝（表现为空数据或 status_code=8）。
// 仅在确认算法版本后使用，并务必先用真实请求校验。
func NewWithUserAgent(ua string) *Signer {
	if ua == "" {
		ua = defaultUserAgent
	}
	return &Signer{fp: Fingerprint(sm3.Reverse(sm3.Sum([]byte(ua)))), variant: defaultVariant, now: time.Now}
}

// Fingerprint 返回该签名器使用的指纹，便于调试与比对。
func (s *Signer) Fingerprint() Fingerprint { return s.fp }

// Sign 生成 a_bogus 值。query 是**不含 a_bogus** 的完整 query string
// （键值对顺序必须与最终发出的请求完全一致），method 是 HTTP 方法。
func (s *Signer) Sign(query, method string) string {
	started := s.now().UnixMilli()
	finished := started + int64(4+s.nextRandom()%5)
	return s.signWith(query, method, started, finished, s.nextRandom(), s.nextRandom(), s.nextRandom())
}

// signWith 是确定性的签名核心：时间戳与随机数由调用方给定。
// 抽出它是为了能用固定向量做回归测试——算法漂移会立刻被测试抓住。
func (s *Signer) signWith(query, method string, started, finished int64, r1, r2, r3 int) string {
	paramsHash := sm3.Double([]byte(query + salt))
	methodHash := sm3.Double([]byte(strings.ToUpper(method) + salt))

	inPayload, inChecksum := s.variant.flagValues()
	payload := buildPayload(paramsHash, methodHash, s.fp, started, finished, inPayload)

	// 校验和覆盖模板（含标志位）。注意校验和用的标志位可能与密文里的不同——
	// 这正是两个变体的唯一差别，见 Variant 的说明。
	checksumSource := payload
	if inChecksum != inPayload {
		checksumSource = buildPayload(paramsHash, methodHash, s.fp, started, finished, inChecksum)
	}
	var checksum byte
	for _, v := range checksumSource {
		checksum ^= v
	}

	payload = append(payload, browserFingerprint...)
	payload = append(payload, checksum)

	prefix := prefixBytes(r1, r2, r3)
	cipher := rc4(payload)

	return encode(append(prefix, cipher...))
}

func (s *Signer) nextRandom() int {
	// a_bogus 只需要三个 0~9999 的随机数填充长度位。
	// math/rand/v2 的顶层函数基于 runtime.fastrand，无全局锁，可高并发调用。
	return rand.IntN(10000)
}

// buildPayload 按固定模板拼装待加密载荷。
//
// 模板中每个位置的含义是逆向所得，顺序与取值都不能改动——服务端会按位置解析。
// 唯一有争议的是 flag 参数（第 34 号位置），它由 Variant 决定，见 Variant 文档。
func buildPayload(paramsHash, methodHash [sm3.Size]byte, fp Fingerprint, started, finished int64, flag byte) []byte {
	return []byte{
		44,
		byte((finished >> 24) & 255),
		0, 0, 0, 0,
		24,
		paramsHash[21],
		methodHash[21],
		0,
		fp[23],
		byte((finished >> 16) & 255),
		0, 0, 0,
		1,
		0,
		239,
		paramsHash[22],
		methodHash[22],
		fp[24],
		byte((finished >> 8) & 255),
		0, 0, 0, 0,
		byte(finished & 255),
		0, 0,
		14,
		byte((started >> 24) & 255),
		byte((started >> 16) & 255),
		0,
		byte((started >> 8) & 255),
		flag, // ← 第 34 号位置：模板版本标志，由 Variant 决定
		byte(finished >> 32),
		1,
		byte(started >> 32),
		1,
		byte(len(browserFingerprint)),
		0, 0, 0,
	}
}

// prefixBytes 把三个随机数编码成 12 个字节的前缀。
func prefixBytes(r1, r2, r3 int) []byte {
	out := make([]byte, 0, 12)
	out = append(out, randomGroup(r1, 1, 2, 5, 45&170)...)
	out = append(out, randomGroup(r2, 1, 0, 0, 0)...)
	out = append(out, randomGroup(r3, 1, 0, 5, 0)...)
	return out
}

// randomGroup 按位拆出一个 16bit 随机数的四个 6bit 片段。
func randomGroup(value, extra1, extra2, extra3, extra4 int) []byte {
	low := value & 255
	high := (value >> 8) & 255
	return []byte{
		byte((low & 170) | extra1),
		byte((low & 85) | extra2),
		byte((high & 170) | extra3),
		byte((high & 85) | extra4),
	}
}

// rc4 是标准 RC4 流加密，密钥为单字节 'y'。
func rc4(plaintext []byte) []byte {
	var state [256]byte
	for i := range state {
		state[i] = byte(i)
	}

	pos := 0
	for i := range state {
		pos = (pos + int(state[i]) + rc4Key) % 256
		state[i], state[pos] = state[pos], state[i]
	}

	pos = 0
	out := make([]byte, len(plaintext))
	for i, v := range plaintext {
		idx := (i + 1) % 256
		pos = (pos + int(state[idx])) % 256
		state[idx], state[pos] = state[pos], state[idx]
		out[i] = state[(int(state[idx])+int(state[pos]))%256] ^ v
	}
	return out
}

// encode 使用自定义字母表做类 Base64 编码：每 3 字节 → 4 字符，右侧补 '='。
func encode(values []byte) string {
	var b strings.Builder
	b.Grow((len(values)+2)/3*4 + 4)

	for i := 0; i < len(values); i += 3 {
		var n uint32
		n = uint32(values[i]) << 16
		if i+1 < len(values) {
			n |= uint32(values[i+1]) << 8
		}
		if i+2 < len(values) {
			n |= uint32(values[i+2])
		}

		b.WriteByte(alphabet[(n>>18)&63])
		b.WriteByte(alphabet[(n>>12)&63])
		if i+1 < len(values) {
			b.WriteByte(alphabet[(n>>6)&63])
		}
		if i+2 < len(values) {
			b.WriteByte(alphabet[n&63])
		}
	}
	for b.Len()%4 != 0 {
		b.WriteByte('=')
	}
	return b.String()
}
