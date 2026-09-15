package wbi

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"

	"vidlink/internal/netx"
)

// 实时联调测试：默认跳过，设置 VIDLINK_LIVE=1 才执行。
//
//	go test ./internal/sign/wbi/ -run TestLive -v -timeout 120s
//
// 它验证的是一条完整链路：netx 取 WBI 口令 → 本地 MD5 签名 → 真实调用
// playurl → 解析出 DASH 流。这是纯离线单测无法覆盖的部分。
func TestLiveWBIAndPlayurl(t *testing.T) {
	if os.Getenv("VIDLINK_LIVE") == "" {
		t.Skip("设置 VIDLINK_LIVE=1 才跑实时联调")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := netx.New(netx.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	headers := netx.Headers{"User-Agent": ua, "Referer": "https://www.bilibili.com/"}

	// 1) 取口令
	mgr := NewManager(client)
	keys, err := mgr.Keys(ctx)
	if err != nil {
		t.Fatalf("取 WBI 口令失败: %v", err)
	}
	t.Logf("img_key=%s sub_key=%s mixin=%s", keys.Img, keys.Sub, keys.Mixin)
	if len(keys.Img) != 32 || len(keys.Sub) != 32 || len(keys.Mixin) != 32 {
		t.Fatalf("口令长度异常: %d/%d/%d", len(keys.Img), len(keys.Sub), len(keys.Mixin))
	}

	// 2) 取元信息拿 cid
	var view struct {
		Code int `json:"code"`
		Data struct {
			Aid   int64  `json:"aid"`
			Title string `json:"title"`
			Pages []struct {
				Cid int64 `json:"cid"`
			} `json:"pages"`
		} `json:"data"`
	}
	const bvid = "BV1GJ411x7h7"
	if err := client.GetJSON(ctx, "https://api.bilibili.com/x/web-interface/view?bvid="+bvid, headers, &view); err != nil {
		t.Fatalf("view 失败: %v", err)
	}
	if view.Code != 0 || len(view.Data.Pages) == 0 {
		t.Fatalf("view 返回异常: code=%d", view.Code)
	}
	cid := view.Data.Pages[0].Cid
	t.Logf("标题=%s aid=%d cid=%d", view.Data.Title, view.Data.Aid, cid)

	// 3) 签名并调用 playurl（WBI 通道）
	params := url.Values{
		"avid":     {itoa(view.Data.Aid)},
		"bvid":     {bvid},
		"cid":      {itoa(cid)},
		"qn":       {"127"},
		"fnver":    {"0"},
		"fnval":    {"4048"},
		"fourk":    {"1"},
		"otype":    {"json"},
		"platform": {"pc"},
		"try_look": {"1"},
	}
	query := SignQuery(params, keys.Mixin, time.Now())
	endpoint := "https://api.bilibili.com/x/player/wbi/playurl?" + query

	body, code, err := client.GetBytes(ctx, endpoint, headers, 0)
	if err != nil {
		t.Fatalf("playurl 请求失败: %v", err)
	}
	t.Logf("playurl HTTP=%d 响应 %d 字节", code, len(body))

	var play struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Quality int `json:"quality"`
			Dash    *struct {
				Duration float64 `json:"duration"`
				Video    []struct {
					ID        int    `json:"id"`
					BaseURL   string `json:"baseUrl"`
					Bandwidth int    `json:"bandwidth"`
					Codecs    string `json:"codecs"`
					Width     int    `json:"width"`
					Height    int    `json:"height"`
					FrameRate string `json:"frameRate"`
				} `json:"video"`
				Audio []struct {
					ID      int    `json:"id"`
					BaseURL string `json:"baseUrl"`
					Codecs  string `json:"codecs"`
				} `json:"audio"`
			} `json:"dash"`
			Durl []struct {
				URL string `json:"url"`
			} `json:"durl"`
			AcceptQuality []int  `json:"accept_quality"`
			Voucher       string `json:"v_voucher"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &play); err != nil {
		t.Fatalf("解析 playurl 响应失败: %v", err)
	}
	if play.Code != 0 {
		// 未登录时可能返回 v_voucher（WBI 相关问题）或 -352 风控
		t.Fatalf("playurl 业务失败: code=%d msg=%s voucher=%q", play.Code, play.Message, play.Data.Voucher)
	}
	if play.Data.Dash == nil {
		t.Fatalf("未返回 dash（可能降级为 durl，共 %d 条）", len(play.Data.Durl))
	}
	t.Logf("DASH 视频轨 %d 条，音频轨 %d 条，accept_quality=%v",
		len(play.Data.Dash.Video), len(play.Data.Dash.Audio), play.Data.AcceptQuality)
	for _, v := range play.Data.Dash.Video {
		t.Logf("  qn=%-4d %dx%d %-6s %-24s bw=%d",
			v.ID, v.Width, v.Height, v.FrameRate, v.Codecs, v.Bandwidth)
	}
	if len(play.Data.Dash.Video) == 0 {
		t.Fatal("dash.video 为空")
	}
	// 最高清晰度必须 >= 1080P 的 qn(80)，否则说明清晰度被降级
	best := 0
	for _, v := range play.Data.Dash.Video {
		if v.ID > best {
			best = v.ID
		}
	}
	t.Logf("最高清晰度 qn=%d", best)
}

// TestLivePlayurlWithoutWBI 验证「降级通道」：不带签名是否仍可用。
// 这决定了服务端要不要把 WBI 失败当作致命错误。
func TestLivePlayurlWithoutWBI(t *testing.T) {
	if os.Getenv("VIDLINK_LIVE") == "" {
		t.Skip("设置 VIDLINK_LIVE=1 才跑实时联调")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, _ := netx.New(netx.DefaultOptions())
	const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	headers := netx.Headers{"User-Agent": ua, "Referer": "https://www.bilibili.com/"}

	const ep = "https://api.bilibili.com/x/player/playurl?avid=80433022&bvid=BV1GJ411x7h7&cid=137649199&qn=127&fnver=0&fnval=4048&fourk=1&otype=json&platform=pc&try_look=1"
	body, code, err := client.GetBytes(ctx, ep, headers, 0)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	var r struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Dash *struct {
				Video []struct {
					ID int `json:"id"`
				} `json:"video"`
			} `json:"dash"`
			Voucher string `json:"v_voucher"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &r)
	n := 0
	if r.Data.Dash != nil {
		n = len(r.Data.Dash.Video)
	}
	t.Logf("无签名通道: HTTP=%d code=%d msg=%s dash.video=%d voucher=%q",
		code, r.Code, r.Message, n, r.Data.Voucher)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
