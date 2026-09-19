// Package server 提供 HTTP 接口层。
//
// 设计目标：极轻量。因此只用标准库 net/http（Go 1.22+ 的 ServeMux 已支持
// 方法与路径通配），不引入任何 Web 框架。整个二进制没有第三方依赖。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vidlink/internal/account"
	"vidlink/internal/config"
	"vidlink/internal/core"
	"vidlink/internal/gate"
	"vidlink/internal/publicq"
	"vidlink/internal/quota"
	"vidlink/internal/rates"
	"vidlink/internal/service"
)

// Version 由 main 注入，便于 /health 暴露构建信息。
var Version = "dev"

// apiVersion 是 URL 里的接口版本段。对外承诺的是这一段的语义稳定性。
const apiVersion = "v1"

// requestIDHeader 是贯穿一次请求的关联标识。
//
// 它同时出现在三个地方，缺一不可：响应头（客户端可以记下来）、
// 错误体的 request_id（用户报障时能直接引用）、以及服务端日志。
// 有了它，"用户说某次请求失败了"才能被定位到具体那一条日志。
const requestIDHeader = "X-Request-Id"

// ctxKeyRequestID 是 request id 在 context 里的键（未导出，避免外部依赖）。
type ctxKey int

const ctxKeyRequestID ctxKey = iota

// maxRequestIDLen 限制客户端自带的 request id 长度。
// 它会进响应头与日志，放任长度就等于给了日志污染的入口。
const maxRequestIDLen = 64

// Server 持有路由与依赖。
type Server struct {
	cfg  *config.Config
	svc  *service.Service
	http *http.Client // 媒体代理专用客户端（长连接、不限总超时）

	log *slog.Logger

	// limiter 是**按 IP** 的请求限流，防的是单机刷接口。
	// 它与按 Key 的并发闸门（gate）是两回事，见 gate 包注释。
	limiter *ipLimiter

	// accounts 是账号账本（配额 + 账号倍率 + 用量）。
	accounts *account.Store
	// gate 是两道并发闸门：每 Key 1 个请求，全局 10 个解析任务。
	gate *gate.Gate
	// quotaTable 是配额系数表：每个端点、每个平台各消耗多少配额。
	// 系数是**数据**而不是散落的常量——调整运营策略时只改这一处。
	quotaTable *quota.Table

	// publicQ 是公共账号（Key 公开）的"每 IP 每日配额"。
	// 公共 Key 是共享的，账本余额对它没有意义，所以单独一套口径。
	publicQ *publicq.Limiter

	// rates 是计费倍率的覆盖层：出厂默认在 quota 包里，管理员改的差值在这里。
	// 计量路径直接读 rates.Table()（读锁），改价不会给每次请求加锁。
	rates *rates.Store

	// sigTTL 是签名凭据的时间容差（见 config.SigTTL）。
	sigTTL time.Duration
	// adminHandle 是管理签名的句柄（adm_ + 16 位），启动时算一次。
	// 每个管理请求都现算一次 SHA-256 没必要，而且它必须与校验时用的
	// 那个值严格一致，缓存下来就没有"两处算法漂移"的余地。
	adminHandle string

	startedAt time.Time
	reqCount  atomic.Int64
	errCount  atomic.Int64
}

// Deps 是 Server 的外部依赖。
//
// 做成结构体而不是一长串参数：将来再加依赖（比如用量导出器）时
// 不必改动所有调用点。
type Deps struct {
	Service    *service.Service
	Accounts   *account.Store
	Gate       *gate.Gate
	QuotaTable *quota.Table
	// PublicQ 可注入（测试用它固定"今天"）；nil 时按配置新建。
	PublicQ *publicq.Limiter
	// Rates 是计费倍率的覆盖层（可编辑 + 落盘 + 审计）。
	// nil 时建一个纯内存的默认表，保证既有调用方与测试零改动。
	Rates  *rates.Store
	Logger *slog.Logger
}

