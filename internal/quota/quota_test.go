package quota

import (
	"errors"
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
