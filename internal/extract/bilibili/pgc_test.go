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
	"vidlink/internal/jsonx"
	"vidlink/internal/netx"
	"vidlink/internal/sign/abogus"
	"vidlink/internal/sign/wbi"
)

// seasonFixture 是 pgc/view/web/season 的真实形状（节选，含 section 里的花絮）。
const seasonFixture = `{"code":0,"message":"success","result":{
  "title":"凡人修仙传","summary":"一个普通山村小子…","cover":"http://i0.hdslb.com/bfs/bangumi/cover.jpg",
  "total":301,
  "episodes":[
    {"id":733316,"aid":478818261,"bvid":"BV1vT411d7QE","cid":1022370693,
     "title":"1","long_title":"凡人风起天南1重制版","duration":1203160,
     "cover":"http://i0.hdslb.com/bfs/bangumi/ep1.jpg"},
    {"id":733317,"aid":478818262,"bvid":"BV1vT411d7QF","cid":1022370694,
     "title":"2","long_title":"","duration":1076000}
  ],
  "section":[{"id":1,"title":"花絮","episodes":[
    {"id":900001,"aid":1,"bvid":"BV1xx","cid":5,"title":"PV","long_title":"预告","duration":60000}
  ]}]}}`

// playurlFixture：non-签名通道必须回带 dash 流，requestPlayURL 才会采信。
const playurlFixture = `{"code":0,"message":"OK","data":{
  "accept_quality":[80,64],"quality":80,
  "dash":{"duration":1204,
    "video":[
      {"id":80,"baseUrl":"https://cdn.example/1080.m4s","base_url":"https://cdn.example/1080.m4s",
       "bandwidth":3000000,"mimeType":"video/mp4","codecs":"avc1.640032","height":1080,"width":1920,"frameRate":"25"},
      {"id":32,"baseUrl":"https://cdn.example/480.m4s","base_url":"https://cdn.example/480.m4s",
       "bandwidth":800000,"mimeType":"video/mp4","codecs":"avc1.64001F","height":480,"width":852,"frameRate":"25"}
    ],
    "audio":[{"id":30280,"baseUrl":"https://cdn.example/audio.m4s","base_url":"https://cdn.example/audio.m4s",
       "bandwidth":128000,"mimeType":"audio/mp4","codecs":"mp4a.40.2"}]}}}`

// viewFixture 是普通稿件接口对 PGC 稿件的返回（真实形状）。
const pgcViewFixture = `{"code":0,"message":"OK","data":{
  "bvid":"BV1vT411d7QE","aid":478818261,"cid":1022370693,
  "title":"【独家】《凡人修仙传之风起天南》重制版第1集【1月国创】",
  "desc":"","pic":"http://i0.hdslb.com/bfs/archive/pic.jpg","duration":1204,
  "owner":{"mid":123456,"name":"哔哩哔哩国创","face":"http://i0.hdslb.com/bfs/face.jpg"},
  "stat":{"view":201565986,"like":1,"reply":2,"favorite":3,"share":4,"danmaku":244523}}}`