// New 构造服务器。
func New(cfg *config.Config, d Deps) (*Server, error) {
	svc, log := d.Service, d.Logger
	if log == nil {
		log = slog.Default()
	}
	if d.QuotaTable == nil {
		d.QuotaTable = quota.DefaultTable()
	}
	if d.Gate == nil {
		d.Gate = gate.New(gate.DefaultOptions())
	}
	if d.PublicQ == nil {
		d.PublicQ = publicq.New(cfg.PublicDailyQuota, nil)
	}
	if d.Rates == nil {
		// 纯内存：改动不落盘。生产路径由 main.go 注入带文件的 Store。
		st, err := rates.New(rates.Options{Seed: cfg.ProxyRate, Table: d.QuotaTable})
		if err != nil {
			return nil, err
		}
		d.Rates = st
	}
	// 计量用的表与可编辑倍率表**必须是同一张**：分成两张的话，
	// 管理面改的是 A、扣费读的是 B，表现就是"改了不生效"。
	d.QuotaTable = d.Rates.Table()
	if cfg.IsEase() && d.Accounts != nil {
		// 免校验模式下账户体系不存在；传进来也用不到，直接忽略以免误用。
		d.Accounts = nil
	}
	// 媒体代理需要流式传输大文件，不能用带总超时的 client。
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	if cfg.Proxy != "" {
		if pu, err := parseProxyURL(cfg.Proxy); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		} else {
			return nil, fmt.Errorf("server: VIDLINK_PROXY 无效: %w", err)
		}
	}

	sigTTL := cfg.SigTTL
	if sigTTL <= 0 {
		// 直接构造 Config 的调用方（测试、内嵌使用）可能没填这个字段。
		// 0 会让所有签名立刻过期，而症状是"签名永远验不过"——极难排查，
		// 所以这里兜一个默认值，而不是让它生效。
		sigTTL = account.DefaultTTL
	}
	adminKey := strings.TrimSpace(cfg.AdminKey)
	adminHandle := ""
	if adminKey != "" {
		adminHandle = account.HandleID(account.AdminPrefix, adminKey)
	}

	return &Server{
		cfg:         cfg,
		svc:         svc,
		http:        &http.Client{Transport: tr},
		log:         log,
		limiter:     newIPLimiter(effectiveRPM(cfg)),
		accounts:    d.Accounts,
		gate:        d.Gate,
		quotaTable:  d.QuotaTable,
		publicQ:     d.PublicQ,
		rates:       d.Rates,
		sigTTL:      sigTTL,
		adminHandle: adminHandle,
		startedAt:   time.Now(),
	}, nil
}

// effectiveRPM 返回实际生效的每 IP 限流值。
//
// 免校验模式一律为 0（不限流）：那是"不要任何请求级拦截"的一部分。
// 配置层已经置 0，这里再判一次是为了兜住"直接构造 Config 的调用方"
// （测试、内嵌使用），让模式语义不依赖于谁怎么填这个字段。
func effectiveRPM(cfg *config.Config) int {
	if cfg.IsEase() {
		return 0
	}
	return cfg.RateLimitRPM
}

// routeSpec 声明一条对外路由。
//
// 为什么用一张表而不是散落的 mux.HandleFunc：路由这个事实有**三个**消费者——
// 注册、鉴权豁免、以及"未匹配时该给 404 还是 405"。三处各写一份路径清单，
// 必然会"加了新接口、忘了加豁免"。让它们都从这一个来源读。
type routeSpec struct {
	method  string // HTTP 方法，用于 405 的 Allow 头
	path    string // 路径模板，{name} 表示单段通配
	public  bool   // 豁免鉴权（探针与前端初始化需要）
	admin   bool   // 用固定管理 Key 鉴权（VIDLINK_ADMIN_KEY），不是账号
	handler http.HandlerFunc
	// endpoint 非空表示这是一条**配额计量**路由。鉴权中间件据此做预授权，
	// handler 据此结算。空串表示不计配额或运维路由。
	endpoint quota.Endpoint
}

