// Package quota 定义配额消耗系数（倍率）表与配额计量公式。
//
// 措辞约定：本包一律使用"配额/消耗/系数"，
// **不使用** 价格、金额、付费、余额、折扣之类的交易词汇。
// 配额是管理员分配给账号的**用量额度**，与任何支付行为无关。
// 对外文案见 docs/配额倍率表.md。
//
// 设计原则
//
//  1. **系数是数据，不是散落在各处的常量。** 全部集中在这张表里。
//     系数会随上游成本与运营策略反复调整，散落意味着每次调整都要翻遍代码
//     还容易漏一处。
//
//  2. **平台差异与账号差异是两个正交的乘数**，不要混在一起：
//
//     实扣 units = 端点系数 × 平台系数 × 账号倍率
//     ↑ 提供什么     ↑ 哪家上游更贵   ↑ 给谁调整
//
//     端点系数解决"这个接口提供多少内容"；
//     平台系数解决"这家上游更贵"（抖音要签名 + IP 维度限流）；
//     账号倍率解决"这个账号单独怎么调"（内部账号、活动账号）。
//
//  3. **系数表只回答"该扣多少"，不碰账本。** 配额属于账号，
//     扣减的原子性由 account 包保证。分开之后，调系数不会碰到账本逻辑。
//
//  4. **默认值与覆盖层分开。** 这个包里放的是**内置默认**（编译进来、
//     不可变），运行期由管理员改出来的差值放在 internal/rates 的覆盖层里，
//     两者叠加才是生效值。这样"出厂价目表"永远可复现，重置只需丢掉覆盖层。
package quota

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"vidlink/internal/core"
)

// Endpoint 是配额端点。
type Endpoint string

const (
	// EndpointSearch 按关键词搜索内容：只给元信息 + id，不含任何直链。
	//
	// **按条计量**（与 batch_links 同一类）：一次搜索返回 n 条结果，就按 n 条
	// 计价，因为每条结果都等价于一次 info 的产出（标题/简介/封面/统计）。
	// 默认 0.25/条 —— 是 info(0.5/条) 的一半：搜索结果只给元信息的一部分
	// （没有档位列表、没有直链），但一次要打多次上游，属实比单条 info 重。
	EndpointSearch Endpoint = "search"
	// EndpointInfo 只返回元信息与可用档位列表，不含任何直链。
	EndpointInfo Endpoint = "info"
	// EndpointLinks 只返回直链，不含任何元信息。
	EndpointLinks Endpoint = "links"
	// EndpointDetail 返回元信息 + 全部档位直链 + 字幕/图集。
	EndpointDetail Endpoint = "detail"
	// EndpointBatchLinks 批量只取直链。按条配额计量。
	EndpointBatchLinks Endpoint = "batch_links"
)

// AllEndpoints 便于遍历与测试。
var AllEndpoints = []Endpoint{
	EndpointSearch, EndpointInfo, EndpointLinks, EndpointDetail, EndpointBatchLinks,
}

// defaultPlatformKey 是"通用"档的键。表里没列出的平台都走它。
const defaultPlatformKey core.Platform = ""

// PlatformDefault 是通用档在**接口与文件里的名字**。
//
// 内部键是空字符串（map 里好写），但对外的 JSON 键不能是 ""——
// 那既不好读，也容易和"没填"混淆，所以对外一律用 "default"。
const PlatformDefault = "default"

// AllPlatforms 是价目表里**单列**的平台，顺序固定（文档与面板都按它排）。
var AllPlatforms = []core.Platform{
	core.PlatformBilibili, core.PlatformDouyin,
	core.PlatformKuaishou, core.PlatformXiaohongshu,
	core.PlatformJianpian,
}

// PlatformName 把内部平台键转成对外名字（通用档 → "default"）。
func PlatformName(p core.Platform) string {
	if p == defaultPlatformKey {
		return PlatformDefault
	}
	return string(p)
}

