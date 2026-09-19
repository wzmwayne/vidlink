package jianpian

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/jsonx"
	"vidlink/internal/netx"
)

// seriesFixture 是详情响应的真实形状（节选）：两条线路、集名不一致
// （"第01集" vs "第1集"）、VIP 线路在 vip_source_list_source 里、带 ftp 直链。
const seriesFixture = `{
  "code": 1, "msg": "查询成功!", "data": {
    "id": 553300, "title": "太空部队", "description": "简介",
    "thumbnail": "/upload/a.jpg", "tvimg": "/upload/b.jpg",
    "finished": 0, "mask": "第2集", "episodes_count": 2,
    "directors": ["导演甲", "导演乙"],
    "source_list_source": [
      {"id": 1, "name": "线路甲", "source_key": "back_a", "vip_source": 0, "source_list": [
        {"id": 33921, "source_name": "第01集", "url": "https://mv.example/a/33921/index.m3u8"},
        {"id": 33922, "source_name": "第02集", "url": "https://mv.example/a/33922/index.m3u8"}]},
      {"id": 2, "name": "线路乙", "source_key": "back_b", "source_list": [
        {"id": 6583678, "source_name": "第1集", "url": "https://mv.example/b/6583678/index.m3u8"},
        {"id": 6583679, "source_name": "第2集", "url": "https://mv.example/b/6583679/index.m3u8"}]}
    ],
    "vip_source_list_source": [
      {"id": 1, "name": "线路甲", "vip_source": 0, "source_list": [
        {"id": 33921, "source_name": "第01集", "url": "https://mv.example/a/33921/index.m3u8"}]}
    ],
    "ftp_list": [{"title": "第01集", "url": "ftp://ftp.example/1.mp4"}]
  }
}`

// movieFixture：电影——每条线路只有"一集"，而集名其实是版本/语言（DVD / 粤语）。
const movieFixture = `{
  "code": 1, "data": {
    "id": 42774, "title": "电影鸭", "thumbnail": "/upload/m.jpg", "mask": "DVD",
    "finished": 0, "episodes_count": "暂无",
    "source_list_source": [
      {"id": 1, "name": "VIP线路", "source_list": [
        {"id": 11279, "source_name": "DVD", "url": "https://mv.example/vip/11279/index.m3u8"}]},
      {"id": 2, "name": "蓝光线路", "source_list": [
        {"id": 99887, "source_name": "粤语", "url": "https://mv.example/blue/99887/index.m3u8"}]}
    ]
  }
}`

func testExtractor(t *testing.T, base string) *Extractor {
	t.Helper()
	client, err := netx.New(netx.Options{Timeout: 5 * time.Second, RatePerSecond: 0})
	if err != nil {
		t.Fatalf("构造客户端失败：%v", err)
	}
	d := &deps.Deps{Client: client, Cookies: deps.NewCookies(),
		Params: deps.DefaultParams(), Now: time.Now}
	ep := DefaultEndpoints()
	if base != "" {
		ep.Init = base + "/init"
		ep.Search = base + "/search"
		ep.Detail = base + "/detail"
		ep.ResourceDomain = base + "/img"
	}
	return New(d).WithEndpoints(ep)
}

// offlineExtractor 把"配置缓存"预置成空值：这样 imageURL/config 不会去请求真网，
// 单测必须离线可跑（否则 CI 与无网环境会随机失败/变慢）。
func offlineExtractor(t *testing.T) *Extractor {
	t.Helper()
	e := testExtractor(t, "")
	e.mu.Lock()
	e.secret, e.imgDomain = "", ""
	e.cfgExp = time.Now().Add(time.Hour)
	e.mu.Unlock()
	return e
}

func buildFrom(t *testing.T, fixture, id string) *core.Video {
	t.Helper()
	e := offlineExtractor(t)
	node, err := jsonx.Unmarshal([]byte(fixture))
	if err != nil {
		t.Fatalf("fixture 不是合法 JSON：%v", err)
	}
	movieID, episodeID, err := splitID(id)
	if err != nil {
		t.Fatalf("splitID(%q)：%v", id, err)
	}
	v, err := e.build(context.Background(), node, movieID, episodeID)
	if err != nil {
		t.Fatalf("build(%q)：%v", id, err)
	}
	return v
}

