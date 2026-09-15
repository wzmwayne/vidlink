package netx

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"time"
)

// Limiter 是"按主机维度"的令牌桶限流器。
//
// 为什么按主机而不是全局：不同平台的容忍度差异极大（B 站宽松、
// 抖音风控严格），全局桶会让慢平台拖垮快平台；按主机隔离后可以各自调速。
//
// 实现用惰性补充（lazy refill）而不是后台 ticker：
// 高并发下没有定时器开销，空闲主机不占 CPU。
type Limiter struct {
	rate  float64 // 每秒补充的令牌数
	burst float64 // 桶容量

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLimiter 构造限流器。rate<=0 表示不限流（Wait 直接返回）。
func NewLimiter(rate float64, burst int) *Limiter {
	if burst < 1 {
		burst = 1
	}
	return &Limiter{
		rate:    rate,
		burst:   float64(burst),
		buckets: make(map[string]*bucket, 16),
	}
}

// Wait 阻塞直到 host 获得一个令牌，或 ctx 结束。
func (l *Limiter) Wait(ctx context.Context, host string) error {
	if l == nil || l.rate <= 0 {
		return nil
	}
	host = canonicalHost(host)

	for {
		wait, ok := l.take(host, time.Now())
		if ok {
			return nil
		}
		if wait <= 0 {
			wait = time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// take 尝试取一个令牌，返回还需等待的时长。
func (l *Limiter) take(host string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[host]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[host] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * l.rate
			if b.tokens > l.burst {
				b.tokens = l.burst
			}
			b.last = now
		}
	}

	if b.tokens >= 1 {
		b.tokens--
		return 0, true
	}
	need := (1 - b.tokens) / l.rate
	return time.Duration(need * float64(time.Second)), false
}

// SetRate 运行期调整某个主机的速率，便于按上游反馈动态降速。
func (l *Limiter) SetRate(rate float64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.rate = rate
	l.mu.Unlock()
}

// Snapshot 返回当前各主机剩余令牌，仅供 /debug 观测。
func (l *Limiter) Snapshot() map[string]float64 {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]float64, len(l.buckets))
	for k, v := range l.buckets {
		out[k] = v.tokens
	}
	return out
}

func canonicalHost(host string) string {
	if host == "" {
		return ""
	}
	// 去掉端口，避免同一主机因端口不同被当成两个桶。
	for i := 0; i < len(host); i++ {
		if host[i] == ':' {
			return host[:i]
		}
	}
	return host
}

// parseProxy 解析代理地址，仅接受 http/https/socks5 scheme。
func parseProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return u, nil
	default:
		return nil, errors.New("不支持的代理协议（仅支持 http/https/socks5）")
	}
}

// fastrand 是无锁伪随机数，用于重试抖动，避免全局 rand 锁竞争。
var fastrandState uint64 = uint64(time.Now().UnixNano()) | 1
var fastrandMu sync.Mutex

func fastrand() uint32 {
	fastrandMu.Lock()
	// xorshift64
	fastrandState ^= fastrandState << 13
	fastrandState ^= fastrandState >> 7
	fastrandState ^= fastrandState << 17
	v := fastrandState
	fastrandMu.Unlock()
	return uint32(v)
}