func (s *Server) routes() []routeSpec {
	specs := []routeSpec{
		// ---- 元信息：公开。探针与前端初始化都要用，拿不到凭据 ----
		{method: http.MethodGet, path: "/v1/version", public: true, handler: s.handleVersion},
		{method: http.MethodGet, path: "/v1/health", public: true, handler: s.handleHealth},
		{method: http.MethodGet, path: "/v1/platforms", public: true, handler: s.handlePlatforms},
		{method: http.MethodGet, path: "/healthz", public: true, handler: s.handleHealthz},
		{method: http.MethodGet, path: "/readyz", public: true, handler: s.handleReadyz},

		// ---- 配额端点：五个 ----
		{method: http.MethodGet, path: "/v1/search", handler: s.handleSearch,
			endpoint: quota.EndpointSearch},
		{method: http.MethodGet, path: "/v1/info", handler: s.handleInfo,
			endpoint: quota.EndpointInfo},
		{method: http.MethodGet, path: "/v1/links", handler: s.handleLinks,
			endpoint: quota.EndpointLinks},
		{method: http.MethodGet, path: "/v1/detail", handler: s.handleDetail,
			endpoint: quota.EndpointDetail},
		{method: http.MethodPost, path: "/v1/batch/links", handler: s.handleBatchLinks,
			endpoint: quota.EndpointBatchLinks},
	}

	// 免校验模式：账户体系整体不存在，所以用量查询与管理面**不注册**。
	// 不注册（而不是注册后返回 403/空数据）意味着它们确实返回 404，
	// 外界探测不到"这里本该有个管理接口"。
	if !s.cfg.IsEase() {
		// ---- 免配额但需要身份：用量查询与自己的流水都要知道"你是谁" ----
		specs = append(specs,
			routeSpec{method: http.MethodGet, path: "/v1/usage", handler: s.handleUsage},
			routeSpec{method: http.MethodGet, path: "/v1/ledger", handler: s.handleLedger},
			// 签发签名凭据：URL 里唯一允许的凭据形态，见 auth.go
			routeSpec{method: http.MethodGet, path: "/v1/sign", handler: s.handleSign},
			// 每日签到领配额：与用量、账单一样属于"身份相关但不计费"的接口
			routeSpec{method: http.MethodPost, path: "/v1/checkin", handler: s.handleCheckIn})
		// ---- 管理面：用固定管理 Key（未配置时每条都恒 403）----
		specs = append(specs, s.adminRoutes()...)
	}

	// 赞赏码：页面在用，所以只在 WebUI 打开时注册；公开（静态图片，无数据）。
	if s.cfg.WebUI {
		specs = append(specs, routeSpec{method: http.MethodGet, path: tipPath, public: true,
			handler: s.handleTip})
	}

	if s.cfg.ProxySrv.Enabled {
		// 注意 endpoint 刻意留空：代理的配额是**按传输体积**在 handler 里
		// 结算的（quota.ProxyCost），不走"端点 × 平台"系数表。
		// 留空还有第二个作用——让它避开按 Key 的解析闸门：一次下载会长时间
		// 占着槽位，串行化会把该账号的解析请求一起堵死。
		specs = append(specs,
			routeSpec{method: http.MethodGet, path: "/v1/proxy", handler: s.handleProxy},
			routeSpec{method: http.MethodHead, path: "/v1/proxy", handler: s.handleProxy})
	}
	return specs
}

// Handler 组装路由与中间件。
//
// 中间件顺序是**契约的一部分**，从外到内：
//
//	Logging → RequestID → Recovery → CORS → RateLimit → Account → mux
//
// 免校验模式（VL_EASE=true）下 RateLimit 已按配置关闭（RPM=0），
// Account 整层不挂载，于是最终链路是 Logging → RequestID → Recovery → CORS → mux。
//
// 三条不能动的约束：
//
//   - **CORS 必须在 RateLimit 与 APIKey 之外。** 浏览器跨域预检（OPTIONS）
//     按 Fetch 规范不携带任何凭据，它天然过不了鉴权。若鉴权跑在 CORS 前面，
//     预检会被 403 掉，浏览器随即放弃正式请求——表现就是"一开 API Key，
//     网页端就完全用不了"，而且看服务端日志只会看到一堆 OPTIONS 403。
//   - **Recovery 必须在 CORS 之内但足够靠外**，保证任何 handler panic
//     都能变成一个结构化错误，而不是被掐断的空连接。
//   - **RequestID 在 Recovery 之外**，这样连 panic 产生的响应也带着 ID。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	specs := s.routes()
	for _, rt := range specs {
		mux.HandleFunc(rt.method+" "+rt.path, rt.handler)
	}
	// 兜底必须注册为无方法的 "/"，否则它接不到"路径对但方法错"的请求。
	// 代价是 ServeMux 自带的 405 不再生效（有兜底模式时它就不给 405 了），
	// 所以 404/405 的判定由 fallback 自己完成。
	mux.HandleFunc("/", s.fallback(specs))

	var h http.Handler = mux
	// 免校验模式：不挂账户中间件——没有 Key 校验、没有按 Key 闸门、没有配额预授权。
	if !s.cfg.IsEase() {
		h = s.withAccount(h, specs)
	}
	h = s.withRateLimit(h)
	h = s.withCORS(h)
	h = s.withRecovery(h)
	h = s.withRequestID(h)
	h = s.withLogging(h)
	return h
}