// NormalizePlatform 把对外名字转成内部平台键。
//
// 认识的只有"default"与四个平台：**不允许**任意字符串进来，否则价目表里
// 会攒下一堆拼错平台名的死行，而且没人知道它们什么时候生效。
func NormalizePlatform(name string) (core.Platform, bool) {
	if name == PlatformDefault {
		return defaultPlatformKey, true
	}
	for _, p := range AllPlatforms {
		if string(p) == name {
			return p, true
		}
	}
	return "", false
}

// ProxyUnitBytes 是媒体代理的计费单位：1 个配额对应 1 MiB 传输量。
//
// 代理**刻意不乘**端点系数与平台系数（docs/配额倍率表.md 有完整说明）：
//
//   - 端点系数回答"这个接口提供多少内容"，而代理提供的是字节搬运，
//     与 info/links/detail 那种"给一份解析结果"不是一回事；
//   - 平台系数回答"哪家上游更贵"（抖音要签名 + IP 维度限流），
//     而代理的目标是任意 CDN，平台系数在这里没有对应物。
//
// 它的成本只与体积线性相关，所以公式单独一条：
//
//	实扣 = 传输体积(MiB) × 1 × 账号倍率
//
// 为什么按体积而不是按次：按次会让"下 4K 原片"和"下 10 秒预览"同价，
// 前者的出口带宽是后者的几百倍。按体积是唯一与真实成本同向的计法。
const ProxyUnitBytes = 1 << 20

// ProxyRate 是媒体代理费率的**内置默认值**：0.5 配额/MiB。
//
// 运行期可被管理员改（存在 internal/rates 的覆盖层里），这里只是出厂默认
// 与"文件不存在时的种子"。
//
// 所有账号一个价（公共 Key 也一样）——分档定价在这个规模上没有意义，
// 只会让"这次要花多少"变成需要查表的题。0.5 这个数是这样定的：
//
//   - 心智模型仍然简单：**1 GB ≈ 512 配额**，70 MB 的视频 ≈ 35 配额；
//   - 代理的真实成本是**服务端出口带宽**（家里的上行 + Cloudflare 隧道），
//     而解析一次只花上游几十到几百 KB。1/MiB 会让"下一条片子"贵过
//     "解析一百次"，0.5 更贴近两者的实际成本比；
//   - 公共入口每天 25 配额 ≈ 50 MB：够试完一条短视频，又不足以把
//     出口带宽变成免费资源。
const ProxyRate = 0.5

// ProxyCost 计算媒体代理一次传输的配额消耗（统一费率 ProxyRate）。
//
// 倍率为 0 的账号（免费账号）仍然是 0，与该账号在其他端点上的语义一致：
// 不扣配额，但调用次数照样累计。
func ProxyCost(bytes int64, accountMultiplier float64) float64 {
	return ProxyCostAt(bytes, ProxyRate, accountMultiplier)
}

// ProxyCostAt 用指定费率计算代理消耗（公共 Key 用 PublicProxyRate）。
func ProxyCostAt(bytes int64, ratePerMiB, accountMultiplier float64) float64 {
	if bytes <= 0 || accountMultiplier <= 0 || ratePerMiB <= 0 {
		return 0
	}
	return round4(float64(bytes) / float64(ProxyUnitBytes) * ratePerMiB * accountMultiplier)
}

// RateLimits 是单个系数的合法范围。
//
// 上限 100 不是运营判断，而是**防手滑**：把 1.2 打成 120 会让一个账号
// 一次调用就被扣光；下限 0 合法（0 表示这个端点不扣配额）。
const (
	MinRate = 0
	MaxRate = 100
)

// Table 是**可变的**倍率表：内置默认 + 运行期覆盖层。
//
// 结构是 map[端点][平台] → 倍率；平台键 defaultPlatformKey 表示通用档。
// 生效值按"覆盖 → 内置默认"逐层回退，找不到平台就回退到通用档，
// 所以新增平台不需要改这张表就有合理的默认系数。
//
// 并发：读路径（每次计量）走读锁，写路径（管理面改价）走写锁。
// 计量本身是纯计算，锁竞争可以忽略。
type Table struct {
	mu       sync.RWMutex
	defaults map[Endpoint]map[core.Platform]float64
	override map[Endpoint]map[core.Platform]float64
	proxy    float64
}