// pgcTestExtractor 造一个把 PGC/稿件/取流三条链路都指向 httptest 的提取器，
// 并把每次请求的路径记下来供断言。
func pgcTestExtractor(t *testing.T, seasonBody, viewBody, playBody string, paths *[]string) *Extractor {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/pgc/season", func(w http.ResponseWriter, r *http.Request) {
		if paths != nil {
			*paths = append(*paths, r.URL.Path+"?"+r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(seasonBody))
	})
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		if paths != nil {
			*paths = append(*paths, r.URL.Path+"?"+r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(viewBody))
	})
	mux.HandleFunc("/playurl", func(w http.ResponseWriter, r *http.Request) {
		if paths != nil {
			*paths = append(*paths, r.URL.Path+"?"+r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(playBody))
	})
	mux.HandleFunc("/wbi/playurl", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":-1,"message":"不该走 WBI（非签名通道已够用）"}`))
	})
	mux.HandleFunc("/nav", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":-101,"data":{"wbi_img":{
			"img_url":"https://i0.hdslb.com/bfs/wbi/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png",
			"sub_url":"https://i0.hdslb.com/bfs/wbi/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.png"}}}`))
	})
	mux.HandleFunc("/view2", func(w http.ResponseWriter, r *http.Request) { // 字幕接口
		_, _ = w.Write([]byte(`{"code":0,"data":{"subtitle":{"subtitles":[]}}}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client, err := netx.New(netx.Options{Timeout: 5 * time.Second, RatePerSecond: 0})
	if err != nil {
		t.Fatal(err)
	}
	d := &deps.Deps{Client: client, Cookies: deps.NewCookies(), Params: deps.DefaultParams(),
		Now: time.Now, WBI: wbi.NewManager(client, wbi.WithEndpoint(ts.URL+"/nav")), ABogus: abogus.New()}
	ep := DefaultEndpoints()
	ep.PGCSeason = ts.URL + "/pgc/season"
	ep.View = ts.URL + "/view"
	ep.PlayURL = ts.URL + "/playurl"
	ep.PlayURLWBI = ts.URL + "/wbi/playurl"
	ep.PlayerV2WBI = ts.URL + "/view2"
	return New(d).WithEndpoints(ep)
}

// TestParseBangumiEpLink：ep 链接 → 季信息 → 该集 bvid/cid → 复用普通取流通道。
func TestParseBangumiEpLink(t *testing.T) {
	var paths []string
	e := pgcTestExtractor(t, seasonFixture, pgcViewFixture, playurlFixture, &paths)
	url := "https://www.bilibili.com/bangumi/play/ep733316"
	v, err := e.Parse(context.Background(),
		&core.URL{Href: url, Host: "www.bilibili.com", Path: "/bangumi/play/ep733316"})
	if err != nil {
		t.Fatalf("番剧 ep 解析失败：%v", err)
	}

	// 三条链路的调用顺序与参数：ep_id → bvid → bvid+cid（取流）
	if len(paths) != 3 {
		t.Fatalf("应恰好打 3 次上游（季/稿件/取流），得到 %d：%v", len(paths), paths)
	}
	if !strings.Contains(paths[0], "ep_id=733316") {
		t.Errorf("第一步应按 ep_id 查季信息：%s", paths[0])
	}
	if !strings.Contains(paths[1], "bvid=BV1vT411d7QE") {
		t.Errorf("第二步应查该集稿件：%s", paths[1])
	}
	if !strings.Contains(paths[2], "bvid=BV1vT411d7QE") || !strings.Contains(paths[2], "cid=1022370693") {
		t.Errorf("第三步取流应带 bvid+cid：%s", paths[2])
	}
	if !strings.Contains(paths[2], "try_look=1") {
		t.Errorf("番剧取流同样要靠 try_look=1 拿高清晰度：%s", paths[2])
	}

	if v.Platform != core.PlatformBilibili || v.ID != "BV1vT411d7QE" {
		t.Errorf("平台/ID 不对：%+v", v)
	}
	if v.Title != "凡人修仙传 第1集 凡人风起天南1重制版" {
		t.Errorf("标题应由季名+集号+集名拼成：%q", v.Title)
	}
	if v.SourceURL != "https://www.bilibili.com/bangumi/play/ep733316" {
		t.Errorf("source_url 应指回番剧页：%q", v.SourceURL)
	}
	if v.Desc == "" || v.Cover == "" || v.Author.Name != "哔哩哔哩国创" {
		t.Errorf("简介/封面/作者应来自季信息或稿件：%+v", v)
	}
	if v.Stats == nil || v.Stats.View != 201565986 {
		t.Errorf("统计应透传：%+v", v.Stats)
	}
	if len(v.Videos) != 2 || v.Best().Height != 1080 {
		t.Fatalf("应拿到 2 条视频轨且最高 1080P：%+v", v.Videos)
	}
	if !v.NeedsMux || len(v.Audios) == 0 {
		t.Error("番剧是 DASH 分离流，应标 needs_mux 且给音频轨")
	}
	if v.Videos[0].Headers["Referer"] == "" {
		t.Error("直链要带 Referer（CDN 防盗链）")
	}
}

