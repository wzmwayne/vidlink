package secsdk

import (
	"strings"
	"testing"
)

// TestURLSearchParamsEncoding 锁住 JavaScript URLSearchParams 的转义规则。
//
// 这是整个包里最容易出错、也最致命的地方：预像与实发字节只要有一处
// 转义不同，签名就是对着平台从未见过的字节算的。特别是 '~'——
// Go 的 url.QueryEscape 不转义它，而 URLSearchParams 会转成 %7E。
func TestURLSearchParamsEncoding(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abcXYZ0189", "abcXYZ0189"},
		{"*", "*"},
		{"-", "-"},
		{".", "."},
		{"_", "_"},
		{"a b", "a%20b"},                   // 空格是 %20，不是 '+' —— 空口无凭，见 golden vector
		{"a~b", "a%7Eb"},                   // Go 标准库会漏掉这一条
		{"a/b", "a%2Fb"},                   // s4 base64 字母表里的 '/' 必须转义
		{"a+b", "a%2Bb"},                   // 字面加号必须转义，避免与空格混淆
		{"Linux x86_64", "Linux%20x86_64"}, // golden vector 里的真实取值
		{"a=b", "a%3Db"},
		{"a&b", "a%26b"},
		{"==", "%3D%3D"},   // msToken 的填充
		{"中", "%E4%B8%AD"}, // 非 ASCII 走 UTF-8 逐字节
		{"", ""},
	}
	for _, c := range cases {
		if got := URLSearchParams([][2]string{{"k", c.in}}); got != "k="+c.want {
			t.Errorf("URLSearchParams(%q) = %q, want %q", c.in, got, "k="+c.want)
		}
	}
}

