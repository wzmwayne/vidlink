package bilibili

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/netx"
	"vidlink/internal/sign/abogus"
	"vidlink/internal/sign/wbi"
)

// searchFixture 是搜索响应的真实形状（节选，字段名与线上一致）。
const searchFixture = `{
  "code": 0, "message": "OK",
  "data": {
    "seid": "15159274152046276600", "page": 1, "pagesize": 2,
    "numResults": 1000, "numPages": 50,
    "result": [
      {"type":"video","author":"Python官方课程","mid":3546597933714079,
       "arcurl":"http://www.bilibili.com/video/av113006243481679",
       "aid":113006243481679,"bvid":"BV1rpWjevEip",
       "title":"【全748集】<em class=\"keyword\">Python</em> 教程 &amp; 实战",
       "description":"零基础 &lt;入门&gt;","pic":"//i0.hdslb.com/bfs/archive/abc.jpg",
       "duration":"12:34","play":12345,"danmaku":67,"pubdate":1700000000},
      {"type":"video","author":"另一个 UP","mid":1,"bvid":"BV2xxxx",
       "title":"没有高亮标签","pic":"http://i1.hdslb.com/x.jpg","duration":"1:02:03",
       "play":0,"danmaku":0}
    ]
  }
}`

func TestParseSearchResponseMapsFields(t *testing.T) {
	res, err := parseSearchResponse([]byte(searchFixture), "python", 1, 2)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if res.Platform != core.PlatformBilibili || res.Keyword != "python" {
		t.Errorf("平台/关键词不对：%+v", res)
	}
	if res.Total != 1000 || res.Page != 1 || res.Limit != 2 {
		t.Errorf("分页字段不对：total=%d page=%d limit=%d", res.Total, res.Page, res.Limit)
	}
	if len(res.Items) != 2 {
		t.Fatalf("应 2 条，得到 %d", len(res.Items))
	}

	it := res.Items[0]
	if it.ID != "BV1rpWjevEip" {
		t.Errorf("bvid 没被当作 ID：%q", it.ID)
	}
	if it.Title != "【全748集】Python 教程 & 实战" {
		t.Errorf("标题没有去标签/反转义：%q", it.Title)
	}
	if it.Desc != "零基础 <入门>" {
		t.Errorf("简介没有反转义：%q", it.Desc)
	}
	if it.Cover != "https://i0.hdslb.com/bfs/archive/abc.jpg" {
		t.Errorf("封面没有补协议：%q", it.Cover)
	}
	if it.URL != "https://www.bilibili.com/video/BV1rpWjevEip" {
		t.Errorf("url 应是规范页面地址：%q", it.URL)
	}
	if it.Duration != 754 {
		t.Errorf("12:34 应 = 754 秒，得到 %v", it.Duration)
	}
	if it.Author.Name != "Python官方课程" || it.Author.ID == "" {
		t.Errorf("作者字段不对：%+v", it.Author)
	}
	if it.Stats == nil || it.Stats.View != 12345 || it.Stats.Danmaku != 67 {
		t.Errorf("统计字段不对：%+v", it.Stats)
	}
	if !strings.HasPrefix(it.PublishedAt, "2023-11-14T") {
		t.Errorf("发布时间应为 RFC3339(UTC)：%q", it.PublishedAt)
	}

	// 第二条：1:02:03 = 3723 秒；play/danmaku 都是 0 → 不给 stats
	if got := res.Items[1].Duration; got != 3723 {
		t.Errorf("1:02:03 应 = 3723 秒，得到 %v", got)
	}
	if res.Items[1].Stats != nil {
		t.Errorf("没有互动数据时不应伪造 stats：%+v", res.Items[1].Stats)
	}
}

func TestParseSearchResponseErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind core.Kind
	}{
		{"风控", `{"code":-412,"message":"请求被拦截"}`, core.KindRateLimit},
		{"风控 352", `{"code":-352,"message":"风控"}`, core.KindRateLimit},
		{"不存在", `{"code":-404,"message":"啥都木有"}`, core.KindNotFound},
		{"坏 JSON", `{`, core.KindUpstream},
	}
	for _, c := range cases {
		_, err := parseSearchResponse([]byte(c.body), "x", 1, 20)
		if err == nil {
			t.Errorf("%s：应当报错", c.name)
			continue
		}
		if core.KindOf(err) != c.kind {
			t.Errorf("%s：kind = %v，应为 %v（%v）", c.name, core.KindOf(err), c.kind, err)
		}
	}
}