// fallback 处理所有没被上面路由匹配的请求，给出 404 或 405。
//
// 这是自己实现而不是交给 ServeMux 的原因：ServeMux 只在**没有**兜底模式时
// 才返回 405。而我们既想要兜底（给出友好的 404 而不是裸的 "404 page not found"），
// 又想要正确的 405，两者不可兼得，于是自己判。
func (s *Server) fallback(specs []routeSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 根路径：开着 WebUI 就给图形化解析页，否则给纯文本导航页。
		if r.URL.Path == "/" {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				if s.cfg.WebUI {
					s.handleUI(w, r)
					return
				}
				s.handleIndex(w, r)
				return
			}
			w.Header().Set("Allow", "GET, HEAD")
			writeErrorStatus(w, http.StatusMethodNotAllowed, "method_not_allowed",
				fmt.Sprintf("%s 不允许用于 /", r.Method))
			return
		}

		// 管理面板：与图形化解析页同一个开关（VL_WEBUI）。
		//
		// 页面本身是**公开的壳**（不含任何账号数据），数据全靠页面里
		// 填的管理 Key 去请求——不公开的话，浏览器连填 Key 的地方都拿不到。
		// 免校验模式下整个管理面不存在，所以这里必须是 404，
		// 不能返回一个"看起来能用但一定失败"的页面。
		if r.URL.Path == adminUIPath && s.cfg.WebUI && !s.cfg.IsEase() {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				s.handleAdminUI(w, r)
				return
			}
			w.Header().Set("Allow", "GET, HEAD")
			writeErrorStatus(w, http.StatusMethodNotAllowed, "method_not_allowed",
				fmt.Sprintf("%s 不允许用于 %s", r.Method, adminUIPath))
			return
		}

		var allow []string
		for _, rt := range specs {
			if matchPath(rt.path, r.URL.Path) {
				allow = append(allow, rt.method)
			}
		}
		if len(allow) > 0 {
			// 路径存在、方法不对 —— 告诉客户端正确的方法是什么
			w.Header().Set("Allow", strings.Join(dedupeStrings(allow), ", "))
			writeErrorStatus(w, http.StatusMethodNotAllowed, "method_not_allowed",
				fmt.Sprintf("%s 不允许用于 %s，可用: %s",
					r.Method, r.URL.Path, strings.Join(dedupeStrings(allow), ", ")))
			return
		}
		writeErrorStatus(w, http.StatusNotFound, "not_found",
			fmt.Sprintf("未知路径: %s", r.URL.Path))
	}
}

// matchAnyPath 判断请求路径是否匹配给定模板中的任意一条。
//
// 用线性扫描而不是 map：路由只有十几条，而 map 只能做精确匹配，
// 无法表达 {name} 通配——那正是需要在这里判对的场景。
func matchAnyPath(templates []string, path string) bool {
	for _, t := range templates {
		if matchPath(t, path) {
			return true
		}
	}
	return false
}