func TestSplitID(t *testing.T) {
	ok := []struct{ in, movie, episode string }{
		{"553300", "553300", ""},
		{"553300_33921", "553300", "33921"},
		{" 553300_33921 ", "553300", "33921"},
	}
	for _, c := range ok {
		m, e, err := splitID(c.in)
		if err != nil {
			t.Errorf("splitID(%q) 不该报错：%v", c.in, err)
			continue
		}
		if m != c.movie || e != c.episode {
			t.Errorf("splitID(%q) = (%q,%q)，应为 (%q,%q)", c.in, m, e, c.movie, c.episode)
		}
	}
	for _, bad := range []string{"", "abc", "553300_", "553300_abc", "553_33921"} {
		if _, _, err := splitID(bad); err == nil {
			t.Errorf("splitID(%q) 应当报错", bad)
		}
	}
}

func TestEpisodeKeyAndFirstInt(t *testing.T) {
	cases := map[string]string{
		"第01集": "n1", "第1集": "n1", "01": "n1", "DVD": "DVD", "粤语": "粤语",
		"1080P": "n1080", "": "",
	}
	for in, want := range cases {
		if got := episodeKey(in); got != want {
			t.Errorf("episodeKey(%q) = %q，应为 %q", in, got, want)
		}
	}
}

// TestBuildSeries：线路合并去重、VIP 优先、集名跨线路对齐、ParseID 拼接。
func TestBuildSeries(t *testing.T) {
	v := buildFrom(t, seriesFixture, "553300")

	if len(v.Lines) != 2 {
		t.Fatalf("应 2 条线路（vip 列表里的是重复项，要去重），得到 %d", len(v.Lines))
	}
	if v.Lines[0].Name != "线路甲" || !v.Lines[0].VIP {
		t.Errorf("VIP 线路应排在第一条：%+v", v.Lines[0])
	}
	if v.Lines[1].Name != "线路乙" || v.Lines[1].VIP {
		t.Errorf("第二条应是普通线路：%+v", v.Lines[1])
	}
	if v.Latest != "第2集" || v.Finished {
		t.Errorf("最新进度/完结标记不对：%q %v", v.Latest, v.Finished)
	}
	if len(v.Episodes) != 4 {
		t.Fatalf("Episodes 应是全部线路 × 全部集（2×2），得到 %d", len(v.Episodes))
	}
	first := v.Episodes[0]
	if first.ID != "33921" || first.ParseID != "553300_33921" || first.Line != "线路甲" || first.Index != 1 {
		t.Errorf("第一集字段不对：%+v", first)
	}
	if first.FTP != "ftp://ftp.example/1.mp4" {
		t.Errorf("ftp 直链应按集名挂上：%q", first.FTP)
	}
	if v.EpisodeOnly {
		t.Error("未指定单集时 EpisodeOnly 应为 false")
	}

	// 默认第 1 集：三条流（两条线路），线路甲在前。
	if len(v.Videos) != 2 {
		t.Fatalf("应 2 条流（一条线路一条），得到 %d", len(v.Videos))
	}
	if v.Videos[0].Quality != "线路甲" || !strings.Contains(v.Videos[0].URL, "/a/33921/") {
		t.Errorf("默认应取 VIP/第一条线路的第 1 集：%+v", v.Videos[0])
	}
	if v.Videos[1].Quality != "线路乙" || !strings.Contains(v.Videos[1].URL, "/b/6583678/") {
		t.Errorf("第二条线路要靠集名规范化（第01集 vs 第1集）对齐：%+v", v.Videos[1])
	}
	if v.Videos[0].MimeType != "application/vnd.apple.mpegurl" {
		t.Errorf("荐片的流是 HLS：%q", v.Videos[0].MimeType)
	}
	if v.Videos[0].Headers["User-Agent"] == "" {
		t.Error("HLS 播放列表需要携带 UA")
	}
}

// TestBuildSeriesWithEpisodeID：传 `<影片ID>_<单集ID>` 时只列这一集，
// 但 streams 仍然覆盖所有线路（同一集的其它线路 ID 不同，靠名称/序号对齐）。
func TestBuildSeriesWithEpisodeID(t *testing.T) {
	v := buildFrom(t, seriesFixture, "553300_6583678")
	if !v.EpisodeOnly {
		t.Fatal("应标记 EpisodeOnly")
	}
	if len(v.Episodes) != 1 || v.Episodes[0].Line != "线路乙" {
		t.Fatalf("只应列出这一集（且标明它属于线路乙）：%+v", v.Episodes)
	}
	if len(v.Videos) != 2 {
		t.Fatalf("streams 仍应覆盖全部线路：%d", len(v.Videos))
	}
	if !strings.Contains(v.Videos[0].URL, "/a/33921/") {
		t.Errorf("第一条流应是默认（VIP）线路的同名集：%+v", v.Videos[0])
	}
	if !strings.Contains(v.Videos[1].URL, "/b/6583678/") {
		t.Errorf("第二条流应是请求的那一集：%+v", v.Videos[1])
	}
}

