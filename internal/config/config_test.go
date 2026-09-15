package config

import (
	"os"
	"testing"
)

// TestEaseModeFromEnv 锁住 VL_EASE 的解析与它对限流的连带影响。
//
// 这个开关决定"整个账户体系是否存在"，一旦被环境变量拼写错误悄悄关掉，
// 表现是"服务照常跑，但谁都能免费用"——所以取值必须被测试钉死。
func TestEaseModeFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false}, // 未设置 = 账户模式
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"on", true},
		{"false", false},
		{"0", false},
		{"no", false},
		{"随便什么", false}, // 认不出来的值一律当关闭，不能悄悄开启免校验
	}
	for _, c := range cases {
		t.Run("VL_EASE="+c.val, func(t *testing.T) {
			t.Setenv("VL_EASE", c.val)
			t.Setenv("VIDLINK_RATE_LIMIT_RPM", "500")

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load 失败: %v", err)
			}
			if cfg.IsEase() != c.want {
				t.Fatalf("IsEase() = %v，想要 %v", cfg.IsEase(), c.want)
			}
			if c.want {
				// 免校验模式必须把按 IP 限流也关掉：否则会出现
				// "不要 Key，但打快一点就 429"这种自相矛盾的状态。
				if cfg.RateLimitRPM != 0 {
					t.Errorf("免校验模式下 RateLimitRPM = %d，想要 0", cfg.RateLimitRPM)
				}
			} else if cfg.RateLimitRPM != 500 {
				t.Errorf("账户模式下 RateLimitRPM = %d，想要沿用环境变量的 500", cfg.RateLimitRPM)
			}
		})
	}
}

// TestAccountModeKeepsLedgerPath：账户模式必须有一个账本落盘路径。
func TestAccountModeKeepsLedgerPath(t *testing.T) {
	t.Setenv("VL_EASE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.AccountsPath == "" {
		t.Fatal("账户模式下 AccountsPath 不应为空（否则账号与配额重启即丢）")
	}
}

// TestProxyEndpointDefaultsFollowMode 锁住媒体代理开关的三条规则：
//
//  1. 账户模式默认关闭（老行为不变）；
//  2. **免校验模式默认开启** —— 浏览器内混流遇到要求 Referer 的 CDN 节点时
//     必须能走代理，默认关着会让那个功能时灵时不灵；
//  3. 显式设置一律优先，尤其"显式 false 要能关掉"。
func TestProxyEndpointDefaultsFollowMode(t *testing.T) {
	cases := []struct {
		ease, endpoint, want string
	}{
		{"", "", "false"},          // 账户模式，未设置 → 关
		{"", "true", "true"},       // 账户模式，显式开
		{"", "false", "false"},     // 账户模式，显式关
		{"true", "", "true"},       // ease，未设置 → 开
		{"true", "true", "true"},   // ease，显式开
		{"true", "false", "false"}, // ease，显式关 ← 用户明确要求的例外
	}
	for _, c := range cases {
		t.Run("ease="+c.ease+",endpoint="+c.endpoint, func(t *testing.T) {
			t.Setenv("VL_EASE", c.ease)
			t.Setenv("VIDLINK_PROXY_ENDPOINT", c.endpoint)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load 失败: %v", err)
			}
			want := c.want == "true"
			if cfg.ProxySrv.Enabled != want {
				t.Fatalf("ProxySrv.Enabled = %v，想要 %v", cfg.ProxySrv.Enabled, want)
			}
		})
	}
}

// TestProxyWhitelistIsOptional：开启代理不再强制要求白名单。
func TestProxyWhitelistIsOptional(t *testing.T) {
	t.Setenv("VL_EASE", "")
	t.Setenv("VIDLINK_PROXY_ENDPOINT", "true")
	t.Setenv("VIDLINK_PROXY_ALLOW_HOSTS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("开启代理但没配白名单不应报错，却得到: %v", err)
	}
	if len(cfg.ProxySrv.AllowHosts) != 0 {
		t.Fatalf("白名单应为空，得到 %v", cfg.ProxySrv.AllowHosts)
	}
}