// matchPath 判断请求路径是否匹配一条路由模板（**忽略方法**）。
// 模板里的 {name} 视为匹配任意单个非空路径段。
func matchPath(pattern, path string) bool {
	// 只去掉**前导**斜杠，保留尾随斜杠的语义："/api/v1/parse/" 与
	// "/api/v1/parse" 是不同的路径。若把尾斜杠一并吃掉，前者会被判成
	// "路径存在但方法不对"而返回 405，掩盖了它其实是个 404。
	pp := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	rp := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(pp) != len(rp) {
		return false
	}
	for i := range pp {
		if strings.HasPrefix(pp[i], "{") && strings.HasSuffix(pp[i], "}") {
			if rp[i] == "" {
				return false
			}
			continue
		}
		if pp[i] != rp[i] {
			return false
		}
	}
	return true
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// --- 中间件 ---

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.errCount.Add(1)
				s.log.Error("panic 已恢复", "path", r.URL.Path, "panic", rec)
				writeError(w, core.E(core.KindInternal, "", "server", "服务内部错误", nil))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder 捕获状态码供日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Flush 转发 Flush，保证代理场景的流式输出不被缓冲。
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withRequestID 为每个请求确定一个关联 ID。
//
// 客户端自带的 ID 会被沿用（跨服务串联时需要），但必须校验：
// 这个值会被回写进响应头、写进错误体、写进日志。放任任意字符串
// 就是开了日志注入与响应头注入的口子（比如带 \r\n 的值）。
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(requestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

// sanitizeRequestID 只接受长度受限的可见 ASCII，其余一律丢弃（返回空串）。
func sanitizeRequestID(v string) string {
	if v == "" || len(v) > maxRequestIDLen {
		return ""
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x21 || c > 0x7e {
			return ""
		}
	}
	return v
}

// newRequestID 生成 128 位随机 ID。
//
// 用 crypto/rand 而不是 math/rand：request id 会出现在日志与错误体里，
// 可预测意味着有人能伪造它去污染日志。
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源不可用是极端情况；退化成时间戳也比 panic 好
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// requestID 从 context 取回 ID；取不到返回空串。
func requestID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		s.reqCount.Add(1)

		// 健康检查噪音太大，降级为 debug
		lvl := slog.LevelInfo
		if strings.HasPrefix(r.URL.Path, "/v1/health") ||
			r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			lvl = slog.LevelDebug
		}
		s.log.Log(r.Context(), lvl, "请求",
			"request_id", requestID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"ms", time.Since(start).Milliseconds(),
			"ip", s.clientIP(r),
			// 凭据来源：排查"403 到底是没带还是带了错的"最快的一眼。
			// 只记录来源（header/bearer/signed-url），不记凭据本身。
			"auth", authLogSource(r),
		)
	})
}

// corsExposeHeaders 是允许浏览器 JS 读取的响应头。
//
// 默认情况下跨域响应里只有极少数头能被 JS 读到，其余一律被 CORS 规范挡掉。
// 这几条都是调用方真正需要的：分页/断点续传要用 Content-Range，
// 排障要用 X-Request-Id，自我节流要用限流三件套，
// 前端要显示"这次用了多少、还剩多少"就必须放行配额两条。
const corsExposeHeaders = "Content-Length, Content-Range, Accept-Ranges, " +
	"X-Request-Id, X-RateLimit-Limit, X-RateLimit-Remaining, Retry-After, " +
	"X-Quota-Consumed, X-Quota-Remaining"

// corsAllowHeaders 是允许浏览器发送的请求头。
// 少了任何一个，浏览器就会在预检阶段拒绝发送——表现为"代码没错但请求发不出去"。
const corsAllowHeaders = "Content-Type, Authorization, X-API-Key, X-Request-Id, Range"