func TestBuildUnknownEpisodeID(t *testing.T) {
	e := offlineExtractor(t)
	node, _ := jsonx.Unmarshal([]byte(seriesFixture))
	_, err := e.build(context.Background(), node, "553300", "999999")
	if err == nil {
		t.Fatal("不存在的单集 ID 应报错")
	}
	if core.KindOf(err) != core.KindNotFound {
		t.Errorf("应是 not_found：%v", err)
	}
	if !strings.Contains(err.Error(), "/v1/info") {
		t.Errorf("报错应告诉用户去哪拿单集 ID：%v", err)
	}
}

// TestBuildMovie：电影不设 latest（mask 是版本不是进度），
// 各线路靠序号兜底对齐（"DVD" vs "粤语"名称对不上）。
func TestBuildMovie(t *testing.T) {
	v := buildFrom(t, movieFixture, "42774")
	if v.Latest != "" {
		t.Errorf("电影不该把版本标签当成更新进度：%q", v.Latest)
	}
	if len(v.Episodes) != 2 {
		t.Fatalf("两条线路各一集，应列 2 条：%d", len(v.Episodes))
	}
	if len(v.Videos) != 2 {
		t.Fatalf("播放地址应覆盖两条线路：%d", len(v.Videos))
	}
	if !strings.Contains(v.Videos[1].URL, "/blue/99887/") {
		t.Errorf("名称不同的线路要靠序号兜底：%+v", v.Videos[1])
	}
	if v.Warning == "" {
		t.Error("线路版本/语言不一致时应有 warning")
	}
}

func TestParseLinesVIPDetection(t *testing.T) {
	node, _ := jsonx.Unmarshal([]byte(seriesFixture))
	data := jsonx.Get(node, "data")
	lines := preferVIP(parseLines(data))
	if len(lines) != 2 {
		t.Fatalf("线路数不对：%d", len(lines))
	}
	if !lines[0].vip {
		t.Error("出现在 vip_source_list_source 里的线路应标记 VIP")
	}
	if lines[1].vip {
		t.Error("普通线路不应标记 VIP")
	}

	// 另一种判定：名字里带 VIP。
	nameVIP, _ := jsonx.Unmarshal([]byte(`{"source_list_source":[
		{"name":"VIP线路","source_list":[{"id":1,"source_name":"第01集","url":"u"}]},
		{"name":"普通线路","vip_source":1,"source_list":[{"id":2,"source_name":"第01集","url":"u"}]}]}`))
	got := preferVIP(parseLines(nameVIP))
	if len(got) != 2 || !got[0].vip || !got[1].vip {
		t.Errorf("按名字与 vip_source 字段都应识别为 VIP：%+v", got)
	}
}

