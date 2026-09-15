package urlx

import "testing"

// TestParseAcceptsSchemeLessURL 覆盖"用户只贴了域名+路径"的输入。
//
// 这不是假想场景：分享面板、聊天窗口、浏览器地址栏里复制出来的内容
// 经常没有 scheme（`www.bilibili.com/video/BV...`）。
// 之前的实现只认 http(s)://，这类输入会被当成"平台内 ID"，
// 于是路由失败并回一句"不支持的链接"——用户看到的就是"识别不了"。
func TestParseAcceptsSchemeLessURL(t *testing.T) {
	cases := []struct {
		in   string
		host string
		path string
	}{
		{"www.bilibili.com/video/BV1294y1Y7tU/", "www.bilibili.com", "/video/BV1294y1Y7tU/"},
		{"www.bilibili.com/video/BV1294y1Y7tU", "www.bilibili.com", "/video/BV1294y1Y7tU"},
		{"bilibili.com/video/BV1294y1Y7tU", "bilibili.com", "/video/BV1294y1Y7tU"},
		{"b23.tv/abc123", "b23.tv", "/abc123"},
		{"v.douyin.com/iRxxxx/", "v.douyin.com", "/iRxxxx/"},
		{"https://www.bilibili.com/video/BV1xx", "www.bilibili.com", "/video/BV1xx"},
		{"http://b23.tv/abc", "b23.tv", "/abc"},
		{"bilibili.com:443/video/BV1xx", "bilibili.com", "/video/BV1xx"},
		{"WWW.Bilibili.COM/video/BV1xx", "www.bilibili.com", "/video/BV1xx"},
		// 中文文案与裸链接粘连（没有空格分隔）
		{"看看这个www.bilibili.com/video/BV1xx", "www.bilibili.com", "/video/BV1xx"},
		// 带 query 与 fragment
		{"bilibili.com/video/BV1xx?p=2&t=3", "bilibili.com", "/video/BV1xx"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			u, err := Parse(c.in)
			if err != nil {
				t.Fatalf("Parse(%q) 报错: %v", c.in, err)
			}
			if u.Host != c.host {
				t.Fatalf("Host = %q，想要 %q", u.Host, c.host)
			}
			if u.Path != c.path {
				t.Fatalf("Path = %q，想要 %q", u.Path, c.path)
			}
			if got := u.Query; c.in == "bilibili.com/video/BV1xx?p=2&t=3" && got == "" {
				t.Fatal("query 丢失了")
			}
		})
	}
}

// TestParseStillTreatsBareIDAsID 保证"补 scheme"没有把裸 ID 误当成域名。
func TestParseStillTreatsBareIDAsID(t *testing.T) {
	for _, in := range []string{
		"BV1294y1Y7tU",        // B 站稿件 ID
		"7658147082263416115", // 抖音 aweme_id
		"7.87 Pjm:/ 复制打开抖音",   // 分享文案的数字前缀，曾被当成域名的一部分
		"1.2.3",               // 版本号
		"v1.0",                // 版本号
		"结尾有个句号。",             // 纯中文
	} {
		t.Run(in, func(t *testing.T) {
			u, err := Parse(in)
			if err != nil {
				t.Fatalf("Parse(%q) 报错: %v", in, err)
			}
			if u.Host != "" {
				t.Fatalf("不该被识别成链接：Host = %q", u.Host)
			}
			if u.ID != in {
				t.Fatalf("应退化为裸 ID，ID = %q", u.ID)
			}
		})
	}
}

// TestExtractPrefersRealLink：文案里同时有裸域名与完整链接时，取完整链接。
func TestExtractPrefersRealLink(t *testing.T) {
	in := "7.87 Pjm:/ 复制打开抖音，看看【某人的作品】https://v.douyin.com/iRxxxx/"
	got, ok := Extract(in)
	if !ok {
		t.Fatal("应从文案里提取到链接")
	}
	if got != "https://v.douyin.com/iRxxxx/" {
		t.Fatalf("提取到 %q", got)
	}
}

// TestNormalizeKeepsScheme：已有 scheme 的链接必须原样保留（含大小写与端口）。
func TestNormalizeKeepsScheme(t *testing.T) {
	for _, in := range []string{
		"https://www.bilibili.com/video/BV1xx",
		"http://127.0.0.1:8080/video",
		"https://b23.tv/AbC",
	} {
		if got, ok := normalize(in); !ok || got != in {
			t.Errorf("normalize(%q) = %q, %v；应原样返回", in, got, ok)
		}
	}
}
