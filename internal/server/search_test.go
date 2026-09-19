package server

import (
	"context"
	"encoding/json"
	"net/http"
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