// DefaultTable 返回内置系数表（没有任何覆盖）。
//
// 数值依据（2026-09 实测定档，完整说明见 docs/配额倍率表.md）：
//
//	info   0.5 —— 只给元信息，不提供可播放资源
//	links  1.0 —— 给直链，这是本服务的核心产出
//	detail 1.2 —— 元信息 + 全部档位的直链，等于打包：
//	              info(0.5) + links(1.0) = 1.5，打包 1.2 相当于八折。
//	              必须严格大于 links，否则 links 会被 detail 完全支配而形同虚设。
//	batch  0.75/条 —— 只给直链的批量版，比单条 links 低 25%，
//	              换取客户把零散请求合并成一次调用（对我们也是省上游）。
//
// 抖音系数：
//
//	info 0.75 / links 1.1 / detail 1.5
//
//	抖音的信息查询与取流走的是**同一条**签名 detail 请求（不像 B 站能只调
//	view 接口），所以 info 档位的上游成本并不比 links 低多少；再加上它是
//	IP 维度限流、最容易触发风控的一家。系数整体上浮是对成本的如实反映。
//
// batch 不支持抖音：批量会瞬间打出多个请求，在 IP 维度限流下等于自残。
//
// B 站不单独设系数：实网验证发现匿名即可拿完整 1080P（非签名 playurl 通道
// + try_look=1），既不需要内嵌账号也不存在额外成本，因此与通用档一致。
//
// 注意：这些是**出厂默认**。运行期管理员可以覆盖任意一格（见 internal/rates），
// 想恢复出厂值就重置覆盖层。
func DefaultTable() *Table {
	return &Table{
		defaults: defaultRates(),
		proxy:    ProxyRate,
	}
}

// defaultRates 是"出厂价目表"。
//
// 只列出**与通用档不同**的平台。没列出的自动走 defaultPlatformKey，
// 所以将来新增平台不需要动这张表就有合理的默认系数。
func defaultRates() map[Endpoint]map[core.Platform]float64 {
	return map[Endpoint]map[core.Platform]float64{
		// 搜索：**0.25/条**。默认一次返回 20 条 → 5 配额；上限 50 条 → 12.5。
		// 比 info(0.5/条) 便宜一半：搜索结果只给元信息的一部分（没有档位列表、
		// 没有直链），但一次搜索要打多次上游（荐片 10 条/页，拿 50 条要 5 次），
		// 所以按条计价、单价减半，两边都说得通。
		// 平台默认值不单列：荐片不额外加价，部署者可在面板上把 jianpian/search 调高。
		EndpointSearch: {
			defaultPlatformKey: 0.25,
		},
		EndpointInfo: {
			defaultPlatformKey:  0.5,
			core.PlatformDouyin: 0.75,
		},
		EndpointLinks: {
			defaultPlatformKey:  1.0,
			core.PlatformDouyin: 1.1,
		},
		EndpointDetail: {
			defaultPlatformKey:  1.2,
			core.PlatformDouyin: 1.5,
		},
		// 批量刻意**不列抖音** —— 它走 batchUnsupported 给出明确原因。
		EndpointBatchLinks: {
			defaultPlatformKey: 0.75,
		},
	}
}

// BatchUnsupportedReason 返回该平台不能进批量的原因；空串表示支持。
//
// 单独维护这张表而不是靠倍率表缺项，是为了给出**可读的原因**：
// 客户看到"抖音按 IP 维度限流"远比看到"倍率未定义"有用。
func BatchUnsupportedReason(p core.Platform) string {
	return batchUnsupported[p]
}

