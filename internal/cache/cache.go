// Package cache 提供进程内的 TTL 缓存与请求合并（singleflight）。
//
// 为什么这两件事必须放在一起：
// 视频解析的瓶颈是上游 RPC，而"热门视频被并发重复请求"是最常见的浪费。
// 只有缓存没有合并，缓存击穿时仍会打出 N 个上游请求；
// 只有合并没有缓存，串行化的收益无法跨请求保持。两者结合后，
// 同一视频的 N 个并发请求只会命中上游 1 次。
//
// 零第三方依赖：用 16 分片降低锁竞争，用惰性过期避免后台清理 goroutine。
package cache

import (
	"context"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

const shardCount = 16

// Cache 是并发安全的泛型 TTL 缓存。
type Cache[V any] struct {
	shards [shardCount]*shard[V]
	ttl    time.Duration
	// negativeTTL 是失败结果的缓存时长（0 表示不缓存失败）。
	negativeTTL time.Duration
	// negativeFilter 决定某个错误是否**值得**被负缓存。
	//
	// 为什么需要它：失败分两类，混在一起会放大故障。
	// "内容已删除"是确定性事实，缓存 45 秒能省下大量无效上游请求；
	// 而"DNS 超时 / 连接被拒"是瞬时基础设施抖动——把它也缓存 45 秒，
	// 等于让一次网络抖动变成该条目 45 秒内**必失败**，反而放大了故障。
	//
	// nil 表示不做区分（全部缓存），保持与旧行为兼容。
	negativeFilter func(error) bool
	// jitter 是 TTL 的随机抖动比例 [0,1)，用于打散同时到期的键。
	jitter float64

	mu    sync.Mutex
	calls map[string]*call[V]

	hits      atomic.Int64
	misses    atomic.Int64
	coalesced atomic.Int64
	loads     atomic.Int64
}

type shard[V any] struct {
	mu sync.RWMutex
	m  map[string]entry[V]
}

type entry[V any] struct {
	val      V
	expireAt time.Time
	// err 非 nil 表示这是一条**负缓存**：上次加载失败，本条目只是
	// 用来在 negativeTTL 内拦住重复打上游。
	//
	// 为什么要把错误一起存下来：负缓存的 val 是 V 的零值。对
	// V = *core.Video 来说零值就是 nil，与"真的缓存了 nil"无法区分，
	// 于是命中时只能返回一个没有错误信息的空值——调用方最终报出
	// "缓存返回了空结果"这种与真实原因（如 DNS 超时）毫无关系的错。
	// 把原始错误存进来，命中的调用方就能拿到真正的原因。
	err error
}

type call[V any] struct {
	wg     sync.WaitGroup
	val    V
	err    error
	cached bool
}

// New 构造缓存。ttl<=0 时退化为纯 singleflight（不保存结果）。
func New[V any](ttl time.Duration, opts ...Option) *Cache[V] {
	c := &Cache[V]{
		ttl:         ttl,
		negativeTTL: 0,
		jitter:      0.1,
		calls:       make(map[string]*call[V], 64),
	}
	for _, o := range opts {
		o(c)
	}
	for i := range c.shards {
		c.shards[i] = &shard[V]{m: make(map[string]entry[V], 64)}
	}
	return c
}

// Option 用于微调缓存行为。
type Option func(any)

// WithNegativeTTL 开启失败结果缓存，防止对"已删除视频"的持续打点。
//
// 配合 WithNegativeFilter 使用，把瞬时故障排除在外。
func WithNegativeTTL(d time.Duration) Option {
	return func(v any) {
		if c, ok := v.(interface{ setNegativeTTL(time.Duration) }); ok {
			c.setNegativeTTL(d)
		}
	}
}

// WithNegativeFilter 限定哪些错误可以被负缓存；pred 返回 false 的错误不缓存。
//
// 典型用法是只放行"确定性"错误（内容不存在、无权限），
// 把超时与上游 5xx 留给下一次请求重试。
func WithNegativeFilter(pred func(error) bool) Option {
	return func(v any) {
		if c, ok := v.(interface{ setNegativeFilter(func(error) bool) }); ok {
			c.setNegativeFilter(pred)
		}
	}
}

// WithJitter 设置 TTL 抖动比例，取值 [0,0.5]。
func WithJitter(ratio float64) Option {
	return func(v any) {
		if c, ok := v.(interface{ setJitter(float64) }); ok {
			c.setJitter(ratio)
		}
	}
}

func (c *Cache[V]) setNegativeTTL(d time.Duration) { c.negativeTTL = d }

func (c *Cache[V]) setNegativeFilter(pred func(error) bool) { c.negativeFilter = pred }

// cacheableNegative 判断这个错误是否值得被负缓存。
func (c *Cache[V]) cacheableNegative(err error) bool {
	if c.negativeFilter == nil {
		return true
	}
	return c.negativeFilter(err)
}
func (c *Cache[V]) setJitter(r float64) {
	if r < 0 {
		r = 0
	}
	if r > 0.5 {
		r = 0.5
	}
	c.jitter = r
}

func (c *Cache[V]) shardFor(key string) *shard[V] {
	// FNV-1a，足够均匀且无分配。
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return c.shards[h%shardCount]
}

// Get 读取一个未过期的值。
//
// 注意：负缓存条目（上次加载失败）在这里表现为 (零值, true)。
// 想拿到失败原因的调用方应当走 GetOrLoad，它会一并返回那个错误。
func (c *Cache[V]) Get(key string) (V, bool) {
	e, ok := c.getEntry(key)
	return e.val, ok
}

// getEntry 是 Get 的带元数据版本，同时供 GetOrLoad 使用。
func (c *Cache[V]) getEntry(key string) (entry[V], bool) {
	var zero entry[V]
	if c == nil || c.ttl <= 0 {
		return zero, false
	}
	s := c.shardFor(key)
	s.mu.RLock()
	e, ok := s.m[key]
	s.mu.RUnlock()
	if !ok || time.Now().After(e.expireAt) {
		c.misses.Add(1)
		if ok {
			// 惰性删除已过期条目。
			s.mu.Lock()
			delete(s.m, key)
			s.mu.Unlock()
		}
		return zero, false
	}
	c.hits.Add(1)
	return e, true
}

// Set 写入一个值，TTL 为构造时的 ttl（叠加抖动）。
func (c *Cache[V]) Set(key string, val V) {
	c.setWithTTL(key, val, c.ttl)
}

func (c *Cache[V]) setWithTTL(key string, val V, ttl time.Duration) {
	c.setEntry(key, val, nil, ttl)
}

func (c *Cache[V]) setEntry(key string, val V, err error, ttl time.Duration) {
	if c == nil || ttl <= 0 {
		return
	}
	if c.jitter > 0 {
		ttl += time.Duration(float64(ttl) * c.jitter * rand.Float64())
	}
	s := c.shardFor(key)
	s.mu.Lock()
	s.m[key] = entry[V]{val: val, err: err, expireAt: time.Now().Add(ttl)}
	s.mu.Unlock()
}

// Delete 主动失效一个键（例如平台 Cookie 变更后）。
func (c *Cache[V]) Delete(key string) {
	if c == nil {
		return
	}
	s := c.shardFor(key)
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
}

// GetOrLoad 是缓存的核心入口：
//  1. 命中缓存 → 直接返回，cached=true；
//  2. 未命中但已有同 key 的加载在飞行 → 等待并复用其结果（cached=true）；
//  3. 否则真正调用 loader，并写回缓存。
//
// loader 一定会在调用方的 ctx 下执行；等待合并结果的 goroutine 不会被
// 首个请求的 ctx 取消影响（这是刻意的：一个客户端断开不应让其他客户端失败）。
func (c *Cache[V]) GetOrLoad(ctx context.Context, key string, loader func(context.Context) (V, error)) (V, bool, error) {
	if e, ok := c.getEntry(key); ok {
		// 命中的可能是负缓存：此时 e.err 是上次失败的真实原因，
		// 必须原样返回，而不是一个没有解释的零值。
		return e.val, true, e.err
	}

	c.mu.Lock()
	if cl, ok := c.calls[key]; ok {
		c.mu.Unlock()
		c.coalesced.Add(1)
		cl.wg.Wait()
		return cl.val, cl.cached, cl.err
	}
	cl := &call[V]{}
	cl.wg.Add(1)
	c.calls[key] = cl
	c.mu.Unlock()

	c.loads.Add(1)
	val, err := loader(ctx)

	c.mu.Lock()
	if err == nil {
		c.Set(key, val)
	} else if c.negativeTTL > 0 && c.cacheableNegative(err) {
		// 失败也缓存一小段时间，避免对坏 ID 反复打上游。
		// 错误一并存下，命中时才能给出真实原因。
		var zero V
		c.setEntry(key, zero, err, c.negativeTTL)
	}
	// 注意：不满足 filter 的错误**不写缓存**，于是下一个请求会重新打上游。
	// 这正是瞬时故障（超时、5xx）需要的语义：给它一个立刻重试的机会，
	// 而不是把一个 45 秒的"必失败窗口"钉在这个 key 上。
	delete(c.calls, key)
	c.mu.Unlock()

	cl.val, cl.err = val, err
	cl.wg.Done()
	return val, false, err
}

// Stats 是一次快照，用于 /api/v1/stats 观测缓存效果。
type Stats struct {
	Entries   int   `json:"entries"`
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	Coalesced int64 `json:"coalesced"` // 被 singleflight 合并的请求数
	Loads     int64 `json:"loads"`     // 真正打到上游的加载次数
}

// Stats 返回当前统计。
func (c *Cache[V]) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	n := 0
	for _, s := range c.shards {
		s.mu.RLock()
		n += len(s.m)
		s.mu.RUnlock()
	}
	return Stats{
		Entries:   n,
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Coalesced: c.coalesced.Load(),
		Loads:     c.loads.Load(),
	}
}

// Purge 清空所有条目，保留统计。
func (c *Cache[V]) Purge() {
	if c == nil {
		return
	}
	for _, s := range c.shards {
		s.mu.Lock()
		s.m = make(map[string]entry[V], 64)
		s.mu.Unlock()
	}
}
