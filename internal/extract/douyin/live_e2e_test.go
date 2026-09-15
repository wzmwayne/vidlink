package douyin

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/netx"
	"vidlink/internal/sign/abogus"
	"vidlink/internal/sign/secsdk"
)

// TestLiveDetailEndToEnd 用真实身份把整条主链路跑一遍，并**真的去下载**校验字节。
//
// 它检查的是本项目最要命的一个问题：参数齐了、签名算出来了、HTTP 200 了，
// 但链接到底能不能播出东西？所以最后一关是取回流的开头字节，验 ftyp 魔数。
//
// 运行：
//
//	VIDLINK_LIVE=1 \
//	VIDLINK_COOKIE_DOUYIN='UIFID_TEMP=<160字符>; s_v_web_id=verify_...; ttwid=...' \
//	go test ./internal/extract/douyin/ -run TestLiveDetailEndToEnd -v
//
// Cookie 先用 tools/douyin-mint/ 铸造。
func TestLiveDetailEndToEnd(t *testing.T) {
	requireLive(t)

	cookie := os.Getenv("VIDLINK_COOKIE_DOUYIN")
	if cookie == "" {
		t.Skip("未设置 VIDLINK_COOKIE_DOUYIN：主链路需要 UIFID_TEMP")
	}
	if secsdk.PickUIFID(cookie) == "" {
		t.Skipf("Cookie 里没有 UIFID_TEMP（或等价名）：%s", cookieNameList(cookie))
	}

	awemeID := os.Getenv("VIDLINK_AWEME_ID")
	if awemeID == "" {
		awemeID = "7658147082263416115"
	}

	client, err := netx.New(netx.DefaultOptions())
	if err != nil {
		t.Fatalf("构造 HTTP 客户端失败: %v", err)
	}
	store := deps.NewCookies()
	store.Set(core.PlatformDouyin, cookie)

	e := New(&deps.Deps{
		Client:  client,
		ABogus:  abogus.New(),
		Cookies: store,
		Params:  deps.DefaultParams(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v, err := e.ParseID(ctx, awemeID)
	if err != nil {
		// 200 + 0 字节是限流，不是代码错——要说清楚，否则会被误当成回归。
		if core.KindOf(err) == core.KindRateLimit {
			t.Skipf("上游限流中（非代码问题，退避后重试）: %v", err)
		}
		t.Fatalf("ParseID 失败: %v", err)
	}

	t.Logf("标题  = %q", v.Title)
	t.Logf("作者  = %q", v.Author.Name)
	t.Logf("视频流 = %d 条，图集 = %d 张", len(v.Videos), len(v.Images))

	if len(v.Videos) == 0 && len(v.Images) == 0 {
		t.Fatalf("既没有视频也没有图集")
	}
	if len(v.Videos) == 0 {
		t.Skip("这是图文作品，跳过视频字节校验")
	}

	for i, s := range v.Videos {
		t.Logf("  流[%d] %dx%d %s codec=%s bw=%d %s",
			i, s.Width, s.Height, s.Quality, s.Codec, s.Bandwidth, truncate(s.URL, 90))
	}

	// 排序后取最优流
	core.SortStreams(v.Videos)
	best := v.Best()
	if best == nil {
		t.Fatal("Best() 返回 nil")
	}

	// 关键：真去下载，验字节。
	headers := netx.Headers{}
	for k, val := range best.Headers {
		headers[k] = val
	}
	if headers["Referer"] == "" {
		headers["Referer"] = "https://www.douyin.com/"
	}
	if headers["User-Agent"] == "" {
		headers["User-Agent"] = deps.DefaultParams().Desktop()
	}

	resp, err := client.Get(ctx, best.URL, headers)
	if err != nil {
		t.Fatalf("取流失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		t.Fatalf("取流返回 %d", resp.StatusCode)
	}
	head := make([]byte, 32)
	n, err := io.ReadFull(resp.Body, head)
	if err != nil && err != io.ErrUnexpectedEOF {
		t.Fatalf("读取流头部失败: %v", err)
	}
	if n < 16 {
		t.Fatalf("只读到 %d 字节，不是有效 MP4", n)
	}
	if !strings.Contains(string(head[:16]), "ftyp") {
		t.Fatalf("不是 MP4（缺 ftyp 魔数）: %q", head[:16])
	}
	t.Logf("字节校验通过：%q content-type=%s content-length=%s",
		head[:16], resp.Header.Get("Content-Type"), resp.Header.Get("Content-Length"))

	// 字段完整性
	if v.ID == "" {
		t.Error("ID 为空")
	}
	if v.SourceURL == "" {
		t.Error("SourceURL 为空")
	}
}

// TestLiveDetailRateLimitIsNotSuccess 确认"200 + 0 字节"被识别成限流而非成功。
//
// 这条不需要真实身份：它直接喂 classifyDetail 一组构造响应，
// 用真实世界里观测到的那个形状（空体 + 200）。
func TestLiveDetailRateLimitIsNotSuccess(t *testing.T) {
	e := &Extractor{d: &deps.Deps{Params: deps.DefaultParams()}}

	cases := []struct {
		name   string
		raw    string
		status int
		want   core.Kind
	}{
		{"200 空体 -> 限流", "", 200, core.KindRateLimit},
		{"200 纯空白 -> 限流", "  \n\t ", 200, core.KindRateLimit},
		{"444 -> 限流", "<html>Access Denied</html>", 444, core.KindRateLimit},
		{"403 -> 禁止", "Blocked by ArgusSecurityPlugin Uifid Not Found", 403, core.KindForbidden},
		{"500 -> 上游", "boom", 500, core.KindUpstream},
		{"200 合法 JSON 但无作品 -> 404", `{"status_code":0}`, 200, core.KindNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.classifyDetail(c.raw, c.status, "7658147082263416115")
			if err == nil {
				t.Fatalf("应当报错，却返回了成功")
			}
			if got := core.KindOf(err); got != c.want {
				t.Fatalf("Kind = %v, want %v (err=%v)", got, c.want, err)
			}
		})
	}
}

// TestDetailParamsAreDeterministic 锁住业务参数的顺序。
//
// a_bogus 是对这条 query 逐字节取的哈希，顺序一变签名就废。
// 这条测试的作用是：有人"顺手整理"参数时立刻变红。
func TestDetailParamsAreDeterministic(t *testing.T) {
	p := detailParams("123")
	if len(p) == 0 {
		t.Fatal("参数为空")
	}
	want := "device_platform=webapp&aid=6383&channel=channel_pc_web&pc_client_type=1&" +
		"version_code=290100&version_name=29.1.0&cookie_enabled=true&screen_width=1920&" +
		"screen_height=1080&browser_language=zh-CN&browser_platform=Win32&browser_name=Chrome&" +
		"browser_version=130.0.0.0&browser_online=true&engine_name=Blink&" +
		"engine_version=130.0.0.0&os_name=Windows&os_version=10&cpu_core_num=12&" +
		"device_memory=8&platform=PC&downlink=10&effective_type=4g&round_trip_time=0&" +
		"update_version_code=170400&aweme_id=123"
	if got := secsdk.URLSearchParams(p); got != want {
		t.Fatalf("参数序列化变了\n got %s\nwant %s", got, want)
	}
	// aweme_id 必须在最后：它是最常变的一个，放末尾便于人眼核对
	if p[len(p)-1][0] != "aweme_id" {
		t.Errorf("aweme_id 应位于末尾，实际在 %d 位", len(p)-1)
	}
	// 任何一个值都不能带空格：a_bogus 层与 secsdk 层对空格的转义规则不同
	for _, kv := range p {
		if strings.ContainsAny(kv[0]+kv[1], " ") {
			t.Errorf("参数 %s 含空格，会触发两层转义不一致", kv[0])
		}
	}
}

// requireLive 统一 live 测试的开关。
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("VIDLINK_LIVE") == "" {
		t.Skip("设置 VIDLINK_LIVE=1 才跑实时联调")
	}
}

// cookieNameList 只打印 cookie 的**名字**，不打印值——避免把身份写进测试日志。
func cookieNameList(cookie string) string {
	var names []string
	for _, part := range strings.Split(cookie, ";") {
		if k, _, ok := strings.Cut(strings.TrimSpace(part), "="); ok && k != "" {
			names = append(names, k)
		}
	}
	return strings.Join(names, ", ")
}