// SearchUnsupportedReason 返回该平台不能搜索的原因；空串表示支持。
//
// 与批量不同的是，这里的判据是**上游事实**（而不是运营策略）：
// 抖音搜索要求登录态、快手搜索要反爬验证、小红书要整套签名，
// 都是 2026-09 实测确认的。原因文案会出现在 /v1/platforms 与
// /v1/search 的报错里——用户搜不了，至少要知道为什么。
func SearchUnsupportedReason(p core.Platform) string {
	return searchUnsupported[p]
}

// unsupportedReason 统一回答"这个端点在这个平台上支持吗"。
//
// 两处判定（Coefficient 与 ValidateCell）共用它，避免"系数算得出来但设不进去"
// 这类自相矛盾的状态。
func unsupportedReason(e Endpoint, p core.Platform) (string, bool) {
	switch e {
	case EndpointBatchLinks:
		r, bad := batchUnsupported[p]
		return r, bad
	case EndpointSearch:
		r, bad := searchUnsupported[p]
		return r, bad
	default:
		return "", false
	}
}

// ErrPlatformUnsupported 表示该端点不支持这个平台。
type ErrPlatformUnsupported struct {
	Endpoint Endpoint
	Platform core.Platform
	Reason   string
}

func (e ErrPlatformUnsupported) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("端点 %s 不支持平台 %s：%s", e.Endpoint, e.Platform, e.Reason)
	}
	return fmt.Sprintf("端点 %s 不支持平台 %s", e.Endpoint, e.Platform)
}

// batchUnsupported 列出"不能进批量"的平台及原因。
var batchUnsupported = map[core.Platform]string{
	core.PlatformDouyin: "抖音按 IP 维度限流，批量会瞬间打出多个请求，容易被判定为异常流量。" +
		"请改用 GET /v1/links 逐条获取",
	core.PlatformJianpian: "荐片不支持链接解析（没有稳定的作品页形态）：批量接口按链接输入，" +
		"荐片请用 platform=jianpian&id=<影片ID> 逐条调 GET /v1/links",
}

// searchUnsupported 列出"不能搜索"的平台及原因（全部为 2026-09 实测结论）。
//
// 这张表是**能力**事实，不是定价：因此它同时也决定了
// /v1/platforms 的 search 字段。将来某个平台的条件具备
// （例如运维提供了抖音登录态 Cookie，或实现了快手的 __NS_sig3 签名），
// 删掉对应那行即可，接口契约不变。
var searchUnsupported = map[core.Platform]string{
	core.PlatformDouyin: "抖音搜索接口要求登录态：未登录请求实测返回 " +
		"status_code=2483「请先登录，再继续搜索吧」；且搜索属受签名保护的重量级接口、" +
		"按 IP 维度限流，本服务只维护访客身份，故不提供搜索",
	core.PlatformKuaishou: "快手搜索接口有反爬校验：实测 visionSearchPhoto 返回 " +
		"result=400002 并给出滑块验证链接（captcha.zt.kuaishou.com），" +
		"需要 __NS_sig3 签名；移动端 rest 搜索接口要求登录态，故不提供搜索",
	core.PlatformXiaohongshu: "小红书搜索接口需要 x-s / x-s-common / x-rap-param 全套签名" +
		"（其中 x-rap-param 需位级兼容 gzip）与登录态，实现成本远超收益，故不提供搜索",
}

