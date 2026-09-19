package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/extract"
	"vidlink/internal/quota"
	"vidlink/internal/service"
)

// searchStub 是一个只实现搜索能力的假提取器：用来把 /v1/search 的
// 契约（形状、按条计价、参数校验、缓存行为）与上游彻底解耦。
type searchStub struct {
	name  core.Platform
	items []core.SearchItem
	total int
	err   error
	calls *int32
}

func (s searchStub) Name() core.Platform  { return s.name }
func (s searchStub) Hosts() []string      { return []string{"searchstub.test"} }
func (s searchStub) Match(*core.URL) bool { return false }

func (s searchStub) Parse(context.Context, *core.URL) (*core.Video, error) {
	return nil, core.NotFound(s.name, "stub 不解析")
}

func (s searchStub) Search(_ context.Context, q core.SearchQuery) (*core.SearchResult, error) {
	if s.calls != nil {
		atomic.AddInt32(s.calls, 1)
	}
	if s.err != nil {
		return nil, s.err
	}
	// 回显请求参数：真实提取器都会填，测试也据此检查没有把参数丢掉。
	return &core.SearchResult{
		Platform: s.name,
		Keyword:  q.Keyword,
		Page:     q.Page,
		Limit:    q.Limit,
		Total:    s.total,
		Items:    s.items,
	}, nil
}

// noSearchStub 是一个**没有**搜索能力的假平台：用来验证
// "能力缺失 + 原因文案"这条路径（真实世界的抖音/快手/小红书就是它）。
type noSearchStub struct{ name core.Platform }

func (s noSearchStub) Name() core.Platform  { return s.name }
func (s noSearchStub) Hosts() []string      { return []string{"nosearch.test"} }
func (s noSearchStub) Match(*core.URL) bool { return false }

func (s noSearchStub) Parse(context.Context, *core.URL) (*core.Video, error) {
	return nil, core.NotFound(s.name, "stub 不解析")
}

// searchStubItems 造 n 条结果。
func searchStubItems(n int) []core.SearchItem {
	out := make([]core.SearchItem, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, core.SearchItem{
			Platform: core.PlatformJianpian,
			Type:     "series",
			ID:       "1000" + string(rune('0'+i%10)),
			Title:    "结果",
			Cover:    "https://img.example/1.jpg",
			Score:    8,
			Year:     2024,
			Category: "电视剧",
			Actors:   []string{"演员"},
		})
	}
	return out
}

func decodeSearch(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, body)
	}
	return out
}

// TestSearchEndpointContract：形状 + 按返回条数计价 + 下一步提示。
func TestSearchEndpointContract(t *testing.T) {
	stub := searchStub{name: core.PlatformJianpian, items: searchStubItems(3), total: 2940}
	env := newTestEnv(t, nil, stub)

	w := do(env.handler(), http.MethodGet,
		"/v1/search?platform=jianpian&keyword=%E5%A4%AA%E7%A9%BA&limit=3", userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d：%s", w.Code, w.Body.String())
	}
	d := decodeSearch(t, w.Body.Bytes())
	if d["platform"] != "jianpian" || d["keyword"] != "太空" {
		t.Errorf("回显字段不对：%v", d)
	}
	if d["count"].(float64) != 3 || d["total"].(float64) != 2940 {
		t.Errorf("count/total 不对：%v / %v", d["count"], d["total"])
	}
	if d["has_more"] != true || !strings.Contains(d["next"].(string), "page=2") {
		t.Errorf("翻页提示不对：has_more=%v next=%v", d["has_more"], d["next"])
	}
	// 按条计价：3 条 × 0.25 = 0.75
	if got := d["cost"].(float64); got != 0.75 {
		t.Errorf("cost = %v，应为 0.75", got)
	}
	if got := w.Header().Get("X-Quota-Consumed"); got != "0.75" {
		t.Errorf("X-Quota-Consumed = %q", got)
	}
	items := d["items"].([]any)
	first := items[0].(map[string]any)
	if first["cover"] == "" || first["id"] == "" {
		t.Errorf("条目缺少封面/ID：%v", first)
	}
	if !strings.Contains(first["detail"].(string), "/v1/detail?platform=jianpian&id=") {
		t.Errorf("条目应给出 detail 入口提示：%v", first["detail"])
	}
}