// TestConfigAndImageURL：secret 与图片域名都来自上游，且带缓存（一次调用只取一次）。
func TestConfigAndImageURL(t *testing.T) {
	var initHits, imgHits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/init":
			atomic.AddInt32(&initHits, 1)
			_, _ = w.Write([]byte(`{"code":1,"data":{"secret":"SECRET123"}}`))
		case "/img":
			atomic.AddInt32(&imgHits, 1)
			_, _ = w.Write([]byte(`{"code":1,"data":{"imgDomain":"img.a.com,img.b.com"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	e := testExtractor(t, ts.URL)
	if got := e.imageURL(context.Background(), "/upload/x.jpg"); got != "https://img.a.com/upload/x.jpg" {
		t.Errorf("相对封面应补成图床地址：%q", got)
	}
	if got := e.imageURL(context.Background(), "https://full/x.jpg"); got != "https://full/x.jpg" {
		t.Errorf("绝对地址不该被改写：%q", got)
	}
	h := e.headers(context.Background())
	if h["signature"] == "" || h["timestamp"] == "" || h["version"] != "610" {
		t.Errorf("应带上可选签名头：%v", h)
	}
	// 再取一次：走缓存，不该再打上游。
	_ = e.imageURL(context.Background(), "/upload/y.jpg")
	_, _ = e.config(context.Background())
	if n := atomic.LoadInt32(&initHits); n != 1 {
		t.Errorf("init 应被缓存：%d 次", n)
	}
	if n := atomic.LoadInt32(&imgHits); n != 1 {
		t.Errorf("图片域名应被缓存：%d 次", n)
	}
}

// TestSearchWindow：上游固定 10 条/页，请求 (page,limit) 要换算成窗口并切片。
func TestSearchWindow(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if r.URL.Query().Get("key") == "fail2" && page == "2" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Query().Get("key") == "fail1" && page == "1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var items []string
		for i := 0; i < pageSize; i++ {
			items = append(items, fmt.Sprintf(`{"id":%s%02d,"title":"t","top_category":{"name":"电影"}}`, page, i))
		}
		_, _ = w.Write([]byte(`{"code":1,"total":999,"data":[` + strings.Join(items, ",") + `]}`))
	}))
	defer ts.Close()

	e := offlineExtractor(t)
	e.ep.Search = ts.URL + "/search"

	// page=2, limit=20 → 上游第 3、4 页
	res, err := e.Search(context.Background(), core.SearchQuery{Keyword: "k", Page: 2, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 20 {
		t.Fatalf("应 20 条，得到 %d", len(res.Items))
	}
	// 上游 10 条/页：请求第 2 页 ×20 条 = 上游第 3、4 页（条目 21..40）。
	if res.Items[0].ID != "300" || res.Items[19].ID != "409" {
		t.Errorf("窗口不对：首=%s 末=%s", res.Items[0].ID, res.Items[19].ID)
	}
	if res.Total != 999 {
		t.Errorf("total 应来自上游：%d", res.Total)
	}

	// page=2, limit=15 → 上游第 2、3 页，丢掉前 5 条
	res, err = e.Search(context.Background(), core.SearchQuery{Keyword: "k", Page: 2, Limit: 15})
	if err != nil {
		t.Fatal(err)
	}
	// 请求第 2 页 ×15 条 = 从第 16 条开始：上游第 2、3 页，丢掉前 5 条。
	if len(res.Items) != 15 || res.Items[0].ID != "205" {
		t.Errorf("带偏移的窗口不对：len=%d 首=%s", len(res.Items), res.Items[0].ID)
	}

	// 后续页失败：给出 warning 但不当成整单失败
	res, err = e.Search(context.Background(), core.SearchQuery{Keyword: "fail2", Page: 1, Limit: 20})
	if err != nil {
		t.Fatalf("第二页失败不该让整单失败：%v", err)
	}
	if res.Warning == "" {
		t.Error("部分页失败应有 warning")
	}
	if len(res.Items) != pageSize {
		t.Errorf("应只剩第一页的 %d 条：%d", pageSize, len(res.Items))
	}

	// 首页失败：整单失败
	if _, err = e.Search(context.Background(), core.SearchQuery{Keyword: "fail1", Page: 1, Limit: 20}); err == nil {
		t.Error("首页失败应报错")
	}
}

// TestSearchItemMapping：荐片搜索条目映射（类型/年份/评分/封面/分类）。
func TestSearchItemMapping(t *testing.T) {
	e := offlineExtractor(t)
	node, _ := jsonx.Unmarshal([]byte(`{
		"id": 20492, "title": "太空先锋", "description": "d",
		"thumbnail": "/upload/x.jpg", "mask": "蓝光中英双字", "score": 7.9,
		"years": [{"year": "1983"}], "top_category": {"name": "电影"},
		"actors": ["艾德·哈里斯"], "directors": ["菲利普·考夫曼"]}`))
	it := e.searchItem(context.Background(), node, "20492")
	if it.Type != "movie" || it.Year != 1983 || it.Score != 7.9 || it.Category != "电影" {
		t.Errorf("字段映射不对：%+v", it)
	}
	if it.Cover != "/upload/x.jpg" {
		t.Errorf("没有图片域名时应保持相对路径（不猜）：%q", it.Cover)
	}
	if len(it.Actors) != 1 || it.Author.Name != "菲利普·考夫曼" {
		t.Errorf("演员/导演不对：%+v", it)
	}
	if it.URL != "" {
		t.Error("荐片没有稳定页面地址，url 应为空")
	}
}
