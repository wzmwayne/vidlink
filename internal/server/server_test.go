package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	"vidlink/internal/rates"
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
		AdminKey:          "vl_admin_test", // 管理凭据是配置项，不是账本里的账号
		PublicKey:         "vl_public",     // 公共入口：Key 公开、按 IP 每日限额
		PublicDailyQuota:  25,
		ProxyRate:         quota.ProxyRate,
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

	adminKey, userKey := cfg.AdminKey, "vl_user_test"
	if userKey == adminKey {
		t.Fatal("测试前提：管理 Key 与账号 Key 必须是两个不同的值")
	}
	if _, err := store.Create(account.Account{
		Key: userKey, Name: "普通用户", Quota: 100, Multiplier: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// 公共账号：与 main.go 的启动引导一致（服务端不会自己建，由启动流程建）
	if cfg.PublicKey != "" {
		if _, _, err := store.EnsurePublic(cfg.PublicKey, "公共账号"); err != nil {
			t.Fatal(err)
		}
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

// TestThreeKeyPassingStyles：头部与 Bearer 传明文 Key，URL 只能传签名。
//
// URL 里那条不是"换个写法"，而是安全要求：明文 Key 一旦进了 URL，
// 就会留在浏览器历史、隧道日志、聊天记录与截屏里，而它长期有效；
// 签名只有几十秒，且签名本身不含任何秘密。
func TestThreeKeyPassingStyles(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	signed := account.SignAt(account.HandlePrefix, env.userKey, time.Now().Unix())
	cases := []struct {
		name string
		path string
		hdr  map[string]string
	}{
		{"X-API-Key", "/v1/usage", map[string]string{"X-API-Key": env.userKey}},
		{"Bearer", "/v1/usage", map[string]string{"Authorization": "Bearer " + env.userKey}},
		{"query（签名）", "/v1/usage?key=" + signed, nil},
	}
	for _, c := range cases {
		if w := do(h, http.MethodGet, c.path, c.hdr); w.Code != http.StatusOK {
			t.Errorf("%s 方式应通过，得到 %d：%s", c.name, w.Code, w.Body.String())
		}
	}
	// 明文 Key 出现在 URL 里必须被拒，而不是"照样能用"
	if w := do(h, http.MethodGet, "/v1/usage?key="+env.userKey, nil); w.Code != http.StatusForbidden {
		t.Errorf("URL 里传明文 Key 应 403，得到 %d", w.Code)
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
		do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/3", userHdr(env.userKey))
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

	w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/2", userHdr(env.userKey))
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

// TestAdminKeyIsFixedAndSeparateFromAccounts 锁住"管理是一个固定 Key，
// 不是账号也不是权限"这套语义：
//
//   - 配置了 VIDLINK_ADMIN_KEY → 只有这个 Key 能进管理面；
//   - 没配置 → 管理接口恒失败（403），而不是退回"某个账号是管理员"；
//   - 账号永远拿不到管理权（提升不了），管理 Key 也不是账号（不能解析）。
func TestAdminKeyIsFixedAndSeparateFromAccounts(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	if w := do(h, http.MethodGet, "/v1/admin/accounts", userHdr(env.userKey)); w.Code != http.StatusForbidden {
		t.Fatalf("普通账号访问管理端点应 403，得到 %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/v1/admin/accounts", nil); w.Code != http.StatusForbidden {
		t.Fatalf("不带 Key 访问管理端点应 403，得到 %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/v1/admin/accounts", userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("管理 Key 应 200，得到 %d（%s）", w.Code, w.Body.String())
	}

	// 管理 Key 不是账号：账本里查不到它，也调不动计量端点与用量查询。
	if _, ok := env.store.Get(env.adminKey); ok {
		t.Error("管理 Key 不应被当成账本里的账号")
	}
	for _, path := range []string{"/v1/usage", "/v1/links?url=stub.test/v/1"} {
		if w := do(h, http.MethodGet, path, userHdr(env.adminKey)); w.Code != http.StatusForbidden {
			t.Errorf("管理 Key 访问 %s 应 403（它不是账号），得到 %d", path, w.Code)
		}
	}

	// 没配管理 Key：管理面整体恒失败，并明确告知原因（不是 404 装作不存在）。
	off := newTestEnv(t, func(c *config.Config) { c.AdminKey = "" }, defaultStub())
	wo := do(off.handler(), http.MethodGet, "/v1/admin/accounts", userHdr(off.userKey))
	if wo.Code != http.StatusForbidden {
		t.Fatalf("未配置管理 Key 时应 403，得到 %d", wo.Code)
	}
	if _, msg, _ := errBody(t, wo); !strings.Contains(msg, "VIDLINK_ADMIN_KEY") {
		t.Errorf("错误信息应指出配置项，得到 %q", msg)
	}
	// 空白字符不算"配置了"
	blank := newTestEnv(t, func(c *config.Config) { c.AdminKey = "   " }, defaultStub())
	if w := do(blank.handler(), http.MethodGet, "/v1/admin/accounts", userHdr(blank.userKey)); w.Code != http.StatusForbidden {
		t.Errorf("管理 Key 为空白时应 403，得到 %d", w.Code)
	}
}

// TestAccountsHaveNoAdminField：账号结构里没有权限位。
//
// 老接口的 `{"admin":true}` 必须**报错**而不是静默忽略——静默忽略会让
// 调用方以为"已经提升成管理员了"，然后对着一个永远 403 的 Key 排查半天。
func TestAccountsHaveNoAdminField(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	for _, probe := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/admin/accounts", `{"name":"想当管理员的账号","admin":true}`},
		{http.MethodPatch, "/v1/admin/accounts/" + env.userKey, `{"admin":true}`},
	} {
		w := doJSON(h, probe.method, probe.path, probe.body, userHdr(env.adminKey))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s %s 带 admin 字段应 400，得到 %d（%s）",
				probe.method, probe.path, w.Code, w.Body.String())
		}
	}

	// 列表里也不能出现 admin 字段
	w := do(h, http.MethodGet, "/v1/admin/accounts", userHdr(env.adminKey))
	if strings.Contains(w.Body.String(), `"admin"`) {
		t.Errorf("账号视图里不该有 admin 字段：%s", w.Body.String())
	}
}

// TestAdminCanAdjustQuotaAndMultiplier 是管理面的核心用例：
// 管理 Key 能调整**其他账号**的配额与账号倍率。
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

	// 设账号倍率（减半）
	w = doJSON(h, http.MethodPatch, "/v1/admin/accounts/"+env.userKey,
		`{"multiplier":0.5}`, userHdr(env.adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("调整倍率应 200，得到 %d", w.Code)
	}
	if got, _ := env.store.Get(env.userKey); got.Multiplier != 0.5 {
		t.Fatalf("倍率应为 0.5，得到 %v", got.Multiplier)
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

// TestAdminCanAddressAccountByHandle：管理面用**句柄**寻址账号。
//
// 这是管理面板能工作的前提：列表只回掩码 Key（明文只在创建时出现一次），
// 所以面板只能拿句柄去改配额/停用/删除。句柄由 Key 派生，
// 拿不到 Key 也冒充不了账号。
func TestAdminCanAddressAccountByHandle(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	w := do(h, http.MethodGet, "/v1/admin/accounts", userHdr(env.adminKey))
	var list struct {
		Accounts []account.Account `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var handle string
	for _, a := range list.Accounts {
		if a.Name == "普通用户" {
			handle = a.ID
		}
	}
	if handle == "" {
		t.Fatalf("列表应包含账号句柄：%s", w.Body.String())
	}
	if handle != account.Handle(env.userKey) {
		t.Errorf("句柄应由 Key 派生：得到 %q", handle)
	}
	if strings.Contains(handle, env.userKey) || !strings.HasPrefix(handle, "acc_") {
		t.Errorf("句柄不能是可反推的 Key 片段：%q", handle)
	}
	// 掩码 Key 依旧出现（人要能认出是哪个账号），但明文不出现
	if strings.Contains(w.Body.String(), env.userKey) {
		t.Error("列表泄露了明文 Key")
	}

	// 用句柄查 / 改 / 删
	if w := do(h, http.MethodGet, "/v1/admin/accounts/"+handle, userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("按句柄查询应 200，得到 %d", w.Code)
	}
	if w := doJSON(h, http.MethodPatch, "/v1/admin/accounts/"+handle,
		`{"note":"按句柄改的"}`, userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("按句柄修改应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	if got, _ := env.store.Get(env.userKey); got.Note != "按句柄改的" {
		t.Fatalf("修改没落到目标账号上：%+v", got)
	}
	// 掩码 Key 不是句柄：拿它当路径参数必须 404，避免"看着像 Key 就能用"
	masked := account.Account{Key: env.userKey}.Masked()
	if w := doJSON(h, http.MethodPatch, "/v1/admin/accounts/"+masked,
		`{"quota":1}`, userHdr(env.adminKey)); w.Code != http.StatusNotFound {
		t.Errorf("掩码 Key 不该被当作句柄，得到 %d", w.Code)
	}
	if w := do(h, http.MethodDelete, "/v1/admin/accounts/"+handle, userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("按句柄删除应 200，得到 %d", w.Code)
	}
	if _, ok := env.store.Get(env.userKey); ok {
		t.Error("账号应已被删除")
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

// TestAdminKeyIsNeverAnAccount：管理 Key 既不能删自己，也不需要保护。
//
// 老实现里有"不能删除当前正在使用的管理员账号"，因为管理员就是一个账号；
// 现在管理 Key 不在账本里，那条保护连同它的语义一起消失：
// 拿管理 Key 当路径参数只会得到 404（账本里没有这个账号）。
func TestAdminKeyIsNeverAnAccount(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	w := do(h, http.MethodDelete, "/v1/admin/accounts/"+env.adminKey, userHdr(env.adminKey))
	if w.Code != http.StatusNotFound {
		t.Fatalf("管理 Key 不是账号，删除应 404，得到 %d（%s）", w.Code, w.Body.String())
	}
	// 删掉账本里唯一的账号之后，管理面依旧可用（这正是把 admin 移出账本换来的）
	if w := do(h, http.MethodDelete, "/v1/admin/accounts/"+env.userKey, userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("删除账号应 200，得到 %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/v1/admin/accounts", userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("账本空了也不该影响管理面，得到 %d", w.Code)
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
		`id="muxVq"`,         // 混流区直接选视频清晰度
		`id="muxAq"`,         // 与音频清晰度
		`id="muxTrackState"`, // 档位状态（没有"取清晰度列表"按钮）
		`id="muxCost"`,       // 代理按体积计费的预估/实际消耗
		`id="muxProxy"`,      // 是否经服务端代理下载（由使用者决定，不自动兜底）
		`applyDetail`,        // 「全部详情」的结果直接喂给混流区
		`/v1/detail`,
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
		"googleapis", "unpkg", "jsdelivr",
	} {
		if strings.Contains(low, forbidden) {
			t.Errorf("页面引用了外部资源: %q", forbidden)
		}
	}
	// 页面**允许**指向项目自己的可点击外链（仓库 / 接口文档），但只允许
	// github.com 一个域，且必须带 rel="noopener"——否则 window.opener
	// 会把本页暴露给被打开的页面。
	checkExternalLinks(t, body)
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
	if len(blocks) < 3 {
		t.Fatalf("页面应有至少三个 script 块（crypto / 主脚本 / 混流），找到 %d 个", len(blocks))
	}
	// 主脚本按"谁定义了 window.VL"来认，而不是按下标：下标会随着
	// 新增脚本块（比如这次的 vl-crypto）而错位。
	main := ""
	for _, b := range blocks {
		if strings.Contains(b[1], "window.VL = {") {
			main = b[1]
		}
	}
	if main == "" {
		t.Fatal("没有任何脚本块导出 window.VL")
	}
	mux := blocks[len(blocks)-1][1]

	m := regexp.MustCompile(`(?s)window\.VL = \{(.*?)\};`).FindStringSubmatch(main)
	if m == nil {
		t.Fatal("主脚本没有导出 window.VL")
	}
	exported := m[1]

	// 主脚本里定义、混流脚本可能想复用的符号
	helpers := []string{
		"api", "el", "copyBtn", "openBtn", "prettySize", "prettyNum", "target",
		"signedQuery", "dlName", "qualityLabel", "DL", "ensureTitle",
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

// TestUIMuxBlockNeverUsesUnexportedMainVars：混流脚本引用的主脚本变量必须已导出。
//
// 这条来自一次真实故障：`updateMuxNote()` 里用了 `myProxyRate`，而它声明在
// **另一个** <script> 的 IIFE 里、也没挂到 window.VL 上。结果是"点一次详情、
// 页面弹 ReferenceError 并显示原始 JSON"——只在"上游给了体积且勾了代理"时
// 才炸，平时的冒烟根本碰不到。
//
// 两个脚本块之间没有共享词法作用域，所以这类错误的唯一防法就是静态比对：
// 主脚本里声明的标识符 ∩ 混流脚本里用到的 − 混流脚本自己声明的 − 已导出的
// 必须为空。
func TestUIMuxBlockNeverUsesUnexportedMainVars(t *testing.T) {
	blocks := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(string(uiHTML), -1)
	var main, mux string
	for _, b := range blocks {
		if strings.Contains(b[1], "window.VL = {") {
			main = b[1]
		}
	}
	if main == "" {
		t.Fatal("找不到主脚本（没有 window.VL 导出）")
	}
	mux = blocks[len(blocks)-1][1]

	// 去掉注释与字符串再扫标识符：注释里提到某个名字（"myProxyRate 曾漏导出"）
	// 和字符串里的文字都不是引用，扫进来就是误报。
	stripJS := func(v string) string {
		var b strings.Builder
		for i := 0; i < len(v); {
			switch {
			case strings.HasPrefix(v[i:], "//"):
				for i < len(v) && v[i] != '\n' {
					i++
				}
			case strings.HasPrefix(v[i:], "/*"):
				if j := strings.Index(v[i+2:], "*/"); j < 0 {
					i = len(v)
				} else {
					i += 2 + j + 2
				}
			case v[i] == '"' || v[i] == '\'' || v[i] == '`':
				q := v[i]
				i++
				for i < len(v) && v[i] != q {
					if v[i] == '\\' {
						i++
					}
					i++
				}
				i++
			default:
				b.WriteByte(v[i])
				i++
			}
		}
		return b.String()
	}
	mainSrc, muxSrc := stripJS(main), stripJS(mux)

	// 主脚本 IIFE 内顶层声明的标识符（两个空格缩进）
	decl := regexp.MustCompile(`(?m)^  (?:let|const|function)\s+([A-Za-z_$][\w$]*)`)
	mainDecls := map[string]bool{}
	for _, m := range decl.FindAllStringSubmatch(mainSrc, -1) {
		mainDecls[m[1]] = true
	}
	if len(mainDecls) < 10 {
		t.Fatalf("只扫到 %d 个主脚本声明，正则可能失效", len(mainDecls))
	}

	// 已导出的名字：window.VL = { a, b, c: () => x, d: y } —— 取键名
	exports := map[string]bool{}
	if m := regexp.MustCompile(`(?s)window\.VL = \{(.*?)\};`).FindStringSubmatch(mainSrc); m != nil {
		for _, part := range strings.Split(m[1], ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			name := part
			if i := strings.Index(part, ":"); i >= 0 {
				name = strings.TrimSpace(part[:i])
			}
			exports[name] = true
		}
	}
	if len(exports) < 10 {
		t.Fatalf("只解析出 %d 个导出名，正则可能失效", len(exports))
	}

	// 混流脚本自己声明的名字（含解构、函数参数与箭头参数），这些不算"来自主脚本"
	local := map[string]bool{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`\b(?:let|const|var|function|class)\s+([A-Za-z_$][\w$]*)`),
		regexp.MustCompile(`\{([^{}]*)\}\s*=`),
		regexp.MustCompile(`\[([^\[\]]*)\]\s*=`),
		regexp.MustCompile(`(?:function\s*[A-Za-z_$\w]*\s*)?\(([^()]*)\)\s*=>`),
		regexp.MustCompile(`\bfunction\s*[A-Za-z_$\w]*\s*\(([^()]*)\)`),
	} {
		for _, m := range re.FindAllStringSubmatch(muxSrc, -1) {
			for _, name := range strings.Split(m[1], ",") {
				name = strings.TrimSpace(name)
				if i := strings.LastIndex(name, ":"); i >= 0 { // 解构里的重命名
					name = strings.TrimSpace(name[i+1:])
				}
				if regexp.MustCompile(`^[A-Za-z_$][\w$]*$`).MatchString(name) {
					local[name] = true
				}
			}
		}
	}

	// 用到的标识符：跳过属性访问（res.ok 里的 ok 不是变量）
	used := map[string]bool{}
	idRe := regexp.MustCompile(`[A-Za-z_$][\w$]*`)
	for _, loc := range idRe.FindAllStringIndex(muxSrc, -1) {
		if loc[0] > 0 && muxSrc[loc[0]-1] == '.' {
			continue
		}
		used[muxSrc[loc[0]:loc[1]]] = true
	}

	for name := range mainDecls {
		if !used[name] || local[name] || exports[name] {
			continue
		}
		t.Errorf("混流脚本用了主脚本里的 %q，但它没出现在 window.VL 的导出列表里"+
			"（运行时会是 xxx is not defined；变量要用 getter 导出，否则快照会停在 null）", name)
	}
}

// TestUIMuxNoteRenders：把混流区的"代理计费提示"函数抽出来在 node 里真跑一遍。
//
// 上一个用例管"引用的东西有没有导出"，这个用例管"跑起来到底出不出文案"：
// 用户看到的是 `ReferenceError: myProxyRate is not defined` 弹在结果区，
// 而这类错误只有在"上游给了两轨体积 + 勾了代理"时才会走到，普通冒烟撞不上。
func TestUIMuxNoteRenders(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("环境里没有 node，跳过")
	}
	blocks := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(string(uiHTML), -1)
	mux := blocks[len(blocks)-1][1]
	head := strings.Index(mux, "function updateMuxNote()")
	if head < 0 {
		t.Fatal("混流脚本里找不到 updateMuxNote")
	}
	tail := strings.Index(mux[head:], "\n  }\n")
	if tail < 0 {
		t.Fatal("找不到 updateMuxNote 的结尾")
	}
	fn := mux[head : head+tail+len("\n  }")]

	driver := `
const src = ` + strconv.Quote(fn) + `;
let text = "";
const box = { set textContent(v) { text = v; }, get textContent() { return text; } };
const $ = (sel) => (sel === "#muxCost" ? box : { checked: true, value: "0" });
const proxyOn = () => true;
const fmtUnits = (n) => String(Math.round(n * 100) / 100);
const proxyCost = () => 0.6;
const tracks = { videos: [{ size: 1048576 }], audios: [{ size: 1048576 }] };
const run = (rate, mult) => {
  const f = new Function("$", "proxyOn", "multiplier", "proxyRate", "fmtUnits",
                         "proxyCost", "tracks", "return " + src);
  f($, proxyOn, () => mult, () => rate, fmtUnits, proxyCost, tracks)();
};
const out = [];
run(0.5, 1); out.push(text);          // 账户模式：费率来自 /v1/usage
run(null, 1); out.push(text);         // 费率还没拿到：回落到 0.5
tracks.videos[0].size = 0;            // 上游没给体积
run(0.5, 1); out.push(text);
console.log(JSON.stringify(out));
`
	dir := t.TempDir()
	js := filepath.Join(dir, "muxnote.js")
	if err := os.WriteFile(js, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, js).CombinedOutput()
	if err != nil {
		t.Fatalf("跑 updateMuxNote 失败（大概又是某个变量没导出）：%v\n%s", err, raw)
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("node 输出不是 JSON：%v\n%s", err, raw)
	}
	if len(got) != 3 {
		t.Fatalf("想要 3 段文案，得到 %d 段：%v", len(got), got)
	}
	for i, want := range []string{"代理按体积计费：0.5", "代理按体积计费：0.5", "上游没给体积"} {
		if !strings.Contains(got[i], want) {
			t.Errorf("第 %d 段文案不对（想要包含 %q）：%s", i+1, want, got[i])
		}
	}
	for _, s := range got {
		if strings.Contains(s, "undefined") || strings.Contains(s, "NaN") {
			t.Errorf("文案里出现了 undefined/NaN：%s", s)
		}
	}
}

// TestUICheckinStateRenders：「签到 / 强制签到」的文案与状态判断。
//
// 签到按钮是这几类 bug 的高发区：不可签时被禁用/隐藏、文案与真实状态不符、
// 或者"签一次成功之后按钮永久变灰"。所以把 checkinState 整段抽出来交给 node，
// 喂四种账号状态跑一遍，断言按钮文案与「可否签到」那一行。
func TestUICheckinStateRenders(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("环境里没有 node，跳过")
	}
	blocks := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(string(uiHTML), -1)
	var main string
	for _, b := range blocks {
		if strings.Contains(b[1], "window.VL = {") {
			main = b[1]
		}
	}
	if main == "" {
		t.Fatal("找不到主脚本")
	}
	head := strings.Index(main, "function checkinState(d) {")
	if head < 0 {
		t.Fatal("主脚本里找不到 checkinState")
	}
	tail := strings.Index(main[head:], "\n  }\n")
	if tail < 0 {
		t.Fatal("找不到 checkinState 的结尾")
	}
	fn := main[head : head+tail+len("\n  }")]

	driver := `
const src = ` + strconv.Quote(fn) + `;
const cases = {
  can:    { name: "可签",   checkin: {enabled: true, daily: 25, cap: 200, checked_in_today: false, last_checkin_day: ""} },
  done:   { name: "已签",   checkin: {enabled: true, daily: 25, cap: 0, checked_in_today: true, last_checkin_day: "2026-09-19"} },
  off:    { name: "未开放", checkin: {enabled: false, daily: 0, cap: 0, checked_in_today: false, last_checkin_day: ""} },
  pub:    { name: "公共Key", checkin: {enabled: false, daily: 0, cap: 0, checked_in_today: false, last_checkin_day: ""}, public: true },
  absent: { name: "无字段", checkin: null },
};
const f = new Function("return " + src)();
const out = {};
for (const k of Object.keys(cases)) out[k] = f(cases[k]);
console.log(JSON.stringify(out));
`
	dir := t.TempDir()
	js := filepath.Join(dir, "checkinstate.js")
	if err := os.WriteFile(js, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(node, js).CombinedOutput()
	if err != nil {
		t.Fatalf("跑 checkinState 失败：%v\n%s", err, raw)
	}
	var got map[string]struct {
		Show   bool   `json:"show"`
		Can    bool   `json:"can"`
		Label  string `json:"label"`
		Title  string `json:"title"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("node 输出不是 JSON：%v\n%s", err, raw)
	}
	want := map[string]struct {
		show, can bool
		label     string
		reasonHas string
	}{
		"can":    {true, true, "签到", "可以：点击「签到」领取 +25 配额（上限 200）"},
		"done":   {true, false, "强制签到", "不可以：今天已经签过了（2026-09-19）"},
		"off":    {true, false, "强制签到", "不可以：账号未开放每日签到"},
		"pub":    {false, false, "签到", "不可以：公共 Key 按 IP 每日自动给额度"},
		"absent": {false, false, "签到", ""},
	}
	for key, w := range want {
		g, ok := got[key]
		if !ok {
			t.Errorf("缺少用例 %s", key)
			continue
		}
		if g.Show != w.show || g.Can != w.can || g.Label != w.label {
			t.Errorf("%s：show/can/label = %v/%v/%q，想要 %v/%v/%q",
				key, g.Show, g.Can, g.Label, w.show, w.can, w.label)
		}
		if w.reasonHas != "" && !strings.Contains(g.Reason, w.reasonHas) {
			t.Errorf("%s：reason = %q，应包含 %q", key, g.Reason, w.reasonHas)
		}
		for _, s := range []string{g.Label, g.Title, g.Reason} {
			if strings.Contains(s, "undefined") || strings.Contains(s, "NaN") {
				t.Errorf("%s：文案里出现 undefined/NaN：%q %q %q", key, g.Label, g.Title, g.Reason)
			}
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

// --- 管理面板 ---

// TestAdminPanelGating：管理面板与解析页同一个开关（VL_WEBUI），
// 但只在账户模式下存在——免校验模式没有账号体系，也就没有可管理的东西。
//
// 页面本身是**公开的壳**：不公开的话浏览器连"填管理 Key 的输入框"都拿不到。
// 壳里不能有任何账号数据或 Key，数据接口依旧要管理 Key。
func TestAdminPanelGating(t *testing.T) {
	const marker = `id="adminkey"`

	// ① 账户模式 + WebUI 关：不是公开路径（没 Key 403），带 Key 也是 404
	off := newTestEnv(t, nil, defaultStub())
	if w := do(off.handler(), http.MethodGet, adminUIPath, nil); w.Code != http.StatusForbidden {
		t.Errorf("WebUI 关时 /admin 无 Key 应 403，得到 %d", w.Code)
	}
	w := do(off.handler(), http.MethodGet, adminUIPath, userHdr(off.userKey))
	if w.Code != http.StatusNotFound {
		t.Errorf("WebUI 关时 /admin 应 404，得到 %d", w.Code)
	}
	if strings.Contains(w.Body.String(), marker) {
		t.Error("WebUI 关时不该返回管理面板")
	}

	// ② 账户模式 + WebUI 开：页面公开可拿，但不含任何 Key；数据仍要管理 Key
	on := newTestEnv(t, func(c *config.Config) { c.WebUI = true }, defaultStub())
	h := on.handler()
	w = do(h, http.MethodGet, adminUIPath, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("WebUI 开时 /admin 应公开返回页面，得到 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q，想要 text/html", ct)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("缺少 nosniff，得到 %q", got)
	}
	body := w.Body.String()
	for _, want := range []string{
		marker,               // 管理 Key 输入框
		`id="keySave"`,       // 保存
		`id="keyClear"`,      // 清除
		"X-API-Key",          // 内部请求自动带 Key
		`vlSignUrl("adm_"`,   // 页面链接自动拼管理签名（绝不放明文 Key）
		"localStorage",       // 只存在本机浏览器
		"/v1/admin/accounts", // 调用的管理接口
		"/v1/admin/stats",
		"/v1/admin/quota",
		"/v1/health", // 用于判断模式与版本
	} {
		if !strings.Contains(body, want) {
			t.Errorf("管理面板缺少 %q", want)
		}
	}
	// 明文 Key 绝不能出现在页面生成的 URL 里（这是本次改动的核心要求）
	for _, bad := range []string{"encodeURIComponent(ADMINKEY)", "encodeURIComponent(APIKEY)"} {
		if strings.Contains(body, bad) {
			t.Errorf("页面里还留着把明文 Key 拼进 URL 的写法：%q", bad)
		}
	}
	if strings.Contains(body, on.userKey) || strings.Contains(body, on.adminKey) {
		t.Error("页面里不应出现任何 Key")
	}
	if w := do(h, http.MethodGet, "/v1/admin/accounts", nil); w.Code != http.StatusForbidden {
		t.Errorf("面板公开不能让管理接口免鉴权，得到 %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/v1/admin/accounts", userHdr(on.userKey)); w.Code != http.StatusForbidden {
		t.Errorf("账号 Key 依然不能进管理面，得到 %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/v1/admin/accounts", userHdr(on.adminKey)); w.Code != http.StatusOK {
		t.Errorf("管理 Key 应 200，得到 %d", w.Code)
	}
	// HEAD 只回头不回体；POST 给 405 而不是 404
	if w := do(h, http.MethodHead, adminUIPath, nil); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD /admin 应 200 且无体，得到 %d / %d 字节", w.Code, w.Body.Len())
	}
	// 非 GET/HEAD 不享受"页面公开"待遇（与根路径一致）：先鉴权，再谈方法。
	if w := do(h, http.MethodPost, adminUIPath, nil); w.Code != http.StatusForbidden {
		t.Errorf("无 Key 的 POST /admin 应 403，得到 %d", w.Code)
	}
	if w := do(h, http.MethodPost, adminUIPath, userHdr(on.userKey)); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("带 Key 的 POST /admin 应 405，得到 %d", w.Code)
	}

	// ③ 免校验模式：管理面整体不存在，/admin 必须 404（哪怕 WebUI 开着）
	ease := newEaseEnv(t, nil, defaultStub())
	if w := do(ease.handler(), http.MethodGet, adminUIPath, nil); w.Code != http.StatusNotFound {
		t.Errorf("免校验模式下 /admin 应 404，得到 %d", w.Code)
	}
	if strings.Contains(do(ease.handler(), http.MethodGet, adminUIPath, nil).Body.String(), marker) {
		t.Error("免校验模式下不该返回管理面板")
	}
}

// TestAdminPanelStaysSelfContained：管理面板不能引用外部资源，
// 且它引用的每个接口路径都必须真实注册。
//
// 后半条是防"文档与实现漂移"的：面板里写了一个不存在的路径，
// 症状是运行到那一步才 404，而静态检查能在构建期挡住。
func TestAdminPanelStaysSelfContained(t *testing.T) {
	body := string(adminHTML)
	low := strings.ToLower(body)
	for _, forbidden := range []string{
		"<script src", "<link ", "@import", "url(http", `src="http`, "src='http",
		"googleapis", "unpkg", "jsdelivr",
		`href="http`, "href='http", // 管理面板一个外链都不放
	} {
		if strings.Contains(low, forbidden) {
			t.Errorf("管理面板引用了外部资源: %q", forbidden)
		}
	}
	if len(body) > 60*1024 {
		t.Errorf("管理面板过大（%d 字节），内嵌资源应保持精简", len(body))
	}

	specs := newTestEnv(t, func(c *config.Config) { c.WebUI = true }, defaultStub()).srv.routes()
	re := regexp.MustCompile(`/v1/[A-Za-z0-9_/]*`)
	seen := map[string]bool{}
	for _, m := range re.FindAllString(body, -1) {
		m = strings.TrimRight(m, "/")
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		ok := false
		for _, rt := range specs {
			if rt.path == m || matchPath(rt.path, m) || strings.HasPrefix(rt.path, m+"/") {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("管理面板引用了不存在的路由 %q", m)
		}
	}
	if len(seen) < 4 {
		t.Fatalf("只扫到 %d 个接口路径，正则可能失效：%v", len(seen), seen)
	}
}

// TestMuxSectionChoosesTracksAndChannel：混流区必须自己带清晰度选择与
// "是否走代理"的开关，而不是复用上面"解析一条"的清晰度、也不是自动兜底。
//
// 这条是静态检查（页面 JS 不在测试里跑），锁的是几个容易在重构中丢掉的
// 事实：两个下拉真的存在、代理 URL 拼上了 Key、代理由复选框决定。
func TestMuxSectionChoosesTracksAndChannel(t *testing.T) {
	body := string(uiHTML)
	for _, want := range []string{
		`id="muxVq"`, `id="muxAq"`, `id="muxProxy"`,
		// 档位来自 /v1/detail（links 只给最优的一条，选不了）
		`"/v1/detail?" + t.q`,
		// 「全部详情」拿到结果后自动填档位：一次请求两个消费者
		`window.VL.applyDetail(res.body, key)`,
		`window.VL.applyDetail = applyTracks`,
		// 代理可用性来自异步的 /v1/health：必须在拿到之后再同步复选框，
		// 否则"服务端开了代理，复选框却灰着点不动"
		`function syncProxyBox()`, `syncProxyBox();`,
		// 选项文本用 textContent，value 只放下标，不把 URL 塞进 DOM
		`el("option", null,`, `o.value = String(i)`,
		// 代理通道：必须把**签名**拼进 URL（明文 Key 不许进 URL），
		// 否则账户模式下 /v1/proxy 直接 403
		`signedQuery("/v1/proxy?url="`,
		// 代理按体积计费：预估展示 + 读响应头拿实际值（前端不编数字）
		`id="muxCost"`, `proxyCost`, `X-Quota-Consumed`, `配额/MiB`,
		`rate_per_mib`, // 费率来自接口，前端不写死
		// 直连失败时的提示要指向那个复选框，而不是自动改走代理
		`使用服务端代理下载`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("混流区块缺少 %q", want)
		}
	}
	// 页面上不该再有"取清晰度列表"这个按钮/文案：档位改为取完整信息时自动解析
	if strings.Contains(body, `id="muxLoad"`) || strings.Contains(body, "取清晰度列表") {
		t.Error("不该再有独立的「取清晰度列表」按钮")
	}
	// 真的不再自动兜底：downloadTrack 里不该再出现"直连失败就自己上代理"的调用
	if strings.Contains(body, "return await once(proxyURL()") {
		t.Error("下载通道仍会自动兜底到代理，应由复选框决定")
	}
}

// TestProxyRejectsUnsafeTargets：代理是风险最高的接口，参数校验要有明确边界。
//
// 四件事：目标地址不许带凭据、长度有上限、Referer 必须是 http/https 绝对地址、
// UA 里的控制字符会被清掉（防请求头注入，虽然后端 transport 也会拦）。
func TestProxyRejectsUnsafeTargets(t *testing.T) {
	var gotReferer, gotUA string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer, gotUA = r.Header.Get("Referer"), r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	env := newTestEnv(t, func(c *config.Config) { c.ProxySrv.Enabled = true }, defaultStub())
	h := env.handler()

	// ① 带 userinfo 的 URL：会把凭据一起发给上游，直接拒
	w := do(h, http.MethodGet, "/v1/proxy?url="+url.QueryEscape("http://user:pass@example.com/x"),
		userHdr(env.userKey))
	if w.Code != http.StatusBadRequest {
		t.Errorf("带用户名密码的 url 应 400，得到 %d", w.Code)
	}

	// ② 超长 URL：没有理由超过 4KB
	long := "https://example.com/" + strings.Repeat("a", 5000)
	if w := do(h, http.MethodGet, "/v1/proxy?url="+url.QueryEscape(long), userHdr(env.userKey)); w.Code != http.StatusBadRequest {
		t.Errorf("超长 url 应 400，得到 %d", w.Code)
	}

	// ③ Referer 必须是 http/https 绝对地址（它会被写进请求头）
	for _, bad := range []string{"not-a-url", "javascript:alert(1)", "//evil.com/", "file:///etc/passwd"} {
		w := do(h, http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL)+
			"&referer="+url.QueryEscape(bad), userHdr(env.userKey))
		if w.Code != http.StatusBadRequest {
			t.Errorf("referer=%q 应 400，得到 %d", bad, w.Code)
		}
	}

	// ④ 合法 Referer 透传；UA 里的控制字符被清掉
	w = do(h, http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL)+
		"&referer="+url.QueryEscape("https://www.bilibili.com/")+
		"&ua="+url.QueryEscape("Evil\r\nX-Injected: 1"), userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("正常请求应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	if gotReferer != "https://www.bilibili.com/" {
		t.Errorf("上游收到 Referer = %q", gotReferer)
	}
	if strings.ContainsAny(gotUA, "\r\n") {
		t.Errorf("UA 里的控制字符没被清掉：%q", gotUA)
	}
	if gotUA != "EvilX-Injected: 1" {
		t.Errorf("UA = %q，想要去掉控制字符后的值", gotUA)
	}

	// ⑤ 白名单按主机后缀收紧（127.0.0.1 不在 example.com 之下 → 403）
	limited := newTestEnv(t, func(c *config.Config) {
		c.ProxySrv.Enabled = true
		c.ProxySrv.AllowHosts = []string{"bilivideo.com"}
	}, defaultStub())
	if w := do(limited.handler(), http.MethodGet,
		"/v1/proxy?url="+url.QueryEscape(upstream.URL), userHdr(limited.userKey)); w.Code != http.StatusForbidden {
		t.Errorf("白名单外的目标应 403，得到 %d", w.Code)
	}
}

// TestProxyAcceptsBareHostSuffix：白名单里写 URL 形态也能命中（规范化在配置层完成）。
func TestProxyAcceptsBareHostSuffix(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	env := newTestEnv(t, func(c *config.Config) {
		c.ProxySrv.Enabled = true
		c.ProxySrv.AllowHosts = []string{"127.0.0.1"} // 规范化后的形态
	}, defaultStub())
	if w := do(env.handler(), http.MethodGet,
		"/v1/proxy?url="+url.QueryEscape(upstream.URL), userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Fatalf("白名单命中应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
}

// TestUIScriptsParse：页面里的每个 <script> 块必须能被 JS 引擎解析。
//
// 这条来自一次真实的静默故障：混流脚本里出现了两个同作用域的
// `const a`，浏览器直接放弃**整块脚本**（控制台只有一行
// "Identifier 'a' has already been declared"），症状是混流功能整个消失，
// 而页面其余部分看起来完全正常。字符串断言查不出这种错，只有真解析一次才行。
//
// 用 node --check（只解析、不执行，不需要 DOM）。环境里没有 node 就跳过：
// 单测不该因为缺一个可选的开发工具而失败，CI 的 ubuntu 镜像自带 node。
func TestUIScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("环境里没有 node，跳过 JS 语法检查")
	}
	blocks := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(string(uiHTML), -1)
	if len(blocks) == 0 {
		t.Fatal("页面里没有 script 块")
	}
	dir := t.TempDir()
	for i, b := range blocks {
		path := filepath.Join(dir, fmt.Sprintf("block%d.js", i))
		if err := os.WriteFile(path, []byte(b[1]), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
			t.Errorf("第 %d 个 script 块语法错误：%v\n%s", i+1, err, out)
		}
	}
}

// --- 签名凭据 ---

// TestSignEndpointIssuesUsableCredential：签发端点要"签了就能用"，且不花配额。
func TestSignEndpointIssuesUsableCredential(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	before, ok := env.store.Get(env.userKey)
	if !ok {
		t.Fatal("测试账号应存在")
	}
	w := do(h, http.MethodGet, "/v1/sign", userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/sign 应 200，得到 %d：%s", w.Code, w.Body.String())
	}
	var body struct {
		Key       string `json:"key"`
		Query     string `json:"query"`
		Type      string `json:"type"`
		TTL       int64  `json:"ttl"`
		IssuedAt  int64  `json:"issued_at"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	handle, ts, _, ok := account.ParseCredential(body.Key)
	if !ok {
		t.Fatalf("签发的凭据形状不对：%q", body.Key)
	}
	if handle != account.Handle(env.userKey) {
		t.Errorf("句柄 = %q，想要 %q", handle, account.Handle(env.userKey))
	}
	if body.Type != "account" || body.TTL != int64(account.DefaultTTL/time.Second) {
		t.Errorf("type/ttl = %q/%d，想要 account/%d", body.Type, body.TTL, int64(account.DefaultTTL/time.Second))
	}
	if body.ExpiresAt != ts+body.TTL || body.IssuedAt != ts {
		t.Errorf("issued_at/expires_at = %d/%d，与票面时间戳 %d + ttl %d 对不上",
			body.IssuedAt, body.ExpiresAt, ts, body.TTL)
	}
	if !strings.HasPrefix(body.Query, "key=acc_") {
		t.Errorf("query = %q，应是以 key=acc_ 开头的可拼接形式", body.Query)
	}

	// 签发是免费的：不扣配额、不增加调用计数
	after, _ := env.store.Get(env.userKey)
	if after.Quota != before.Quota || after.Calls != before.Calls {
		t.Errorf("签发不该动配额/调用数：%v/%d → %v/%d",
			before.Quota, before.Calls, after.Quota, after.Calls)
	}

	// 立刻拿去用：解析类接口（这里用不花上游的 /v1/usage）应放行
	if w := do(h, http.MethodGet, "/v1/usage?"+body.Query, nil); w.Code != http.StatusOK {
		t.Errorf("签发的凭据应立即可用，得到 %d：%s", w.Code, w.Body.String())
	}
	// 也应当能用于计量端点（走的是同一条认证路径）
	if w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1&"+body.Query, nil); w.Code != http.StatusOK {
		t.Errorf("签发的凭据在计量端点上应放行，得到 %d：%s", w.Code, w.Body.String())
	}
}

// TestSignedQueryRejections：每一种"签名不对"都要有各自的稳定错误码。
//
// 分开的意义在于可操作性：过期要提示重新签发，时间戳在未来要提示对表，
// 签名不符要提示 Key 不对。三种都只回 403 而文案一样的话，
// 用户只能靠猜。
func TestSignedQueryRejections(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	now := time.Now().Unix()
	good := account.SignAt(account.HandlePrefix, env.userKey, now)
	handle, ts, sig, ok := account.ParseCredential(good)
	if !ok {
		t.Fatal("黄金凭据解析失败")
	}
	flip := func(s string) string {
		b := []byte(s)
		if b[0] == 'a' {
			b[0] = 'b'
		} else {
			b[0] = 'a'
		}
		return string(b)
	}
	// 句柄是别人的（不存在的账号），但签名是用**我们自己的** Key 算的：
	// 服务端查不到句柄 → 与"Key 无效"同一口径，不泄漏账号是否存在。
	ghost := account.HandleID(account.HandlePrefix, "vl_ghost")
	ghostCred := account.SignAtWith(ghost, env.userKey, now)

	cases := []struct{ name, key, wantKind string }{
		{"过期的签名", account.SignAt(account.HandlePrefix, env.userKey, now-3600), "signature_expired"},
		{"时间戳在未来", account.SignAt(account.HandlePrefix, env.userKey, now+3600), "signature_future"},
		{"签名被改过", handle + "." + account.FormatTS(ts) + "." + flip(sig), "signature_invalid"},
		{"时间戳被改过", handle + "." + account.FormatTS(ts+1) + "." + sig, "signature_invalid"},
		{"URL 里放明文 Key", env.userKey, "key_format"},
		{"形状不对", "acc_eb331e889382bce5", "key_format"},
		{"前缀不对", "xyz_eb331e889382bce5.68a3e658." + sig, "key_format"},
		{"句柄不存在", ghostCred, "key_invalid"},
		{"管理签名用在账号接口", account.SignAt(account.AdminPrefix, env.adminKey, now), "key_scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(h, http.MethodGet, "/v1/usage?key="+tc.key, nil)
			if w.Code != http.StatusForbidden {
				t.Fatalf("应 403，得到 %d：%s", w.Code, w.Body.String())
			}
			if kind, _, _ := errBody(t, w); kind != tc.wantKind {
				t.Errorf("错误码 = %q，想要 %q（%s）", kind, tc.wantKind, w.Body.String())
			}
		})
	}
}

// TestAdminSignatureAuth：管理面同样只认签名（URL 里不放明文管理 Key）。
func TestAdminSignatureAuth(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	now := time.Now().Unix()

	signed := account.SignAt(account.AdminPrefix, env.adminKey, now)
	if w := do(h, http.MethodGet, "/v1/admin/stats?key="+signed, nil); w.Code != http.StatusOK {
		t.Fatalf("管理签名应通过，得到 %d：%s", w.Code, w.Body.String())
	}
	// 管理面板的链接就是这样生成的：/v1/admin/sign 拿一条签名
	w := do(h, http.MethodGet, "/v1/admin/sign", userHdr(env.adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/admin/sign 应 200，得到 %d：%s", w.Code, w.Body.String())
	}
	var body struct {
		Key  string `json:"key"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "admin" || !strings.HasPrefix(body.Key, "adm_") {
		t.Fatalf("管理签发结果不对：type=%q key=%q", body.Type, body.Key)
	}
	if w := do(h, http.MethodGet, "/v1/admin/accounts?key="+body.Key, nil); w.Code != http.StatusOK {
		t.Errorf("签发的管理凭据应能用，得到 %d：%s", w.Code, w.Body.String())
	}
	// 反过来：账号 Key 拿不到管理签名，账号签名也进不了管理面
	if w := do(h, http.MethodGet, "/v1/admin/sign", userHdr(env.userKey)); w.Code != http.StatusForbidden {
		t.Errorf("账号 Key 不能签发管理签名，得到 %d", w.Code)
	}
	accSigned := account.SignAt(account.HandlePrefix, env.userKey, now)
	w = do(h, http.MethodGet, "/v1/admin/stats?key="+accSigned, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("账号签名用在管理面应 403，得到 %d", w.Code)
	}
	if kind, _, _ := errBody(t, w); kind != "key_scope" {
		t.Errorf("错误码 = %q，想要 key_scope", kind)
	}
	// 句柄对不上（签名合法但不是派生的那个 adm_ 句柄）→ 无效
	alien := account.SignAtWith(account.HandleID(account.AdminPrefix, "别的管理 Key"), env.adminKey, now)
	w = do(h, http.MethodGet, "/v1/admin/stats?key="+alien, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("句柄不匹配的管理签名应 403，得到 %d", w.Code)
	}
	if kind, _, _ := errBody(t, w); kind != "key_invalid" {
		t.Errorf("错误码 = %q，想要 key_invalid", kind)
	}
	// 过期的管理签名
	old := account.SignAt(account.AdminPrefix, env.adminKey, now-3600)
	w = do(h, http.MethodGet, "/v1/admin/stats?key="+old, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("过期管理签名应 403，得到 %d", w.Code)
	}
	if kind, _, _ := errBody(t, w); kind != "signature_expired" {
		t.Errorf("错误码 = %q，想要 signature_expired", kind)
	}
}

// TestPublicAccountCanSign：公共账号也能签名，且**不构成绕过**——
// 仍然按每 IP 日配额结算，而不是账本余额。
func TestPublicAccountCanSign(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	w := do(h, http.MethodGet, "/v1/sign", userHdr("vl_public"))
	if w.Code != http.StatusOK {
		t.Fatalf("公共账号应能签发，得到 %d：%s", w.Code, w.Body.String())
	}
	var body struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if w := do(h, http.MethodGet, "/v1/usage?"+body.Query, nil); w.Code != http.StatusOK {
		t.Fatalf("公共账号的签名应能用，得到 %d：%s", w.Code, w.Body.String())
	}
	if w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1&"+body.Query, nil); w.Code != http.StatusOK {
		t.Fatalf("公共账号的签名在计量端点应放行，得到 %d：%s", w.Code, w.Body.String())
	}
}

// TestUISignatureMatchesGo：页面里那份**手写的** HMAC-SHA256 必须与 Go 侧
// 算出完全一样的签名。
//
// 这是"前端自己算签名"这个设计唯一的单点风险：页面的 SHA-256 是纯 JS
// 实现（因为没有 crypto.subtle 的可用性保证），只要有一处常量、移位或
// 长度编码写错，用户看到的就是一片 403 而不知道原因。
// 所以这里把页面里的那段脚本抽出来交给 node，跑 SHA-256/HMAC 的标准向量
// 与项目黄金向量，并与 account.SignAt 的产物逐字符比对。
func TestUISignatureMatchesGo(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("环境里没有 node，跳过 JS 密码学交叉验证")
	}
	extract := func(html string) string {
		t.Helper()
		for _, b := range regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(html, -1) {
			if strings.Contains(b[1], "vl-crypto") {
				return b[1]
			}
		}
		t.Fatal("页面里找不到 vl-crypto 脚本块")
		return ""
	}
	uiBlock := extract(string(uiHTML))
	adminBlock := extract(string(adminHTML))
	// 两个页面必须用**同一份**实现：抄一份改一处是这类代码最典型的腐烂方式
	if uiBlock != adminBlock {
		t.Error("解析页与管理面板里的 vl-crypto 实现不一致，两份必须逐字符相同")
	}
	// 每页只能有一份：同名 const 在两个经典 <script> 里重复声明会让
	// 后一个块整个 SyntaxError（而且是在浏览器里才炸，node --check 单块看不出来）
	for name, html := range map[string]string{"ui.html": string(uiHTML), "admin.html": string(adminHTML)} {
		n := 0
		for _, b := range regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(html, -1) {
			if strings.Contains(b[1], "function vlHmacSha256Hex") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s 里有 %d 份 HMAC 实现，必须恰好 1 份", name, n)
		}
	}
	// 两页各自把"用哪把 Key、哪个前缀"写在自己的 signedQuery 里：
	// 前缀写错（账号页用了 adm_）只会在运行时表现为 403，静态钉住它。
	for name, tc := range map[string]struct{ html, want string }{
		"ui.html":    {string(uiHTML), `vlSignUrl("acc_", APIKEY, url)`},
		"admin.html": {string(adminHTML), `vlSignUrl("adm_", ADMINKEY, url)`},
	} {
		if !strings.Contains(tc.html, tc.want) {
			t.Errorf("%s 里的 signedQuery 应是 %s", name, tc.want)
		}
	}

	driver := `
const out = [];
out.push(vlSha256Hex("abc"));
out.push(vlSha256Hex(""));
out.push(vlHmacSha256Hex("Jefe", "what do ya want for nothing?"));
const rep = (b, n) => new Uint8Array(n).fill(b);
out.push(vlHex(vlHmacSha256(rep(0x0b, 20), vlUtf8("Hi There"))));
out.push(vlHex(vlHmacSha256(rep(0xaa, 131), vlUtf8("Test Using Larger Than Block-Size Key - Hash Key First"))));
out.push(vlCredentialAt("acc_", "vl_demo_key", 0x68a3e658));
out.push(vlSignUrl("acc_", "vl_demo_key", "/v1/usage", 0x68a3e658));
out.push(vlSignUrl("acc_", "", "/v1/usage", 123));
console.log(JSON.stringify(out));
`
	dir := t.TempDir()
	jsPath := filepath.Join(dir, "crypto.js")
	if err := os.WriteFile(jsPath, []byte(uiBlock+"\n"+driver), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, jsPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node 跑页面里的密码学实现失败：%v\n%s", err, out)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node 输出不是 JSON：%v\n%s", err, out)
	}
	golden := account.SignAt(account.HandlePrefix, "vl_demo_key", 0x68a3e658)
	want := []string{
		"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", // SHA-256("abc")
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // SHA-256("")
		"5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843", // RFC 4231 #2
		"b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7", // RFC 4231 #1
		"60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54", // RFC 4231 #6
		golden,                    // 与 Go 的 account.SignAt 同源
		"/v1/usage?key=" + golden, // URL 拼装
		"/v1/usage",               // 免校验模式：没有 Key 就不签
	}
	if len(got) != len(want) {
		t.Fatalf("node 返回 %d 项，想要 %d 项：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项不一致：\n  JS   = %s\n  Go/向量 = %s", i, got[i], want[i])
		}
	}
}