func TestURLSearchParamsJoinsWithAmpersand(t *testing.T) {
	got := URLSearchParams([][2]string{{"a", "1"}, {"b", "2"}, {"c", ""}})
	if want := "a=1&b=2&c="; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCookieValue(t *testing.T) {
	header := "ttwid=abc; UIFID_TEMP=deadbeef; s_v_web_id=verify_x; __ac_nonce=zz"
	cases := []struct{ name, want string }{
		{"UIFID_TEMP", "deadbeef"},
		{"uifid_temp", "deadbeef"}, // 名不区分大小写
		{"ttwid", "abc"},
		{"s_v_web_id", "verify_x"},
		{"missing", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := CookieValue(header, c.name); got != c.want {
			t.Errorf("CookieValue(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	if got := CookieValue("", "ttwid"); got != "" {
		t.Errorf("空 header 应返回空串，得到 %q", got)
	}
}

func TestPickUIFID(t *testing.T) {
	// UIFID_TEMP 是浏览器铸造的常规拼写，应能取到
	if got := PickUIFID("ttwid=x; UIFID_TEMP=v1; s_v_web_id=y"); got != "v1" {
		t.Errorf("got %q, want v1", got)
	}
	// 小写 uifid 优先于 UIFID_TEMP：与 SDK 的查找顺序一致
	if got := PickUIFID("UIFID_TEMP=temp; uifid=primary"); got != "primary" {
		t.Errorf("got %q, want primary", got)
	}
	// 没有就是空串——这是"身份不足"的正常信号，不是错误
	if got := PickUIFID("ttwid=x; s_v_web_id=y"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := PickUIFID(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestSplitPairsDecodes(t *testing.T) {
	got := SplitPairs("a=1&b=a%2Bb&c=%E4%B8%AD&d=")
	want := [][2]string{{"a", "1"}, {"b", "a+b"}, {"c", "中"}, {"d", ""}}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项 = %v, want %v", i, got[i], want[i])
		}
	}
	if SplitPairs("") != nil {
		t.Error("空串应返回 nil")
	}
}

// TestSignGoldenVector 是本包唯一的"真相来源"。
//
// goldenPairs 是 2026-09-14 从一个真实 Chrome 会话里抓下来的
// /aweme/v1/web/aweme/detail/ 请求的全部参数，顺序即发送顺序；
// goldenSignature 是**浏览器自己算出来的** x-secsdk-web-signature。
//
// 注意末位的 timestamp 已从 pairs 里去掉：它由 Sign 追加，这正是调用方的用法。
// 如果本包的转义规则或 md5 预像有任何一处不对，这里立刻会红。
func TestSignGoldenVector(t *testing.T) {
	const (
		goldenUIFID     = "c4a29131752d59acb78af076c3dbdd52744118e38e80b4b96439ef1e20799db01a0c4ac4a3ea1e0f87c07483346e81f906ccb8fe50c6c49784990a89a174240041b35e8f47651d22c5664965fa04bbe4"
		goldenTimestamp = 1789393023
		goldenSignature = "f9a7f5cb32dc76ccf960d3f4e4c58b6a"
	)

	// 浏览器实发参数，去掉末尾的 timestamp（由 Sign 追加）。
	pairs := goldenPairs[:len(goldenPairs)-1]

	query, sig, headers := Sign(pairs, goldenUIFID, goldenTimestamp)

	if sig != goldenSignature {
		t.Fatalf("签名不一致\n  got  %s\n  want %s\n  query=%s", sig, goldenSignature, query)
	}
	if !strings.HasSuffix(query, "&"+SignatureParam+"="+goldenSignature) {
		t.Errorf("query 未以签名结尾: %s", query)
	}
	// uifid 与 timestamp 必须都在被签名的 query 里
	if !strings.Contains(query, "uifid="+goldenUIFID) {
		t.Error("query 里没有 uifid")
	}
	if !strings.Contains(query, "timestamp=1789393023") {
		t.Error("query 里没有 timestamp")
	}
	// 三个头部
	if headers[UIFIDParam] != goldenUIFID {
		t.Errorf("头部 uifid = %q", headers[UIFIDParam])
	}
	if headers[SignatureParam] != goldenSignature {
		t.Errorf("头部签名 = %q", headers[SignatureParam])
	}
	if headers[ExpireHeader] != "1789393023" {
		t.Errorf("头部 expire = %q", headers[ExpireHeader])
	}
}

// TestSignAppendsVisitorParams 验证追加顺序：uifid 在前、timestamp 在后。
func TestSignAppendsVisitorParams(t *testing.T) {
	pairs := [][2]string{{"aweme_id", "123"}, {"a_bogus", "xyz"}}
	query, _, _ := Sign(pairs, "UF", 100)
	want := "aweme_id=123&a_bogus=xyz&uifid=UF&timestamp=100"
	if !strings.HasPrefix(query, want) {
		t.Fatalf("query = %q\n want prefix %q", query, want)
	}
}

// TestSignDoesNotDuplicateUIFID：pairs 里已有 uifid 时保留原位、不追加第二个。
// 多一个 uifid 会改变预像，平台会拒绝。
//
// 注意预像里的 uifid 用的是**调用方传入的那个**，不是 pairs 里那一个：
// 官方 SDK 就是这么做的（头部发的也是它），本包与之一致。生产路径上两者
// 恒等——调用方总是先 PickUIFID 再把它传进来——但行为必须写进测试，
// 免得将来有人"顺手修正"成从 pairs 里取，那会静默改变签名字节。
func TestSignDoesNotDuplicateUIFID(t *testing.T) {
	pairs := [][2]string{{"a", "1"}, {"uifid", "ORIGINAL"}, {"b", "2"}}
	query, sig, headers := Sign(pairs, "OTHER", 100)

	if strings.Count(query, "uifid=") != 1 {
		t.Fatalf("uifid 出现了 %d 次: %s", strings.Count(query, "uifid="), query)
	}
	if !strings.Contains(query, "uifid=ORIGINAL") {
		t.Fatalf("原有的 uifid 被覆盖了: %s", query)
	}
	// 位置必须保持在 b 之前
	if strings.Index(query, "uifid=ORIGINAL") > strings.Index(query, "b=2") {
		t.Fatalf("uifid 的位置被改动了: %s", query)
	}
	// 签名的预像 = 传入的 uifid + 实际发送的 query
	preimage := strings.TrimSuffix(query, "&"+SignatureParam+"="+sig)
	if want := Signature("OTHER", 100, preimage); sig != want {
		t.Fatalf("签名预像不符: got %s want %s", sig, want)
	}
	// 头部发的也是传入的那个
	if headers[UIFIDParam] != "OTHER" {
		t.Fatalf("头部 uifid = %q, want OTHER", headers[UIFIDParam])
	}
}

func TestSignatureIsDeterministic(t *testing.T) {
	// 同一秒内、同样的输入必须得到同样的签名：它是纯函数，没有随机数。
	a := Signature("u", 1700000000, "x=1")
	b := Signature("u", 1700000000, "x=1")
	if a != b {
		t.Fatalf("非确定性: %s vs %s", a, b)
	}
	if len(a) != 32 {
		t.Fatalf("md5 十六进制应为 32 字符，得到 %d", len(a))
	}
	if a == Signature("u", 1700000001, "x=1") {
		t.Error("timestamp 不同，签名应不同")
	}
	if a == Signature("v", 1700000000, "x=1") {
		t.Error("uifid 不同，签名应不同")
	}
	if a == Signature("u", 1700000000, "x=2") {
		t.Error("query 不同，签名应不同")
	}
}

func TestSignZeroTimestampUsesNow(t *testing.T) {
	query, sig, headers := Sign([][2]string{{"a", "1"}}, "u", 0)
	if sig == "" || headers[ExpireHeader] == "" {
		t.Fatal("timestamp=0 时应取当前时间")
	}
	if !strings.Contains(query, TimestampParam+"="+headers[ExpireHeader]) {
		t.Error("query 里的 timestamp 与头部不一致")
	}
}
