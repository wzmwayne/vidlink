package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"vidlink/internal/account"
	"vidlink/internal/config"
	"vidlink/internal/core"
	"vidlink/internal/deps"
	"vidlink/internal/extract"
	"vidlink/internal/gate"
	"vidlink/internal/netx"
	"vidlink/internal/quota"
	"vidlink/internal/service"
)

// stubExtractor 不做任何网络请求，只为让注册表非空并返回固定结果。
type stubExtractor struct {
	name   core.Platform
	videos []core.Stream
	fail   error
}

func (s stubExtractor) Name() core.Platform  { return s.name }
func (s stubExtractor) Hosts() []string      { return []string{"stub.test"} }
func (s stubExtractor) Match(*core.URL) bool { return true }

func (s stubExtractor) Parse(context.Context, *core.URL) (*core.Video, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	v := &core.Video{
		Platform: s.name, ID: "stub", Title: "STUB",
		Author: core.Author{Name: "作者"},
		Videos: append([]core.Stream(nil), s.videos...),
	}
	core.SortStreams(v.Videos)
	return v, nil
}

func defaultStub() stubExtractor {
	return stubExtractor{
		name: core.PlatformBilibili,
		videos: []core.Stream{
			{URL: "https://cdn/1080.m4s", Height: 1080, Width: 1920, Codec: "avc1.640032",
				Quality: "1080P", QualityID: 80,
				Headers: map[string]string{"Referer": "https://www.bilibili.com/"}},
			{URL: "https://cdn/480.m4s", Height: 480, Width: 852, Codec: "avc1.64001F",
				Quality: "480P", QualityID: 32},
		},
	}
}

// blockingExtractor 阻塞在 release 上，用于制造"并发进行中"的状态。
type blockingExtractor struct {
	name    core.Platform
	release chan struct{}
}

func (b blockingExtractor) Name() core.Platform  { return b.name }
func (b blockingExtractor) Hosts() []string      { return []string{"stub.test"} }
func (b blockingExtractor) Match(*core.URL) bool { return true }

func (b blockingExtractor) Parse(context.Context, *core.URL) (*core.Video, error) {
	<-b.release
	s := defaultStub()
	return s.Parse(context.Background(), nil)
}

type testEnv struct {
	srv      *Server
	store    *account.Store
	adminKey string
	userKey  string
}