// TestFrontendsSendSignaturesNotPlainKeys：两个页面对外发出的凭据一律是签名。
//
// 明文 Key 只该存在于本机浏览器的输入框与 localStorage 里。一旦它被放进
// 请求头或 URL，就会出现在 DevTools 网络面板、反代日志与任何抓包里——
// 而它长期有效，泄漏一次就等于把账号交出去。签名只有几十秒。
func TestFrontendsSendSignaturesNotPlainKeys(t *testing.T) {
	cases := map[string]struct {
		html     string
		wantSign string
		badPlain []string
	}{
		"ui.html": {
			html:     string(uiHTML),
			wantSign: `vlCredentialAt("acc_", APIKEY`,
			badPlain: []string{`"X-API-Key": APIKEY`, `"X-API-Key"] = APIKEY`, `encodeURIComponent(APIKEY)`},
		},
		"admin.html": {
			html:     string(adminHTML),
			wantSign: `vlCredentialAt("adm_", ADMINKEY`,
			badPlain: []string{`"X-API-Key"] = ADMINKEY`, `"X-API-Key": ADMINKEY`, `encodeURIComponent(ADMINKEY)`},
		},
	}
	for name, tc := range cases {
		if !strings.Contains(tc.html, tc.wantSign) {
			t.Errorf("%s 里的请求凭据应是现算签名：找不到 %s", name, tc.wantSign)
		}
		for _, bad := range tc.badPlain {
			if strings.Contains(tc.html, bad) {
				t.Errorf("%s 里还有把明文 Key 放进请求/URL 的写法：%s", name, bad)
			}
		}
	}
}