// TestParseBangumiEpInSection：花絮/OVA 挂在 result.section[].episodes 里，也要能找到。
func TestParseBangumiEpInSection(t *testing.T) {
	e := pgcTestExtractor(t, seasonFixture, pgcViewFixture, playurlFixture, nil)
	v, err := e.Parse(context.Background(), &core.URL{
		Href: "https://www.bilibili.com/bangumi/play/ep900001",
		Host: "www.bilibili.com", Path: "/bangumi/play/ep900001",
	})
	if err != nil {
		t.Fatalf("花絮集应可解析：%v", err)
	}
	if v.SourceURL != "https://www.bilibili.com/bangumi/play/ep900001" {
		t.Errorf("source_url 不对：%q", v.SourceURL)
	}
}

// TestParseBangumiParseIDByEpNumber：platform=bilibili&id=ep733316 也能用。
func TestParseBangumiParseIDByEpNumber(t *testing.T) {
	e := pgcTestExtractor(t, seasonFixture, pgcViewFixture, playurlFixture, nil)
	v, err := e.ParseID(context.Background(), "EP733316") // 大小写都该认
	if err != nil {
		t.Fatalf("按 ep 号解析失败：%v", err)
	}
	if v.ID != "BV1vT411d7QE" {
		t.Errorf("ID 不对：%q", v.ID)
	}
}

// TestParseBangumiSeasonLinkRejected：季链接必须落到具体一集，且报错要能照着做。
func TestParseBangumiSeasonLinkRejected(t *testing.T) {
	e := pgcTestExtractor(t, seasonFixture, pgcViewFixture, playurlFixture, nil)
	_, err := e.Parse(context.Background(), &core.URL{
		Href: "https://www.bilibili.com/bangumi/play/ss28747",
		Host: "www.bilibili.com", Path: "/bangumi/play/ss28747",
	})
	if err == nil {
		t.Fatal("季链接应报错（不能猜第 1 集）")
	}
	if core.KindOf(err) != core.KindUnsupport {
		t.Errorf("应是 unsupported：%v", err)
	}
	if !strings.Contains(err.Error(), "ep") || !strings.Contains(err.Error(), "ss28747") {
		t.Errorf("报错应给出具体一集的地址写法：%v", err)
	}
}

// TestParseBangumiMissingEpisode：ep 不在集列表里 → 404，而不是拿别的集冒充。
func TestParseBangumiMissingEpisode(t *testing.T) {
	e := pgcTestExtractor(t, seasonFixture, pgcViewFixture, playurlFixture, nil)
	_, err := e.Parse(context.Background(), &core.URL{
		Href: "https://www.bilibili.com/bangumi/play/ep999999",
		Host: "www.bilibili.com", Path: "/bangumi/play/ep999999",
	})
	if err == nil || core.KindOf(err) != core.KindNotFound {
		t.Fatalf("不存在的集应 404：%v", err)
	}
}

// TestParseBangumiVipEpisodeError：会员/付费集的表现是 playurl -404，
// 必须翻译成"用户能理解"的 403，而不是 502 上游故障。
func TestParseBangumiVipEpisodeError(t *testing.T) {
	e := pgcTestExtractor(t, seasonFixture, pgcViewFixture,
		`{"code":-404,"message":"啥都木有"}`, nil)
	_, err := e.Parse(context.Background(), &core.URL{
		Href: "https://www.bilibili.com/bangumi/play/ep733316",
		Host: "www.bilibili.com", Path: "/bangumi/play/ep733316",
	})
	if err == nil {
		t.Fatal("取不到流应报错")
	}
	if core.KindOf(err) != core.KindForbidden {
		t.Errorf("会员/付费集应报 403（而不是 502 上游故障）：%v", err)
	}
	if !strings.Contains(err.Error(), "会员") {
		t.Errorf("报错应说明可能原因：%v", err)
	}
}

