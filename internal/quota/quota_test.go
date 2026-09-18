package quota

import (
	"errors"
	"math"
	"strings"
	"testing"

	"vidlink/internal/core"
)

func TestDefaultCoefficients(t *testing.T) {
	tb := DefaultTable()

	cases := []struct {
		ep   Endpoint
		p    core.Platform
		want float64
	}{
		{EndpointInfo, core.PlatformBilibili, 0.5},
		{EndpointInfo, core.PlatformKuaishou, 0.5},
		{EndpointInfo, core.PlatformXiaohongshu, 0.5},
		{EndpointInfo, core.PlatformDouyin, 0.75},

		{EndpointLinks, core.PlatformBilibili, 1.0},
		{EndpointLinks, core.PlatformDouyin, 1.1},

		{EndpointDetail, core.PlatformBilibili, 1.2},
		{EndpointDetail, core.PlatformDouyin, 1.5},

		{EndpointBatchLinks, core.PlatformBilibili, 0.75},
		{EndpointBatchLinks, core.PlatformKuaishou, 0.75},
	}
	for _, c := range cases {
		got, err := tb.Coefficient(c.ep, c.p)
		if err != nil {
			t.Errorf("Base(%s, %s) 报错: %v", c.ep, c.p, err)
			continue
		}
		if got != c.want {
			t.Errorf("Base(%s, %s) = %v, want %v", c.ep, c.p, got, c.want)
		}
	}
}

// TestUnknownPlatformFallsBackToDefault：新增平台不需要改系数表就有合理定价。
func TestUnknownPlatformFallsBackToDefault(t *testing.T) {
	tb := DefaultTable()
	got, err := tb.Coefficient(EndpointLinks, core.Platform("weibo"))
	if err != nil {
		t.Fatalf("未知平台应回退到通用档，却报错: %v", err)
	}
	if got != 1.0 {
		t.Fatalf("未知平台 links 应为通用档 1.0，得到 %v", got)
	}
}

// TestDetailCoefficientExceedsLinks 锁住定价的核心约束。
//
// 若 detail ≤ links，客户花同样的钱能拿到严格更多的内容，
// links 就成了"被支配选项"，永远卖不出去，本该收 1.5 的单只能收 1.0。
func TestDetailCoefficientExceedsLinks(t *testing.T) {
	tb := DefaultTable()
	for _, p := range []core.Platform{
		core.PlatformBilibili, core.PlatformDouyin, core.PlatformKuaishou,
	} {
		links, _ := tb.Coefficient(EndpointLinks, p)
		detail, _ := tb.Coefficient(EndpointDetail, p)
		if detail <= links {
			t.Errorf("平台 %s：detail(%v) 必须严格大于 links(%v)", p, detail, links)
		}
	}
}

// TestDetailHasBundleDiscount 锁住"打包折扣"这个卖点。
func TestDetailHasBundleDiscount(t *testing.T) {
	tb := DefaultTable()
	for _, p := range []core.Platform{core.PlatformBilibili, core.PlatformDouyin} {
		info, _ := tb.Coefficient(EndpointInfo, p)
		links, _ := tb.Coefficient(EndpointLinks, p)
		detail, _ := tb.Coefficient(EndpointDetail, p)
		if detail >= info+links {
			t.Errorf("平台 %s：detail(%v) 应低于 info+links(%v)，否则没有打包折扣",
				p, detail, info+links)
		}
	}
}

func TestBatchRejectsDouyin(t *testing.T) {
	tb := DefaultTable()
	_, err := tb.Coefficient(EndpointBatchLinks, core.PlatformDouyin)
	if err == nil {
		t.Fatal("批量应拒绝抖音")
	}
	var e ErrPlatformUnsupported
	if !errors.As(err, &e) {
		t.Fatalf("错误类型应为 ErrPlatformUnsupported，得到 %T", err)
	}
	if e.Reason == "" {
		t.Error("应给出可读的原因，而不是只说'不支持'")
	}
}

func TestConsume(t *testing.T) {
	tb := DefaultTable()
	cases := []struct {
		name  string
		ep    Endpoint
		p     core.Platform
		items int
		mult  float64
		want  float64
	}{
		{"单条 links 原价", EndpointLinks, core.PlatformBilibili, 1, 1.0, 1.0},
		{"单条 links 五折", EndpointLinks, core.PlatformBilibili, 1, 0.5, 0.5},
		{"单条 links 免费", EndpointLinks, core.PlatformBilibili, 1, 0.0, 0.0},
		{"单条 links 加价", EndpointLinks, core.PlatformBilibili, 1, 2.0, 2.0},
		{"抖音 detail 原价", EndpointDetail, core.PlatformDouyin, 1, 1.0, 1.5},
		{"批量 5 条", EndpointBatchLinks, core.PlatformBilibili, 5, 1.0, 3.75},
		{"批量 20 条", EndpointBatchLinks, core.PlatformBilibili, 20, 1.0, 15.0},
		{"批量 5 条五折", EndpointBatchLinks, core.PlatformBilibili, 5, 0.5, 1.875},
		{"items<1 视为 1", EndpointInfo, core.PlatformBilibili, 0, 1.0, 0.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := tb.Consume(c.ep, c.p, c.items, c.mult)
			if err != nil {
				t.Fatalf("报错: %v", err)
			}
			if got != c.want {
				t.Fatalf("Charge = %v, want %v", got, c.want)
			}
		})
	}
}