// TestSignedCredentialWorksInHeader：签名既可以放 URL，也可以放请求头。
//
// 前端走的就是后者——这样明文 Key 根本不进网络。服务端按**形态**识别，
// 而不是"URL 里才允许是签名"。
func TestSignedCredentialWorksInHeader(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	now := time.Now().Unix()

	acc := account.SignAt(account.HandlePrefix, env.userKey, now)
	if w := do(h, http.MethodGet, "/v1/usage", userHdr(acc)); w.Code != http.StatusOK {
		t.Fatalf("账号签名放在 X-API-Key 头里应通过，得到 %d：%s", w.Code, w.Body.String())
	}
	if w := do(h, http.MethodGet, "/v1/usage", map[string]string{"Authorization": "Bearer " + acc}); w.Code != http.StatusOK {
		t.Fatalf("账号签名放在 Bearer 里应通过，得到 %d：%s", w.Code, w.Body.String())
	}
	adm := account.SignAt(account.AdminPrefix, env.adminKey, now)
	if w := do(h, http.MethodGet, "/v1/admin/stats", userHdr(adm)); w.Code != http.StatusOK {
		t.Fatalf("管理签名放在请求头里应通过，得到 %d：%s", w.Code, w.Body.String())
	}
	// URL 与头同时出现时头部优先：URL 里的过期签名不该把有效身份顶掉
	old := account.SignAt(account.HandlePrefix, env.userKey, now-3600)
	if w := do(h, http.MethodGet, "/v1/usage?key="+old, userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Fatalf("头部有效明文时不该被 URL 里的过期签名影响，得到 %d", w.Code)
	}
	// 但 URL 里放明文 Key 依然要拒（哪怕头部没带）
	if w := do(h, http.MethodGet, "/v1/usage?key="+env.userKey, nil); w.Code != http.StatusForbidden {
		t.Errorf("URL 里放明文 Key 应 403，得到 %d", w.Code)
	}
}

