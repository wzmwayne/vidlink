// Package netx 是项目的 HTTP 基础设施：连接池、按主机限流、重试、UA 伪装。
//
// 设计目标（对应"极轻量 + 高并发 + 高效率"）：
//   - 全局共享一个 http.Client，最大化连接复用，避免每请求新建 Transport；
//   - 按上游主机做令牌桶限流，既保护上游（防风控），也保护自己（防雪崩）；
//   - 重试带指数退避 + 抖动，且只对幂等请求与可重试状态码生效；
//   - 零第三方依赖，仅用标准库。
package netx

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Headers 是请求头集合的便捷别名。
type Headers map[string]string

// Options 是 Client 的构造参数，零值即合理默认。
type Options struct {
	// Timeout 是单次请求（含重定向）的总超时。
	Timeout time.Duration
	// MaxIdleConnsPerHost 决定到单个上游的并发保活连接数，是并发能力的核心旋钮。
	MaxIdleConnsPerHost int
	// MaxConnsPerHost 限制到单个上游的并发连接上限；0 表示不限制。
	MaxConnsPerHost int
	// Proxy 是形如 http://host:port 的代理地址，空表示直连。
	Proxy string
	// RatePerSecond / Burst 是每主机令牌桶参数；RatePerSecond<=0 表示不限流。
	RatePerSecond float64
	Burst         int
	// Retries 是失败后的额外重试次数。
	Retries int
	// InsecureSkipVerify 跳过硬证书校验，仅用于本机抓包调试。
	InsecureSkipVerify bool
}

// DefaultOptions 返回面向"高并发抓取"调优过的默认参数。
func DefaultOptions() Options {
	return Options{
		Timeout:             12 * time.Second,
		MaxIdleConnsPerHost: 32,
		MaxConnsPerHost:     0,
		RatePerSecond:       8,
		Burst:               16,
		Retries:             2,
	}
}

// Client 是并发安全的上游请求客户端。
type Client struct {
	hc      *http.Client
	limiter *Limiter
	retries int
}

// New 依据 opts 构造 Client。
func New(opts Options) (*Client, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 12 * time.Second
	}
	if opts.MaxIdleConnsPerHost <= 0 {
		opts.MaxIdleConnsPerHost = 32
	}
	if opts.Burst <= 0 {
		opts.Burst = 16
	}

	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          opts.MaxIdleConnsPerHost * 8,
		MaxIdleConnsPerHost:   opts.MaxIdleConnsPerHost,
		MaxConnsPerHost:       opts.MaxConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: opts.Timeout,
		WriteBufferSize:       16 << 10,
		ReadBufferSize:        32 << 10,
	}
	if opts.Proxy != "" {
		pu, err := parseProxy(opts.Proxy)
		if err != nil {
			return nil, fmt.Errorf("netx: 代理地址无效: %w", err)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	if opts.InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 仅供调试
	}

	return &Client{
		hc:      &http.Client{Transport: tr, Timeout: opts.Timeout},
		limiter: NewLimiter(opts.RatePerSecond, opts.Burst),
		retries: opts.Retries,
	}, nil
}

// HTTP 暴露底层 http.Client，供需要精细控制的场景（如流式代理）使用。
func (c *Client) HTTP() *http.Client { return c.hc }

// Limiter 暴露限流器，便于观测。
func (c *Client) Limiter() *Limiter { return c.limiter }

// ErrTooManyRedirects 表示短链展开时重定向层数超限。
var ErrTooManyRedirects = errors.New("netx: 重定向次数过多")

// Do 执行一次请求，自动应用限流、重试与请求体缓冲。
//
// req 的 Body 若不为 nil，调用方必须保证它可重复读（本函数只读一次并缓存）。
func (c *Client) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("netx: 读取请求体失败: %w", err)
		}
		body = b
	}

	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
		}
		if err := c.limiter.Wait(ctx, req.URL.Host); err != nil {
			return nil, err
		}

		attemptReq := req.Clone(ctx)
		if body != nil {
			attemptReq.Body = io.NopCloser(bytes.NewReader(body))
			attemptReq.ContentLength = int64(len(body))
			attemptReq.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
		}

		resp, err := c.hc.Do(attemptReq)
		if err != nil {
			// 上下文取消属于调用方主动放弃，不重试。
			if ctx.Err() != nil {
				return nil, err
			}
			lastErr = err
			continue
		}
		if !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		// 可重试状态码：丢弃响应体以便复用连接。
		lastErr = fmt.Errorf("netx: 上游返回 %d", resp.StatusCode)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}
	return nil, lastErr
}