// TestPgctTitleAndLabel：标题拼接与集号规范化。
func TestPGCFormatting(t *testing.T) {
	node, err := jsonx.Unmarshal([]byte(seasonFixture))
	if err != nil {
		t.Fatal(err)
	}
	season := jsonx.Get(node, "result")
	cases := []struct {
		ep   pgcEpisode
		want string
	}{
		{pgcEpisode{Label: "1", Name: "凡人风起天南1重制版"}, "凡人修仙传 第1集 凡人风起天南1重制版"},
		{pgcEpisode{Label: "2"}, "凡人修仙传 第2集"},
		{pgcEpisode{Label: "OVA 1", Name: "特别篇"}, "凡人修仙传 OVA 1 特别篇"},
		{pgcEpisode{Name: "剧场版"}, "凡人修仙传 剧场版"},
		{pgcEpisode{}, "凡人修仙传"},
	}
	for _, c := range cases {
		if got := pgcTitle(season, c.ep); got != c.want {
			t.Errorf("pgcTitle(%+v) = %q，应为 %q", c.ep, got, c.want)
		}
	}
	if got := epLabel("007"); got != "第7集" {
		t.Errorf("前导零应被规范化：%q", got)
	}
	if got := epLabel("SP1"); got != "SP1" {
		t.Errorf("非数字集号应原样保留：%q", got)
	}
}

// TestFollowBangumiRedirect：普通稿件入口（如 av 号）指向番剧时，
// 直接跟着 redirect_url 解析那一集，而不是把用户打发走。
func TestFollowBangumiRedirect(t *testing.T) {
	var seasonHits, pgcViewHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("bvid") == "" {
			// 第一次：普通入口，返回"指向番剧"
			_, _ = w.Write([]byte(`{"code":0,"data":{"aid":478818261,
				"redirect_url":"https://www.bilibili.com/bangumi/play/ep733316"}}`))
			return
		}
		pgcViewHits++
		_, _ = w.Write([]byte(pgcViewFixture))
	})
	mux.HandleFunc("/pgc/season", func(w http.ResponseWriter, r *http.Request) {
		seasonHits++
		_, _ = w.Write([]byte(seasonFixture))
	})
	mux.HandleFunc("/playurl", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(playurlFixture))
	})
	mux.HandleFunc("/view2", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"subtitle":{"subtitles":[]}}}`))
	})
	mux.HandleFunc("/nav", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":-101,"data":{"wbi_img":{
			"img_url":"https://i0.hdslb.com/bfs/wbi/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png",
			"sub_url":"https://i0.hdslb.com/bfs/wbi/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.png"}}}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	e := pgcTestExtractor(t, seasonFixture, pgcViewFixture, playurlFixture, nil)
	e.ep.View = ts.URL + "/view"
	e.ep.PGCSeason = ts.URL + "/pgc/season"
	e.ep.PlayURL = ts.URL + "/playurl"
	e.ep.PlayerV2WBI = ts.URL + "/view2"

	v, err := e.Parse(context.Background(), &core.URL{
		Href: "https://www.bilibili.com/video/av478818261",
		Host: "www.bilibili.com", Path: "/video/av478818261",
	})
	if err != nil {
		t.Fatalf("应跟着 redirect 解析成功：%v", err)
	}
	if seasonHits != 1 || pgcViewHits != 1 {
		t.Errorf("应查一次季信息、一次该集稿件：season=%d view=%d", seasonHits, pgcViewHits)
	}
	if v.SourceURL != "https://www.bilibili.com/bangumi/play/ep733316" {
		t.Errorf("source_url 应指向番剧页：%q", v.SourceURL)
	}
}

func TestEpFromURL(t *testing.T) {
	cases := map[string]int64{
		"https://www.bilibili.com/bangumi/play/ep733316":     733316,
		"https://www.bilibili.com/bangumi/play/ep733316?x=1": 733316,
		"https://www.bilibili.com/bangumi/play/ss28747":      0,
		"": 0,
	}
	for in, want := range cases {
		if got := epFromURL(in); got != want {
			t.Errorf("epFromURL(%q) = %d，应为 %d", in, got, want)
		}
	}
}