// --- 媒体代理的按体积配额 ---

// TestProxyChargesBySize：代理按传输体积扣配额（统一费率 0.5 配额/MiB × 账号倍率）。
//
// 这是代理与其它端点的根本区别：解析端点按"端点 × 平台"定价，
// 而代理搬的是任意 CDN 的字节，成本只与体积线性相关。
func TestProxyChargesBySize(t *testing.T) {
	const size = 3 << 20 // 3 MiB
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = w.Write(make([]byte, size))
	}))
	defer upstream.Close()

	env := newTestEnv(t, func(c *config.Config) { c.ProxySrv.Enabled = true }, defaultStub())
	h := env.handler()

	// ① 倍率 1.0：3 MiB → 3 配额
	w := do(h, http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL), userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("代理应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Quota-Consumed"); got != "1.5" {
		t.Errorf("X-Quota-Consumed = %q，想要 1.5（3 MiB × 0.5）", got)
	}
	if got := w.Header().Get("X-Quota-Remaining"); got != "98.5" {
		t.Errorf("X-Quota-Remaining = %q，想要 98.5", got)
	}
	if got, _ := env.store.Get(env.userKey); got.Quota != 98.5 || got.Used != 1.5 || got.Calls != 1 {
		t.Errorf("账本 = quota %v / used %v / calls %v，想要 98.5 / 1.5 / 1", got.Quota, got.Used, got.Calls)
	}

	// ② 倍率 0.5：同样的 3 MiB → 0.75 配额（账号倍率生效）
	half := 0.5
	if _, err := env.store.Update(env.userKey, account.Patch{Multiplier: &half}); err != nil {
		t.Fatal(err)
	}
	w = do(h, http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL), userHdr(env.userKey))
	if got := w.Header().Get("X-Quota-Consumed"); got != "0.75" {
		t.Errorf("半倍率下 X-Quota-Consumed = %q，想要 0.75", got)
	}
	if got, _ := env.store.Get(env.userKey); got.Quota != 97.75 {
		t.Errorf("配额 = %v，想要 97.75", got.Quota)
	}

	// ③ 平台系数不参与：换一个"平台"（这里用抖音的解析端点系数 1.1）
	//    作对照，代理的计价必须与它无关 —— 3 MiB 仍是 1.5（倍率 0.5）。
	if got := quota.ProxyCost(size, 0.5); got != 0.75 {
		t.Errorf("quota.ProxyCost(3MiB, 0.5) = %v，想要 0.75", got)
	}
	if got := quota.ProxyCost(size, 1.1); got != 1.65 {
		t.Errorf("quota.ProxyCost 只该乘账号倍率：%v", got)
	}
}