// TestSearchChargesPerReturnedItem：没有结果就一分不扣；缓存命中照样计费
// （缓存是省上游，不是免费额度）。
func TestSearchChargesPerReturnedItem(t *testing.T) {
	var calls int32
	env := newTestEnv(t, nil, searchStub{name: core.PlatformJianpian, items: searchStubItems(2), calls: &calls})
	h := env.handler()

	w := do(h, http.MethodGet, "/v1/search?platform=jianpian&keyword=a", userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d：%s", w.Code, w.Body.String())
	}
	d := decodeSearch(t, w.Body.Bytes())
	if got := d["cost"].(float64); got != 0.5 {
		t.Fatalf("2 条应扣 0.5，得到 %v", got)
	}

	// 第二次命中缓存：上游只该被调一次，但配额照样扣。
	w2 := do(h, http.MethodGet, "/v1/search?platform=jianpian&keyword=a", userHdr(env.userKey))
	d2 := decodeSearch(t, w2.Body.Bytes())
	if got := d2["cost"].(float64); got != 0.5 {
		t.Errorf("缓存命中也应扣费：%v", got)
	}
	if d2["cached"] != true {
		t.Errorf("第二次应标记 cached=true：%v", d2)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("上游调用次数 = %d，应为 1（缓存 + 单飞）", n)
	}

	// 空结果：不扣。
	empty := newTestEnv(t, nil, searchStub{name: core.PlatformJianpian}).handler()
	w3 := do(empty, http.MethodGet, "/v1/search?platform=jianpian&keyword=none", userHdr(env.userKey))
	d3 := decodeSearch(t, w3.Body.Bytes())
	if _, ok := d3["cost"]; ok {
		t.Errorf("空结果不应有 cost 字段：%v", d3)
	}
}

// TestSearchParameterValidation：参数范围与错误码。
func TestSearchParameterValidation(t *testing.T) {
	env := newTestEnv(t, nil, searchStub{name: core.PlatformJianpian, items: searchStubItems(1)})
	h := env.handler()

	cases := []struct {
		name string
		path string
		kind string
	}{
		{"缺平台", "/v1/search?keyword=x", "unsupported"},
		{"未知平台", "/v1/search?platform=nope&keyword=x", "unsupported"},
		{"不支持搜索的平台", "/v1/search?platform=xiaohongshu&keyword=x", "unsupported"},
		{"缺关键词", "/v1/search?platform=jianpian", "bad_input"},
		{"关键词过长", "/v1/search?platform=jianpian&keyword=" + strings.Repeat("a", 65), "bad_input"},
		{"limit 非法", "/v1/search?platform=jianpian&keyword=x&limit=0", "bad_input"},
		{"limit 过大", "/v1/search?platform=jianpian&keyword=x&limit=51", "bad_input"},
		{"page 过大", "/v1/search?platform=jianpian&keyword=x&page=51", "bad_input"},
	}
	for _, c := range cases {
		w := do(h, http.MethodGet, c.path, userHdr(env.userKey))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s：应 400，得到 %d（%s）", c.name, w.Code, w.Body.String())
			continue
		}
		kind, msg, _ := errBody(t, w)
		if kind != c.kind {
			t.Errorf("%s：kind = %q，应为 %q", c.name, kind, c.kind)
		}
		if msg == "" {
			t.Errorf("%s：错误文案不应为空", c.name)
		}
	}

	// 不支持搜索的平台，报错里必须带"为什么"。
	w := do(h, http.MethodGet, "/v1/search?platform=kuaishou&keyword=x", userHdr(env.userKey))
	if _, msg, _ := errBody(t, w); !strings.Contains(msg, "反爬") {
		t.Errorf("快手的报错应说明原因：%s", msg)
	}
}

