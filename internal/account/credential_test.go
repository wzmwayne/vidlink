package account

import (
	"strings"
	"testing"
	"time"
)

// 黄金向量：这条串是**线上格式的契约**。
//
// 它由两个互不相干的实现算出并逐字符比对过（openssl dgst -hmac 与
// Python hmac.new），因此这里写死的是"格式"，不是"某次实现的结果"：
// 将来谁重构若悄悄改了消息拼法、大小写或分段方式，这个测试立刻红。
const (
	goldenSecret = "vl_demo_key"
	goldenHandle = "acc_17090c89a1ca6ee3" // "acc_" + hex(SHA-256(secret))[:8]
	goldenTS     = int64(0x68a3e658)      // 1755571800
	goldenMAC    = "35c1daf1f0dfefd2bb15906d25ff8e6e5dfaf2ce9757749b7ccbcb0c4a2a46c9"
	goldenCred   = "acc_17090c89a1ca6ee3.68a3e658." + goldenMAC
)

func TestHandleIDIsFixedWidth(t *testing.T) {
	for _, key := range []string{"vl_public", "vl_user_test", "Tkey", strings.Repeat("x", 200)} {
		for _, prefix := range []string{HandlePrefix, AdminPrefix} {
			h := HandleID(prefix, key)
			if len(h) != 20 {
				t.Fatalf("HandleID(%q, %q) = %q，长度 %d，想要 20", prefix, key, h, len(h))
			}
			if !ValidHandle(h) {
				t.Fatalf("HandleID(%q, %q) = %q，ValidHandle 却不认", prefix, key, h)
			}
		}
	}
	if h := Handle("vl_demo_key"); h != goldenHandle {
		t.Fatalf("Handle(vl_demo_key) = %q，想要 %q", h, goldenHandle)
	}
}

func TestGoldenCredentialVector(t *testing.T) {
	got := SignAt(HandlePrefix, goldenSecret, goldenTS)
	if got != goldenCred {
		t.Fatalf("SignAt = %q\n  想要 = %q", got, goldenCred)
	}
	if len(got) != CredLen {
		t.Fatalf("凭据长度 = %d，想要 %d", len(got), CredLen)
	}

	handle, ts, sig, ok := ParseCredential(got)
	if !ok {
		t.Fatalf("ParseCredential(%q) 失败", got)
	}
	if handle != goldenHandle || ts != goldenTS || sig != goldenMAC {
		t.Fatalf("拆解结果 = (%q, %d, %q)，想要 (%q, %d, %q)",
			handle, ts, sig, goldenHandle, goldenTS, goldenMAC)
	}
}

