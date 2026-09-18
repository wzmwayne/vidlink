package rates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vidlink/internal/core"
	"vidlink/internal/quota"
)

func f64(v float64) *float64 { return &v }

func TestDefaultsWithoutFile(t *testing.T) {
	st, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	v := st.View()
	if v.Source != "defaults" || v.ProxyRate != quota.ProxyRate {
		t.Fatalf("空路径应是内置默认：%+v", v)
	}
	if len(v.Overrides) != 0 {
		t.Errorf("不该有覆盖：%+v", v.Overrides)
	}
	if got, err := st.Table().Coefficient(quota.EndpointLinks, core.PlatformDouyin); err != nil || got != 1.1 {
		t.Errorf("默认抖音 links = %v / %v，想要 1.1", got, err)
	}
}

// TestSeedOnlyWhenFileAbsent：环境变量只在**文件不存在**时决定代理费率。
//
// 否则管理员在面板上改的值会被下次重启用 env 悄悄覆盖回去。
func TestSeedOnlyWhenFileAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.json")
	st, err := New(Options{Path: path, Seed: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Table().ProxyRate(); got != 0.9 {
		t.Fatalf("首次应使用种子 0.9，得到 %v", got)
	}
	// 落盘：改一格就会写文件
	if _, err := st.Apply(map[string]map[string]*float64{
		"douyin": {"links": f64(2.5)},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// 第二次启动：用不同的种子，但文件已存在 → 文件优先
	st2, err := New(Options{Path: path, Seed: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	if got := st2.Table().ProxyRate(); got != 0.9 {
		t.Errorf("文件存在时应以文件为准（0.9），得到 %v", got)
	}
	if st2.View().Source != "file" {
		t.Errorf("source = %q，想要 file", st2.View().Source)
	}
}

func TestApplyAndPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.json")
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	st, err := New(Options{Path: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	view, err := st.Apply(map[string]map[string]*float64{
		"douyin":   {"links": f64(2.5)},
		"default":  {"links": f64(1.4)},
		"kuaishou": {"info": f64(0.4)},
	}, f64(0.6))
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if got, _ := st.Table().Coefficient(quota.EndpointLinks, core.PlatformDouyin); got != 2.5 {
		t.Errorf("生效值 = %v，想要 2.5", got)
	}
	// 通用档被改，未单列的平台（快手）走它
	if got, _ := st.Table().Coefficient(quota.EndpointLinks, core.PlatformKuaishou); got != 1.4 {
		t.Errorf("快手 links 应走通用档 1.4，得到 %v", got)
	}
	if view.ProxyRate != 0.6 {
		t.Errorf("代理费率 = %v，想要 0.6", view.ProxyRate)
	}
	if len(view.History) != 1 || !strings.Contains(view.History[0].Detail, "douyin/links: 1.1 → 2.5") {
		t.Errorf("审计内容不对：%+v", view.History)
	}

	// 重启：全部还原
	st2, err := New(Options{Path: path})
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	if got, _ := st2.Table().Coefficient(quota.EndpointLinks, core.PlatformDouyin); got != 2.5 {
		t.Errorf("重启后生效值 = %v，想要 2.5", got)
	}
	if got := st2.Table().ProxyRate(); got != 0.6 {
		t.Errorf("重启后代理费率 = %v，想要 0.6", got)
	}
	if got := len(st2.View().History); got != 1 {
		t.Errorf("重启后历史 = %d 条，想要 1", got)
	}
	// 文件里只存覆盖层，不存出厂默认
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Version != fileVersion {
		t.Errorf("版本不对：%d", f.Version)
	}
	// 文件是"平台 → 端点"的覆盖层，没改过的端点不写
	if len(f.Rates) != 3 || f.Rates["douyin"]["links"] != 2.5 || f.Rates["default"]["links"] != 1.4 {
		t.Errorf("文件内容不对：%s", raw)
	}
	if _, ok := f.Rates["detail"]; ok {
		t.Error("没改过的端点不该写进文件（否则代码里的默认值改了它不会跟）")
	}
	if f.ProxyRate == nil || *f.ProxyRate != 0.6 {
		t.Errorf("代理费率没写进文件：%s", raw)
	}
}

// TestApplyClearCell：nil 表示清除该格的自定义，回到内置默认。
func TestApplyClearCell(t *testing.T) {
	st, _ := New(Options{})
	if _, err := st.Apply(map[string]map[string]*float64{
		"douyin": {"links": f64(3)},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(map[string]map[string]*float64{
		"douyin": {"links": nil},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Table().Coefficient(quota.EndpointLinks, core.PlatformDouyin); got != 1.1 {
		t.Errorf("清除后应回到内置默认 1.1，得到 %v", got)
	}
	if n := len(st.View().Overrides); n != 0 {
		t.Errorf("覆盖层应为空，得到 %+v", st.View().Overrides)
	}
}

func TestApplyValidatesCells(t *testing.T) {
	st, _ := New(Options{})
	before := st.View()
	cases := []struct {
		name  string
		cells map[string]map[string]*float64
		proxy *float64
	}{
		{"未知平台", map[string]map[string]*float64{"weibo": {"links": f64(1)}}, nil},
		{"未知端点", map[string]map[string]*float64{"douyin": {"parse": f64(1)}}, nil},
		{"负数", map[string]map[string]*float64{"douyin": {"links": f64(-1)}}, nil},
		{"超上限", map[string]map[string]*float64{"douyin": {"links": f64(quota.MaxRate + 1)}}, nil},
		{"抖音批量", map[string]map[string]*float64{"douyin": {"batch_links": f64(1)}}, nil},
		{"代理费率超限", nil, f64(quota.MaxRate + 1)},
		{"代理费率负数", nil, f64(-0.1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := st.Apply(c.cells, c.proxy); err == nil {
				t.Fatal("应当报错")
			}
		})
	}
	after := st.View()
	if len(after.Overrides) != 0 || after.ProxyRate != before.ProxyRate {
		t.Errorf("校验失败不该改任何东西：%+v", after)
	}
}

// TestApplyRollsBackOnWriteFailure：落盘失败要回滚内存（否则内存与磁盘不一致）。
//
// 用"把目标路径变成一个目录"来制造写失败：rename 到一个目录必然失败，
// 而且不依赖文件权限——测试可能以 root 跑，只读目录拦不住 root。
func TestApplyRollsBackOnWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.json")
	st, err := New(Options{Path: path})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(map[string]map[string]*float64{
		"douyin": {"links": f64(9)},
	}, f64(3)); err == nil {
		t.Fatal("写不进去应当报错")
	}
	if got, _ := st.Table().Coefficient(quota.EndpointLinks, core.PlatformDouyin); got != 1.1 {
		t.Errorf("失败后内存应回滚到 1.1，得到 %v", got)
	}
	if got := st.Table().ProxyRate(); got != quota.ProxyRate {
		t.Errorf("失败后代理费率应回滚，得到 %v", got)
	}
	if len(st.View().History) != 0 {
		t.Errorf("失败不该留下审计：%+v", st.View().History)
	}
}

func TestReset(t *testing.T) {
	st, _ := New(Options{Seed: 0.7})
	if _, err := st.Apply(map[string]map[string]*float64{
		"douyin": {"links": f64(4)},
	}, f64(2)); err != nil {
		t.Fatal(err)
	}
	v, err := st.Reset()
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Overrides) != 0 {
		t.Errorf("重置后不该有覆盖：%+v", v.Overrides)
	}
	if v.ProxyRate != 0.7 {
		t.Errorf("重置后代理费率应回到种子 0.7，得到 %v", v.ProxyRate)
	}
	if got, _ := st.Table().Coefficient(quota.EndpointLinks, core.PlatformDouyin); got != 1.1 {
		t.Errorf("重置后应回到内置默认，得到 %v", got)
	}
	if len(v.History) != 2 || v.History[1].Action != ActionReset {
		t.Errorf("重置应留一条审计：%+v", v.History)
	}
}

func TestHistoryCapped(t *testing.T) {
	st, _ := New(Options{})
	for i := 0; i < HistoryKeep+10; i++ {
		if _, err := st.Apply(map[string]map[string]*float64{
			"douyin": {"links": f64(float64(i) / 10)},
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(st.View().History); got != HistoryKeep {
		t.Errorf("历史应封顶在 %d，得到 %d", HistoryKeep, got)
	}
}

func TestLoadRejectsBrokenFile(t *testing.T) {
	cases := map[string]string{
		"坏 JSON": "{not json",
		"版本不认识":  `{"version": 99, "rates": {}}`,
		"未知字段":   `{"version": 1, "rate": {}}`,
		"非法倍率":   `{"version": 1, "rates": {"links": {"douyin": 999}}}`,
		"未知平台":   `{"version": 1, "rates": {"weibo": {"links": 1}}}`,
		"抖音批量":   `{"version": 1, "rates": {"douyin": {"batch_links": 1}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rates.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := New(Options{Path: path}); err == nil {
				t.Fatal("损坏的倍率文件必须拒绝启动")
			}
		})
	}
}

// TestFilePermissions：倍率文件不该让同机其他用户读到（可能含运营信息）。
func TestFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "rates.json")
	st, err := New(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(map[string]map[string]*float64{"douyin": {"links": f64(2)}}, nil); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("文件权限 = %o，想要 600", perm)
	}
}
