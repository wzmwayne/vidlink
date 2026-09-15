package gate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPerKeyRejectsSecondRequest(t *testing.T) {
	g := New(Options{PerKey: 1, Global: 100})

	rel, err := g.AcquireKey("k")
	if err != nil {
		t.Fatalf("第一次应成功: %v", err)
	}

	// 同一个 Key 的第二个请求必须**立即**失败，而不是排队等待
	start := time.Now()
	if _, err := g.AcquireKey("k"); !errors.Is(err, ErrKeyBusy) {
		t.Fatalf("第二次应报 ErrKeyBusy，得到 %v", err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("Key 冲突应立返回，却花了 %v", d)
	}

	// 不同 Key 互不影响
	rel2, err := g.AcquireKey("other")
	if err != nil {
		t.Fatalf("不同 Key 不应互相阻塞: %v", err)
	}
	rel2()

	rel()
	// 释放后应能再次获取
	rel3, err := g.AcquireKey("k")
	if err != nil {
		t.Fatalf("释放后应能重新获取: %v", err)
	}
	rel3()
}

// TestReleaseIsIdempotent：重复释放不能把计数减成负数，
// 否则同一个 Key 就能绕过并发限制。
func TestReleaseIsIdempotent(t *testing.T) {
	g := New(Options{PerKey: 1, Global: 100})

	rel, _ := g.AcquireKey("k")
	rel()
	rel() // 再释放一次

	rel2, err := g.AcquireKey("k")
	if err != nil {
		t.Fatalf("重复释放后应仍能被正常占用: %v", err)
	}
	// 此时若计数被减成负，第二次获取就会错误地通过
	if _, err := g.AcquireKey("k"); !errors.Is(err, ErrKeyBusy) {
		t.Fatalf("重复释放破坏了并发计数，第二次获取未被告知忙: %v", err)
	}
	rel2()
}

func TestGlobalLimitAndQueue(t *testing.T) {
	g := New(Options{PerKey: 10, Global: 2, MaxQueue: 10, WaitTimeout: 5 * time.Second})
	ctx := context.Background()

	r1, err := g.AcquireTask(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := g.AcquireTask(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 第三个应排队，而不是立刻失败
	got := make(chan struct{})
	go func() {
		r3, err := g.AcquireTask(ctx)
		if err != nil {
			t.Errorf("排队的任务应最终成功: %v", err)
			close(got)
			return
		}
		r3()
		close(got)
	}()

	select {
	case <-got:
		t.Fatal("全局槽位已满时第三个任务不应立刻通过")
	case <-time.After(80 * time.Millisecond):
		// 符合预期：正在排队
	}

	r1() // 腾出一个槽位
	select {
	case <-got:
		// 排队者被唤醒，符合预期
	case <-time.After(2 * time.Second):
		t.Fatal("释放槽位后排队的任务未被唤醒")
	}
	r2()
}

func TestQueueFull(t *testing.T) {
	g := New(Options{PerKey: 10, Global: 1, MaxQueue: 0, WaitTimeout: time.Second})
	ctx := context.Background()

	rel, err := g.AcquireTask(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	// MaxQueue=0 表示不允许排队 → 立即拒绝
	start := time.Now()
	if _, err := g.AcquireTask(ctx); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("应报 ErrQueueFull，得到 %v", err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("队满应立即拒绝，却花了 %v", d)
	}
}

func TestQueueTimeout(t *testing.T) {
	g := New(Options{PerKey: 10, Global: 1, MaxQueue: 10, WaitTimeout: 60 * time.Millisecond})
	ctx := context.Background()

	rel, err := g.AcquireTask(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	if _, err := g.AcquireTask(ctx); !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("应报 ErrQueueTimeout，得到 %v", err)
	}
}

func TestContextCancelUnblocksQueue(t *testing.T) {
	g := New(Options{PerKey: 10, Global: 1, MaxQueue: 10, WaitTimeout: 10 * time.Second})

	rel, err := g.AcquireTask(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := g.AcquireTask(ctx)
		done <- err
	}()

	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("应返回 context.Canceled，得到 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后排队者未被唤醒")
	}
}

// TestTwoGatesAreIndependent：Key 闸门与 Task 闸门计数单位不同，
// 一个批量请求只占 1 个 Key 槽位，但它的每条任务各占一个 Task 槽位。
func TestTwoGatesAreIndependent(t *testing.T) {
	g := New(Options{PerKey: 1, Global: 10, MaxQueue: 0, WaitTimeout: time.Second})

	keyRel, err := g.AcquireKey("customer")
	if err != nil {
		t.Fatal(err)
	}
	defer keyRel()

	// 同一 Key 下仍可以取多个 Task 槽位（批量场景）
	var rels []func()
	for i := 0; i < 3; i++ {
		r, err := g.AcquireTask(context.Background())
		if err != nil {
			t.Fatalf("第 %d 个任务应能取得槽位: %v", i+1, err)
		}
		rels = append(rels, r)
	}
	if st := g.Stats(); st.GlobalInUse != 3 {
		t.Fatalf("全局占用 = %d, want 3", st.GlobalInUse)
	}
	// 但第二个 HTTP 请求会被 Key 闸门挡住
	if _, err := g.AcquireKey("customer"); !errors.Is(err, ErrKeyBusy) {
		t.Fatalf("Key 闸门应挡住第二个请求，得到 %v", err)
	}
	for _, r := range rels {
		r()
	}
}

// TestGlobalLimitHoldsUnderLoad：并发压测，确认不超卖槽位。
func TestGlobalLimitHoldsUnderLoad(t *testing.T) {
	const limit = 4
	g := New(Options{PerKey: 100, Global: limit, MaxQueue: 1000, WaitTimeout: 10 * time.Second})

	var inUse, maxSeen int64
	var wg sync.WaitGroup
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := g.AcquireTask(ctx)
			if err != nil {
				return
			}
			cur := atomic.AddInt64(&inUse, 1)
			for {
				old := atomic.LoadInt64(&maxSeen)
				if cur <= old || atomic.CompareAndSwapInt64(&maxSeen, old, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt64(&inUse, -1)
			rel()
		}()
	}
	wg.Wait()

	if maxSeen > limit {
		t.Fatalf("同时占用峰值 %d 超过上限 %d", maxSeen, limit)
	}
	if st := g.Stats(); st.GlobalInUse != 0 {
		t.Fatalf("全部释放后占用应为 0，得到 %d", st.GlobalInUse)
	}
}

func TestStats(t *testing.T) {
	g := New(Options{PerKey: 1, Global: 2, MaxQueue: 0})
	rel, _ := g.AcquireKey("a")
	_, _ = g.AcquireKey("a") // 触发 key busy

	if st := g.Stats(); st.RejectedBusy != 1 {
		t.Fatalf("Key 冲突计数 = %d, want 1", st.RejectedBusy)
	}
	rel()

	t1, _ := g.AcquireTask(context.Background())
	t2, _ := g.AcquireTask(context.Background())
	_, _ = g.AcquireTask(context.Background()) // 队满

	st := g.Stats()
	if st.GlobalLimit != 2 || st.GlobalInUse != 2 || st.GlobalFree != 0 {
		t.Fatalf("全局统计不对: %+v", st)
	}
	if st.RejectedQueue != 1 {
		t.Fatalf("队满计数 = %d, want 1", st.RejectedQueue)
	}
	if st.ActiveKeys != 0 {
		t.Fatalf("Key 已释放，活跃 Key 应为 0，得到 %d", st.ActiveKeys)
	}
	t1()
	t2()
}

// TestKeyMapDoesNotGrowForever：Key 释放后要从 map 里清掉，
// 否则长期运行会随访问过的 Key 数量无限增长。
func TestKeyMapDoesNotGrowForever(t *testing.T) {
	g := New(Options{PerKey: 1, Global: 1000})
	for i := 0; i < 500; i++ {
		rel, err := g.AcquireKey(string(rune('A' + i%26)))
		if err != nil {
			t.Fatalf("第 %d 次: %v", i, err)
		}
		rel()
	}
	if st := g.Stats(); st.ActiveKeys != 0 {
		t.Fatalf("释放后活跃 Key 应为 0，得到 %d（map 在泄漏）", st.ActiveKeys)
	}
}
