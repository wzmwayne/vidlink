package config

import "testing"

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