func newTestEnv(t *testing.T, mutate func(*config.Config), exts ...core.Extractor) *testEnv {
	t.Helper()
	cfg := &config.Config{
		RateLimitRPM:      0, // 默认不限流；测限流的用例自己打开
		CORSOrigins:       []string{"*"},
		Service:           service.DefaultOptions(),
		Net:               config.NetOptions{Timeout: 5 * time.Second},
		BatchMin:          5,
		BatchMax:          20,
		PerKeyConcurrency: 1,
		GlobalConcurrency: 10,
		QueueMax:          30,
		QueueWaitTimeout:  2 * time.Second,
	}
	if mutate != nil {
		mutate(cfg)
	}
	client, err := netx.New(netx.Options{Timeout: cfg.Net.Timeout})
	if err != nil {
		t.Fatalf("构造 HTTP 客户端失败: %v", err)
	}
	d := &deps.Deps{Client: client, Cookies: deps.NewCookies(),
		Params: deps.DefaultParams(), Now: time.Now}
	svc := service.New(d, extract.NewRegistryWith(exts...), cfg.Service)

	store, err := account.New(account.Options{}) // 纯内存：测试不需要落盘
	if err != nil {
		t.Fatalf("构造账本失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	adminKey, userKey := "vl_admin_test", "vl_user_test"
	if _, err := store.Create(account.Account{
		Key: adminKey, Name: "管理员", Admin: true, Quota: 1e9, Multiplier: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(account.Account{
		Key: userKey, Name: "普通用户", Quota: 100, Multiplier: 1,
	}); err != nil {
		t.Fatal(err)
	}

	g := gate.New(gate.Options{
		PerKey: cfg.PerKeyConcurrency, Global: cfg.GlobalConcurrency,
		MaxQueue: cfg.QueueMax, WaitTimeout: cfg.QueueWaitTimeout,
	})
	srv, err := New(cfg, Deps{
		Service: svc, Accounts: store, Gate: g,
		QuotaTable: quota.DefaultTable(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("构造 server 失败: %v", err)
	}
	return &testEnv{srv: srv, store: store, adminKey: adminKey, userKey: userKey}
}

func (e *testEnv) handler() http.Handler { return e.srv.Handler() }

func do(h http.Handler, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func doJSON(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func userHdr(key string) map[string]string { return map[string]string{"X-API-Key": key} }

func errBody(t *testing.T, w *httptest.ResponseRecorder) (kind, msg, reqID string) {
	t.Helper()
	var body struct {
		Error struct {
			Kind      string `json:"kind"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, w.Body.String())
	}
	return body.Error.Kind, body.Error.Message, body.Error.RequestID
}

// --- 中间件契约 ---

// TestCORSPreflightIsNotBlockedByAuth 是最重要的一条。
//
// 浏览器跨域预检按 Fetch 规范**不携带任何凭据**，因此天然过不了鉴权。
// 若鉴权跑在 CORS 之前，预检会被 403，浏览器随即放弃正式请求——
// 症状是"一开鉴权网页端就完全用不了"，而日志里只有一串看不懂的 OPTIONS 403。
func TestCORSPreflightIsNotBlockedByAuth(t *testing.T) {
	h := newTestEnv(t, nil, defaultStub()).handler()

	w := do(h, http.MethodOptions, "/v1/links", map[string]string{
		"Origin":                         "https://app.example.com",
		"Access-Control-Request-Method":  "GET",
		"Access-Control-Request-Headers": "x-api-key",
	})
	if w.Code != http.StatusNoContent {
		t.Fatalf("预检应返回 204，得到 %d：%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Allow-Origin = %q", got)
	}
	allow := w.Header().Get("Access-Control-Allow-Headers")
	for _, need := range []string{"X-API-Key", "Authorization", "Content-Type"} {
		if !strings.Contains(allow, need) {
			t.Errorf("Allow-Headers 缺少 %s（当前 %q）", need, allow)
		}
	}
	// 配额相关的响应头必须能被浏览器 JS 读到，否则前端无法展示剩余配额
	expose := w.Header().Get("Access-Control-Expose-Headers")
	for _, need := range []string{"X-Quota-Consumed", "X-Quota-Remaining", "X-Request-Id"} {
		if !strings.Contains(expose, need) {
			t.Errorf("Expose-Headers 缺少 %s（当前 %q）", need, expose)
		}
	}
}

func TestCORSOnErrorResponses(t *testing.T) {
	h := newTestEnv(t, nil, defaultStub()).handler()
	w := do(h, http.MethodGet, "/v1/links", map[string]string{"Origin": "https://a.com"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("无 Key 访问配额端点应 403，得到 %d", w.Code)
	}
	if w.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Error("403 响应缺少 Access-Control-Allow-Origin，浏览器读不到错误详情")
	}
}

func TestPublicRoutesSkipAuth(t *testing.T) {
	h := newTestEnv(t, nil, defaultStub()).handler()
	for _, p := range []string{"/v1/version", "/v1/health", "/v1/platforms", "/healthz", "/readyz"} {
		if w := do(h, http.MethodGet, p, nil); w.Code != http.StatusOK {
			t.Errorf("%s 应免鉴权且返回 200，得到 %d", p, w.Code)
		}
	}
}

func TestQuotaRoutesRequireKey(t *testing.T) {
	h := newTestEnv(t, nil, defaultStub()).handler()
	for _, p := range []string{
		"/v1/info?url=https://stub.test/v/1",
		"/v1/links?url=https://stub.test/v/1",
		"/v1/detail?url=https://stub.test/v/1",
		"/v1/usage",
	} {
		if w := do(h, http.MethodGet, p, nil); w.Code != http.StatusForbidden {
			t.Errorf("%s 无 Key 应 403，得到 %d", p, w.Code)
		}
	}
	if w := doJSON(h, http.MethodPost, "/v1/batch/links", `{"urls":[]}`, nil); w.Code != http.StatusForbidden {
		t.Errorf("batch 无 Key 应 403，得到 %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/v1/usage", userHdr("nope")); w.Code != http.StatusForbidden {
		t.Errorf("无效 Key 应 403，得到 %d", w.Code)
	}
}

func TestThreeKeyPassingStyles(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	cases := []struct {
		name string
		path string
		hdr  map[string]string
	}{
		{"X-API-Key", "/v1/usage", map[string]string{"X-API-Key": env.userKey}},
		{"Bearer", "/v1/usage", map[string]string{"Authorization": "Bearer " + env.userKey}},
		{"query", "/v1/usage?key=" + env.userKey, nil},
	}
	for _, c := range cases {
		if w := do(h, http.MethodGet, c.path, c.hdr); w.Code != http.StatusOK {
			t.Errorf("%s 方式应通过，得到 %d", c.name, w.Code)
		}
	}
}

func TestUnknownPathReturns404(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/nope"},
		{http.MethodGet, "/nope"},
		{http.MethodPost, "/v1/nope"},
		{http.MethodGet, "/api/v1/parse"}, // 旧路径已下线
		{http.MethodGet, "/v1/links/"},    // 尾斜杠是不同路径
	} {
		w := do(h, tc.method, tc.path, userHdr(env.userKey))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s 应 404，得到 %d（%s）", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

func TestMethodNotAllowedReturns405(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	w := do(h, http.MethodPost, "/v1/links", userHdr(env.userKey))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("应 405，得到 %d（%s）", w.Code, w.Body.String())
	}
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow 头应含 GET，得到 %q", allow)
	}
	if w := do(h, http.MethodGet, "/v1/batch/links", userHdr(env.userKey)); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/batch/links 应 405，得到 %d", w.Code)
	}
}

func TestRequestIDGeneratedAndEchoed(t *testing.T) {
	h := newTestEnv(t, nil, defaultStub()).handler()

	w := do(h, http.MethodGet, "/healthz", nil)
	if id := w.Header().Get(requestIDHeader); len(id) != 32 {
		t.Errorf("应自动生成 32 位 ID，得到 %q", id)
	}
	w2 := do(h, http.MethodGet, "/healthz", map[string]string{requestIDHeader: "trace-abc"})
	if got := w2.Header().Get(requestIDHeader); got != "trace-abc" {
		t.Errorf("应沿用客户端 ID，得到 %q", got)
	}
}

func TestRequestIDRejectsInjection(t *testing.T) {
	h := newTestEnv(t, nil, defaultStub()).handler()
	for _, bad := range []string{
		"abc\r\nX-Evil: 1", "含中文",
		strings.Repeat("a", maxRequestIDLen+1), "has space",
	} {
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		r.Header[requestIDHeader] = []string{bad}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		got := w.Header().Get(requestIDHeader)
		if got == bad || got == "" || strings.ContainsAny(got, "\r\n") {
			t.Errorf("危险 ID %q 未被安全处理，得到 %q", bad, got)
		}
	}
}

func TestErrorBodyCarriesRequestID(t *testing.T) {
	h := newTestEnv(t, nil, defaultStub()).handler()
	w := do(h, http.MethodGet, "/v1/nope", map[string]string{requestIDHeader: "trace-xyz"})
	if _, _, id := errBody(t, w); id != "trace-xyz" {
		t.Fatalf("错误体 request_id = %q, want trace-xyz", id)
	}
}

func TestRateLimitHeaders(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.RateLimitRPM = 6000 })
	w := do(env.handler(), http.MethodGet, "/healthz", nil)
	if w.Header().Get("X-RateLimit-Limit") == "" || w.Header().Get("X-RateLimit-Remaining") == "" {
		t.Error("缺少限流响应头")
	}
}

func TestReadyzReflectsRegistry(t *testing.T) {
	if w := do(newTestEnv(t, nil).handler(), http.MethodGet, "/readyz", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("无提取器时应 503，得到 %d", w.Code)
	}
	if w := do(newTestEnv(t, nil, defaultStub()).handler(), http.MethodGet, "/readyz", nil); w.Code != http.StatusOK {
		t.Errorf("有提取器时应 200，得到 %d", w.Code)
	}
}

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"/v1/links", "/v1/links", true},
		{"/v1/links", "/v1/links/", false},
		{"/v1/admin/accounts/{key}", "/v1/admin/accounts/vl_x", true},
		{"/v1/admin/accounts/{key}", "/v1/admin/accounts", false},
		{"/v1/admin/accounts/{key}", "/v1/admin/accounts//x", false},
		{"/healthz", "/healthz", true},
		{"/healthz", "/healthz/x", false},
	}
	for _, c := range cases {
		if got := matchPath(c.pattern, c.path); got != c.want {
			t.Errorf("matchPath(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

// --- 配额档位边界 ---

// TestLinksReturnsOnlyURL：links 档"只有直链"，不含任何内容元信息。
func TestLinksReturnsOnlyURL(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	w := do(env.handler(), http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["url"] == nil {
		t.Fatal("links 必须返回 url")
	}
	for _, f := range []string{"title", "author", "stats", "cover", "platform", "id"} {
		if _, ok := m[f]; ok {
			t.Errorf("links 档不应包含元信息 %q", f)
		}
	}
}

// TestInfoReturnsQualitiesWithoutURLs：info 给档位列表但不给直链。
func TestInfoReturnsQualitiesWithoutURLs(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	w := do(env.handler(), http.MethodGet, "/v1/info?url=https://stub.test/v/1", userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", w.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["title"] == nil {
		t.Error("info 应含标题")
	}
	if m["videos"] != nil {
		t.Error("info 不应含 videos")
	}
	if m["qualities"] == nil {
		t.Error("info 应含 qualities，否则客户无从选择分辨率")
	}
	if strings.Contains(w.Body.String(), "https://cdn/") {
		t.Error("info 档泄露了直链")
	}
}

func TestConsumptionDeductsQuota(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	before, _ := env.store.Get(env.userKey)

	w := do(env.handler(), http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", w.Code)
	}
	after, _ := env.store.Get(env.userKey)

	// B 站 links 的系数是 1.0
	if got := before.Quota - after.Quota; got != 1.0 {
		t.Fatalf("应消耗 1.0 配额，实耗 %v", got)
	}
	if after.Calls != 1 {
		t.Fatalf("应记 1 次调用，得到 %d", after.Calls)
	}
	if w.Header().Get("X-Quota-Consumed") != "1" {
		t.Errorf("X-Quota-Consumed = %q, want 1", w.Header().Get("X-Quota-Consumed"))
	}
	if w.Header().Get("X-Quota-Remaining") == "" {
		t.Error("缺少 X-Quota-Remaining")
	}
}

// TestFailureDoesNotConsume：上游失败不消耗配额。
func TestFailureDoesNotConsume(t *testing.T) {
	bad := stubExtractor{name: core.PlatformBilibili,
		fail: core.Errf(core.KindUpstream, core.PlatformBilibili, "parse", "上游炸了")}
	env := newTestEnv(t, nil, bad)

	before, _ := env.store.Get(env.userKey)
	w := do(env.handler(), http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey))
	if w.Code == http.StatusOK {
		t.Fatal("上游失败时不应返回 200")
	}
	after, _ := env.store.Get(env.userKey)
	if after.Quota != before.Quota {
		t.Fatalf("失败不应消耗配额：before=%v after=%v", before.Quota, after.Quota)
	}
	if after.Calls != 0 {
		t.Fatalf("失败不应计次，得到 %d", after.Calls)
	}
}

// TestQuotaExhaustedIsRejected：配额用尽在**解析之前**就被挡住。
func TestQuotaExhaustedIsRejected(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	zero := 0.0
	if _, err := env.store.Update(env.userKey, account.Patch{Quota: &zero}); err != nil {
		t.Fatal(err)
	}
	w := do(env.handler(), http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey))
	// 用 429 而不是 402：402 字面就是"需要付款"，会把用量上限说成商业交易
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("配额用尽应 429，得到 %d（%s）", w.Code, w.Body.String())
	}
	kind, msg, _ := errBody(t, w)
	if kind != "quota_exhausted" {
		t.Errorf("kind = %q, want quota_exhausted", kind)
	}
	for _, bad := range []string{"付款", "充值", "购买", "付费", "余额"} {
		if strings.Contains(msg, bad) {
			t.Errorf("错误信息不应出现交易类措辞 %q：%s", bad, msg)
		}
	}
}

// TestAccountMultiplierApplies：账号系数决定消耗折算比例（促销/友情的实现方式）。
func TestAccountMultiplierApplies(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	half := 0.5
	if _, err := env.store.Update(env.userKey, account.Patch{Multiplier: &half}); err != nil {
		t.Fatal(err)
	}
	before, _ := env.store.Get(env.userKey)
	w := do(env.handler(), http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", w.Code)
	}
	after, _ := env.store.Get(env.userKey)
	if got := before.Quota - after.Quota; got != 0.5 {
		t.Fatalf("五折系数应消耗 0.5，实耗 %v", got)
	}
}

// TestZeroQuotaAccountIsUnmeteredButCounted：系数为 0 不消耗配额，但仍计次。
func TestZeroQuotaAccountIsUnmeteredButCounted(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	zero := 0.0
	if _, err := env.store.Update(env.userKey, account.Patch{Multiplier: &zero}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		w := do(env.handler(), http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey))
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次应 200，得到 %d", i+1, w.Code)
		}
	}
	after, _ := env.store.Get(env.userKey)
	if after.Quota != 100 {
		t.Fatalf("零系数不应消耗配额，得到 %v", after.Quota)
	}
	if after.Calls != 3 {
		t.Fatalf("零系数也要计次，得到 %d", after.Calls)
	}
}

func TestQualitySelection(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	var m map[string]any
	w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey))
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if !strings.Contains(m["url"].(string), "1080") {
		t.Errorf("不指定分辨率应返回最高，得到 %v", m["url"])
	}

	var m2 map[string]any
	w2 := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1&quality=480", userHdr(env.userKey))
	_ = json.Unmarshal(w2.Body.Bytes(), &m2)
	if !strings.Contains(m2["url"].(string), "480") {
		t.Errorf("quality=480 应返回 480P，得到 %v", m2["url"])
	}

	// 不存在的分辨率：报错**且不消耗配额**
	before, _ := env.store.Get(env.userKey)
	w3 := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1&quality=2160", userHdr(env.userKey))
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("不存在的分辨率应 400，得到 %d", w3.Code)
	}
	after, _ := env.store.Get(env.userKey)
	if after.Quota != before.Quota {
		t.Error("分辨率不存在时不应消耗配额")
	}
}

// --- 并发闸门 ---

// TestPerKeyConcurrencyIsEnforced：同一个 Key 的第二个请求返回 429。
func TestPerKeyConcurrencyIsEnforced(t *testing.T) {
	block := make(chan struct{})
	env := newTestEnv(t, nil, blockingExtractor{name: core.PlatformBilibili, release: block})
	h := env.handler()

	done := make(chan struct{})
	go func() {
		do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey))
		close(done)
	}()
	time.Sleep(80 * time.Millisecond) // 等第一个请求进入解析

	w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/2", userHdr(env.userKey))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("同 Key 并发应 429，得到 %d（%s）", w.Code, w.Body.String())
	}
	if kind, _, _ := errBody(t, w); kind != "concurrency_limited" {
		t.Errorf("kind = %q, want concurrency_limited", kind)
	}
	// 不同的 Key 不受影响
	done2 := make(chan struct{})
	go func() {
		do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/3", userHdr(env.adminKey))
		close(done2)
	}()
	time.Sleep(80 * time.Millisecond)

	close(block)
	<-done
	<-done2
}

// TestGlobalGateOverloadIsUnavailable：全局解析槽位满、排队也满时返回 503。
//
// 这一条锁的是"过载不是故障"：如果把闸门拒绝映射成 500，
// 监控会把它算成服务端错误，运维会去查一个根本没坏的服务。
func TestGlobalGateOverloadIsUnavailable(t *testing.T) {
	block := make(chan struct{})
	// MaxQueue=0：全局槽位被占时立即拒绝，不需要等待。
	env := newTestEnv(t, func(c *config.Config) {
		c.GlobalConcurrency = 1
		c.QueueMax = 0
		c.PerKeyConcurrency = 10 // 让按 Key 的闸门不要先拦住第二个请求
	}, blockingExtractor{name: core.PlatformBilibili, release: block})
	h := env.handler()

	done := make(chan struct{})
	go func() {
		do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey))
		close(done)
	}()
	time.Sleep(80 * time.Millisecond) // 等它占住唯一的全局槽位

	w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/2", userHdr(env.adminKey))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("过载应 503，得到 %d（%s）", w.Code, w.Body.String())
	}
	if kind, _, _ := errBody(t, w); kind != "unavailable" {
		t.Errorf("kind = %q, want unavailable", kind)
	}
	close(block)
	<-done
}

// TestUsageIsNotBlockedByParseGate：解析进行中查用量不应被按 Key 闸门误伤。
//
// 闸门的作用是"一个账号同时只解析一条"，读接口不占解析槽位，也就没有理由被它拦住。
func TestUsageIsNotBlockedByParseGate(t *testing.T) {
	block := make(chan struct{})
	env := newTestEnv(t, nil, blockingExtractor{name: core.PlatformBilibili, release: block})
	h := env.handler()

	done := make(chan struct{})
	go func() {
		do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey))
		close(done)
	}()
	time.Sleep(80 * time.Millisecond) // 让解析占住该 Key 的槽位

	// 同一个 Key：用量查询必须是 200
	if w := do(h, http.MethodGet, "/v1/usage", userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Errorf("解析中查用量应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	// 同一个 Key：再起一次解析仍然应被拒（闸门本身没坏）
	if w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/2", userHdr(env.userKey)); w.Code != http.StatusTooManyRequests {
		t.Errorf("同 Key 第二个解析应 429，得到 %d", w.Code)
	}

	close(block)
	<-done
}

// --- 批量 ---

func batchBody(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = `"https://stub.test/v/` + string(rune('a'+i%26)) + `"`
	}
	return `{"urls":[` + strings.Join(items, ",") + `]}`
}

func TestBatchLimits(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	// 少于 5 条：拒绝，并提示改用单条接口
	w := doJSON(h, http.MethodPost, "/v1/batch/links", batchBody(4), userHdr(env.userKey))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("4 条应 400，得到 %d（%s）", w.Code, w.Body.String())
	}
	if _, msg, _ := errBody(t, w); !strings.Contains(msg, "/v1/links") {
		t.Errorf("应提示改用单条接口，得到 %q", msg)
	}
	// 多于 20 条：拒绝
	if w := doJSON(h, http.MethodPost, "/v1/batch/links", batchBody(21), userHdr(env.userKey)); w.Code != http.StatusBadRequest {
		t.Fatalf("21 条应 400，得到 %d", w.Code)
	}
	// 5 条：通过，且按 0.75/条 计
	w = doJSON(h, http.MethodPost, "/v1/batch/links", batchBody(5), userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("5 条应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	var resp batchLinksResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Success != 5 {
		t.Fatalf("应成功 5 条，得到 %d", resp.Success)
	}
	if resp.Cost != 3.75 {
		t.Fatalf("5 条应消耗 3.75，得到 %v", resp.Cost)
	}
	after, _ := env.store.Get(env.userKey)
	if after.Quota != 96.25 {
		t.Fatalf("配额应为 100-3.75=96.25，得到 %v", after.Quota)
	}
}

// TestBatchChargesOnlySuccess：批量里失败的条目不计入消耗。
func TestBatchChargesOnlySuccess(t *testing.T) {
	bad := stubExtractor{name: core.PlatformBilibili,
		fail: core.Errf(core.KindNotFound, core.PlatformBilibili, "parse", "不存在")}
	env := newTestEnv(t, nil, bad)

	before, _ := env.store.Get(env.userKey)
	w := doJSON(env.handler(), http.MethodPost, "/v1/batch/links", batchBody(5), userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", w.Code)
	}
	after, _ := env.store.Get(env.userKey)
	if after.Quota != before.Quota {
		t.Fatalf("全部失败时不应消耗配额：before=%v after=%v", before.Quota, after.Quota)
	}
	var resp batchLinksResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Failed != 5 || resp.Success != 0 {
		t.Fatalf("应 5 条失败，得到 success=%d failed=%d", resp.Success, resp.Failed)
	}
}

// --- 管理面 ---

func TestAdminRequiresAdminFlag(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	if w := do(h, http.MethodGet, "/v1/admin/accounts", userHdr(env.userKey)); w.Code != http.StatusForbidden {
		t.Fatalf("普通账号访问管理端点应 403，得到 %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/v1/admin/accounts", userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("管理员应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
}

// TestAdminCanAdjustQuotaAndMultiplier 是本次需求的核心：
// 管理员能调整**其他账号**的配额与折扣系数（促销、友情）。
func TestAdminCanAdjustQuotaAndMultiplier(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	// 增量调整配额
	w := doJSON(h, http.MethodPatch, "/v1/admin/accounts/"+env.userKey,
		`{"add_quota":500}`, userHdr(env.adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("调整配额应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	if got, _ := env.store.Get(env.userKey); got.Quota != 600 {
		t.Fatalf("配额应 100+500=600，得到 %v", got.Quota)
	}

	// 设折扣系数（促销五折）
	w = doJSON(h, http.MethodPatch, "/v1/admin/accounts/"+env.userKey,
		`{"multiplier":0.5}`, userHdr(env.adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("调整系数应 200，得到 %d", w.Code)
	}
	if got, _ := env.store.Get(env.userKey); got.Multiplier != 0.5 {
		t.Fatalf("系数应为 0.5，得到 %v", got.Multiplier)
	}

	// 直接设配额
	w = doJSON(h, http.MethodPatch, "/v1/admin/accounts/"+env.userKey,
		`{"quota":42}`, userHdr(env.adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("设配额应 200，得到 %d", w.Code)
	}
	if got, _ := env.store.Get(env.userKey); got.Quota != 42 {
		t.Fatalf("配额应为 42，得到 %v", got.Quota)
	}
}

// TestAdminCanPromoteToAdmin：管理员能调整别人的管理员标记。
func TestAdminCanPromoteToAdmin(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	w := doJSON(h, http.MethodPatch, "/v1/admin/accounts/"+env.userKey,
		`{"admin":true}`, userHdr(env.adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("提升管理员应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	if w := do(h, http.MethodGet, "/v1/admin/accounts", userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Fatalf("提升后应能访问管理端点，得到 %d", w.Code)
	}
}

func TestAdminCreateReturnsPlainKeyOnce(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	w := doJSON(h, http.MethodPost, "/v1/admin/accounts", `{"name":"新账号"}`, userHdr(env.adminKey))
	if w.Code != http.StatusCreated {
		t.Fatalf("创建应 201，得到 %d（%s）", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	key, _ := resp["key"].(string)
	if key == "" {
		t.Fatal("创建时应返回明文 Key（只此一次）")
	}
	// 默认倍率必须是 1（标准），而不是 0——0 是"不扣配额"这个有含义的取值
	acct := resp["account"].(map[string]any)
	if acct["multiplier"] != 1.0 {
		t.Fatalf("默认倍率应为 1，得到 %v", acct["multiplier"])
	}
	// 列表、详情、修改三处都只给掩码
	for _, probe := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/v1/admin/accounts", ""},
		{http.MethodGet, "/v1/admin/accounts/" + key, ""},
		{http.MethodPatch, "/v1/admin/accounts/" + key, `{"note":"改一下"}`},
	} {
		var w2 *httptest.ResponseRecorder
		if probe.body == "" {
			w2 = do(h, probe.method, probe.path, userHdr(env.adminKey))
		} else {
			w2 = doJSON(h, probe.method, probe.path, probe.body, userHdr(env.adminKey))
		}
		if strings.Contains(w2.Body.String(), key) {
			t.Errorf("%s %s 泄露了明文 Key", probe.method, probe.path)
		}
	}
}

func TestAdminCannotDeleteSelf(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	w := do(env.handler(), http.MethodDelete, "/v1/admin/accounts/"+env.adminKey, userHdr(env.adminKey))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("删除自己应被拒绝，得到 %d", w.Code)
	}
	if _, ok := env.store.Get(env.adminKey); !ok {
		t.Fatal("管理员账号不应被删除")
	}
}

// TestUsageEndpointSelfDescribesAndHidesKey：用量端点自解释且不回显 Key。
func TestUsageEndpointSelfDescribesAndHidesKey(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	w := do(env.handler(), http.MethodGet, "/v1/usage", userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", w.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"quota", "unit", "multiplier", "rates", "used", "calls"} {
		if m[f] == nil {
			t.Errorf("usage 应包含 %q", f)
		}
	}
	if strings.Contains(w.Body.String(), env.userKey) {
		t.Error("usage 不应回显明文 Key")
	}
	// 措辞上不得出现商业交易字样
	body := w.Body.String()
	for _, bad := range []string{"付款", "充值", "购买", "付费", "价格", "金额"} {
		if strings.Contains(body, bad) {
			t.Errorf("响应里出现了交易类措辞 %q", bad)
		}
	}
}

func TestVersionEndpoint(t *testing.T) {
	w := do(newTestEnv(t, nil, defaultStub()).handler(), http.MethodGet, "/v1/version", nil)
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["api_version"] != apiVersion {
		t.Errorf("api_version = %v, want %v", m["api_version"], apiVersion)
	}
}

// --- 免校验模式（VL_EASE=true）---

// newEaseEnv 构造一个免校验模式的服务端：没有账本、没有账号、没有闸门键。
func newEaseEnv(t *testing.T, mutate func(*config.Config), exts ...core.Extractor) *testEnv {
	t.Helper()
	cfg := &config.Config{
		Ease:              true,
		WebUI:             true, // 与 config.Load 的默认值一致：ease 下默认开
		RateLimitRPM:      1,    // 故意给一个"会限流"的值，验证模式本身把它按住了
		CORSOrigins:       []string{"*"},
		Service:           service.DefaultOptions(),
		Net:               config.NetOptions{Timeout: 5 * time.Second},
		BatchMin:          5,
		BatchMax:          20,
		PerKeyConcurrency: 1,
		GlobalConcurrency: 10,
		QueueMax:          30,
		QueueWaitTimeout:  2 * time.Second,
	}
	if mutate != nil {
		mutate(cfg)
	}
	client, err := netx.New(netx.Options{Timeout: cfg.Net.Timeout})
	if err != nil {
		t.Fatalf("构造 HTTP 客户端失败: %v", err)
	}
	d := &deps.Deps{Client: client, Cookies: deps.NewCookies(),
		Params: deps.DefaultParams(), Now: time.Now}
	svc := service.New(d, extract.NewRegistryWith(exts...), cfg.Service)

	// 关键：账户依赖刻意传 nil——免校验模式不得依赖任何账本。
	srv, err := New(cfg, Deps{
		Service: svc, Accounts: nil,
		Gate:       gate.New(gate.Options{PerKey: 1, Global: 10, MaxQueue: 30, WaitTimeout: 2 * time.Second}),
		QuotaTable: quota.DefaultTable(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("构造 server 失败: %v", err)
	}
	return &testEnv{srv: srv}
}

// TestEaseModeNeedsNoKey：不带任何凭据也能解析，且不回写配额头。
func TestEaseModeNeedsNoKey(t *testing.T) {
	env := newEaseEnv(t, nil, defaultStub())
	w := do(env.handler(), http.MethodGet, "/v1/links?url=https://stub.test/v/1", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("免校验模式应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Quota-Consumed"); got != "" {
		t.Errorf("免校验模式不应回写 X-Quota-Consumed，得到 %q", got)
	}
	if got := w.Header().Get("X-Quota-Remaining"); got != "" {
		t.Errorf("免校验模式不应回写 X-Quota-Remaining，得到 %q", got)
	}
	// 响应体是正常的 links 结构
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["url"] == "" || body["url"] == nil {
		t.Errorf("应返回直链，得到 %v", body)
	}
}

// TestEaseModeHidesAccountEndpoints：账户相关端点全部 404（不是 403）。
func TestEaseModeHidesAccountEndpoints(t *testing.T) {
	env := newEaseEnv(t, nil, defaultStub())
	h := env.handler()

	for _, path := range []string{
		"/v1/usage",
		"/v1/admin/accounts",
		"/v1/admin/accounts/vl_x",
		"/v1/admin/stats",
		"/v1/admin/quota",
	} {
		w := do(h, http.MethodGet, path, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s 应 404（该端点不存在），得到 %d（%s）", path, w.Code, w.Body.String())
		}
	}
	// 管理面写接口同样不存在
	if w := doJSON(h, http.MethodPost, "/v1/admin/accounts", `{"name":"x"}`, nil); w.Code != http.StatusNotFound {
		t.Errorf("POST /v1/admin/accounts 应 404，得到 %d", w.Code)
	}
}

// TestEaseModeReportsModeWithoutRates：/v1/health 与 /v1/platforms 明确报告模式。
func TestEaseModeReportsModeWithoutRates(t *testing.T) {
	env := newEaseEnv(t, nil, defaultStub())
	h := env.handler()

	var health map[string]any
	w := do(h, http.MethodGet, "/v1/health", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["mode"] != "ease" {
		t.Errorf("/v1/health mode = %v，想要 ease", health["mode"])
	}

	var pf map[string]any
	w = do(h, http.MethodGet, "/v1/platforms", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &pf); err != nil {
		t.Fatal(err)
	}
	if pf["mode"] != "ease" {
		t.Errorf("/v1/platforms mode = %v，想要 ease", pf["mode"])
	}
	if _, has := pf["unit"]; has {
		t.Error("免校验模式不应返回 unit（不存在配额这回事）")
	}
	first := pf["platforms"].([]any)[0].(map[string]any)
	if _, has := first["rates"]; has {
		t.Error("免校验模式不应返回 rates（不存在计量）")
	}
}

// TestEaseModeDoesNotSerializeRequests：没有按 Key 闸门，第二个并发解析不被拒。
func TestEaseModeDoesNotSerializeRequests(t *testing.T) {
	block := make(chan struct{})
	env := newEaseEnv(t, nil, blockingExtractor{name: core.PlatformBilibili, release: block})
	h := env.handler()

	first := make(chan int, 1)
	go func() {
		first <- do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", nil).Code
	}()
	time.Sleep(80 * time.Millisecond) // 让第一个请求占住解析槽位

	second := make(chan int, 1)
	go func() {
		second <- do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/2", nil).Code
	}()

	// 第二个请求必须也在"进行中"，而不是已经被 429 拒掉
	select {
	case code := <-second:
		t.Fatalf("免校验模式不应拒绝并发请求，却立刻返回了 %d", code)
	case <-time.After(150 * time.Millisecond):
	}

	close(block)
	if code := <-first; code != http.StatusOK {
		t.Errorf("第一个请求应 200，得到 %d", code)
	}
	if code := <-second; code != http.StatusOK {
		t.Errorf("第二个请求应 200，得到 %d", code)
	}
}

// TestEaseModeIgnoresRateLimit：按 IP 限流在免校验模式下被按住（哪怕配置给了值）。
func TestEaseModeIgnoresRateLimit(t *testing.T) {
	env := newEaseEnv(t, func(c *config.Config) { c.RateLimitRPM = 1 }, defaultStub())
	h := env.handler()
	for i := 0; i < 8; i++ {
		w := do(h, http.MethodGet, "/v1/platforms", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求应 200（不应限流），得到 %d", i+1, w.Code)
		}
	}
}

// TestEaseModeBatchLinksWithoutKey：批量在免校验模式可用，cost 恒为 0。
func TestEaseModeBatchLinksWithoutKey(t *testing.T) {
	env := newEaseEnv(t, nil, defaultStub())
	w := doJSON(env.handler(), http.MethodPost, "/v1/batch/links", batchBody(5), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("免校验模式批量应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["success"] != float64(5) {
		t.Errorf("success = %v，想要 5", resp["success"])
	}
	if resp["cost"] != float64(0) {
		t.Errorf("免校验模式 cost 应为 0，得到 %v", resp["cost"])
	}
}

// TestEaseModeRootServesUI：免校验模式下根路径必须是那个图形化页面。
func TestEaseModeRootServesUI(t *testing.T) {
	env := newEaseEnv(t, nil, defaultStub())
	w := do(env.handler(), http.MethodGet, "/", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("根路径应 200，得到 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q，想要 text/html", ct)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("缺少 nosniff，得到 %q", got)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="url"`,        // 链接输入
		`id="quality"`,    // 清晰度
		`id="platform"`,   // 平台下拉
		`id="batch"`,      // 批量输入
		`data-act="info"`, // 三个解析动作
		`data-act="links"`,
		`data-act="detail"`,
		`data-act="batch"`,
		`/v1/health`, // 它会去调这些接口
		`/v1/platforms`,
		`/v1/batch/links`,
		`X-Quota`, // 页面显式说明本模式不计量
		// 浏览器内混流：流式下载（OPFS）→ 片段级重排 → 保存到本地
		`id="muxStart"`,
		`id="muxVideo"`,
		`id="muxSave"`,
		`navigator.storage.getDirectory`,
		`canPlayType`, // 无 H.264 解码器的浏览器要给出解释，而不是静默失败
		`代理下载`,        // 视频轨与音频轨都要有代理入口（音频同样受防盗链限制）
		`前端混合`,        // 下载命名里的四种类型
		`原生混合`,
		`+ "VL" +`,      // 命名拼接：标题 + "VL" + 类型 + 清晰度/码率
		`id="apikey"`,   // 账户模式：页面上填 Key
		`id="usagesec"`, // 账户模式：用量与账户信息
		`/v1/usage`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("页面缺少 %q", want)
		}
	}
	// 页面必须自包含：不能有外链脚本/样式/字体，否则离线与内网环境会残缺。
	//
	// 这里查的是**资源引用**（src/link/@import/url(...)），不是裸子串——
	// 页面注释里出现 "mcdn.bilivideo.cn" 这类域名是在讲 CDN 行为，
	// 用 "cdn." 做子串匹配会把这种说明误判成外链。
	low := strings.ToLower(body)
	for _, forbidden := range []string{
		"<script src", "<link ", "@import", "url(http", `src="http`, "src='http",
		`href="http`, "href='http", "googleapis", "unpkg", "jsdelivr",
	} {
		if strings.Contains(low, forbidden) {
			t.Errorf("页面引用了外部资源: %q", forbidden)
		}
	}
	// 上限设为 120KB：混流器（自己实现的 fMP4 重排）占了大头，
	// 但它换掉的是"外链 mp4box.js / 25MB ffmpeg.wasm"这条路。
	if len(body) > 120*1024 {
		t.Errorf("页面过大（%d 字节），内嵌资源应保持精简", len(body))
	}
}

// TestEaseModeRootUIIsHeadSafe：HEAD 只回头不回体。
func TestEaseModeRootUIIsHeadSafe(t *testing.T) {
	env := newEaseEnv(t, nil, defaultStub())
	w := do(env.handler(), http.MethodHead, "/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD / 应 200，得到 %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD 不应有响应体，得到 %d 字节", w.Body.Len())
	}
}

// TestAccountModeRootStaysText：账户模式下根路径仍是纯文本导航页，且需要 Key。
//
// 两件事一起锁：
//   - 不给网页（账户模式常部署在公网，界面要凭据才有意义，
//     而"Key 从哪来"没有干净的答案）；
//   - 不带 Key 访问它同样 403 —— 根路径不在公开端点清单里，
//     这条与"除公开端点外都要鉴权"保持一致。
func TestAccountModeRootStaysText(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	if w := do(h, http.MethodGet, "/", nil); w.Code != http.StatusForbidden {
		t.Errorf("账户模式不带 Key 访问 / 应 403，得到 %d", w.Code)
	}

	w := do(h, http.MethodGet, "/", userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("带 Key 访问 / 应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("账户模式根路径 Content-Type = %q，想要 text/plain", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/v1/admin/") {
		t.Error("账户模式的导航页应提到管理端点")
	}
	if strings.Contains(body, "<html") {
		t.Error("账户模式的根路径不应返回网页")
	}
}

// TestSchemeLessURLIsAccepted：用户只贴域名+路径（没有 http://）也要能解析。
//
// 从分享面板、聊天窗口、浏览器地址栏复制出来的链接经常没有 scheme。
// 这类输入以前会被当成"平台内 ID"，路由失败后回一句"不支持的链接"，
// 用户看到的现象就是"识别不了"。
func TestSchemeLessURLIsAccepted(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	for _, in := range []string{
		"stub.test/v/1",
		"www.stub.test/v/1",
		"https://stub.test/v/1",
		"看看这个stub.test/v/1",
	} {
		w := do(h, http.MethodGet, "/v1/links?url="+url.QueryEscape(in), userHdr(env.userKey))
		if w.Code != http.StatusOK {
			t.Errorf("%q 应 200，得到 %d（%s）", in, w.Code, w.Body.String())
		}
	}
	// 裸 ID 不能被误判成域名：它应走"识别不出链接"的分支，
	// 并提示改用 platform + id（那条路是通的，见下一个断言）。
	w := do(h, http.MethodGet, "/v1/links?url=BV1294y1Y7tU", userHdr(env.userKey))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("裸 ID 经 url= 应 400，得到 %d（%s）", w.Code, w.Body.String())
	}
	kind, msg, _ := errBody(t, w)
	if kind != "bad_input" {
		t.Errorf("kind = %q，想要 bad_input", kind)
	}
	if !strings.Contains(msg, "平台") {
		t.Errorf("错误信息应提示改用 platform + id，得到 %q", msg)
	}

	// 按 ID 解析的正路
	if w := do(h, http.MethodGet,
		"/v1/links?platform=bilibili&id=BV1294y1Y7tU", userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Errorf("platform + id 应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
}

// TestProxyHostAllowList：白名单留空 = 不限制；填了则按后缀收紧。
func TestProxyHostAllowList(t *testing.T) {
	open := newTestEnv(t, nil, defaultStub())
	for _, host := range []string{"evil.example.com", "cdn.bilivideo.com", "127.0.0.1"} {
		if !open.srv.proxyHostAllowed(host) {
			t.Errorf("白名单留空应放行 %s", host)
		}
	}

	limited := newTestEnv(t, func(c *config.Config) {
		c.ProxySrv.Enabled = true
		c.ProxySrv.AllowHosts = []string{"bilivideo.com", "douyinvod.com"}
	}, defaultStub())
	cases := map[string]bool{
		"cdn.bilivideo.com":     true,
		"upos-sz.bilivideo.com": true,
		"douyinvod.com":         true,
		"evil-bilivideo.com":    false, // 后缀匹配不能用 Contains
		"bilivideo.com.evil.cn": false,
		"example.com":           false,
		"":                      false,
	}
	for host, want := range cases {
		if got := limited.srv.proxyHostAllowed(host); got != want {
			t.Errorf("proxyHostAllowed(%q) = %v，想要 %v", host, got, want)
		}
	}
}

// TestEaseModeProxyEnabledByDefault：免校验模式下 /v1/proxy 默认就挂载。
func TestEaseModeProxyEnabledByDefault(t *testing.T) {
	env := newEaseEnv(t, func(c *config.Config) {
		c.ProxySrv.Enabled = true // 等价于 config.Load 在 ease 下的默认值
	}, defaultStub())
	// 缺 url 参数 → 400（说明路由存在）；若路由不存在会是 404
	w := do(env.handler(), http.MethodGet, "/v1/proxy", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("/v1/proxy 应存在（400 缺参数），得到 %d（%s）", w.Code, w.Body.String())
	}
	// 白名单留空时不拦任何域名 —— 判定逻辑单独测（TestProxyHostAllowList），
	// 这里只确认配置面上确实是"空"的，不发真实上游请求（单测不该依赖网络）。
	if got := env.srv.cfg.ProxySrv.AllowHosts; len(got) != 0 {
		t.Fatalf("白名单应为空，得到 %v", got)
	}
}

// TestProxySetsDownloadFilename：/v1/proxy 能把"直链"变成一个名字正确的下载。
//
// 没有它时浏览器只能按 URL 最后一段命名，实测就是下载到一个叫
// `proxy`、没有后缀的文件——内容对，但用户拿到手完全不知道是什么。
func TestProxySetsDownloadFilename(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "4")
		_, _ = w.Write([]byte("data"))
	}))
	defer upstream.Close()

	env := newTestEnv(t, func(c *config.Config) { c.ProxySrv.Enabled = true }, defaultStub())
	h := env.handler()

	name := "【官方MV】Never Gonna Give You UpVL视频1080P.mp4"
	// 账户模式下代理要鉴权，带上普通用户的 Key
	w := do(h, http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL)+
		"&filename="+url.QueryEscape(name)+"&type=video/mp4", userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("代理应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("Content-Disposition = %q，想要 attachment", cd)
	}
	// 中文名按 RFC 2231 编码；解析回来必须与请求的一致
	_, params, err := mime.ParseMediaType(cd)
	if err != nil {
		t.Fatalf("Content-Disposition 不合法: %v (%q)", err, cd)
	}
	if params["filename"] != name {
		t.Errorf("filename = %q，想要 %q", params["filename"], name)
	}
	if ct := w.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("Content-Type = %q，想要覆盖成 video/mp4", ct)
	}
}

// TestProxyFilenameIsSanitized：文件名来自查询参数，必须消毒。
func TestProxyFilenameIsSanitized(t *testing.T) {
	cases := map[string]string{
		`a/b/c.mp4`:          "abc.mp4",        // 路径分隔符：防目录穿越
		`..`:                 "",               // 纯点号：文件系统里有特殊含义
		"a\r\nX-Evil: 1.mp4": "aX-Evil: 1.mp4", // 控制字符：防响应头注入
		`a"b.mp4`:            "ab.mp4",         // 引号：防闭合 Content-Disposition
		``:                   "",               // 空：调用方据此不设头
		`  正常 名字 .mp4  `:     "正常 名字 .mp4",     // 中文与空格要保留
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q，想要 %q", in, got, want)
		}
	}
	// 超长要截断（按 rune 而不是字节，避免把中文截成半个字符）
	long := strings.Repeat("名", 300)
	if got := sanitizeFilename(long); len([]rune(got)) != 120 {
		t.Errorf("超长文件名截断后 rune 数 = %d，想要 120", len([]rune(got)))
	}
}

// TestProxyContentTypeWhitelist：type 参数不能被用来让代理回 text/html。
func TestProxyContentTypeWhitelist(t *testing.T) {
	allow := []string{"video/mp4", "audio/mp4", "application/octet-stream", "VIDEO/MP4", "video/mp4; charset=x"}
	deny := []string{"text/html", "application/javascript", "image/svg+xml", "", "  "}
	for _, v := range allow {
		if got := safeContentType(v); got == "" {
			t.Errorf("safeContentType(%q) 应放行", v)
		}
	}
	for _, v := range deny {
		if got := safeContentType(v); got != "" {
			t.Errorf("safeContentType(%q) = %q，应拒绝", v, got)
		}
	}
}

// TestUIScriptsShareAllHelpers：页面里两个 <script> 是各自独立的 IIFE，
// 第二个（混流器）只能用第一个显式挂在 window.VL 上的东西。
//
// 这条测试来自两次真实踩坑：dlName 与 ensureTitle 都曾在混流脚本里
// 直接调用而没被导出，运行到那一步才报 "xxx is not defined"——
// 而"运行到那一步"意味着用户已经等完了下载。静态检查能在构建期挡住它。
func TestUIScriptsShareAllHelpers(t *testing.T) {
	body := string(uiHTML)
	blocks := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(body, -1)
	if len(blocks) < 2 {
		t.Fatalf("页面应有至少两个 script 块，找到 %d 个", len(blocks))
	}
	main, mux := blocks[0][1], blocks[len(blocks)-1][1]

	m := regexp.MustCompile(`(?s)window\.VL = \{(.*?)\};`).FindStringSubmatch(main)
	if m == nil {
		t.Fatal("主脚本没有导出 window.VL")
	}
	exported := m[1]

	// 主脚本里定义、混流脚本可能想复用的符号
	helpers := []string{
		"api", "el", "copyBtn", "openBtn", "prettySize", "prettyNum", "target",
		"dlName", "qualityLabel", "DL", "ensureTitle",
	}
	for _, h := range helpers {
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(h) + `\b`).MatchString(mux) {
			continue // 没用到就不必导出
		}
		if !strings.Contains(exported, h) {
			t.Errorf("混流脚本用了 %s，但它没出现在 window.VL 的导出列表里"+
				"（否则运行时会 xxx is not defined）", h)
		}
	}
}

// TestWebUIGating：图形化页面由 VL_WEBUI 控制，默认值跟随运行模式。
//
//	免校验模式：默认开（本来就是"自用工具"的场景）
//	账户模式  ：默认关（常部署在公网，不该默认多一个界面）；
//	            显式打开时页面**公开可访问**——否则用户连填 Key 的地方都没有，
//	            但页面上的数据（解析、用量、代理）仍然全部要 Key。
func TestWebUIGating(t *testing.T) {
	const marker = `id="muxStart"` // 图形化页面独有

	// ① 账户模式 + WebUI 关（默认）：根路径是纯文本导航页，且仍然要 Key
	off := newTestEnv(t, nil, defaultStub())
	if w := do(off.handler(), http.MethodGet, "/", nil); w.Code != http.StatusForbidden {
		t.Errorf("账户模式+WebUI 关，无 Key 应 403，得到 %d", w.Code)
	}
	w := do(off.handler(), http.MethodGet, "/", userHdr(off.userKey))
	if strings.Contains(w.Body.String(), marker) {
		t.Error("WebUI 关时不该返回图形化页面")
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("WebUI 关时 Content-Type = %q，想要 text/plain", ct)
	}

	// ② 账户模式 + WebUI 开：页面公开可拿（不含数据），数据接口仍要 Key
	on := newTestEnv(t, func(c *config.Config) { c.WebUI = true }, defaultStub())
	h := on.handler()
	w = do(h, http.MethodGet, "/", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), marker) {
		t.Fatalf("WebUI 开时根路径应公开返回页面，得到 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q，想要 text/html", ct)
	}
	// 页面本身不含任何账号数据；数据接口没有 Key 依然 403
	if strings.Contains(w.Body.String(), on.userKey) || strings.Contains(w.Body.String(), on.adminKey) {
		t.Error("页面里不应出现任何 Key")
	}
	if w := do(h, http.MethodGet, "/v1/usage", nil); w.Code != http.StatusForbidden {
		t.Errorf("WebUI 开着也不能让 /v1/usage 免鉴权，得到 %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/v1/usage", userHdr(on.userKey)); w.Code != http.StatusOK {
		t.Errorf("带 Key 的 /v1/usage 应 200，得到 %d", w.Code)
	}
	// 健康检查要如实告知 WebUI 状态
	var health map[string]any
	hw := do(h, http.MethodGet, "/v1/health", nil)
	if err := json.Unmarshal(hw.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["webui"] != true || health["mode"] != "account" {
		t.Errorf("health 未如实反映: webui=%v mode=%v", health["webui"], health["mode"])
	}

	// ③ 账户模式下其它路径也不该吐 HTML（WebUI 只改了根路径的行为）
	for _, path := range []string{"/index.html", "/ui", "/v1/usage", "/v1/platforms", "/nope"} {
		w := do(h, http.MethodGet, path, userHdr(on.userKey))
		if strings.Contains(w.Body.String(), marker) {
			t.Errorf("账户模式 %s 不该返回图形化页面", path)
		}
		if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
			t.Errorf("账户模式 %s 的 Content-Type = %q，不该是 HTML", path, ct)
		}
	}

	// ④ 免校验模式 + 显式关：退回纯文本导航页（ease 版本）
	easeOff := newEaseEnv(t, func(c *config.Config) { c.WebUI = false }, defaultStub())
	w = do(easeOff.handler(), http.MethodGet, "/", nil)
	if strings.Contains(w.Body.String(), marker) {
		t.Error("ease + WebUI=false 时不该返回图形化页面")
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("ease + WebUI=false 时 Content-Type = %q，想要 text/plain", ct)
	}
}