// TestParseDotVL：.vl 的解析要足够宽容——它是给人手写的，
// 而"因为一行写错服务就起不来"的代价远大于忽略那一行。
func TestParseDotVL(t *testing.T) {
	in := "\ufeff# vidlink 配置\n" + // BOM + 注释
		"VL_EASE=true\n" +
		"\n" +
		"   \t \n" +
		"export VIDLINK_BATCH_MAX = 7 \n" + // shell 风格 + 等号两侧空格
		`VIDLINK_ADMIN_KEY="vl_admin_引号包裹"` + "\n" +
		"VIDLINK_CACHE_TTL=15m\r\n" + // CRLF
		"# VIDLINK_PROXY_ENDPOINT=true\n" + // 注释掉的一行
		"这行没有等号\n" +
		"=没有键\n" +
		"VIDLINK_COOKIE_DOUYIN=UIFID_TEMP=a=b;ttwid=c\n" // 值里含 = 要保留
	got := parseDotVL([]byte(in))
	want := map[string]string{
		"VL_EASE":               "true",
		"VIDLINK_BATCH_MAX":     "7",
		"VIDLINK_ADMIN_KEY":     "vl_admin_引号包裹",
		"VIDLINK_CACHE_TTL":     "15m",
		"VIDLINK_COOKIE_DOUYIN": "UIFID_TEMP=a=b;ttwid=c",
	}
	if len(got) != len(want) {
		t.Fatalf("解析出 %d 项，想要 %d 项：%v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q，想要 %q", k, got[k], v)
		}
	}
}

// TestDotVLIsFallbackForEnv：.vl 与进程环境变量同时存在时的优先级。
func TestDotVLIsFallbackForEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/.vl", []byte("VL_EASE=true\nVIDLINK_BATCH_MAX=7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 切到该目录：.vl 的查找顺序里包含工作目录
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWD) }()

	// ① 环境变量没设 → 用文件里的值
	t.Setenv("VL_EASE", "")
	t.Setenv("VIDLINK_BATCH_MAX", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !cfg.IsEase() {
		t.Error("VL_EASE=true 应来自 .vl 文件")
	}
	if cfg.BatchMax != 7 {
		t.Errorf("BatchMax = %d，想要文件里的 7", cfg.BatchMax)
	}
	if cfg.ConfigFile == "" {
		t.Error("ConfigFile 应记录实际生效的文件路径")
	}

	// ② 环境变量优先：临时覆盖一个值不该去改文件
	t.Setenv("VIDLINK_BATCH_MAX", "9")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.BatchMax != 9 {
		t.Errorf("BatchMax = %d，环境变量应覆盖文件里的 7", cfg.BatchMax)
	}
}

// TestDotVLAbsentIsFine：没有 .vl 是常态，不能有任何副作用。
func TestDotVLAbsentIsFine(t *testing.T) {
	dir := t.TempDir()
	oldWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWD) }()
	t.Setenv("VL_EASE", "")
	if _, err := Load(); err != nil {
		t.Fatalf("没有 .vl 时 Load 不应报错: %v", err)
	}
}

// TestFirstDotVLOrder：候选顺序就是优先级，必须钉死。
//
// 顺序错了的后果很隐蔽：装在 ~/.local/bin 的那个 .vl 会压掉所有按目录的配置，
// 表现为"我在项目目录里写的 .vl 完全没生效"。
func TestFirstDotVLOrder(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(a+"/.vl", []byte("VIDLINK_BATCH_MAX=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b+"/.vl", []byte("VIDLINK_BATCH_MAX=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 第一个存在的胜出
	kv, path, err := firstDotVL([]string{a + "/.vl", b + "/.vl"})
	if err != nil {
		t.Fatal(err)
	}
	if kv["VIDLINK_BATCH_MAX"] != "1" || path != a+"/.vl" {
		t.Fatalf("应取第一个候选，得到 %v (%s)", kv, path)
	}
	// 前面的不存在就跳到后面那个
	kv, path, err = firstDotVL([]string{a + "/没有这个文件", b + "/.vl"})
	if err != nil {
		t.Fatal(err)
	}
	if kv["VIDLINK_BATCH_MAX"] != "2" || path != b+"/.vl" {
		t.Fatalf("应跳过不存在的候选，得到 %v (%s)", kv, path)
	}
	// 一个都不存在：返回空 map，不报错
	kv, path, err = firstDotVL([]string{a + "/x", b + "/y"})
	if err != nil || path != "" || len(kv) != 0 {
		t.Fatalf("都不存在时应返回空结果，得到 %v %q %v", kv, path, err)
	}
}

// TestVLConfigOverride：$VL_CONFIG 显式指定配置文件，写错路径必须报错。
func TestVLConfigOverride(t *testing.T) {
	dir := t.TempDir()
	conf := dir + "/custom.vl"
	if err := os.WriteFile(conf, []byte("VL_EASE=true\nVIDLINK_BATCH_MAX=5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VL_CONFIG", conf)
	t.Setenv("VL_EASE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !cfg.IsEase() || cfg.BatchMax != 5 || cfg.ConfigFile != conf {
		t.Fatalf("未按 VL_CONFIG 读取: ease=%v batch=%d file=%s",
			cfg.IsEase(), cfg.BatchMax, cfg.ConfigFile)
	}

	// 指向不存在的文件：报错而不是默默跑默认值
	t.Setenv("VL_CONFIG", dir+"/不存在.vl")
	if _, err := Load(); err == nil {
		t.Fatal("VL_CONFIG 指向不存在的文件时应报错")
	}
}