func (s *Server) withCORS(next http.Handler) http.Handler {
	allowAll := len(s.cfg.CORSOrigins) == 0
	for _, o := range s.cfg.CORSOrigins {
		if o == "*" {
			allowAll = true
		}
	}
	allowed := make(map[string]bool, len(s.cfg.CORSOrigins))
	for _, o := range s.cfg.CORSOrigins {
		allowed[o] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			switch {
			case allowAll:
				w.Header().Set("Access-Control-Allow-Origin", "*")
			case allowed[origin]:
				w.Header().Set("Access-Control-Allow-Origin", origin)
				// 按 Origin 变化的响应绝不能被共享缓存复用
				w.Header().Add("Vary", "Origin")
			}
			// PUT/DELETE 是管理面改倍率用的（/v1/admin/quota）
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, HEAD, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", corsAllowHeaders)
			w.Header().Set("Access-Control-Expose-Headers", corsExposeHeaders)
			w.Header().Set("Access-Control-Max-Age", "600")
		}

		// 预检请求到此为止。
		//
		// 它必须在这里短路，不能继续往下走：预检按规范**不携带凭据**，
		// 往下走必然被限流或鉴权拒掉，浏览器随即判定跨域失败，
		// 真正的请求根本不会发出。本中间件位于 RateLimit/APIKey 之外，
		// 就是为了让这一条成立。
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, remaining, retryAfter := s.limiter.allow(s.clientIP(r))

		// 限流信息对调用方是刚需：没有它，客户端只能靠撞 429 来摸索节奏。
		w.Header().Set("X-RateLimit-Limit", fmt.Sprint(s.limiter.capacity()))
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(int(remaining)))

		if !ok {
			secs := int(retryAfter.Seconds() + 0.999)
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", fmt.Sprint(secs))
			writeError(w, core.Errf(core.KindRateLimit, "", "ratelimit",
				"请求过于频繁：当前配额为每 IP %d 次/分钟，"+
					"请在 %d 秒后重试（响应头 X-RateLimit-* 与 Retry-After 给出实时额度）",
				s.cfg.RateLimitRPM, secs))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- 处理器 ---

// handleVersion 返回服务与接口版本。
//
// 单独开一个端点而不是只藏在 /health 里：调用方需要能在**不解析运维信息**
// 的前提下确认自己在跟哪个版本的接口说话。刻意不暴露 Go 版本等运行时细节。
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     Version,
		"api_version": apiVersion,
		"platform":    runtime.GOOS + "/" + runtime.GOARCH,
	})
}

// handleHealthz 是**存活**探针：进程还在跑就返回 200。
//
// 它刻意不检查任何依赖：活着的意义就是活着。检查依赖会让"上游抖动"
// 触发编排系统重启一个本来健康、且重启也无济于事的进程。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"status": "alive"})
}

