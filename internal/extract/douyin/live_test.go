package douyin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"vidlink/internal/netx"
	"vidlink/internal/sign/abogus"
)

// buildDetailQuery 构造 web detail 接口的完整 query。
//
// 顺序必须与最终发送的请求完全一致——a_bogus 是对 query 字符串做的哈希，
// 顺序变了签名就废了。因此这里返回的是**字符串**而不是 url.Values。
func buildDetailQuery(awemeID string) string {
	params := [][2]string{
		{"device_platform", "webapp"},
		{"aid", "6383"},
		{"channel", "channel_pc_web"},
		{"pc_client_type", "1"},
		{"version_code", "290100"},
		{"version_name", "29.1.0"},
		{"cookie_enabled", "true"},
		{"screen_width", "1920"},
		{"screen_height", "1080"},
		{"browser_language", "zh-CN"},
		{"browser_platform", "Win32"},
		{"browser_name", "Chrome"},
		{"browser_version", "130.0.0.0"},
		{"browser_online", "true"},
		{"engine_name", "Blink"},
		{"engine_version", "130.0.0.0"},
		{"os_name", "Windows"},
		{"os_version", "10"},
		{"cpu_core_num", "12"},
		{"device_memory", "8"},
		{"platform", "PC"},
		{"downlink", "10"},
		{"effective_type", "4g"},
		{"round_trip_time", "0"},
		{"update_version_code", "170400"},
		{"aweme_id", awemeID},
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		parts = append(parts, p[0]+"="+p[1])
	}
	return strings.Join(parts, "&")
}

// TestLiveDouyinVariant 用真实接口判定 a_bogus 模板变体哪个正确。
//
//	VIDLINK_LIVE=1 VIDLINK_AWEME_ID=<真实作品ID> go test ./internal/extract/douyin/ -run TestLiveDouyinVariant -v
//
// 这是本项目唯一能"离线之外"验证 a_bogus 的手段：
// 服务端对签名错误的响应是 **HTTP 200 + 空 body**（不是 403），
// 所以判据就是"响应体是否非空且含 aweme_detail"。
func TestLiveDouyinVariant(t *testing.T) {
	if os.Getenv("VIDLINK_LIVE") == "" {
		t.Skip("设置 VIDLINK_LIVE=1 才跑实时联调")
	}
	awemeID := os.Getenv("VIDLINK_AWEME_ID")
	if awemeID == "" {
		awemeID = "7658147082263416115"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := netx.New(netx.Options{
		Timeout:             15 * time.Second,
		MaxIdleConnsPerHost: 8,
		RatePerSecond:       1,
		Burst:               2,
		Retries:             0,
	})
	if err != nil {
		t.Fatal(err)
	}

	query := buildDetailQuery(awemeID)
	const endpoint = "https://www.douyin.com/aweme/v1/web/aweme/detail/?"

	headers := netx.Headers{
		"User-Agent":      abogus.DefaultUserAgent(),
		"Referer":         "https://www.douyin.com/",
		"Accept-Language": "zh-CN,zh;q=0.9",
	}

	// Cookie 来源优先级：环境变量 > 自助铸造 ttwid
	cookie := os.Getenv("VIDLINK_COOKIE_DOUYIN")
	if cookie == "" {
		minted, err := mintTTWID(ctx, client)
		if err != nil {
			t.Logf("⚠️ 铸造 ttwid 失败: %v", err)
		} else {
			cookie = minted
			t.Logf("✅ 自助铸造 ttwid 成功: %s", truncate(minted, 60))
		}
	} else {
		t.Log("使用 VIDLINK_COOKIE_DOUYIN")
	}
	if cookie != "" {
		headers["Cookie"] = cookie
	}

	// 先测"完全不带 a_bogus"作为基线
	baseline, code, err := client.GetBytes(ctx, endpoint+query, headers, 0)
	if err != nil {
		t.Fatalf("基线请求失败: %v", err)
	}
	t.Logf("【基线 无签名】HTTP=%d body=%d 字节", code, len(baseline))

	variants := []struct {
		name string
		v    abogus.Variant
	}{
		{"VectorFaithful(密文=2,校验和按3)", abogus.VariantVectorFaithful},
		{"SourceFaithful(密文=3,校验和按3)", abogus.VariantSourceFaithful},
	}

	for _, tc := range variants {
		signer := abogus.New().WithVariant(tc.v)
		sig := signer.Sign(query, "GET")
		full := endpoint + query + "&a_bogus=" + urlEncode(sig)

		body, status, err := client.GetBytes(ctx, full, headers, 0)
		if err != nil {
			t.Logf("【%s】请求失败: %v", tc.name, err)
			continue
		}
		hasDetail := strings.Contains(string(body), "aweme_detail")
		statusMsg := extractStatusMsg(body)
		t.Logf("【%s】HTTP=%d body=%d 字节 aweme_detail=%v status_msg=%q",
			tc.name, status, len(body), hasDetail, statusMsg)
		if hasDetail {
			t.Logf("  ✅ 该变体有效！签名长度=%d", len(sig))
			return
		}
	}
	t.Log("❌ 两个变体都未拿到数据。可能原因：需要登录 Cookie / 算法已再次变更 / IP 被风控")
}

// ttwidRegister 是字节通用的设备标识注册接口，**无需登录**即可换取 ttwid。
//
// 这是抖音匿名访问的入场券：没有 ttwid，detail 接口一律返回 200 + 空 body。
const ttwidRegister = "https://ttwid.bytedance.com/ttwid/union/register/"

func mintTTWID(ctx context.Context, client *netx.Client) (string, error) {
	payload := `{"region":"cn","aid":1768,"needFid":false,"service":"www.ixigua.com",` +
		`"migrate_info":{"ticket":"","source":"node"},"cbUrlProtocol":"https","union":true}`

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ttwidRegister,
		strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", abogus.DefaultUserAgent())

	resp, err := client.Do(ctx, req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	for _, c := range resp.Cookies() {
		if c.Name == "ttwid" && c.Value != "" {
			return "ttwid=" + c.Value, nil
		}
	}
	return "", fmt.Errorf("响应中未找到 ttwid（HTTP %d）", resp.StatusCode)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func extractStatusMsg(body []byte) string {
	s := string(body)
	for _, key := range []string{`"status_msg":"`, `"message":"`} {
		if i := strings.Index(s, key); i >= 0 {
			rest := s[i+len(key):]
			if j := strings.IndexByte(rest, '"'); j >= 0 {
				return rest[:j]
			}
		}
	}
	return ""
}

// urlEncode 按 encodeURIComponent 语义编码 a_bogus 值。
// 字母表含 '/'，padding 是 '='，直接拼进 query 会被误解。
func urlEncode(s string) string {
	const upper = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%c%c", upper[c>>4], upper[c&0x0f])
	}
	return b.String()
}
