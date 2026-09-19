package quota

import (
	"errors"
	"testing"

	"vidlink/internal/core"
)

// TestSearchIsPerItemAndCheaperThanInfo：搜索按"条"计价（0.25/条），
// 是 info(0.5/条) 的一半；一次 20 条的搜索总价 5 配额。
func TestSearchIsPerItemAndCheaperThanInfo(t *testing.T) {
	tb := DefaultTable()
	per, err := tb.Coefficient(EndpointSearch, core.PlatformBilibili)
	if err != nil {
		t.Fatalf("B 站应可搜索：%v", err)
	}
	if per != 0.25 {
		t.Errorf("search 默认系数 = %v，应为 0.25", per)
	}
	info, err := tb.Coefficient(EndpointInfo, core.PlatformBilibili)
	if err != nil {
		t.Fatal(err)
	}
	if per*2 != info {
		t.Errorf("search(%v) 应恰好是 info(%v) 的一半", per, info)
	}

	units, err := tb.Consume(EndpointSearch, core.PlatformBilibili, 20, 1)
	if err != nil {
		t.Fatal(err)
	}
	if units != 5 {
		t.Errorf("20 条 = %v，应为 5", units)
	}
	// 荐片同样按条：50 条 = 12.5
	units, err = tb.Consume(EndpointSearch, core.PlatformJianpian, 50, 1)
	if err != nil {
		t.Fatal(err)
	}
	if units != 12.5 {
		t.Errorf("荐片 50 条 = %v，应为 12.5", units)
	}
}

// TestSearchRejectsUnsupportedPlatforms：不能搜索的平台必须
// 既算不出系数、也不允许在管理面设系数——两处用同一份判定。
func TestSearchRejectsUnsupportedPlatforms(t *testing.T) {
	tb := DefaultTable()
	for _, p := range []core.Platform{
		core.PlatformDouyin, core.PlatformKuaishou, core.PlatformXiaohongshu,
	} {
		_, err := tb.Coefficient(EndpointSearch, p)
		if err == nil {
			t.Errorf("%s 不该有 search 系数", p)
			continue
		}
		var e ErrPlatformUnsupported
		if !errors.As(err, &e) {
			t.Errorf("%s：错误类型应为 ErrPlatformUnsupported，得到 %T", p, err)
		}
		if SearchUnsupportedReason(p) == "" {
			t.Errorf("%s：应给出可读原因", p)
		}
		if v := 0.5; ValidateCell(EndpointSearch, p, v) == nil {
			t.Errorf("%s：管理面不该允许给它设 search 系数", p)
		}
	}
	// 可搜索的平台：系数算得出、也能设。
	if err := ValidateCell(EndpointSearch, core.PlatformJianpian, 0.4); err != nil {
		t.Errorf("荐片应可设 search 系数：%v", err)
	}
	if SearchUnsupportedReason(core.PlatformBilibili) != "" {
		t.Error("B 站可搜索，不该有'不支持原因'")
	}
}

// TestEveryEndpointHasADefaultRate：新增端点却忘了配默认系数，
// 会在运行期变成"这个端点算不出价"，属于最难排查的一类问题。
func TestEveryEndpointHasADefaultRate(t *testing.T) {
	tb := DefaultTable()
	for _, ep := range AllEndpoints {
		for _, p := range append(append([]core.Platform{}, AllPlatforms...), defaultPlatformKey) {
			if _, err := tb.Coefficient(ep, p); err != nil {
				// 明确"该端点不支持该平台"是允许的；其它错误不行。
				var unsupported ErrPlatformUnsupported
				if !errors.As(err, &unsupported) {
					t.Errorf("%s/%s：没有可用系数：%v", PlatformName(p), ep, err)
				}
			}
		}
	}
	// 搜索必须对所有平台都有答案（要么能搜，要么明确不支持）。
	for _, p := range AllPlatforms {
		if _, err := tb.Coefficient(EndpointSearch, p); err != nil {
			if SearchUnsupportedReason(p) == "" {
				t.Errorf("%s：既算不出 search 系数，又没说不支持", p)
			}
		}
	}
}

// TestMaxCoefficientIncludesSearch：预授权按最贵平台算，搜索也要有上界。
func TestMaxCoefficientIncludesSearch(t *testing.T) {
	tb := DefaultTable()
	if got := tb.MaxCoefficient(EndpointSearch); got != 0.25 {
		t.Errorf("search 的预授权上界 = %v，应为 0.25", got)
	}
}

// TestJianpianIsInPriceTable：荐片必须出现在对外价目表里
// （否则 /v1/platforms、/v1/usage.rates 与管理面板都会漏掉它）。
func TestJianpianIsInPriceTable(t *testing.T) {
	found := false
	for _, p := range AllPlatforms {
		if p == core.PlatformJianpian {
			found = true
		}
	}
	if !found {
		t.Fatal("AllPlatforms 缺少 jianpian")
	}
	if _, ok := NormalizePlatform("jianpian"); !ok {
		t.Error("NormalizePlatform 不认识 jianpian")
	}
	c := DefaultTable().Coefficients(core.PlatformJianpian)
	for _, ep := range AllEndpoints {
		if ep == EndpointBatchLinks {
			continue // 荐片不支持批量
		}
		if _, ok := c[ep]; !ok {
			t.Errorf("荐片缺少端点系数：%s", ep)
		}
	}
}