// handleReadyz 是**就绪**探针：这个实例现在能不能接流量。
//
// 判据是"有没有可用的提取器"。Docker 里能跑起来、但一个平台都没注册
// （配置错误或构建被裁剪）的实例，应当被摘掉流量而不是接住请求再报错。
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	pf := s.svc.Platforms()
	if len(pf) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not_ready",
			"reason": "没有注册任何平台提取器",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ready",
		"platforms": len(pf),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"status":      "ok",
		"version":     Version,
		"api_version": apiVersion,
		"mode":        s.modeName(),
		"uptime_sec":  int(time.Since(s.startedAt).Seconds()),
		"requests":    s.reqCount.Load(),
		"errors":      s.errCount.Load(),
		"proxy":       s.cfg.ProxySrv.Enabled,
		"webui":       s.cfg.WebUI,
		"cache":       s.svc.CacheStats(),
		// time 与 sign_ttl 是给"签名过期/时间戳在未来"这类报错服务的信息：
		// 调用方拿本机 `date +%s` 一减就知道自己的钟偏了多少——
		// 这在本地自行签发（openssl / 脚本）的场景里是最常见的原因。
		"time":     time.Now().Unix(),
		"sign_ttl": int(s.sigTTL / time.Second),
	}
	// 公共入口：Key 是公开信息，页面据此给出"免注册试用"的入口；
	// 免校验模式下整条概念不存在，不报。
	if !s.cfg.IsEase() && strings.TrimSpace(s.cfg.PublicKey) != "" {
		body["public"] = map[string]any{
			"key":          s.cfg.PublicKey,
			"daily_per_ip": s.publicQ.Limit(),
			"ledger":       false,
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// handleParse 解析单个链接：GET /api/v1/parse?url=<分享文案或链接>

// handleParseByID 按平台 + ID 解析：GET /api/v1/parse/{platform}/{id}

// batchRequest 是批量解析的请求体。

// handleIndex 返回根路径的纯文本导航页（账户模式）。
//
// 免校验模式下根路径走 handleUI，返回内嵌的图形化解析页。
//
// 只有 "/" 会走到这里——其他未匹配路径由 fallback 处理成 404/405。
// 这里给文本而不是 JSON，是因为用浏览器点开会更顺眼；
// 而真正的接口消费者不会来访问根路径。
//
// 免校验模式下列的是另一份清单：没有 Key、没有账号、没有配额，
// 于是 /v1/usage 与 /v1/admin/* 根本不出现。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if s.cfg.IsEase() {
		fmt.Fprintf(w, `vidlink %s — 多平台视频解析 API（免校验模式）

解析端点（无需 API Key，不计量配额）
  GET  /v1/search?platform=&keyword=  搜索（前 20 条，可 page/limit）
  GET  /v1/info?url=             元信息 + 档位列表，无直链
  GET  /v1/links?url=&quality=   仅直链
  GET  /v1/detail?url=           元信息 + 全部档位直链
  POST /v1/batch/links           批量直链，%d~%d 条，不支持抖音

其他端点
  GET  /v1/platforms             平台清单
  GET  /v1/version  /v1/health   版本与状态
  GET  /healthz  /readyz         存活 / 就绪探针

免校验模式已开启（VL_EASE=true）：没有 API Key、没有账号、没有配额，
/v1/usage 与 /v1/admin/* 不存在。切勿把本服务直接暴露到公网。
接口文档：docs/API.md
`, Version, s.cfg.BatchMin, s.cfg.BatchMax)
	} else {
		fmt.Fprintf(w, `vidlink %s — 多平台视频解析 API（计量单位：%s）

计量端点（消耗配额，需 API Key）
  GET  /v1/search?platform=&keyword=  0.25/条（默认 20 条，可 page/limit）
                                    搜索：元信息 + id，直链要再调 detail/links
  GET  /v1/info?url=             0.5（抖音 0.75）    元信息 + 档位列表，无直链
  GET  /v1/links?url=&quality=   1.0（抖音 1.1）     仅直链
  GET  /v1/detail?url=           1.2（抖音 1.5）     元信息 + 全部档位直链
  POST /v1/batch/links           0.75/条，%d~%d 条，不支持抖音

不消耗配额的端点
  GET  /v1/usage                 本 Key 的配额、用量与账号倍率
  GET  /v1/platforms             平台清单与各端点系数
  GET  /v1/version  /v1/health   版本与状态
  GET  /healthz  /readyz         存活 / 就绪探针

管理端点（需固定管理 Key：配置项 VIDLINK_ADMIN_KEY）：/v1/admin/*
接口文档：docs/API.md     机器可读规格：docs/openapi.yaml
配额倍率说明：docs/配额倍率表.md
`, Version, unitName, s.cfg.BatchMin, s.cfg.BatchMax)
		if s.cfg.WebUI {
			fmt.Fprintf(w, "\n图形界面\n  %s  解析页\n  %s       管理面板（填管理 Key 后可用）\n", "/", adminUIPath)
		}
	}
	if s.cfg.ProxySrv.Enabled {
		fmt.Fprint(w, "\nGET /v1/proxy?url=<媒体地址>   （流式代理，支持 Range）\n")
	}
}

func (s *Server) writeServiceError(w http.ResponseWriter, err error) {
	s.errCount.Add(1)
	writeError(w, err)
}