// TestProxyRangeChargesOnlyTheRange：Range 请求只按这一段计费。
//
// 若拿 Content-Range 里的**总长**去扣，播放器每拖一次进度条就会被按
// 整部片子收费（实测 1KB 的 seek 会被扣 67 配额）。
func TestProxyRangeChargesOnlyTheRange(t *testing.T) {
	const total, chunk = 64 << 20, 1024
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			t.Errorf("Range 没有被透传")
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", chunk-1, total))
		w.Header().Set("Content-Length", strconv.Itoa(chunk))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, chunk))
	}))
	defer upstream.Close()

	env := newTestEnv(t, func(c *config.Config) { c.ProxySrv.Enabled = true }, defaultStub())
	r := httptest.NewRequest(http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL), nil)
	r.Header.Set("X-API-Key", env.userKey)
	r.Header.Set("Range", "bytes=0-1023")
	w := httptest.NewRecorder()
	env.handler().ServeHTTP(w, r)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("应透传 206，得到 %d", w.Code)
	}
	// 1024 字节 ≈ 0.00098 MiB × 0.5 → round4 到 0.0005（精度到万分之一配额）
	if got := w.Header().Get("X-Quota-Consumed"); got != "0.0005" {
		t.Errorf("X-Quota-Consumed = %q，想要 0.0005（按 1KB 而不是 64MB）", got)
	}
	if got, _ := env.store.Get(env.userKey); got.Quota < 99.99 {
		t.Errorf("配额 = %v，不该按整部片子扣", got.Quota)
	}
}

// TestProxyQuotaExhaustedIsRejected：配额不够时**一个字节都不发**。
func TestProxyQuotaExhaustedIsRejected(t *testing.T) {
	const size = 4 << 20 // 4 MiB → 4 配额，而账户只剩 1
	served := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = w.Write(make([]byte, size))
	}))
	defer upstream.Close()

	env := newTestEnv(t, func(c *config.Config) { c.ProxySrv.Enabled = true }, defaultStub())
	if _, err := env.store.Update(env.userKey, account.Patch{Quota: floatPtr(1)}); err != nil {
		t.Fatal(err)
	}

	w := do(env.handler(), http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL), userHdr(env.userKey))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("配额不足应 429，得到 %d（%s）", w.Code, w.Body.String())
	}
	if kind, msg, _ := errBody(t, w); kind != "quota_exhausted" || !strings.Contains(msg, "配额不足") {
		t.Errorf("kind=%q msg=%q", kind, msg)
	}
	if strings.Contains(w.Body.String(), "\x00") || w.Body.Len() >= size {
		t.Error("配额不足时不该发出任何媒体字节")
	}
	if got, _ := env.store.Get(env.userKey); got.Quota != 1 {
		t.Errorf("配额不足时不该扣减，得到 %v", got.Quota)
	}
	_ = served
}

// TestProxyHeadIsNotCharged：HEAD 没有响应体，不计费也不回配额头。
func TestProxyHeadIsNotCharged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(8<<20))
	}))
	defer upstream.Close()

	env := newTestEnv(t, func(c *config.Config) { c.ProxySrv.Enabled = true }, defaultStub())
	w := do(env.handler(), http.MethodHead, "/v1/proxy?url="+url.QueryEscape(upstream.URL), userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD 应 200，得到 %d", w.Code)
	}
	if got := w.Header().Get("X-Quota-Consumed"); got != "" {
		t.Errorf("HEAD 不该计费，得到 X-Quota-Consumed=%q", got)
	}
	if got, _ := env.store.Get(env.userKey); got.Quota != 100 {
		t.Errorf("HEAD 不该扣配额，得到 %v", got.Quota)
	}
}

// TestProxyUnknownLengthChargesActual：上游不给长度（chunked）时，
// 按实际发出的字节补扣——这是兜底路径，但也不能白送。
func TestProxyUnknownLengthChargesActual(t *testing.T) {
	const size = 2 << 20
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 不设 Content-Length：Go 会自动改用 chunked，代理侧拿到 ContentLength=-1
		buf := make([]byte, 64<<10)
		for sent := 0; sent < size; sent += len(buf) {
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	env := newTestEnv(t, func(c *config.Config) { c.ProxySrv.Enabled = true }, defaultStub())
	w := do(env.handler(), http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL), userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", w.Code)
	}
	if got, _ := env.store.Get(env.userKey); got.Used != 1 {
		t.Errorf("未知长度时按实际字节扣：used=%v，想要 1（2 MiB × 0.5）", got.Used)
	}
}

// TestProxyEaseModeChargesNothing：免校验模式没有账户，代理照常但不计费。
func TestProxyEaseModeChargesNothing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		_, _ = w.Write(make([]byte, 1024))
	}))
	defer upstream.Close()

	env := newEaseEnv(t, func(c *config.Config) { c.ProxySrv.Enabled = true }, defaultStub())
	w := do(env.handler(), http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", w.Code)
	}
	if got := w.Header().Get("X-Quota-Consumed"); got != "" {
		t.Errorf("免校验模式不该回写配额头，得到 %q", got)
	}
}

func floatPtr(f float64) *float64 { return &f }

// TestAdminPanelShowsProxyPricing：管理面板要显示代理的计费口径。
//
// 代理是唯一不乘平台系数的出口；面板不显示它，管理员核对用量时
// 只会看到 used 在涨、却对不上任何一条系数。
func TestAdminPanelShowsProxyPricing(t *testing.T) {
	body := string(adminHTML)
	for _, want := range []string{"d.proxy", "媒体代理", "platform_factor", "unit_bytes"} {
		if !strings.Contains(body, want) {
			t.Errorf("管理面板缺少 %q", want)
		}
	}
}

// --- 配额流水（账单）接口 ---