// TestSearchUnaffectedByUpstreamError：上游错误不扣配额，并映射成合适的状态码。
func TestSearchUnaffectedByUpstreamError(t *testing.T) {
	env := newTestEnv(t, nil, searchStub{name: core.PlatformJianpian,
		err: core.Errf(core.KindRateLimit, core.PlatformJianpian, "api", "风控")})
	w := do(env.handler(), http.MethodGet, "/v1/search?platform=jianpian&keyword=x", userHdr(env.userKey))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("风控应 429，得到 %d", w.Code)
	}
	if _, ok := decodeSearch(t, w.Body.Bytes())["cost"]; ok {
		t.Error("失败不应出现 cost")
	}
}

// TestSearchEaseModeNeedsNoKeyAndNoQuota：免校验模式下搜索照常可用，但不计量。
func TestSearchEaseModeNeedsNoKeyAndNoQuota(t *testing.T) {
	env := newEaseEnv(t, nil, searchStub{name: core.PlatformJianpian, items: searchStubItems(2)})
	w := do(env.handler(), http.MethodGet, "/v1/search?platform=jianpian&keyword=x", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("免校验模式应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	d := decodeSearch(t, w.Body.Bytes())
	if _, ok := d["cost"]; ok {
		t.Errorf("免校验模式不应出现 cost：%v", d)
	}
	if w.Header().Get("X-Quota-Consumed") != "" {
		t.Error("免校验模式不应写配额响应头")
	}
}

// TestPlatformsExposeSearchCapability：能力（接口实现）与原因（能力表）必须一致，
// 且两者都要出现在 /v1/platforms 上。
func TestPlatformsExposeSearchCapability(t *testing.T) {
	env := newTestEnv(t, nil,
		searchStub{name: core.PlatformJianpian, items: searchStubItems(1)},
		noSearchStub{name: core.PlatformDouyin})
	w := do(env.handler(), http.MethodGet, "/v1/platforms", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", w.Code)
	}
	var body struct {
		Platforms []struct {
			Name         string `json:"name"`
			Search       bool   `json:"search"`
			SearchReason string `json:"search_reason"`
		} `json:"platforms"`
		Limits map[string]any `json:"limits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	got := map[string]struct {
		ok     bool
		reason string
	}{}
	for _, p := range body.Platforms {
		got[p.Name] = struct {
			ok     bool
			reason string
		}{p.Search, p.SearchReason}
	}
	if !got["jianpian"].ok {
		t.Error("jianpian 应可搜索（stub 实现了 SearchExtractor）")
	}
	if got["douyin"].ok || !strings.Contains(got["douyin"].reason, "2483") {
		t.Errorf("抖音应不可搜索且说明需要登录态：%+v", got["douyin"])
	}
	if _, ok := body.Limits["searchable_platforms"]; !ok {
		t.Errorf("limits 应给出可搜索平台：%v", body.Limits)
	}
}

// TestSearchCapabilityMatchesQuotaReasons 防止"两处真相"漂移：
// 能力的事实来源是"提取器实现了 SearchExtractor"，配额表里的原因是"为什么没有实现"。
// 两者一旦对不上，用户就会看到自相矛盾的 /v1/platforms 或算不出系数。
func TestSearchCapabilityMatchesQuotaReasons(t *testing.T) {
	d := &deps.Deps{}
	svc := service.New(d, extract.NewRegistry(d), service.DefaultOptions())
	table := quota.DefaultTable()

	seenSearchable := 0
	for _, p := range quota.AllPlatforms {
		searchable := svc.Searchable(p)
		reason := quota.SearchUnsupportedReason(p)
		if searchable {
			seenSearchable++
		}
		if searchable && reason != "" {
			t.Errorf("%s：实现了搜索能力，但配额表仍说不支持（%s）", p, reason)
		}
		if !searchable && reason == "" {
			t.Errorf("%s：没有搜索能力，却没有给出原因", p)
		}
		_, err := table.Coefficient(quota.EndpointSearch, p)
		if searchable && err != nil {
			t.Errorf("%s：可搜索却算不出系数：%v", p, err)
		}
		if !searchable && err == nil {
			t.Errorf("%s：不可搜索却能算出系数", p)
		}
	}
	if seenSearchable == 0 {
		t.Fatal("没有任何平台可搜索：内置注册表可能没接上")
	}
}

// TestUISeriesSelectionWiring：前端必须真的能选集——
// 线路按钮、选集按钮、parse_id、以及"切线路要带上 quality 重跑同一档位"。
func TestUISeriesSelectionWiring(t *testing.T) {
	ui := string(uiHTML)
	for _, want := range []string{
		`function renderSeries(`, `renderSeries(d.series, d)`,
		`function switchLine(`, `function pickEpisode(`,
		`ep.parse_id`, `s.in_lines`, `s.truncated`,
		`switchLine(ln.name)`, `pickEpisode(ep, d)`,
		// 切线路 = 用 quality=<线路名> 重跑上一次的动作（不擅自升级档位）
		`$("#quality").value = name`,
		`lastAct === "links" || lastAct === "info" || lastAct === "detail"`,
		// 选集 = 把 parse_id 填进表单再取直链
		`$("#vid").value = ep.parse_id || ep.id`,
		// 剧集型平台的搜索卡片主按钮是「详情（选集）」：光有影片 ID 取不了流
		`const needsEpisode = it.platform === "jianpian"`,
		`"详情（选集）"`,
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("解析页缺少 %q", want)
		}
	}
	// quality 必须同时作用于 info/detail/links，否则"切线路"只影响其中一档
	if !strings.Contains(ui, `act === "links" || act === "info" || act === "detail"`) {
		t.Error("run() 里 quality 没有覆盖三类计价端点")
	}
}

// TestUIHLSMergeWiring：混流区必须支持 HLS(TS) 分片合并，且**直连 CDN**。
func TestUIHLSMergeWiring(t *testing.T) {
	ui := string(uiHTML)
	for _, want := range []string{
		`function mergeHLS(`, `function parseM3U8(`, `function looksHLS(`,
		`function applyHLS(`, `function startMerge(`,
		`window.VL.applyHLS = applyHLS`, `window.VL.startMerge = startMerge`,
		`#EXT-X-KEY`, `#EXT-X-STREAM-INF`, `#EXT-X-BYTERANGE`, `#EXT-X-MEDIA-SEQUENCE`,
		`AES-CBC`, `crypto.subtle`, `importKey`,
		`ivForSeq`, // 无显式 IV 时按规范用媒体序号当 IV
		`"ts"`,     // 产物是 .ts（顺序拼接，不转封装）
		`下载并合并（TS）`,
		// 安全上下文提示：非 https/localhost 时 Web Crypto 不可用
		`请用 https 或 http://localhost 打开本页`,
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("解析页缺少 %q", want)
		}
	}
	// TS 分片必须直连：合并分支里不能出现代理路径
	head := strings.Index(ui, "async function mergeHLS(")
	if head < 0 {
		t.Fatal("找不到 mergeHLS")
	}
	tail := strings.Index(ui[head:], "\n  // startMerge")
	if tail < 0 {
		t.Fatal("找不到 mergeHLS 的结尾")
	}
	body := ui[head : head+tail]
	if strings.Contains(body, "/v1/proxy") {
		t.Error("mergeHLS 不该走服务端代理：分片 CDN 不在白名单里，代理会拒")
	}
	if !strings.Contains(body, "fetchBytes(") {
		t.Error("mergeHLS 应直接用 fetch 拉分片")
	}
}

// TestUISectionOrder：结果区在前、混流区在后（用户要求的顺序）。
func TestUISectionOrder(t *testing.T) {
	ui := string(uiHTML)
	iRes := strings.Index(ui, `<!-- 结果 -->`)
	iMux := strings.Index(ui, `<!-- 浏览器内混流`)
	if iRes < 0 || iMux < 0 {
		t.Fatalf("找不到区块注释：结果=%d 混流=%d", iRes, iMux)
	}
	if iRes > iMux {
		t.Error("混流区应在结果区下方")
	}
	// 搜索区在批量区下方
	iBatch := strings.Index(ui, `<!-- 批量 -->`)
	iSearch := strings.Index(ui, `<!-- 搜索 -->`)
	if iBatch < 0 || iSearch < 0 || iBatch > iSearch {
		t.Error("搜索区应在批量区下方")
	}
}

// TestUIM3U8Parsing 把混流脚本里的 m3u8 解析原样抽出来，在 node 里跑真实形状的
// 播放列表：相对地址、AES-128 密钥行、byterange、master 变体、IV 回退规则。
//
// 这层是"HLS 分片合并"最容易写错的地方（相对路径、序号→IV、byterange 续偏移），
// 而它在浏览器里出错只会表现为"合出来的文件播不了"，值得静态钉住。
func TestUIM3U8Parsing(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("环境里没有 node，跳过")
	}
	blocks := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(string(uiHTML), -1)
	mux := blocks[len(blocks)-1][1]

	var src strings.Builder
	for _, name := range []string{"function absURL(", "function parseM3U8(", "function hexToBytes(",
		"function ivForSeq("} {
		head := strings.Index(mux, name)
		if head < 0 {
			t.Fatalf("混流脚本里找不到 %s", name)
		}
		tail := strings.Index(mux[head:], "\n  }\n")
		if tail < 0 {
			t.Fatalf("找不到 %s 的结尾", name)
		}
		src.WriteString(mux[head : head+tail+len("\n  }")])
		src.WriteString("\n")
	}

	media := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXT-X-KEY:METHOD=AES-128,URI=\"/api/v2/vip/regular/secrets/33921?sign=abc\"\n" +
		"#EXTINF:10.000,\nsegment_000.ts?a=1\n" +
		"#EXTINF:10.000,\nsegment_001.ts?a=1\n" +
		"#EXT-X-BYTERANGE:1024\nsegment_002.ts?a=1\n" +
		"#EXT-X-BYTERANGE:2048@4096\nsegment_003.ts?a=1\n" +
		"#EXT-X-ENDLIST"
	master := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=800000,RESOLUTION=1080x608\n3000k/hls/mixed.m3u8\n" +
		"#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=2000000,RESOLUTION=1920x1080\n6000k/hls/mixed.m3u8"

	driver := `
const src = ` + strconv.Quote(src.String()) + `;
` + src.String() + `
const base = "https://mv.example/api/v2/vip/normal/33921/index.m3u8";
const media = parseM3U8(` + strconv.Quote(media) + `, base);
const master = parseM3U8(` + strconv.Quote(master) + `, base);
const out = {
  segs: media.segs.length,
  first: media.segs[0].url,
  key: media.key.method + " " + media.key.uri,
  keyIV: media.key.iv,
  mediaSeq: media.mediaSeq,
  r0: media.segs[2].range,
  r1: media.segs[3].range,
  variants: master.master.length,
  best: master.master.sort((a,b) => b.bandwidth - a.bandwidth)[0].url,
  iv5: Array.from(ivForSeq(5)).join(","),
  sameHost: absURL("/x/y", "https://a.example/z/index.m3u8"),
};
console.log(JSON.stringify(out));
`
	dir := t.TempDir()
	js := filepath.Join(dir, "m3u8.js")
	if err := os.WriteFile(js, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, js).CombinedOutput()
	if err != nil {
		t.Fatalf("跑 m3u8 解析失败：%v\n%s", err, raw)
	}
	var got struct {
		Segs     int
		First    string
		Key      string
		KeyIV    []byte
		MediaSeq int
		R0       struct{ Off, Len int }
		R1       struct{ Off, Len int }
		Variants int
		Best     string
		IV5      string
		SameHost string
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("node 输出不是 JSON：%v\n%s", err, raw)
	}
	if got.Segs != 4 {
		t.Errorf("分片数 = %d，应为 4", got.Segs)
	}
	if got.First != "https://mv.example/api/v2/vip/normal/33921/segment_000.ts?a=1" {
		t.Errorf("相对分片地址没有正确解析：%q", got.First)
	}
	if got.Key != "AES-128 https://mv.example/api/v2/vip/regular/secrets/33921?sign=abc" {
		t.Errorf("密钥地址（相对）没有解析：%q", got.Key)
	}
	if got.KeyIV != nil {
		t.Errorf("没有显式 IV 时应为 null（由媒体序号推），得到 %v", got.KeyIV)
	}
	if got.R0.Off != 0 || got.R0.Len != 1024 {
		t.Errorf("byterange（省略 offset）应从 0 开始：%+v", got.R0)
	}
	if got.R1.Off != 4096 || got.R1.Len != 2048 {
		t.Errorf("byterange（显式 offset）解析不对：%+v", got.R1)
	}
	if got.Variants != 2 || !strings.HasSuffix(got.Best, "6000k/hls/mixed.m3u8") {
		t.Errorf("master 变体（取最高码率）不对：%d %q", got.Variants, got.Best)
	}
	if got.IV5 != "0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,5" {
		t.Errorf("无显式 IV 时按规范用媒体序号（大端 128 位）：%s", got.IV5)
	}
	if got.SameHost != "https://a.example/x/y" {
		t.Errorf("绝对路径应相对主机解析：%q", got.SameHost)
	}
}

// linkIDStub 实现 ByIDExtractor + LinkIDChecker：模拟"取流必须指定到单集"的平台。
type linkIDStub struct{ name core.Platform }

func (s linkIDStub) Name() core.Platform  { return s.name }
func (s linkIDStub) Hosts() []string      { return []string{"linkid.test"} }
func (s linkIDStub) Match(*core.URL) bool { return false }

func (s linkIDStub) ParseID(context.Context, string) (*core.Video, error) {
	return &core.Video{
		Platform: s.name, ID: "553300", Title: "t",
		Videos: []core.Stream{{URL: "https://cdn.example/x.mp4"}},
	}, nil
}

func (s linkIDStub) Parse(context.Context, *core.URL) (*core.Video, error) {
	return nil, core.NotFound(s.name, "stub 不按链接解析")
}

func (s linkIDStub) CheckLinkID(id string) error {
	if !strings.Contains(id, "_") {
		return core.BadInput(s.name, "取直链必须指定到单集：用 id=<影片ID>_<单集ID>")
	}
	return nil
}

// TestLinksRequiresFullIDWhenPlatformSaysSo：links 会先让平台校验 ID；
// info/detail 不受这条限制（它们本来就是"先看清单"的档位）。
func TestLinksRequiresFullIDWhenPlatformSaysSo(t *testing.T) {
	env := newTestEnv(t, nil, linkIDStub{name: core.PlatformJianpian})
	h := env.handler()

	w := do(h, http.MethodGet, "/v1/links?platform=jianpian&id=553300", userHdr(env.userKey))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("裸影片 ID 取流应 400，得到 %d（%s）", w.Code, w.Body.String())
	}
	if _, msg, _ := errBody(t, w); !strings.Contains(msg, "单集") {
		t.Errorf("报错应说明必须指定单集：%s", msg)
	}
	// 未带 key 的调用会先被鉴权挡掉，这里只关心校验本身
	if w := do(h, http.MethodGet, "/v1/links?platform=jianpian&id=553300_33921", userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Errorf("影片 ID_单集 ID 应放行，得到 %d（%s）", w.Code, w.Body.String())
	}
	if w := do(h, http.MethodGet, "/v1/detail?platform=jianpian&id=553300", userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Errorf("detail 不该被这条规则限制，得到 %d", w.Code)
	}
}
