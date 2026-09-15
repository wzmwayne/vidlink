package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