// TestLedgerSelfService：用户读自己的流水，读不到别人的，也不该被计量。
func TestLedgerSelfService(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	if w := do(h, http.MethodGet, "/v1/ledger", nil); w.Code != http.StatusForbidden {
		t.Fatalf("不带 Key 读流水应 403，得到 %d", w.Code)
	}
	// 先用这个 Key 解析一次，制造一条"使用"流水
	if w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Fatalf("解析应 200，得到 %d", w.Code)
	}
	before, _ := env.store.Get(env.userKey)

	w := do(h, http.MethodGet, "/v1/ledger", userHdr(env.userKey))
	if w.Code != http.StatusOK {
		t.Fatalf("读自己的流水应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Quota-Consumed"); got != "" {
		t.Errorf("读流水不该消耗配额，却回写了 X-Quota-Consumed=%q", got)
	}
	var resp struct {
		Scope   string           `json:"scope"`
		Account map[string]any   `json:"account"`
		Entries []map[string]any `json:"entries"`
		Totals  map[string]struct {
			Count int64   `json:"count"`
			Units float64 `json:"units"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Scope != "self" {
		t.Errorf("scope = %q，想要 self", resp.Scope)
	}
	if strings.Contains(w.Body.String(), env.userKey) {
		t.Error("流水里不该出现明文 Key")
	}
	if len(resp.Entries) < 2 { // create + consume
		t.Fatalf("流水至少应有建号与一次使用，得到 %d 条", len(resp.Entries))
	}
	newest := resp.Entries[0]
	if newest["type"] != "consume" {
		t.Errorf("最新一条应为 consume，得到 %v", newest["type"])
	}
	if got, _ := newest["detail"].(string); !strings.Contains(got, "links/bilibili") {
		t.Errorf("消耗流水的说明应写明端点与平台，得到 %q", got)
	}
	if tot := resp.Totals["consume"]; tot.Count != 1 || tot.Units != -1 {
		t.Errorf("consume 汇总 = %+v，想要 1 次 / -1", tot)
	}

	// 筛选与参数校验
	w = do(h, http.MethodGet, "/v1/ledger?type=consume&limit=1", userHdr(env.userKey))
	var one struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if len(one.Entries) != 1 || one.Entries[0]["type"] != "consume" {
		t.Errorf("type/limit 筛选没生效：%v", one.Entries)
	}
	for _, bad := range []string{"?limit=abc", "?limit=0", "?type=bogus"} {
		if w := do(h, http.MethodGet, "/v1/ledger"+bad, userHdr(env.userKey)); w.Code != http.StatusBadRequest {
			t.Errorf("%s 应 400，得到 %d", bad, w.Code)
		}
	}
	after, _ := env.store.Get(env.userKey)
	if after.Quota != before.Quota || after.Used != before.Used {
		t.Errorf("读流水不该动账本：before=%v/%v after=%v/%v", before.Quota, before.Used, after.Quota, after.Used)
	}
}

// TestAdminLedgerTotalAndScoped：管理员能读总账单，也能按句柄/明文 Key 读单个账号。
func TestAdminLedgerTotalAndScoped(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	if w := do(h, http.MethodGet, "/v1/admin/ledger", userHdr(env.userKey)); w.Code != http.StatusForbidden {
		t.Fatalf("普通账号读管理流水应 403，得到 %d", w.Code)
	}
	// 制造一条使用记录（用户账号）
	if w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Fatalf("解析应 200，得到 %d", w.Code)
	}

	w := do(h, http.MethodGet, "/v1/admin/ledger", userHdr(env.adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("总账单应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	var all struct {
		Scope   string           `json:"scope"`
		Entries []map[string]any `json:"entries"`
		Totals  map[string]struct {
			Count int64   `json:"count"`
			Units float64 `json:"units"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if all.Scope != "all" {
		t.Errorf("scope = %q，想要 all", all.Scope)
	}
	if all.Totals["consume"].Count != 1 {
		t.Errorf("总账单应含 1 次使用，得到 %+v", all.Totals["consume"])
	}
	if len(all.Entries) < 2 {
		t.Fatalf("总账单流水太少：%d", len(all.Entries))
	}

	// 按句柄读单个账号
	user, _ := env.store.Get(env.userKey)
	w = do(h, http.MethodGet, "/v1/admin/ledger?id="+user.ID, userHdr(env.adminKey))
	if w.Code != http.StatusOK {
		t.Fatalf("按句柄读应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	var scoped struct {
		Scope   string           `json:"scope"`
		Account map[string]any   `json:"account"`
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &scoped); err != nil {
		t.Fatal(err)
	}
	if scoped.Scope != "account" {
		t.Errorf("scope = %q，想要 account", scoped.Scope)
	}
	if scoped.Account["id"] != user.ID {
		t.Errorf("account.id = %v，想要 %v", scoped.Account["id"], user.ID)
	}
	for _, e := range scoped.Entries {
		if e["id"] != user.ID {
			t.Errorf("按账号筛选后混进了别的账号：%v", e["id"])
		}
	}
	// 明文 Key 也能寻址（运维手上通常就是 Key）；参数名是 account（或 id），
	// 不能是 key —— 那个名字现在是"凭据"，URL 里只收签名。
	if w := do(h, http.MethodGet, "/v1/admin/ledger?account="+env.userKey, userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Errorf("按明文 Key 读应 200，得到 %d", w.Code)
	}
	// 不存在的账号 → 404；类型筛选照旧生效
	if w := do(h, http.MethodGet, "/v1/admin/ledger?id=acc_不存在", userHdr(env.adminKey)); w.Code != http.StatusNotFound {
		t.Errorf("不存在的句柄应 404，得到 %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/v1/admin/ledger?type=create", userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Errorf("按类型筛选应 200，得到 %d", w.Code)
	}
}

// TestEaseModeHidesLedger：免校验模式没有账户，流水接口也不存在。
func TestEaseModeHidesLedger(t *testing.T) {
	env := newEaseEnv(t, nil, defaultStub())
	for _, path := range []string{"/v1/ledger", "/v1/admin/ledger"} {
		if w := do(env.handler(), http.MethodGet, path, nil); w.Code != http.StatusNotFound {
			t.Errorf("免校验模式 %s 应 404，得到 %d", path, w.Code)
		}
	}
}

// TestLedgerUIWiring：两个页面都要有流水入口，且调用的是真实存在的路由。
func TestLedgerUIWiring(t *testing.T) {
	ui := string(uiHTML)
	for _, want := range []string{
		`id="ledgersec"`, `id="ledgerType"`, `id="ledgerRefresh"`, `loadLedger`,
		`"/v1/ledger" + qs`,
		// 各端点系数表里必须有代理那一行（口径与平台系数不同）
		`媒体代理`, `配额/MiB`,
		// 签到按钮常驻在「刷新用量」旁边，状态判定走纯函数 checkinState
		`id="checkinBtn"`, `checkinState(`, `"强制签到"`, `"可否签到"`,
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("解析页缺少 %q", want)
		}
	}
	// 按钮不许再 hidden/disabled：不可签时是「强制签到」，不是消失或变灰
	if strings.Contains(ui, `class="primary hidden" id="checkinBtn"`) {
		t.Error("签到按钮不该带 hidden：不可签时应显示为「强制签到」")
	}
	if strings.Contains(ui, `cb.disabled = !!`) || strings.Contains(ui, `cbtn.disabled = !!`) {
		t.Error("签到按钮不该被禁用：状态可能是旧的，点一下让服务端给权威答复")
	}
	admin := string(adminHTML)
	for _, want := range []string{
		`id="ledgersec"`, `id="ledgerWho"`, `id="ledgerLoad"`, `loadLedger`,
		`"/v1/admin/ledger" + qs`,
		`&account=`, // 过滤器参数名是 account，不能是 key（key 现在是"凭据"）
	} {
		if !strings.Contains(admin, want) {
			t.Errorf("管理面板缺少 %q", want)
		}
	}
	// 流水过滤器里不许再出现 ?key=：那个名字现在只意味着"URL 里的签名凭据"
	if strings.Contains(admin, `qs += "&key="`) {
		t.Error("管理面板的流水过滤器还在用 &key=，应改成 &account=")
	}
}

// --- 公共 Key（免费试用） ---

// TestEmptyKeyHintsPublicKey：没带 Key 时要直接告诉对方可以用公共 Key。
//
// "缺少 API Key" 会让人以为必须先注册；而这里本来就有一个免注册入口，
// 把 Key 写进错误信息里是最省事的一次转化。
func TestEmptyKeyHintsPublicKey(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	w := do(env.handler(), http.MethodGet, "/v1/links?url=https://stub.test/v/1", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("无 Key 应 403，得到 %d", w.Code)
	}
	_, msg, _ := errBody(t, w)
	if !strings.Contains(msg, "vl_public") || !strings.Contains(msg, "免费") {
		t.Errorf("提示里应给出公共 Key 与免费字样，得到 %q", msg)
	}

	// 关掉公共入口（PublicKey 为空）时，提示回到通用版本
	off := newTestEnv(t, func(c *config.Config) { c.PublicKey = "" }, defaultStub())
	w = do(off.handler(), http.MethodGet, "/v1/links?url=https://stub.test/v/1", nil)
	if _, msg, _ := errBody(t, w); strings.Contains(msg, "vl_public") {
		t.Errorf("未开启公共入口时不该提它：%q", msg)
	}
}

// TestPublicKeyUsesPerIPDailyQuota：公共 Key 的额度是"每 IP 每天 100"，
// 与账本余额无关，且不同 IP 各记各的。
func TestPublicKeyUsesPerIPDailyQuota(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	// ① 公共 Key 能用：扣的是每 IP 额度，账本余额（0）不参与
	w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.srv.cfg.PublicKey))
	if w.Code != http.StatusOK {
		t.Fatalf("公共 Key 应可用，得到 %d（%s）", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Quota-Consumed"); got != "1" {
		t.Errorf("X-Quota-Consumed = %q，想要 1", got)
	}
	if got := w.Header().Get("X-Quota-Remaining"); got != "24" {
		t.Errorf("X-Quota-Remaining = %q，想要 24（每 IP 每日 25 扣掉 1）", got)
	}
	pub, ok := env.store.Get(env.srv.cfg.PublicKey)
	if !ok || !pub.PublicAccount {
		t.Fatal("公共账号应存在且带 public 标记")
	}
	if pub.Quota != 0 {
		t.Errorf("公共账号不该用账本余额，得到 %v", pub.Quota)
	}
	if pub.Used != 1 || pub.Calls != 1 {
		t.Errorf("公共账号的用量与调用次数仍应累计：used=%v calls=%v", pub.Used, pub.Calls)
	}

	// ② 换一个 IP：额度独立
	r := httptest.NewRequest(http.MethodGet, "/v1/links?url=https://stub.test/v/1", nil)
	r.Header.Set("X-API-Key", env.srv.cfg.PublicKey)
	r.RemoteAddr = "203.0.113.9:5555"
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r)
	if w2.Code != http.StatusOK {
		t.Fatalf("另一个 IP 应可用，得到 %d", w2.Code)
	}
	if got := w2.Header().Get("X-Quota-Remaining"); got != "24" {
		t.Errorf("新 IP 的剩余额度应重新从 25 开始，得到 %q", got)
	}

	// ③ 额度用尽 → 429 public_quota_exhausted（同一个 IP 连续调用）
	r3 := httptest.NewRequest(http.MethodGet, "/v1/links?url=https://stub.test/v/1", nil)
	r3.Header.Set("X-API-Key", env.srv.cfg.PublicKey)
	r3.RemoteAddr = "198.51.100.7:1111"
	h.ServeHTTP(httptest.NewRecorder(), r3) // 先花掉 1
	// 直接把额度打满
	for i := 0; i < 24; i++ {
		rr := httptest.NewRequest(http.MethodGet, "/v1/links?url=https://stub.test/v/1", nil)
		rr.Header.Set("X-API-Key", env.srv.cfg.PublicKey)
		rr.RemoteAddr = "198.51.100.7:1111"
		h.ServeHTTP(httptest.NewRecorder(), rr)
	}
	rr := httptest.NewRequest(http.MethodGet, "/v1/links?url=https://stub.test/v/1", nil)
	rr.Header.Set("X-API-Key", env.srv.cfg.PublicKey)
	rr.RemoteAddr = "198.51.100.7:1111"
	wr := httptest.NewRecorder()
	h.ServeHTTP(wr, rr)
	if wr.Code != http.StatusTooManyRequests {
		t.Fatalf("额度用尽应 429，得到 %d（%s）", wr.Code, wr.Body.String())
	}
	if kind, msg, _ := errBody(t, wr); kind != "public_quota_exhausted" ||
		!strings.Contains(msg, "免费额度") || !strings.Contains(msg, "独立 Key") {
		t.Errorf("kind=%q msg=%q", kind, msg)
	}
}

// TestPublicUsageAndNoLedger：公共 Key 的用量是"本 IP 今天"的口径，且不给账单。
func TestPublicUsageAndNoLedger(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	w := do(h, http.MethodGet, "/v1/usage", userHdr(env.srv.cfg.PublicKey))
	if w.Code != http.StatusOK {
		t.Fatalf("公共 Key 查用量应 200，得到 %d", w.Code)
	}
	var u map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if u["public"] != true {
		t.Errorf("usage 应标明 public=true：%v", u["public"])
	}
	if _, ok := u["daily"]; !ok {
		t.Error("usage 应带 daily（本 IP 今日额度）")
	}
	if strings.Contains(w.Body.String(), "bill") {
		t.Error("公共账号不该出现账单字样")
	}

	// 公共 Key 读账单：403 且说明原因
	wt := do(h, http.MethodGet, "/v1/ledger", userHdr(env.srv.cfg.PublicKey))
	if wt.Code != http.StatusForbidden {
		t.Fatalf("公共 Key 读账单应 403，得到 %d（%s）", wt.Code, wt.Body.String())
	}
	if _, msg, _ := errBody(t, wt); !strings.Contains(msg, "独立 Key") {
		t.Errorf("应提示去申请独立 Key，得到 %q", msg)
	}
	// 普通账号照旧能读自己的账单（这条是上一轮做的功能，别被公共 Key 的规则带偏）
	if wp := do(h, http.MethodGet, "/v1/ledger", userHdr(env.userKey)); wp.Code != http.StatusOK {
		t.Errorf("普通账号读自己的账单应 200，得到 %d", wp.Code)
	}
}

// TestPublicKeyCanUseProxyFromDailyQuota：公共 Key 允许走代理，按体积从每日额度里扣。
func TestPublicKeyCanUseProxyFromDailyQuota(t *testing.T) {
	const size = 2 << 20 // 2 MiB → 2 配额
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = w.Write(make([]byte, size))
	}))
	defer upstream.Close()

	env := newTestEnv(t, func(c *config.Config) { c.ProxySrv.Enabled = true }, defaultStub())
	w := do(env.handler(), http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL),
		userHdr(env.srv.cfg.PublicKey))
	if w.Code != http.StatusOK {
		t.Fatalf("公共 Key 代理应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	// 统一费率：2 MiB × 0.5 = 1 配额
	if got := w.Header().Get("X-Quota-Consumed"); got != "1" {
		t.Errorf("X-Quota-Consumed = %q，想要 1（2 MiB × 0.5）", got)
	}
	if got := w.Header().Get("X-Quota-Remaining"); got != "24" {
		t.Errorf("X-Quota-Remaining = %q，想要 24", got)
	}
	if pub, _ := env.store.Get(env.srv.cfg.PublicKey); pub.Used != 1 {
		t.Errorf("公共账号用量应累计为 1，得到 %v", pub.Used)
	}
	// **所有账号同一费率**：普通账号同一份流量扣得一样多
	other := newTestEnv(t, func(c *config.Config) { c.ProxySrv.Enabled = true }, defaultStub())
	wo := do(other.handler(), http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL),
		userHdr(other.userKey))
	if got := wo.Header().Get("X-Quota-Consumed"); got != "1" {
		t.Errorf("普通账号 X-Quota-Consumed = %q，想要 1（与公共 Key 同价）", got)
	}
}

// TestHealthExposesPublicKey：健康检查要告诉页面公共入口怎么用。
func TestHealthExposesPublicKey(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	w := do(env.handler(), http.MethodGet, "/v1/health", nil)
	var h map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatal(err)
	}
	pub, ok := h["public"].(map[string]any)
	if !ok {
		t.Fatalf("health 应带 public 段：%s", w.Body.String())
	}
	if pub["key"] != env.srv.cfg.PublicKey || pub["daily_per_ip"] != float64(25) {
		t.Errorf("public 段内容不对：%v", pub)
	}
	// 免校验模式没有账户体系，不该报公共入口
	ease := newEaseEnv(t, nil, defaultStub())
	we := do(ease.handler(), http.MethodGet, "/v1/health", nil)
	if strings.Contains(we.Body.String(), `"public"`) {
		t.Error("免校验模式不该出现 public 段")
	}
}

