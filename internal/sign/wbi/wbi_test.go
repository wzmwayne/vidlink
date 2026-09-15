package wbi

import (
	"net/url"
	"testing"
	"time"
)

// 测试向量直接取自 bilibili-API-collect 的 WBI 文档，
// 任何算法漂移都会在这里被抓出来。
func TestMixinKey(t *testing.T) {
	got := MixinKey("7cd084941338484aae1ad9425b84077c", "4932caff0ff746eab6f01bf08b70ac45")
	const want = "ea1db124af3c7062474693fa704f4ff8"
	if got != want {
		t.Fatalf("MixinKey = %q, want %q", got, want)
	}
}

func TestSignQueryDocVector(t *testing.T) {
	mixin := MixinKey("7cd084941338484aae1ad9425b84077c", "4932caff0ff746eab6f01bf08b70ac45")
	params := url.Values{
		"foo": {"114"},
		"bar": {"514"},
		"zab": {"1919810"},
	}
	// 文档中的 wts = 1702204169
	at := time.Unix(1702204169, 0)
	got := SignQuery(params, mixin, at)
	const want = "bar=514&foo=114&wts=1702204169&zab=1919810&w_rid=8f6f2b5b3d485fe1886cec6a0be8c5d4"
	if got != want {
		t.Fatalf("SignQuery =\n  %s\nwant\n  %s", got, want)
	}
}

func TestSignQueryUnicodeAndSpace(t *testing.T) {
	// 文档示例：空格必须编码为 %20（不是 '+'），中文按 UTF-8 百分号编码，字母大写。
	params := url.Values{
		"foo": {"one one four"},
		"bar": {"五一四"},
		"baz": {"1919810"},
	}
	got := canonicalQuery(cleanValues(params))
	const want = "bar=%E4%BA%94%E4%B8%80%E5%9B%9B&baz=1919810&foo=one%20one%20four"
	if got != want {
		t.Fatalf("canonicalQuery =\n  %s\nwant\n  %s", got, want)
	}
}

func TestStripIllegal(t *testing.T) {
	cases := map[string]string{
		"a!b'c(d)e*f": "abcdef",
		"clean":       "clean",
		"":            "",
	}
	for in, want := range cases {
		if got := stripIllegal(in); got != want {
			t.Errorf("stripIllegal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilenameKey(t *testing.T) {
	got := filenameKey("https://i0.hdslb.com/bfs/wbi/7cd084941338484aae1ad9425b84077c.png")
	const want = "7cd084941338484aae1ad9425b84077c"
	if got != want {
		t.Fatalf("filenameKey = %q, want %q", got, want)
	}
	if got := filenameKey(""); got != "" {
		t.Fatalf("filenameKey(\"\") = %q, want 空", got)
	}
	if got := filenameKey("abc"); got != "abc" {
		t.Fatalf("filenameKey(\"abc\") = %q, want abc", got)
	}
}

func TestEncodeURIComponentMatchesJS(t *testing.T) {
	cases := map[string]string{
		"safe-_.!~*'()": "safe-_.!~*'()",
		"a b":           "a%20b",
		"中":             "%E4%B8%AD",
		"a+b":           "a%2Bb",
		"x&y=z":         "x%26y%3Dz",
	}
	for in, want := range cases {
		if got := encodeURIComponent(in); got != want {
			t.Errorf("encodeURIComponent(%q) = %q, want %q", in, got, want)
		}
	}
}
