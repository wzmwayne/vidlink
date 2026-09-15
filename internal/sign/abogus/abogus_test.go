package abogus

import "testing"

// goldenQuery 是参考实现黄金向量所用的 query（逐字复制，顺序不可改）。
const goldenQuery = "device_platform=webapp&aid=6383&channel=channel_pc_web&pc_client_type=1" +
	"&version_code=290100&version_name=29.1.0&cookie_enabled=true&screen_width=1920" +
	"&screen_height=1080&browser_language=zh-CN&browser_platform=Win32&browser_name=Chrome" +
	"&browser_version=130.0.0.0&browser_online=true&engine_name=Blink&engine_version=130.0.0.0" +
	"&os_name=Windows&os_version=10&cpu_core_num=12&device_memory=8&platform=PC&downlink=10" +
	"&effective_type=4g&from_user_page=1&locate_query=false&need_time_list=1" +
	"&pc_libra_divert=Windows&publish_video_strategy_type=2&round_trip_time=0" +
	"&show_live_replay_strategy=1&time_list_query=0&whale_cut_token=&update_version_code=170400" +
	"&msToken=&aweme_id=7450123456789012345"

// goldenWant 是参考实现给出的期望签名（逐字复制）。
const goldenWant = "E7mhBdugDifihdWk5l/LfY3q6fuVYmQ/0SVkMD2ffaDOJL39HMOk9exobQ4vpY2NZfmv2-ujy5kSYrrMicQnA3v6HSRKl2xp-g00t-P2so0j5ZhjCfuDnzfF-vzWt-Bd-Jd3Ech/ovKSKYi0AIee-wHvyhnFwo8sNiD4"

const (
	goldenStarted  = int64(1720000000123)
	goldenFinished = int64(1720000000129)
)

// TestDefaultSignerUsesVerifiedFingerprint 锁定「默认走已验证常量」这一契约。
//
// 之所以需要这个测试：指纹无法由 UA 推导（见 Fingerprint 文档），
// 一旦有人把它「优化」成 reverse(SM3(ua))，黄金向量会失败。
func TestDefaultSignerUsesVerifiedFingerprint(t *testing.T) {
	if got := New().Fingerprint(); got != VerifiedFingerprint {
		t.Fatal("New() 未使用 VerifiedFingerprint")
	}
	// 由 UA 派生的指纹必须与已验证常量不同——这正是不能想当然的地方。
	if NewWithUserAgent(defaultUserAgent).Fingerprint() == VerifiedFingerprint {
		t.Fatal("reverse(SM3(defaultUA)) 意外等于 VerifiedFingerprint，文档结论需修订")
	}
}

// TestSignDeterministic 用参考实现的黄金向量做回归。
//
// 固定 started/finished/三个随机数后，a_bogus 必须逐字节可复现。
func TestSignDeterministic(t *testing.T) {
	got := New().signWith(goldenQuery, "GET", goldenStarted, goldenFinished, 1234, 5678, 9012)
	if got != goldenWant {
		t.Fatalf("signWith 与黄金向量不一致:\n  got  %s\n  want %s", got, goldenWant)
	}
}

// TestVariantSourceFaithfulDiffersByOneByte 把这个已知矛盾固化成测试。
//
// 参考实现的源码与它自己的黄金向量互相矛盾：模板第 34 号标志位在密文里
// 必须是 2，校验和却要按 3 算。若照抄源码（VariantSourceFaithful），
// 输出会与黄金向量差恰好 1 个字符。
//
// 这个断言的价值是：一旦差异位置或数量变了，说明我们对模板的理解又变了，
// 必须重新评估——而不是悄悄漂移。
func TestVariantSourceFaithfulDiffersByOneByte(t *testing.T) {
	got := New().WithVariant(VariantSourceFaithful).
		signWith(goldenQuery, "GET", goldenStarted, goldenFinished, 1234, 5678, 9012)

	if len(got) != len(goldenWant) {
		t.Fatalf("长度不同: %d vs %d", len(got), len(goldenWant))
	}
	var diffs []int
	for i := 0; i < len(got); i++ {
		if got[i] != goldenWant[i] {
			diffs = append(diffs, i)
		}
	}
	if len(diffs) != 1 {
		t.Fatalf("预期恰好 1 处差异，实际 %d 处：%v\ngot  %s\nwant %s",
			len(diffs), diffs, got, goldenWant)
	}
	t.Logf("照抄源码的变体与黄金向量差 1 字符（位置 %d: %q vs %q）——已知矛盾，见 Variant 文档",
		diffs[0], got[diffs[0]], goldenWant[diffs[0]])
}

// TestSignShape 校验输出形态：长度、字符集、padding。
func TestSignShape(t *testing.T) {
	s := New()
	for i := 0; i < 50; i++ {
		got := s.Sign("aid=6383&aweme_id=7450123456789012345", "GET")
		if len(got) == 0 {
			t.Fatal("签名结果为空")
		}
		if len(got)%4 != 0 {
			t.Fatalf("签名长度 %d 不是 4 的倍数: %q", len(got), got)
		}
		for _, c := range got {
			if c == '=' {
				continue
			}
			if !isAlphabetChar(byte(c)) {
				t.Fatalf("签名含字母表外字符 %q: %q", c, got)
			}
		}
	}
}

// TestSignVariesWithQuery 保证签名随参数变化（不是常量）。
func TestSignVariesWithQuery(t *testing.T) {
	s := New()
	a := s.Sign("aid=6383&aweme_id=1", "GET")
	b := s.Sign("aid=6383&aweme_id=2", "GET")
	if a == b {
		t.Fatal("不同 aweme_id 产生了相同签名")
	}
}

// TestSignerConcurrent 验证签名器无可变共享状态。
//
// 注意：本环境不支持 -race（ThreadSanitizer VMA 报错），因此这里靠
// 多 goroutine 调用 + 结果非空来间接保证；签名器本身是纯函数式的。
func TestSignerConcurrent(t *testing.T) {
	s := New()
	const workers = 32
	done := make(chan string, workers)
	for i := 0; i < workers; i++ {
		go func() {
			done <- s.Sign("aid=6383&aweme_id=7450123456789012345", "GET")
		}()
	}
	for i := 0; i < workers; i++ {
		if got := <-done; got == "" {
			t.Fatal("并发调用返回空签名")
		}
	}
}

func isAlphabetChar(c byte) bool {
	for i := 0; i < len(alphabet); i++ {
		if alphabet[i] == c {
			return true
		}
	}
	return false
}