// TestTipImageIsPublicAndEmbedded：赞赏码是公开静态资源，且页面引用它。
func TestTipImageIsPublicAndEmbedded(t *testing.T) {
	if len(tipPNG) == 0 || len(tipPNG) > 200*1024 {
		t.Fatalf("内嵌的赞赏码大小不合适：%d 字节", len(tipPNG))
	}
	env := newTestEnv(t, func(c *config.Config) { c.WebUI = true }, defaultStub())
	w := do(env.handler(), http.MethodGet, tipPath, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%s 应公开可访问，得到 %d", tipPath, w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q，想要 image/png", ct)
	}
	if w.Body.Len() != len(tipPNG) {
		t.Errorf("返回字节数 = %d，想要 %d", w.Body.Len(), len(tipPNG))
	}
	// 页面里要有折叠的赞赏码区块，且不是外链
	page := string(uiHTML)
	if !strings.Contains(page, `src="/tip.png"`) || !strings.Contains(page, "赞赏") {
		t.Error("解析页应有赞赏码区块并引用 /tip.png")
	}
	if !strings.Contains(page, `id="tipsec"`) {
		t.Error("赞赏码应放在默认折叠的 details 里")
	}
}

// TestCheckInEndpoint：每日签到接口。
//
//   - 每天一次：第二次返回 already_checked_in，不重复加
//   - 不改余额以外的东西，也不消耗配额（它是来领配额的）
//   - 公共 Key 不需要签到（它的额度是每 IP 每日）
//   - 未开放签到的账号明确报错
func TestCheckInEndpoint(t *testing.T) {
	grant, cap := 25.0, 60.0
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	if _, err := env.store.Update(env.userKey, account.Patch{
		Quota: floatPtr(10), DailyGrant: &grant, GrantCap: &cap,
	}); err != nil {
		t.Fatal(err)
	}

	type resp struct {
		Granted   float64 `json:"granted"`
		Balance   float64 `json:"balance"`
		Already   bool    `json:"already_checked_in"`
		AtCap     bool    `json:"at_cap"`
		CheckedIn bool    `json:"checked_in"`
		Message   string  `json:"message"`
	}
	post := func(key string) (*httptest.ResponseRecorder, resp) {
		w := do(h, http.MethodPost, "/v1/checkin", userHdr(key))
		var r resp
		_ = json.Unmarshal(w.Body.Bytes(), &r)
		return w, r
	}

	// ① 首次签到：10 → 35
	w, r := post(env.userKey)
	if w.Code != http.StatusOK || r.Granted != 25 || r.Balance != 35 || !r.CheckedIn {
		t.Fatalf("首次签到 = %d %+v（%s）", w.Code, r, w.Body.String())
	}
	if got := w.Header().Get("X-Quota-Consumed"); got != "" {
		t.Errorf("签到不该消耗配额，却回写了 X-Quota-Consumed=%q", got)
	}
	// ② 同日再签：already
	_, r = post(env.userKey)
	if !r.Already || r.Granted != 0 {
		t.Errorf("第二次签到应 already：%+v", r)
	}
	if a, _ := env.store.Get(env.userKey); a.Quota != 35 {
		t.Errorf("重复签到不该改余额：%v", a.Quota)
	}
	// ③ 上限截断：把余额调到 50，模拟跨天（直接改 GrantDay）
	if _, err := env.store.Update(env.userKey, account.Patch{Quota: floatPtr(50)}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.Update(env.userKey, account.Patch{Name: ptrStr("换个名字绕过当天限制")}); err != nil {
		t.Fatal(err)
	}
	// 直接把 GrantDay 清掉（等价于"到了第二天"）
	env.store.ClearCheckInDay(env.userKey)
	_, r = post(env.userKey)
	if r.Granted != 10 || r.Balance != 60 {
		t.Errorf("应被界限截断成 +10 → 60：%+v", r)
	}
	// ④ 已达界限：不增加、也不占当天机会
	if _, err := env.store.Update(env.userKey, account.Patch{Quota: floatPtr(60)}); err != nil {
		t.Fatal(err)
	}
	env.store.ClearCheckInDay(env.userKey)
	_, r = post(env.userKey)
	if !r.AtCap || r.Granted != 0 || !strings.Contains(r.Message, "界限") {
		t.Errorf("已达界限应 at_cap：%+v", r)
	}

	// ⑤ 公共 Key：明确拒绝并说明原因
	if w, _ := post(env.srv.cfg.PublicKey); w.Code != http.StatusForbidden {
		t.Errorf("公共 Key 签到应 403，得到 %d（%s）", w.Code, w.Body.String())
	}
	// ⑥ 未开放签到的账号：400 + 指路
	if _, err := env.store.Update(env.userKey, account.Patch{DailyGrant: floatPtr(0)}); err != nil {
		t.Fatal(err)
	}
	env.store.ClearCheckInDay(env.userKey)
	if w, _ := post(env.userKey); w.Code != http.StatusBadRequest {
		t.Errorf("未开放签到应 400，得到 %d（%s）", w.Code, w.Body.String())
	}
	// ⑦ 没有 Key → 403
	if w := do(h, http.MethodPost, "/v1/checkin", nil); w.Code != http.StatusForbidden {
		t.Errorf("无 Key 签到应 403，得到 %d", w.Code)
	}
	// ⑧ usage 里要说清签到口径与今天签没签
	if _, err := env.store.Update(env.userKey, account.Patch{DailyGrant: &grant}); err != nil {
		t.Fatal(err)
	}
	uw := do(h, http.MethodGet, "/v1/usage", userHdr(env.userKey))
	var u map[string]any
	if err := json.Unmarshal(uw.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	ci, ok := u["checkin"].(map[string]any)
	if !ok {
		t.Fatalf("usage 应有 checkin 段：%s", uw.Body.String())
	}
	if ci["daily"] != 25.0 || ci["endpoint"] != "POST /v1/checkin" {
		t.Errorf("checkin 段内容不对：%v", ci)
	}
}

// TestEaseModeHidesCheckIn：免校验模式没有账户，签到接口也不存在。
func TestEaseModeHidesCheckIn(t *testing.T) {
	env := newEaseEnv(t, nil, defaultStub())
	if w := do(env.handler(), http.MethodPost, "/v1/checkin", nil); w.Code != http.StatusNotFound {
		t.Errorf("免校验模式签到应 404，得到 %d", w.Code)
	}
}

func ptrStr(s string) *string { return &s }

// TestCheckInIsNeverAutomatic：**只有签到会加配额**。
//
// 上一版曾做成"每天第一次调用时自动补额"，这条用例把它钉死：
// 普通解析、代理、用量查询都不会动 GrantDay，也不会多给配额——
// 加配额只发生在 POST /v1/checkin。
func TestCheckInIsNeverAutomatic(t *testing.T) {
	grant, cap := 25.0, 100.0
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	if _, err := env.store.Update(env.userKey, account.Patch{
		Quota: floatPtr(0), DailyGrant: &grant, GrantCap: &cap,
	}); err != nil {
		t.Fatal(err)
	}

	// 余额 0：普通解析应该 429（预授权拦下），而不是"先自动补额再放行"
	w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("余额 0 且未签到时解析应 429，得到 %d（%s）", w.Code, w.Body.String())
	}
	if a, _ := env.store.Get(env.userKey); a.Quota != 0 || a.GrantDay != "" {
		t.Fatalf("没签到就不该加配额：quota=%v grant_day=%q", a.Quota, a.GrantDay)
	}

	// 用量查询、账单、代理也不会触发补额
	do(h, http.MethodGet, "/v1/usage", userHdr(env.userKey))
	do(h, http.MethodGet, "/v1/ledger", userHdr(env.userKey))
	if a, _ := env.store.Get(env.userKey); a.Quota != 0 || a.GrantDay != "" {
		t.Fatalf("读接口不该加配额：quota=%v grant_day=%q", a.Quota, a.GrantDay)
	}

	// 签到之后余额才有了，随后解析才走得通
	if w := do(h, http.MethodPost, "/v1/checkin", userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Fatalf("签到应 200，得到 %d", w.Code)
	}
	if a, _ := env.store.Get(env.userKey); a.Quota != 25 || a.GrantDay == "" {
		t.Fatalf("签到后应有 25 配额与签到日期：%+v", a)
	}
	if w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Fatalf("签到后解析应 200，得到 %d", w.Code)
	}
	// 解析只扣 1，不会因为"今天第一次用"再多给
	if a, _ := env.store.Get(env.userKey); a.Quota != 24 {
		t.Errorf("解析后余额应为 24，得到 %v", a.Quota)
	}
}

// TestUsageAlwaysReportsCheckInAvailability：账号信息接口必须明确"可否签到"。
func TestUsageAlwaysReportsCheckInAvailability(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()

	// ① 未开放签到：checkin.enabled = false（而不是没有这个字段）
	w := do(h, http.MethodGet, "/v1/usage", userHdr(env.userKey))
	var u map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	ci, ok := u["checkin"].(map[string]any)
	if !ok {
		t.Fatalf("无论如何都要有 checkin 段：%s", w.Body.String())
	}
	if ci["enabled"] != false {
		t.Errorf("未开放签到时应 enabled=false，得到 %v", ci["enabled"])
	}

	// ② 开放签到：enabled=true，并带今天签没签
	// 余额要低于界限，否则签到会正确地返回 at_cap（那是另一条用例）
	grant, cap := 25.0, 100.0
	if _, err := env.store.Update(env.userKey, account.Patch{
		Quota: floatPtr(50), DailyGrant: &grant, GrantCap: &cap,
	}); err != nil {
		t.Fatal(err)
	}
	w = do(h, http.MethodGet, "/v1/usage", userHdr(env.userKey))
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	ci, _ = u["checkin"].(map[string]any)
	if ci["enabled"] != true || ci["daily"] != 25.0 || ci["cap"] != 100.0 {
		t.Errorf("checkin 段内容不对：%v", ci)
	}
	if ci["checked_in_today"] != false {
		t.Errorf("还没签到，checked_in_today 应为 false：%v", ci)
	}
	// 签一次之后 usage 要反映出来
	do(h, http.MethodPost, "/v1/checkin", userHdr(env.userKey))
	w = do(h, http.MethodGet, "/v1/usage", userHdr(env.userKey))
	_ = json.Unmarshal(w.Body.Bytes(), &u)
	ci, _ = u["checkin"].(map[string]any)
	if ci["checked_in_today"] != true {
		t.Errorf("签到后 checked_in_today 应为 true：%v", ci)
	}

	// ③ 管理面：账号视图直接给 can_check_in
	aw := do(h, http.MethodGet, "/v1/admin/accounts", userHdr(env.adminKey))
	if !strings.Contains(aw.Body.String(), `"can_check_in":true`) {
		t.Errorf("管理账号视图应带 can_check_in：%s", aw.Body.String())
	}
	off := true
	if _, err := env.store.Update(env.userKey, account.Patch{Disabled: &off}); err != nil {
		t.Fatal(err)
	}
	aw = do(h, http.MethodGet, "/v1/admin/accounts", userHdr(env.adminKey))
	if !strings.Contains(aw.Body.String(), `"can_check_in":false`) {
		t.Errorf("停用账号的 can_check_in 应为 false：%s", aw.Body.String())
	}
}

// checkExternalLinks 校验页面里的可点击外链只有 github.com 一个域，
// 且都带了 rel="noopener"。
//
// 页面承诺"不加载任何外部资源"，但**允许**指向项目仓库与文档的外链
// （用户要求把开源信息与接口文档放在页面上）。两者的区别是"加载"与
// "点开才走"——脚本、样式、图片、字体一律不许外链，超链接可以。
func checkExternalLinks(t *testing.T, body string) {
	t.Helper()
	re := regexp.MustCompile(`href="(https?://[^"]+)"`)
	found := 0
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		found++
		u, err := url.Parse(m[1])
		if err != nil {
			t.Errorf("外链不是合法 URL：%q", m[1])
			continue
		}
		if u.Host != "github.com" {
			t.Errorf("页面里不允许出现指向 %s 的外链（只允许 github.com）", u.Host)
		}
		// 同一个 <a> 标签里必须有 rel="noopener"
		idx := strings.Index(body, m[0])
		tagEnd := strings.Index(body[idx:], ">")
		if tagEnd < 0 {
			t.Errorf("外链标签不完整：%q", m[0])
			continue
		}
		tag := body[idx : idx+tagEnd]
		if !strings.Contains(tag, `rel="noopener`) {
			t.Errorf("外链缺少 rel=\"noopener\"：%s", tag)
		}
		if !strings.Contains(tag, `target="_blank"`) {
			t.Errorf("外链应在新标签页打开：%s", tag)
		}
	}
	if found < 3 {
		t.Errorf("用户页应至少有仓库 / 接口文档 / OpenAPI 三个外链，找到 %d 个", found)
	}
}

