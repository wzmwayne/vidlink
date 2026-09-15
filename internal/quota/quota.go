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
package quota

import (
	"fmt"

	"vidlink/internal/core"
)

// Endpoint 是四个配额端点。
type Endpoint string

const (
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
	EndpointInfo, EndpointLinks, EndpointDetail, EndpointBatchLinks,
}

// defaultPlatformKey 是"通用"档的键。表里没列出的平台都走它。
const defaultPlatformKey core.Platform = ""

// Table 是倍率表。
//
// 结构是 map[端点][平台] → 倍率；平台键为 defaultPlatformKey 表示通用档。
// 没在表里出现的平台组合一律走通用档，这样新增平台不需要改这张表就有合理的默认系数。
type Table struct {
	base map[Endpoint]map[core.Platform]float64
}

// DefaultTable 返回内置系数表。
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
func DefaultTable() *Table {
	return &Table{base: map[Endpoint]map[core.Platform]float64{
		// 只列出**与通用档不同**的平台。没列出的自动走 defaultPlatformKey，
		// 所以将来新增平台不需要动这张表就有合理的默认系数。
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
	}}
}

// BatchUnsupportedReason 返回该平台不能进批量的原因；空串表示支持。
//
// 单独维护这张表而不是靠倍率表缺项，是为了给出**可读的原因**：
// 客户看到"抖音按 IP 维度限流"远比看到"倍率未定义"有用。
func BatchUnsupportedReason(p core.Platform) string {
	return batchUnsupported[p]
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
}

// Coefficient 返回一次调用的端点系数（已含平台差异；不含账号倍率）。
//
// 对批量端点，返回的是**每条**的系数。
func (t *Table) Coefficient(e Endpoint, p core.Platform) (float64, error) {
	if e == EndpointBatchLinks {
		if reason, bad := batchUnsupported[p]; bad {
			return 0, ErrPlatformUnsupported{Endpoint: e, Platform: p, Reason: reason}
		}
	}
	byPlatform, ok := t.base[e]
	if !ok {
		return 0, fmt.Errorf("未知端点 %q", e)
	}
	if v, ok := byPlatform[p]; ok {
		return v, nil
	}
	if v, ok := byPlatform[defaultPlatformKey]; ok {
		return v, nil
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

// MaxBase 返回该端点在所有平台上的最高基础倍率。
//
// 用途是**预授权**：解析开始前我们还不知道是哪家平台（要解析链接才知道），
// 但可以先按最贵的可能值检查配额够不够。这样既不会让配额为空的客户
// 未计入消耗到我们打上游的成本，也不会因为事后才发现配额不足而对他已经拿到的
// 结果已经返回了才发现配额不足。
//
// 真正的扣减配额仍按**实际平台**的系数结算，所以预授权只是上限检查，
// 不会多扣。
func (t *Table) MaxCoefficient(e Endpoint) float64 {
	byPlatform, ok := t.base[e]
	if !ok {
		return 0
	}
	var max float64
	for p, v := range byPlatform {
		if p == defaultPlatformKey {
			continue // 通用档不是"某个平台"，不参与取最大
		}
		if v > max {
			max = v
		}
	}
	if def, ok := byPlatform[defaultPlatformKey]; ok && def > max {
		max = def
	}
	return max
}