// TestVerifyTimeTolerance：±30 秒容差是**精确**的边界，不是"差不多"。
//
// ts 是函数的入参而不是内部 time.Now()，所以这里可以逐秒断言，
// 不需要假时钟，也不依赖窗口相位。
func TestVerifyTimeTolerance(t *testing.T) {
	ttl := DefaultTTL
	cases := []struct {
		name  string
		now   int64
		want  SigStatus
		delta string
	}{
		{"同一秒", goldenTS, SigOK, "0"},
		{"早 30 秒（临界内）", goldenTS + 30, SigOK, "+30"},
		{"晚 30 秒（临界内）", goldenTS - 30, SigOK, "-30"},
		{"过期 1 秒", goldenTS + 31, SigExpired, "+31"},
		{"未来 1 秒", goldenTS - 31, SigFuture, "-31"},
		{"过期一小时", goldenTS + 3600, SigExpired, "+3600"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(tc.now, 0)
			if got := VerifyCredential(goldenSecret, goldenHandle, goldenMAC, goldenTS, now, ttl); got != tc.want {
				t.Fatalf("Δ=%s 时结论 = %v（%s），想要 %v", tc.delta, got, got.Code(), tc.want)
			}
		})
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	now := time.Unix(goldenTS, 0)
	t.Run("换 Key", func(t *testing.T) {
		if got := VerifyCredential("vl_other_key", goldenHandle, goldenMAC, goldenTS, now, DefaultTTL); got != SigInvalid {
			t.Fatalf("换了 Key 却得到 %v", got)
		}
	})
	t.Run("改时间戳", func(t *testing.T) {
		// 把票面时间往后挪 1 秒：签名原料随之变化，旧签名必然对不上。
		if got := VerifyCredential(goldenSecret, goldenHandle, goldenMAC, goldenTS+1, now, DefaultTTL); got != SigInvalid {
			t.Fatalf("改了时间戳却得到 %v", got)
		}
	})
	t.Run("改句柄", func(t *testing.T) {
		other := Handle("vl_other_key")
		if got := VerifyCredential(goldenSecret, other, goldenMAC, goldenTS, now, DefaultTTL); got != SigInvalid {
			t.Fatalf("换了句柄却得到 %v", got)
		}
	})
	t.Run("改签名一位", func(t *testing.T) {
		bad := []byte(goldenMAC)
		if bad[0] == 'a' {
			bad[0] = 'b'
		} else {
			bad[0] = 'a'
		}
		if got := VerifyCredential(goldenSecret, goldenHandle, string(bad), goldenTS, now, DefaultTTL); got != SigInvalid {
			t.Fatalf("改了签名却得到 %v", got)
		}
	})
	t.Run("大小写", func(t *testing.T) {
		if got := VerifyCredential(goldenSecret, goldenHandle, strings.ToUpper(goldenMAC), goldenTS, now, DefaultTTL); got != SigInvalid {
			t.Fatalf("大写签名却得到 %v", got)
		}
	})
}

func TestParseCredentialRejectsMalformed(t *testing.T) {
	good := goldenCred
	cases := map[string]string{
		"空串":           "",
		"少一位":          good[:len(good)-1],
		"多一位":          good + "0",
		"缺第一个分隔符":      strings.Replace(good, ".", "", 1),
		"缺第二个分隔符":      goldenHandle + ".68a3e658" + goldenMAC,
		"句柄前缀不对":       "xyz_17090c89a1ca6ee3.68a3e658." + goldenMAC,
		"句柄短一位":        "acc_17090c89a1ca6ee" + ".68a3e658." + goldenMAC,
		"句柄含大写":        "acc_17090C89a1ca6ee3.68a3e658." + goldenMAC,
		"句柄含非 hex":     "acc_17090c89a1ca6eez.68a3e658." + goldenMAC,
		"时间戳含大写":       "acc_17090c89a1ca6ee3.68A3E658." + goldenMAC,
		"时间戳含非 hex":    "acc_17090c89a1ca6ee3.68a3e65z." + goldenMAC,
		"签名短一位":        goldenHandle + ".68a3e658." + goldenMAC[:63],
		"明文 Key 当凭据":   "vl_demo_key",
		"只有句柄":         goldenHandle,
		"ACCOUNT 大写前缀": "ACC_17090c89a1ca6ee3.68a3e658." + goldenMAC,
	}
	for name, in := range cases {
		if _, _, _, ok := ParseCredential(in); ok {
			t.Errorf("%s：ParseCredential(%q) 竟然通过了", name, in)
		}
	}
	if _, _, _, ok := ParseCredential(good); !ok {
		t.Errorf("正确凭据被拒了：%q", good)
	}
}

func TestSignAtWithMatchesSignAt(t *testing.T) {
	base := SignAt(HandlePrefix, goldenSecret, goldenTS)
	if got := SignAtWith(goldenHandle, goldenSecret, goldenTS); got != base {
		t.Fatalf("SignAtWith = %q，与 SignAt = %q 不一致", got, base)
	}
}

func TestSigStatusCodesAreStable(t *testing.T) {
	// 错误码会出现在 API 响应里，改它就是破坏兼容，这里钉死。
	for _, tc := range []struct {
		s    SigStatus
		want string
	}{
		{SigOK, "ok"},
		{SigExpired, "signature_expired"},
		{SigFuture, "signature_future"},
		{SigInvalid, "signature_invalid"},
	} {
		if got := tc.s.Code(); got != tc.want {
			t.Errorf("Code() = %q，想要 %q", got, tc.want)
		}
	}
}