// Get 发起 GET 并返回响应；调用方负责关闭 Body。
func (c *Client) Get(ctx context.Context, url string, h Headers) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, h)
	return c.Do(ctx, req)
}

// GetBytes 发起 GET 并一次性读取响应体（上限 maxBytes，0 表示 8MiB）。
func (c *Client) GetBytes(ctx context.Context, url string, h Headers, maxBytes int64) ([]byte, int, error) {
	if maxBytes <= 0 {
		maxBytes = 8 << 20
	}
	resp, err := c.Get(ctx, url, h)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// GetString 发起 GET 并返回字符串响应体。
func (c *Client) GetString(ctx context.Context, url string, h Headers) (string, error) {
	b, _, err := c.GetBytes(ctx, url, h, 0)
	return string(b), err
}

// GetText 与 GetString 相同，但额外返回 HTTP 状态码。
// 页面类请求需要状态码来判断风控（如小红书 461/471）。
func (c *Client) GetText(ctx context.Context, url string, h Headers) (string, int, error) {
	b, code, err := c.GetBytes(ctx, url, h, 0)
	return string(b), code, err
}

// GetJSON 发起 GET 并把响应体解码到 out。
func (c *Client) GetJSON(ctx context.Context, url string, h Headers, out any) error {
	b, code, err := c.GetBytes(ctx, url, h, 0)
	if err != nil {
		return err
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("netx: 上游返回 %d", code)
	}
	return decodeJSON(b, out)
}

// PostJSON 以 JSON 请求体发起 POST 并把响应解码到 out。
func (c *Client) PostJSON(ctx context.Context, url string, h Headers, payload any, out any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return fmt.Errorf("netx: 编码请求体失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	applyHeaders(req, h)

	resp, err := c.Do(ctx, req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("netx: 上游返回 %d: %s", resp.StatusCode, truncate(b, 256))
	}
	return decodeJSON(b, out)
}

// ResolveLocation 只发一次请求、不跟随重定向，返回 Location 头。
// 用于展开抖音 v.douyin.com、快手 v.kuaishou.com、B 站 b23.tv 等短链。
func (c *Client) ResolveLocation(ctx context.Context, rawURL string, h Headers) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	applyHeaders(req, h)

	// 用不跟随重定向的 client，但共享同一 Transport（连接池不浪费）。
	noRedirect := &http.Client{
		Transport: c.hc.Transport,
		Timeout:   c.hc.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if err := c.limiter.Wait(ctx, req.URL.Host); err != nil {
		return "", err
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))

	if loc := resp.Header.Get("Location"); loc != "" {
		return loc, nil
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return rawURL, nil // 没有重定向，说明本身就是最终地址
	}
	return "", fmt.Errorf("netx: 短链展开失败，状态码 %d", resp.StatusCode)
}

// --- 内部辅助 ---

func applyHeaders(req *http.Request, h Headers) {
	for k, v := range h {
		req.Header.Set(k, v)
	}
}

// decodeJSON 容忍上游返回的 JSON 尾随垃圾字符（部分平台会带 XSSI 前缀）。
func decodeJSON(b []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(stripXSSIPrefix(b))))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("netx: 解码 JSON 失败: %w (前 200 字节: %s)", err, truncate(b, 200))
	}
	return nil
}

// stripXSSIPrefix 去掉 `)]}',`、`{}&&` 这类防 JSON 劫持前缀。
func stripXSSIPrefix(b []byte) []byte {
	if bytes.HasPrefix(b, []byte("{}&&")) {
		return b[4:]
	}
	if len(b) > 0 && (b[0] == ')' || b[0] == '\'') {
		if i := bytes.IndexByte(b, '\n'); i > 0 && i < 32 {
			return b[i+1:]
		}
	}
	return b
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusRequestTimeout,      // 408
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	// 抖动，避免惊群
	return d + time.Duration(fastrand()%100)*time.Millisecond
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