// TestConsumeRejectsNegativeMultiplier：负系数会变成"倒贴钱"，必须归零。
func TestConsumeRejectsNegativeMultiplier(t *testing.T) {
	tb := DefaultTable()
	got, err := tb.Consume(EndpointLinks, core.PlatformBilibili, 1, -5)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("负系数应被归零，得到 %v", got)
	}
}

// TestConsumeRounding：金额必须干净，否则账本里会累积浮点噪声。
func TestConsumeRounding(t *testing.T) {
	tb := DefaultTable()
	got, err := tb.Consume(EndpointBatchLinks, core.PlatformBilibili, 3, 1.0)
	if err != nil {
		t.Fatal(err)
	}
	// 0.75*3 = 2.25
	if got != 2.25 {
		t.Fatalf("Charge = %v, want 2.25", got)
	}
	got2, _ := tb.Consume(EndpointInfo, core.PlatformDouyin, 1, 0.33)
	// 0.75*0.33 = 0.2475
	if got2 != 0.2475 {
		t.Fatalf("Charge = %v, want 0.2475", got2)
	}
}

func TestBatchUnsupportedReason(t *testing.T) {
	if r := BatchUnsupportedReason(core.PlatformDouyin); r == "" {
		t.Error("抖音应有明确的不支持原因")
	}
	if r := BatchUnsupportedReason(core.PlatformBilibili); r != "" {
		t.Errorf("B 站应支持批量，却返回了原因 %q", r)
	}
}

func TestCoefficients(t *testing.T) {
	tb := DefaultTable()
	r := tb.Coefficients(core.PlatformDouyin)
	if r[EndpointLinks] != 1.1 || r[EndpointDetail] != 1.5 {
		t.Fatalf("抖音报价不对: %+v", r)
	}
	// 抖音不支持批量，报价里不应出现
	if _, ok := r[EndpointBatchLinks]; ok {
		t.Error("批量不支持抖音，报价里不应包含它")
	}
}

// --- 可编辑倍率（覆盖层） ---

// TestOverridesLayerOverDefaults：覆盖层优先，清除后回到内置默认。
func TestOverridesLayerOverDefaults(t *testing.T) {
	tb := DefaultTable()
	v := 3.0
	tb.SetOverride(EndpointLinks, core.PlatformDouyin, &v)
	if got, _ := tb.Coefficient(EndpointLinks, core.PlatformDouyin); got != 3 {
		t.Errorf("覆盖后应为 3，得到 %v", got)
	}
	// 通用档被覆盖 → 未单列的平台跟着变
	d := 2.0
	tb.SetOverride(EndpointLinks, "", &d)
	if got, _ := tb.Coefficient(EndpointLinks, core.Platform("weibo")); got != 2 {
		t.Errorf("未知平台应走通用档覆盖值 2，得到 %v", got)
	}
	// 清除覆盖 → 回到内置默认
	tb.SetOverride(EndpointLinks, core.PlatformDouyin, nil)
	tb.SetOverride(EndpointLinks, "", nil)
	if got, _ := tb.Coefficient(EndpointLinks, core.PlatformDouyin); got != 1.1 {
		t.Errorf("清除后应为内置默认 1.1，得到 %v", got)
	}
	if n := len(tb.Overrides()); n != 0 {
		t.Errorf("覆盖层应为空，得到 %+v", tb.Overrides())
	}
	// 出厂默认不受影响（可复现）
	if len(tb.Defaults()) != len(AllEndpoints) {
		t.Errorf("出厂默认应覆盖四个端点，得到 %+v", tb.Defaults())
	}
}

// TestMaxCoefficientFollowsOverrides：预授权上限必须跟着改价走。
//
// 否则降价之后老上限仍然误伤低余额账号，涨价则会漏掉预授权。
func TestMaxCoefficientFollowsOverrides(t *testing.T) {
	tb := DefaultTable()
	if got := tb.MaxCoefficient(EndpointLinks); got != 1.1 {
		t.Fatalf("默认 links 上限应为 1.1，得到 %v", got)
	}
	v := 7.5
	tb.SetOverride(EndpointLinks, core.PlatformKuaishou, &v)
	if got := tb.MaxCoefficient(EndpointLinks); got != 7.5 {
		t.Errorf("覆盖后上限应为 7.5，得到 %v", got)
	}
	tb.SetOverride(EndpointLinks, core.PlatformKuaishou, nil)
	if got := tb.MaxCoefficient(EndpointLinks); got != 1.1 {
		t.Errorf("清除后上限应回到 1.1，得到 %v", got)
	}
}