// Coefficient 返回一次调用的端点系数（已含平台差异；不含账号倍率）。
//
// 解析顺序：覆盖层的具体平台 → 覆盖层的通用档 → 内置默认的具体平台 →
// 内置默认的通用档。前两层让管理员改的值生效，后两层让"没改过的平台"
// 与"新平台"都有价。
//
// 对批量端点，返回的是**每条**的系数。
func (t *Table) Coefficient(e Endpoint, p core.Platform) (float64, error) {
	if reason, bad := unsupportedReason(e, p); bad {
		return 0, ErrPlatformUnsupported{Endpoint: e, Platform: p, Reason: reason}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.coefficientLocked(e, p)
}

func (t *Table) coefficientLocked(e Endpoint, p core.Platform) (float64, error) {
	for _, layer := range []map[Endpoint]map[core.Platform]float64{t.override, t.defaults} {
		byPlatform, ok := layer[e]
		if !ok {
			continue
		}
		if v, ok := byPlatform[p]; ok {
			return v, nil
		}
		if v, ok := byPlatform[defaultPlatformKey]; ok {
			return v, nil
		}
	}
	if _, ok := t.defaults[e]; !ok {
		return 0, fmt.Errorf("未知端点 %q", e)
	}
	return 0, fmt.Errorf("端点 %q 没有可用的倍率", e)
}

// Consume 计算一次调用实际要扣的 units。
//
//	units = 端点系数 × 条数 × 账号倍率
//
// items 对非批量端点固定传 1。
//
// 账号倍率由管理员设置：
//
//	1.0 = 标准   0.5 = 减半   0.0 = 不扣配额（但仍记用量与调用次数）
//	2.0 = 加倍（可用于内部统计口径或限制高消耗账号）
//
// 结果四舍五入到小数点后 4 位，避免浮点误差在账本里累积成
// 0.30000000000000004 这种值。
func (t *Table) Consume(e Endpoint, p core.Platform, items int, accountMultiplier float64) (float64, error) {
	if items < 1 {
		items = 1
	}
	if accountMultiplier < 0 {
		accountMultiplier = 0
	}
	base, err := t.Coefficient(e, p)
	if err != nil {
		return 0, err
	}
	return round4(base * float64(items) * accountMultiplier), nil
}

func round4(v float64) float64 {
	// 用整数运算避免 math.Round 的边界怪癖：v*10000 四舍五入回整数再除。
	n := int64(v*10000 + 0.5)
	if v < 0 {
		n = int64(v*10000 - 0.5)
	}
	return float64(n) / 10000
}

// Coefficients 返回某个平台各端点的完整系数，用于 /v1/usage 与 /v1/platforms，
// 让调用方自己就能算出每次调用扣多少配额。
func (t *Table) Coefficients(p core.Platform) map[Endpoint]float64 {
	out := make(map[Endpoint]float64, len(AllEndpoints))
	for _, e := range AllEndpoints {
		if v, err := t.Coefficient(e, p); err == nil {
			out[e] = v
		}
	}
	return out
}

// MaxCoefficient 返回该端点在所有平台上的最高**生效**系数。
//
// 用途是**预授权**：解析开始前我们还不知道是哪家平台（要解析链接才知道），
// 但可以先按最贵的可能值检查配额够不够。这样既不会让配额为空的客户
// 未计入消耗到我们打上游的成本，也不会事后才发现配额不足。
//
// 真正的扣减配额仍按**实际平台**的系数结算，所以预授权只是上限检查，
// 不会多扣。改价后这个上限会立刻跟着变——否则降价之后老上限仍会误伤。
func (t *Table) MaxCoefficient(e Endpoint) float64 {
	var max float64
	for _, p := range AllPlatforms {
		if v, err := t.Coefficient(e, p); err == nil && v > max {
			max = v
		}
	}
	if v, err := t.Coefficient(e, defaultPlatformKey); err == nil && v > max {
		max = v
	}
	return max
}

// --- 覆盖层（管理面）---

// Overrides 返回当前的覆盖层副本（只含被管理员改过的格子）。
func (t *Table) Overrides() map[Endpoint]map[core.Platform]float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return copyRates(t.override)
}

// Defaults 返回内置默认的副本（出厂价目表）。
func (t *Table) Defaults() map[Endpoint]map[core.Platform]float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return copyRates(t.defaults)
}