func TestParseSearchResponseEmptyResult(t *testing.T) {
	res, err := parseSearchResponse([]byte(`{"code":0,"data":{"numResults":0,"result":null}}`), "x", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if res.Items == nil {
		t.Error("空结果也要给空数组（不是 null），客户端省一次判空")
	}
	if len(res.Items) != 0 {
		t.Errorf("应为 0 条：%d", len(res.Items))
	}
}

// TestSearchUsesPlainChannelThenWbiOnRiskControl：首选非签名通道；
// 只有风控类错误才换 WBI 通道重试，其它错误不该多打一次上游。
func TestSearchUsesPlainChannelThenWbiOnRiskControl(t *testing.T) {
	var plainHits, wbiHits int
	var lastQuery string

	mux := http.NewServeMux()
	mux.HandleFunc("/x/web-interface/search/type", func(w http.ResponseWriter, r *http.Request) {
		plainHits++
		if !strings.Contains(r.URL.RawQuery, "search_type=video") ||
			!strings.Contains(r.URL.RawQuery, "page_size=2") {
			t.Errorf("搜索参数不对：%s", r.URL.RawQuery)
		}
		if r.Header.Get("Referer") == "" {
			t.Error("搜索请求应带 Referer")
		}
		_, _ = w.Write([]byte(`{"code":-412,"message":"请求被拦截"}`))
	})
	mux.HandleFunc("/x/web-interface/wbi/search/type", func(w http.ResponseWriter, r *http.Request) {
		wbiHits++
		lastQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(searchFixture))
	})
	mux.HandleFunc("/x/web-interface/nav", func(w http.ResponseWriter, r *http.Request) {
		// wbi_img 是两个 32 位十六进制文件名；MixinKey 需要 64 个字符。
		_, _ = w.Write([]byte(`{"code":-101,"data":{"wbi_img":{
			"img_url":"https://i0.hdslb.com/bfs/wbi/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png",
			"sub_url":"https://i0.hdslb.com/bfs/wbi/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.png"}}}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	e := newTestExtractor(t, ts.URL)
	res, err := e.Search(context.Background(), core.SearchQuery{Keyword: "python", Page: 1, Limit: 2})
	if err != nil {
		t.Fatalf("搜索失败：%v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("应拿到 2 条，得到 %d", len(res.Items))
	}
	if plainHits != 1 || wbiHits != 1 {
		t.Errorf("通道使用次数不对：plain=%d wbi=%d", plainHits, wbiHits)
	}
	if !strings.Contains(lastQuery, "w_rid=") || !strings.Contains(lastQuery, "wts=") {
		t.Errorf("WBI 通道必须带签名：%s", lastQuery)
	}
}

func TestSearchDoesNotRetryOnPermanentError(t *testing.T) {
	var plainHits, wbiHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/x/web-interface/search/type", func(w http.ResponseWriter, r *http.Request) {
		plainHits++
		_, _ = w.Write([]byte(`{"code":-404,"message":"啥都木有"}`))
	})
	mux.HandleFunc("/x/web-interface/wbi/search/type", func(w http.ResponseWriter, r *http.Request) {
		wbiHits++
		_, _ = w.Write([]byte(searchFixture))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	e := newTestExtractor(t, ts.URL)
	if _, err := e.Search(context.Background(), core.SearchQuery{Keyword: "x", Limit: 2}); err == nil {
		t.Fatal("应报错")
	}
	if plainHits != 1 || wbiHits != 0 {
		t.Errorf("永久错误不该换通道：plain=%d wbi=%d", plainHits, wbiHits)
	}
}

// newTestExtractor 构造一个把上游指向 httptest 的提取器。
func newTestExtractor(t *testing.T, base string) *Extractor {
	t.Helper()
	client, err := netx.New(netx.Options{Timeout: 5 * time.Second, RatePerSecond: 0})
	if err != nil {
		t.Fatalf("构造 HTTP 客户端失败：%v", err)
	}
	d := &deps.Deps{
		Client:  client,
		Cookies: deps.NewCookies(),
		Params:  deps.DefaultParams(),
		Now:     time.Now,
		WBI:     wbi.NewManager(client, wbi.WithEndpoint(base+"/x/web-interface/nav")),
		ABogus:  abogus.New(),
	}
	ep := DefaultEndpoints()
	ep.Search = base + "/x/web-interface/search/type"
	ep.SearchWBI = base + "/x/web-interface/wbi/search/type"
	ep.Nav = base + "/x/web-interface/nav"
	return New(d).WithEndpoints(ep)
}