// clientIP 解析客户端 IP，仅在配置了可信代理头时才信任该头。
func (s *Server) clientIP(r *http.Request) string {
	if h := s.cfg.TrustedProxyHeader; h != "" {
		if v := r.Header.Get(h); v != "" {
			if i := strings.IndexByte(v, ','); i > 0 {
				return strings.TrimSpace(v[:i])
			}
			return strings.TrimSpace(v)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- 响应工具 ---

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // 保留 URL 里的 & 与中文，便于人眼核对
	_ = enc.Encode(body)
}

// writeError 输出一个结构化领域错误。
func writeError(w http.ResponseWriter, err error) {
	writeErrorBody(w, statusFor(core.KindOf(err)), kindName(core.KindOf(err)), err.Error())
}

// writeErrorStatus 输出不属于领域分类的错误（路由层的 404 / 405）。
//
// 路由问题不是"领域错误"，硬塞进 core.Kind 只会污染那套分类，
// 所以这里直接指定状态码与 kind 字符串。
func writeErrorStatus(w http.ResponseWriter, status int, kind, msg string) {
	writeErrorBody(w, status, kind, msg)
}

// writeErrorBody 是错误响应的唯一出口。
//
// 它把 request_id 一并带上：用户报障时能直接给出这个值，运维据此在日志里
// 定位到那一次请求，不必靠时间戳和客户端 IP 去猜。
//
// request_id 是从**响应头回读**的，而不是作为参数层层传递：
// withRequestID 在任何处理器之前就把它写进了响应头，回读拿到的是同一个值。
// 这样每个写错误的地方都不必再额外接一个 *http.Request 参数。
// 代价是一条隐式约束——withRequestID 必须在所有处理器之外，Handler() 里已保证。
func writeErrorBody(w http.ResponseWriter, status int, kind, msg string) {
	body := map[string]any{
		"kind":    kind,
		"message": msg,
	}
	if id := w.Header().Get(requestIDHeader); id != "" {
		body["request_id"] = id
	}
	writeJSON(w, status, map[string]any{"error": body})
}

func statusFor(k core.Kind) int {
	switch k {
	case core.KindBadInput:
		return http.StatusBadRequest
	case core.KindUnsupport:
		return http.StatusBadRequest
	case core.KindNotFound:
		return http.StatusNotFound
	case core.KindForbidden:
		return http.StatusForbidden
	case core.KindRateLimit:
		return http.StatusTooManyRequests
	case core.KindTimeout:
		return http.StatusGatewayTimeout
	case core.KindUpstream:
		return http.StatusBadGateway
	case core.KindUnavailable:
		// 过载用 503 而不是 500：500 会让人以为服务坏了，
		// 而这里服务是好的，只是主动拒绝了这一次请求（负载卸载）。
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func kindName(k core.Kind) string {
	switch k {
	case core.KindBadInput:
		return "bad_input"
	case core.KindUnsupport:
		return "unsupported"
	case core.KindNotFound:
		return "not_found"
	case core.KindForbidden:
		return "forbidden"
	case core.KindRateLimit:
		return "rate_limited"
	case core.KindTimeout:
		return "timeout"
	case core.KindUpstream:
		return "upstream_error"
	case core.KindUnavailable:
		return "unavailable"
	default:
		return "internal"
	}
}

func bearer(h string) string {
	const p = "Bearer "
	if strings.HasPrefix(h, p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// --- IP 限流（令牌桶，惰性补充）---

type ipLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
	ratePer float64 // 每秒补充令牌数
	burst   float64
	lastGC  time.Time
}

type ipBucket struct {
	tokens float64
	last   time.Time
}

func newIPLimiter(rpm int) *ipLimiter {
	l := &ipLimiter{
		buckets: make(map[string]*ipBucket, 256),
		lastGC:  time.Now(),
	}
	if rpm > 0 {
		l.ratePer = float64(rpm) / 60.0
		l.burst = float64(max(5, rpm/6))
	}
	return l
}

// allow 消耗一个令牌。
//
// 返回剩配额度与"还要等多久"，是为了让调用方能从响应头里读到自己的
// 实时配额，而不是靠不断撞 429 来试探节奏。
func (l *ipLimiter) allow(ip string) (ok bool, remaining float64, retryAfter time.Duration) {
	if l.ratePer <= 0 {
		return true, 0, 0
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// 顺带做一次惰性清理，避免 IP 无限增长（每 5 分钟一次）
	if now.Sub(l.lastGC) > 5*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}

	b, ok2 := l.buckets[ip]
	if !ok2 {
		b = &ipBucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	} else {
		b.tokens = minF(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.ratePer)
		b.last = now
	}
	if b.tokens < 1 {
		// 还差多少令牌，就还要等多久
		need := 1 - b.tokens
		return false, b.tokens, time.Duration(need / l.ratePer * float64(time.Second))
	}
	b.tokens--
	return true, b.tokens, 0
}

// capacity 是桶容量（即突发上限），用于 X-RateLimit-Limit。
func (l *ipLimiter) capacity() int {
	if l.burst <= 0 {
		return 0
	}
	return int(l.burst)
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// parseProxyURL 解析并校验代理地址。
func parseProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http", "https", "socks5":
		return u, nil
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q（支持 http/https/socks5）", u.Scheme)
	}
}