// TestValidateCell：接口与前端共用的校验口径。
func TestValidateCell(t *testing.T) {
	ok := []struct {
		ep Endpoint
		p  core.Platform
		v  float64
	}{
		{EndpointLinks, core.PlatformDouyin, 0},
		{EndpointLinks, core.PlatformDouyin, MaxRate},
		{EndpointBatchLinks, core.PlatformBilibili, 0.75},
	}
	for _, c := range ok {
		if err := ValidateCell(c.ep, c.p, c.v); err != nil {
			t.Errorf("%s/%s=%v 应合法：%v", c.p, c.ep, c.v, err)
		}
	}
	bad := []struct {
		ep Endpoint
		p  core.Platform
		v  float64
	}{
		{EndpointLinks, core.PlatformDouyin, -0.1},
		{EndpointLinks, core.PlatformDouyin, MaxRate + 0.1},
		{EndpointBatchLinks, core.PlatformDouyin, 1}, // 平台能力问题
		{Endpoint("parse"), core.PlatformDouyin, 1},  // 未知端点
	}
	for _, c := range bad {
		if err := ValidateCell(c.ep, c.p, c.v); err == nil {
			t.Errorf("%s/%s=%v 应被拒", c.p, c.ep, c.v)
		}
	}
	if err := ValidateCell(EndpointLinks, core.PlatformDouyin, math.NaN()); err == nil {
		t.Error("NaN 应被拒")
	}
}

// TestWarningsDetectBrokenInvariants：不变量只提示不阻止，但必须提示得到。
func TestWarningsDetectBrokenInvariants(t *testing.T) {
	tb := DefaultTable()
	for _, w := range tb.Warnings() {
		t.Errorf("出厂默认不该有告警：%q", w)
	}
	// detail 改成和 links 一样 → 支配问题
	v := 1.0
	tb.SetOverride(EndpointDetail, core.PlatformBilibili, &v)
	if !hasWarning(tb.Warnings(), "支配") {
		t.Errorf("应提示 links 被支配：%v", tb.Warnings())
	}
	// detail 改得比 info+links 还贵 → 打包折扣消失
	v = 5.0
	tb.SetOverride(EndpointDetail, core.PlatformBilibili, &v)
	if !hasWarning(tb.Warnings(), "打包折扣") {
		t.Errorf("应提示折扣消失：%v", tb.Warnings())
	}
	// 某端点设成 0 → 不扣配额提示；设成很大 → 手滑提示
	v = 0
	tb.SetOverride(EndpointInfo, core.PlatformBilibili, &v)
	if !hasWarning(tb.Warnings(), "不扣配额") {
		t.Errorf("应提示 0 值：%v", tb.Warnings())
	}
	v = 50
	tb.SetOverride(EndpointInfo, core.PlatformBilibili, &v)
	if !hasWarning(tb.Warnings(), "手滑") {
		t.Errorf("应提示过大的值：%v", tb.Warnings())
	}
}

func hasWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// TestProxyRateIsIndependent：代理费率与平台系数完全无关（不许被它们带偏）。
func TestProxyRateIsIndependent(t *testing.T) {
	tb := DefaultTable()
	base := ProxyCost(3<<20, 1) // 3 MiB
	if base != 1.5 {
		t.Fatalf("3 MiB 应扣 1.5，得到 %v", base)
	}
	for _, e := range AllEndpoints {
		for _, p := range AllPlatforms {
			v := 50.0
			tb.SetOverride(e, p, &v)
		}
	}
	if got := ProxyCost(3<<20, 1); got != base {
		t.Errorf("改平台系数后代理计费不该变：%v → %v", base, got)
	}
	tb.SetProxyRate(2)
	if got := ProxyCostAt(3<<20, tb.ProxyRate(), 1); got != 6 {
		t.Errorf("代理费率改成 2 后 3 MiB 应扣 6，得到 %v", got)
	}
}

// TestPlatformVocabulary：对外名字与内部键的映射。
func TestPlatformVocabulary(t *testing.T) {
	if quota := PlatformName(""); quota != PlatformDefault {
		t.Errorf("通用档对外名 = %q，想要 %q", quota, PlatformDefault)
	}
	if p, ok := NormalizePlatform(PlatformDefault); !ok || p != "" {
		t.Errorf("default 应映射到内部通用键，得到 %q/%v", p, ok)
	}
	for _, name := range []string{"bilibili", "douyin", "kuaishou", "xiaohongshu"} {
		if _, ok := NormalizePlatform(name); !ok {
			t.Errorf("%s 应被认识", name)
		}
	}
	if _, ok := NormalizePlatform("weibo"); ok {
		t.Error("未收录的平台名不该被接受（否则价目表会攒下死行）")
	}
}
