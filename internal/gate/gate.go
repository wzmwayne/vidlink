// Package gate 是两道并发闸门。
//
// 为什么是两道，而不是一道
//
// 它们防的是完全不同的东西，计数单位也不同：
//
//	AcquireKey  —— 每个 API Key 同时只允许 1 个请求。
//	               防的是"一个客户把服务占满"，属于公平性与成本控制。
//	               超出**立即报错**，不排队：让客户自己去串行化，
//	               比让他无限等待更诚实（等待还会占着连接和内存）。
//
//	AcquireTask —— 全服务同时最多 10 个解析任务，超出的排队。
//	               防的是"上游被我们打爆"以及"自己被 goroutine 撑爆"。
//	               这是保护上游与自己的最后一道闸门。
//
// **为什么不能只做一道**：批量接口会一次解析 20 条。若按"HTTP 请求"计数，
// 一个批量请求只占 1 个槽位，但它内部会同时打出多个上游请求——
// 全局上限 10 就形同虚设。所以按"解析任务"计数，批量的每一条都要来抢槽。
//
// 于是两道闸门的分工是：
//
//	Key 闸门 计"HTTP 请求"，保证一个客户同时只做一件事；
//	Task 闸门 计"解析任务"，保证整机对上游的压力有硬上限。
//
// 排队而不是拒绝
//
// Task 闸门在满时会排队，但排队必须有边界，否则等待者本身就会耗尽内存：
//
//	最大排队数   超出 → 立即拒绝（503），避免无限堆积 goroutine
//	最长等待时间 超出 → 拒绝，避免客户端连接被长时间吊住
//
// 两者都是可配的，默认值见 DefaultOptions。
package gate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// 闸门拒绝的两种原因。调用方据此选 HTTP 状态码与提示语。
var (
	// ErrKeyBusy 该 Key 已有请求在跑。立即返回，不排队。
	ErrKeyBusy = errors.New("同一个 API Key 同时只允许一个请求")

	// ErrQueueFull 排队的人太多了。
	ErrQueueFull = errors.New("服务繁忙：排队已满")

	// ErrQueueTimeout 排了太久还没轮到。
	ErrQueueTimeout = errors.New("服务繁忙：排队超时")
)

// Options 是闸门参数。
type Options struct {
	// PerKey 是每个 Key 的并发上限。默认 1。
	PerKey int
	// Global 是全局解析任务上限。默认 10。
	Global int
	// MaxQueue 是最大排队数。默认 30（约 3 倍全局容量）。
	MaxQueue int
	// WaitTimeout 是最长排队时间。默认 15s——比单次解析超时略短，
	// 保证客户端拿到明确答复而不是被吊到自身超时。
	WaitTimeout time.Duration
}

// DefaultOptions 返回默认参数。
func DefaultOptions() Options {
	return Options{
		PerKey:      1,
		Global:      10,
		MaxQueue:    30,
		WaitTimeout: 15 * time.Second,
	}
}

func (o Options) normalize() Options {
	d := DefaultOptions()
	if o.PerKey <= 0 {
		o.PerKey = d.PerKey
	}
	if o.Global <= 0 {
		o.Global = d.Global
	}
	if o.MaxQueue < 0 {
		o.MaxQueue = d.MaxQueue
	}
	if o.WaitTimeout <= 0 {
		o.WaitTimeout = d.WaitTimeout
	}
	return o
}

// Gate 是两道闸门。
type Gate struct {
	opts Options

	// global 用带缓冲 channel 当信号量：满时发送会阻塞，天然形成排队。
	global chan struct{}

	mu     sync.Mutex
	perKey map[string]int

	// waiting 是当前排队中的任务数，用于 MaxQueue 判断。
	waiting atomic.Int64

	// 统计，供 /v1/admin/stats 观测闸门是否成为瓶颈。
	rejectedKey   atomic.Int64
	rejectedQueue atomic.Int64
	timedOut      atomic.Int64
}

// New 构造闸门。
func New(opts Options) *Gate {
	opts = opts.normalize()
	return &Gate{
		opts:   opts,
		global: make(chan struct{}, opts.Global),
		perKey: make(map[string]int, 64),
	}
}

// Options 返回生效的参数（便于日志与文档展示）。
func (g *Gate) Options() Options { return g.opts }

// AcquireKey 占用该 Key 的一个并发位。
//
// 超出上限时**立即返回 ErrKeyBusy**，不排队——见包注释的理由。
// 返回的 release 必须被调用（用 defer），否则该 Key 会被永久锁死。
func (g *Gate) AcquireKey(key string) (release func(), err error) {
	g.mu.Lock()
	if g.perKey[key] >= g.opts.PerKey {
		g.mu.Unlock()
		g.rejectedKey.Add(1)
		return nil, ErrKeyBusy
	}
	g.perKey[key]++
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.perKey[key]--
			if g.perKey[key] <= 0 {
				// 清理空条目，否则 Key 多了之后这个 map 只增不减
				delete(g.perKey, key)
			}
			g.mu.Unlock()
		})
	}, nil
}

// AcquireTask 占用一个全局解析槽位，必要时排队。
//
// 批量接口应当**每条任务各调一次**，而不是整个请求只调一次——
// 否则全局上限会被批量绕过。
func (g *Gate) AcquireTask(ctx context.Context) (release func(), err error) {
	// 先试一次无阻塞获取：空闲时不必碰等待计数，也不必启动定时器。
	select {
	case g.global <- struct{}{}:
		return g.releaseTask, nil
	default:
	}

	// 需要排队。waiting 只统计"正在等待、还没拿到槽位"的 goroutine，
	// 所以占位要在**尝试获取之前**完成，且无论走哪条分支都要还回去。
	if n := g.waiting.Add(1); n > int64(g.opts.MaxQueue) {
		g.waiting.Add(-1)
		g.rejectedQueue.Add(1)
		return nil, ErrQueueFull
	}
	defer g.waiting.Add(-1)

	timer := time.NewTimer(g.opts.WaitTimeout)
	defer timer.Stop()

	select {
	case g.global <- struct{}{}:
		return g.releaseTask, nil
	case <-timer.C:
		g.timedOut.Add(1)
		return nil, ErrQueueTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *Gate) releaseTask() { <-g.global }

// Stats 是闸门的实时观测值。
type Stats struct {
	GlobalLimit   int   `json:"global_limit"`
	GlobalInUse   int   `json:"global_in_use"`
	GlobalFree    int   `json:"global_free"`
	Waiting       int64 `json:"waiting"`
	MaxQueue      int   `json:"max_queue"`
	ActiveKeys    int   `json:"active_keys"`
	RejectedBusy  int64 `json:"rejected_key_busy"`
	RejectedQueue int64 `json:"rejected_queue_full"`
	TimedOut      int64 `json:"timed_out"`
}

// Stats 返回当前观测值。
func (g *Gate) Stats() Stats {
	g.mu.Lock()
	keys := len(g.perKey)
	g.mu.Unlock()
	used := len(g.global)
	return Stats{
		GlobalLimit:   g.opts.Global,
		GlobalInUse:   used,
		GlobalFree:    g.opts.Global - used,
		Waiting:       g.waiting.Load(),
		MaxQueue:      g.opts.MaxQueue,
		ActiveKeys:    keys,
		RejectedBusy:  g.rejectedKey.Load(),
		RejectedQueue: g.rejectedQueue.Load(),
		TimedOut:      g.timedOut.Load(),
	}
}