// SetOverride 写入/清除一格覆盖。v 为 nil 表示清除该格（回到内置默认）。
func (t *Table) SetOverride(e Endpoint, p core.Platform, v *float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if v == nil {
		if byPlatform, ok := t.override[e]; ok {
			delete(byPlatform, p)
			if len(byPlatform) == 0 {
				delete(t.override, e)
			}
		}
		return
	}
	if t.override == nil {
		t.override = make(map[Endpoint]map[core.Platform]float64, len(AllEndpoints))
	}
	if t.override[e] == nil {
		t.override[e] = make(map[core.Platform]float64, len(AllPlatforms)+1)
	}
	t.override[e][p] = *v
}

// ReplaceOverrides 整体替换覆盖层（用于加载文件与重置）。
func (t *Table) ReplaceOverrides(m map[Endpoint]map[core.Platform]float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.override = copyRates(m)
}

// ProxyRate 返回媒体代理费率的生效值（配额/MiB）。
func (t *Table) ProxyRate() float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.proxy <= 0 {
		return ProxyRate
	}
	return t.proxy
}

// SetProxyRate 设置代理费率。
func (t *Table) SetProxyRate(v float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.proxy = v
}

// ValidateCell 校验一格的取值。管理 API 与前端共用这一份口径。
func ValidateCell(e Endpoint, p core.Platform, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s/%s 的系数必须是有限数字", PlatformName(p), e)
	}
	if v < MinRate || v > MaxRate {
		return fmt.Errorf("%s/%s 的系数 %v 超出允许范围 [%v, %v]",
			PlatformName(p), e, v, MinRate, MaxRate)
	}
	if reason, bad := unsupportedReason(e, p); bad {
		return fmt.Errorf("%s 不支持 %s（%s），不能给它设系数",
			PlatformName(p), e, reason)
	}
	for _, known := range AllEndpoints {
		if known == e {
			return nil
		}
	}
	return fmt.Errorf("未知端点 %q", e)
}

// Warnings 检查当前**生效值**是否偏离设计意图，返回人能读的提示。
//
// 刻意只提示不阻止：定价是运营决策，代码不该替管理员做判断。
// 但像"detail 比 links 还便宜"这种多半是手滑，值得在面板上红一下。
func (t *Table) Warnings() []string {
	out := []string{} // 非 nil：JSON 里是 [] 而不是 null，客户端不用区分两种空
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, p := range append(append([]core.Platform{}, AllPlatforms...), defaultPlatformKey) {
		links, lerr := t.Coefficient(EndpointLinks, p)
		detail, derr := t.Coefficient(EndpointDetail, p)
		info, ierr := t.Coefficient(EndpointInfo, p)
		if lerr == nil && derr == nil && detail <= links {
			add(fmt.Sprintf("%s：detail(%v) ≤ links(%v)，links 会被完全支配",
				PlatformName(p), detail, links))
		}
		if lerr == nil && derr == nil && ierr == nil && detail >= info+links {
			add(fmt.Sprintf("%s：detail(%v) ≥ info+links(%v)，打包折扣消失",
				PlatformName(p), detail, info+links))
		}
	}
	for _, e := range AllEndpoints {
		for _, p := range append(append([]core.Platform{}, AllPlatforms...), defaultPlatformKey) {
			v, err := t.Coefficient(e, p)
			if err != nil {
				continue
			}
			if v == 0 {
				add(fmt.Sprintf("%s/%s = 0：该端点不扣配额", PlatformName(p), e))
			}
			if v > 10 {
				add(fmt.Sprintf("%s/%s = %v：超过 10，确认不是手滑？", PlatformName(p), e, v))
			}
		}
	}
	sort.Strings(out)
	return out
}

func copyRates(in map[Endpoint]map[core.Platform]float64) map[Endpoint]map[core.Platform]float64 {
	if in == nil {
		return nil
	}
	out := make(map[Endpoint]map[core.Platform]float64, len(in))
	for e, byPlatform := range in {
		cp := make(map[core.Platform]float64, len(byPlatform))
		for p, v := range byPlatform {
			cp[p] = v
		}
		out[e] = cp
	}
	return out
}