// TestUserPageShowsOpenSourceNotice：用户页头部要有开源信息、官方测试地址
// 与"别靠换 IP 刷额度"的说明。
func TestUserPageShowsOpenSourceNotice(t *testing.T) {
	body := string(uiHTML)
	for _, want := range []string{
		`id="ossNote"`,
		"https://github.com/wzmwayne/vidlink",
		"/blob/master/docs/API.md",
		"/blob/master/docs/openapi.yaml",
		"https://vl.wzml.cc.cd",
		"AGPL-3.0",
		"别用换 IP 的方式刷额度",
		"约 8 MB",
		`id="ratesNote"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("用户页缺少 %q", want)
		}
	}
	// 两种模式都要看到这块提示：它不能挂在只在账户模式显示的区块里
	if strings.Contains(body, `id="ossNote" class="hidden"`) {
		t.Error("开源提示不该默认隐藏")
	}
	// 公共额度耗尽的错误文案也要给出部署建议（用户明确要求）
	env := newTestEnv(t, nil, defaultStub())
	r := httptest.NewRequest(http.MethodGet, "/v1/links?url=https://stub.test/v/1", nil)
	r.Header.Set("X-API-Key", env.srv.cfg.PublicKey)
	r.RemoteAddr = "192.0.2.10:1234"
	// 先把当天额度打满
	for i := 0; i < 25; i++ {
		rr := httptest.NewRequest(http.MethodGet, "/v1/links?url=https://stub.test/v/1", nil)
		rr.Header.Set("X-API-Key", env.srv.cfg.PublicKey)
		rr.RemoteAddr = "192.0.2.10:1234"
		env.handler().ServeHTTP(httptest.NewRecorder(), rr)
	}
	w := httptest.NewRecorder()
	env.handler().ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("额度用尽应 429，得到 %d", w.Code)
	}
	_, msg, _ := errBody(t, w)
	if !strings.Contains(msg, "自己部署") || !strings.Contains(msg, "github.com/wzmwayne/vidlink") {
		t.Errorf("429 文案应给出「自己部署」的建议与仓库链接，得到 %q", msg)
	}
}

// TestAdminPanelHasEditableRateGrid：管理面板要能改倍率（网格 + PUT）。
func TestAdminPanelHasEditableRateGrid(t *testing.T) {
	body := string(adminHTML)
	for _, want := range []string{
		`id="rateEditor"`, `id="rateDirty"`,
		`"proxyRateIn"`, `"data-platform"`, `"data-endpoint"`,
		`"/v1/admin/quota"`, `method: "PUT"`, `method: "DELETE"`,
		"保存改动", "恢复内置默认", "重新载入",
		"rateEdits", "warnings", "history",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("管理面板缺少 %q", want)
		}
	}
}

// --- 计费倍率：管理面可编辑 ---

// TestAdminQuotaUpdateRequiresAdmin：改价只有管理 Key 能做。
func TestAdminQuotaUpdateRequiresAdmin(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	body := `{"rates":{"douyin":{"links":5}}}`

	if w := doJSON(h, http.MethodPut, "/v1/admin/quota", body, nil); w.Code != http.StatusForbidden {
		t.Errorf("无 Key 改价应 403，得到 %d", w.Code)
	}
	if w := doJSON(h, http.MethodPut, "/v1/admin/quota", body, userHdr(env.userKey)); w.Code != http.StatusForbidden {
		t.Errorf("普通账号改价应 403，得到 %d", w.Code)
	}
	if w := do(h, http.MethodDelete, "/v1/admin/quota", userHdr(env.userKey)); w.Code != http.StatusForbidden {
		t.Errorf("普通账号复位应 403，得到 %d", w.Code)
	}
	// 管理 Key：能改
	if w := doJSON(h, http.MethodPut, "/v1/admin/quota", body, userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("管理 Key 改价应 200，得到 %d（%s）", w.Code, w.Body.String())
	}
	// 免校验模式：整条路由不存在
	ease := newEaseEnv(t, nil, defaultStub())
	if w := doJSON(ease.handler(), http.MethodPut, "/v1/admin/quota", body, nil); w.Code != http.StatusNotFound {
		t.Errorf("免校验模式改价应 404，得到 %d", w.Code)
	}
	if w := do(ease.handler(), http.MethodDelete, "/v1/admin/quota", nil); w.Code != http.StatusNotFound {
		t.Errorf("免校验模式复位应 404，得到 %d", w.Code)
	}
}

// TestAdminQuotaUpdateChangesMetering：改完立刻按新系数扣费。
func TestAdminQuotaUpdateChangesMetering(t *testing.T) {
	dy := stubExtractor{name: core.PlatformDouyin, videos: defaultStub().videos}
	env := newTestEnv(t, nil, dy)
	h := env.handler()

	// 先按默认价（抖音 links 1.1）
	w := do(h, http.MethodGet, "/v1/links?platform=douyin&id=1", userHdr(env.userKey))
	if got := w.Header().Get("X-Quota-Consumed"); got != "1.1" {
		t.Fatalf("默认抖音 links 应扣 1.1，得到 %q", got)
	}
	// 改价：抖音 links = 5
	if w := doJSON(h, http.MethodPut, "/v1/admin/quota",
		`{"rates":{"douyin":{"links":5}}}`, userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("改价失败：%d（%s）", w.Code, w.Body.String())
	}
	w = do(h, http.MethodGet, "/v1/links?platform=douyin&id=2", userHdr(env.userKey))
	if got := w.Header().Get("X-Quota-Consumed"); got != "5" {
		t.Errorf("改价后应扣 5，得到 %q", got)
	}
	// 对外读数同步
	var usage map[string]any
	uw := do(h, http.MethodGet, "/v1/usage", userHdr(env.userKey))
	_ = json.Unmarshal(uw.Body.Bytes(), &usage)
	rates := usage["rates"].(map[string]any)["douyin"].(map[string]any)
	if rates["links"] != 5.0 {
		t.Errorf("/v1/usage 的抖音 links = %v，想要 5", rates["links"])
	}
	if usage["rates_note"] == nil {
		t.Error("/v1/usage 应说明系数是当前部署的实时值")
	}
	// 平台清单同步
	pw := do(h, http.MethodGet, "/v1/platforms", nil)
	if !strings.Contains(pw.Body.String(), `"links":5`) {
		t.Errorf("/v1/platforms 应反映新价：%s", pw.Body.String())
	}
	// 清除自定义 → 回到默认
	if w := doJSON(h, http.MethodPut, "/v1/admin/quota",
		`{"rates":{"douyin":{"links":null}}}`, userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("清除失败：%d（%s）", w.Code, w.Body.String())
	}
	w = do(h, http.MethodGet, "/v1/links?platform=douyin&id=3", userHdr(env.userKey))
	if got := w.Header().Get("X-Quota-Consumed"); got != "1.1" {
		t.Errorf("清除后应回到 1.1，得到 %q", got)
	}
}

// TestAdminQuotaUpdateRecomputesPreauth：预授权上限跟着改价走（降价不再误伤，
// 涨价不漏预授权）。
func TestAdminQuotaUpdateRecomputesPreauth(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	if _, err := env.store.Update(env.userKey, account.Patch{Quota: floatPtr(8)}); err != nil {
		t.Fatal(err)
	}
	// 默认上限 1.1 ≤ 8 → 能解析
	if w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Fatalf("默认价应能解析，得到 %d", w.Code)
	}
	// 把 bilibili links 提到 10 → 预授权按最贵档 10 检查 → 余额 8 不够
	if w := doJSON(h, http.MethodPut, "/v1/admin/quota",
		`{"rates":{"bilibili":{"links":10}}}`, userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("改价失败：%d", w.Code)
	}
	w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey))
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("涨价后预授权应拦下（429），得到 %d（%s）", w.Code, w.Body.String())
	}
	// 复位 → 又能解析
	if w := do(h, http.MethodDelete, "/v1/admin/quota", userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("复位失败：%d", w.Code)
	}
	if w := do(h, http.MethodGet, "/v1/links?url=https://stub.test/v/1", userHdr(env.userKey)); w.Code != http.StatusOK {
		t.Errorf("复位后应恢复，得到 %d", w.Code)
	}
}

// TestProxyCostIgnoresPlatformRates：代理永不使用平台系数。
//
// 这是用户明确要求的不变量：把平台系数改到天上，1 MiB 的代理还是 0.5。
func TestProxyCostIgnoresPlatformRates(t *testing.T) {
	const size = 2 << 20 // 2 MiB
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		_, _ = w.Write(make([]byte, size))
	}))
	defer upstream.Close()

	env := newTestEnv(t, func(c *config.Config) { c.ProxySrv.Enabled = true }, defaultStub())
	h := env.handler()

	w := do(h, http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL), userHdr(env.userKey))
	if got := w.Header().Get("X-Quota-Consumed"); got != "1" {
		t.Fatalf("2 MiB 默认应扣 1（0.5/MiB），得到 %q", got)
	}
	// 把所有平台系数改到 50（含批量），代理仍按 0.5/MiB
	if w := doJSON(h, http.MethodPut, "/v1/admin/quota",
		`{"rates":{"default":{"info":50,"links":50,"detail":50,"batch_links":50}}}`,
		userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("改价失败：%d（%s）", w.Code, w.Body.String())
	}
	w = do(h, http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL), userHdr(env.userKey))
	if got := w.Header().Get("X-Quota-Consumed"); got != "1" {
		t.Errorf("平台系数改成 50 后代理仍应扣 1，得到 %q", got)
	}
	// 改代理费率本身则生效
	if w := doJSON(h, http.MethodPut, "/v1/admin/quota",
		`{"proxy_rate":2}`, userHdr(env.adminKey)); w.Code != http.StatusOK {
		t.Fatalf("改代理费率失败：%d（%s）", w.Code, w.Body.String())
	}
	w = do(h, http.MethodGet, "/v1/proxy?url="+url.QueryEscape(upstream.URL), userHdr(env.userKey))
	if got := w.Header().Get("X-Quota-Consumed"); got != "4" {
		t.Errorf("代理费率改成 2 后 2 MiB 应扣 4，得到 %q", got)
	}
	// 平台系数改了 → 预授权上限也跟着变（说明计量表与可编辑表是同一张）
	aw := do(h, http.MethodGet, "/v1/admin/quota", userHdr(env.adminKey))
	var admin map[string]any
	if err := json.Unmarshal(aw.Body.Bytes(), &admin); err != nil {
		t.Fatal(err)
	}
	pre := admin["preauth_max"].(map[string]any)
	if pre["links"] != 50.0 {
		t.Errorf("links 的预授权上限 = %v，想要 50", pre["links"])
	}
}

// TestAdminQuotaValidation：非法请求整体拒绝，且不改动现状。
func TestAdminQuotaValidation(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	h := env.handler()
	cases := []struct{ name, body string }{
		{"未知平台", `{"rates":{"weibo":{"links":1}}}`},
		{"未知端点", `{"rates":{"douyin":{"parse":1}}}`},
		{"负数", `{"rates":{"douyin":{"links":-1}}}`},
		{"超出上限", `{"rates":{"douyin":{"links":9999}}}`},
		{"抖音批量不支持", `{"rates":{"douyin":{"batch_links":1}}}`},
		{"代理费率非法", `{"proxy_rate":-1}`},
		{"未知字段", `{"rate":{"douyin":{"links":1}}}`},
		{"空请求", `{}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doJSON(h, http.MethodPut, "/v1/admin/quota", c.body, userHdr(env.adminKey))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("应 400，得到 %d（%s）", w.Code, w.Body.String())
			}
		})
	}
	// 现状未变
	gw := do(h, http.MethodGet, "/v1/admin/quota", userHdr(env.adminKey))
	var d map[string]any
	if err := json.Unmarshal(gw.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d["overrides"] != nil {
		t.Errorf("失败的请求不该留下覆盖：%v", d["overrides"])
	}
	if d["source"] != "defaults" {
		t.Errorf("source = %v，想要 defaults", d["source"])
	}
	if d["limits"] == nil || d["warnings"] == nil {
		t.Error("GET 应带 limits 与 warnings")
	}
}

// TestCORSPreflightAllowsPutDelete：跨域面板要能改倍率（预检必须放行 PUT/DELETE）。
func TestCORSPreflightAllowsPutDelete(t *testing.T) {
	env := newTestEnv(t, nil, defaultStub())
	w := do(env.handler(), http.MethodOptions, "/v1/admin/quota", map[string]string{
		"Origin":                        "https://panel.example.com",
		"Access-Control-Request-Method": "PUT",
	})
	if w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("预检应 200/204，得到 %d", w.Code)
	}
	allow := w.Header().Get("Access-Control-Allow-Methods")
	for _, m := range []string{"PUT", "DELETE"} {
		if !strings.Contains(allow, m) {
			t.Errorf("Allow-Methods 应包含 %s，得到 %q", m, allow)
		}
	}
}

// TestRatesPersistAcrossRestart：改过的倍率要能从文件读回来（重启不丢）。
func TestRatesPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rates.json")

	build := func() *Server {
		st, err := rates.New(rates.Options{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		env := newTestEnv(t, nil, defaultStub())
		// 两张表必须一起换（server.New 里的不变量：计量表 == 可编辑倍率表）
		env.srv.rates = st
		env.srv.quotaTable = st.Table()
		return env.srv
	}
	srv := build()
	if w := doJSON(srv.Handler(), http.MethodPut, "/v1/admin/quota",
		`{"rates":{"bilibili":{"links":2.5}},"proxy_rate":0.25}`,
		userHdr("vl_admin_test")); w.Code != http.StatusOK {
		t.Fatalf("改价失败：%d（%s）", w.Code, w.Body.String())
	}

	// 模拟重启：新建一个指向同一文件的 server
	srv2 := build()
	w := do(srv2.Handler(), http.MethodGet, "/v1/admin/quota", userHdr("vl_admin_test"))
	var d map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d["source"] != "file" {
		t.Errorf("重启后 source = %v，想要 file", d["source"])
	}
	proxy := d["proxy"].(map[string]any)
	if proxy["rate_per_mib"] != 0.25 {
		t.Errorf("重启后代理费率 = %v，想要 0.25", proxy["rate_per_mib"])
	}
	plats := d["platforms"].(map[string]any)
	if plats["bilibili"].(map[string]any)["links"] != 2.5 {
		t.Errorf("重启后 bilibili links = %v，想要 2.5",
			plats["bilibili"].(map[string]any)["links"])
	}
}
