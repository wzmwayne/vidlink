package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type video struct{ ID string }

// TestNegativeCacheKeepsTheRealError 锁住一个真实踩过的坑。
//
// 场景：一次**偶发**失败（实测是 DNS 超时）被写进负缓存，随后 45 秒内
// 所有同 key 请求都命中那条负缓存。如果负缓存只存了 V 的零值、没存错误，
// 调用方就会拿到 (nil, nil, nil)，只能自己编一个"缓存返回了空结果"
// 之类的错误——把"DNS 超时"报成了"缓存故障"，排障时南辕北辙。
//
// 这里断言：命中负缓存时，返回的必须是**原来那个错误**。
func TestNegativeCacheKeepsTheRealError(t *testing.T) {
	c := New[*video](time.Minute, WithNegativeTTL(time.Minute))
	ctx := context.Background()
	wantErr := errors.New("dial tcp: lookup v.douyin.com: i/o timeout")

	var calls int32
	load := func(context.Context) (*video, error) {
		atomic.AddInt32(&calls, 1)
		return nil, wantErr
	}

	// 第一次：真打 loader
	v, hit, err := c.GetOrLoad(ctx, "k", load)
	if v != nil || hit {
		t.Fatalf("首次应未命中且返回 nil，得到 v=%v hit=%v", v, hit)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("首次错误 = %v, want %v", err, wantErr)
	}

	// 第二次：应命中负缓存，**不得**再打上游，且必须带回同一个错误
	v, hit, err = c.GetOrLoad(ctx, "k", load)
	if hit != true {
		t.Fatalf("第二次应命中负缓存，hit=%v", hit)
	}
	if err == nil {
		t.Fatal("命中负缓存时错误丢了：调用方会拿到 (nil, true, nil) 无法判断原因")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("命中负缓存后错误 = %v, want %v", err, wantErr)
	}
	if v != nil {
		t.Fatalf("负缓存的值应为零值，得到 %v", v)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("loader 被调用了 %d 次，负缓存未生效", n)
	}
}

// TestNegativeCacheExpires 确认负缓存会过期，不会永久挡住一个 ID。
func TestNegativeCacheExpires(t *testing.T) {
	c := New[*video](time.Minute, WithNegativeTTL(20*time.Millisecond))
	ctx := context.Background()
	boom := errors.New("boom")

	var calls int32
	load := func(context.Context) (*video, error) {
		atomic.AddInt32(&calls, 1)
		return nil, boom
	}
	_, _, _ = c.GetOrLoad(ctx, "k", load)
	time.Sleep(60 * time.Millisecond)
	_, hit, _ := c.GetOrLoad(ctx, "k", load)
	if hit {
		t.Fatal("负缓存过期后不应再命中")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("loader 调用次数 = %d, want 2", n)
	}
}

// TestNegativeCacheDoesNotPoisonSuccess 确认失败之后仍能写入成功结果。
func TestNegativeCacheDoesNotPoisonSuccess(t *testing.T) {
	c := New[*video](time.Minute, WithNegativeTTL(10*time.Millisecond))
	ctx := context.Background()

	_, _, _ = c.GetOrLoad(ctx, "k", func(context.Context) (*video, error) {
		return nil, errors.New("transient")
	})
	time.Sleep(40 * time.Millisecond)

	v, hit, err := c.GetOrLoad(ctx, "k", func(context.Context) (*video, error) {
		return &video{ID: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("重试应成功，得到 %v", err)
	}
	if hit {
		t.Fatal("重试不应命中负缓存")
	}
	if v == nil || v.ID != "ok" {
		t.Fatalf("值 = %v, want ok", v)
	}

	// 再读一次，这次是正常正缓存：不应带错误
	v2, hit2, err2 := c.GetOrLoad(ctx, "k", func(context.Context) (*video, error) {
		t.Fatal("正缓存命中时不应调用 loader")
		return nil, nil
	})
	if err2 != nil || !hit2 || v2 == nil || v2.ID != "ok" {
		t.Fatalf("正缓存命中异常: v=%v hit=%v err=%v", v2, hit2, err2)
	}
}

// TestGetOnNegativeEntryReturnsZeroTrue 记录 Get 的行为边界：
// 它不知道错误，所以负缓存条目对它表现为 (零值, true)。
// 需要错误信息的调用方必须用 GetOrLoad。
func TestGetOnNegativeEntryReturnsZeroTrue(t *testing.T) {
	c := New[*video](time.Minute, WithNegativeTTL(time.Minute))
	_, _, _ = c.GetOrLoad(context.Background(), "k", func(context.Context) (*video, error) {
		return nil, errors.New("x")
	})
	v, ok := c.Get("k")
	if v != nil || !ok {
		t.Fatalf("Get 对负缓存条目 = (%v, %v), want (nil, true)", v, ok)
	}
}

// TestNegativeFilterExcludesTransientErrors 是"不要放大故障"的回归测试。
//
// 背景（实测踩过）：一次 DNS 超时被写进负缓存，随后 45 秒内所有同 key
// 请求都在 3 毫秒内返回同一个错误——一次网络抖动被放大成"该条目 45 秒内必失败"。
//
// 区分标准是"这个结论会不会变"：内容已删除是事实，超时不是。
func TestNegativeFilterExcludesTransientErrors(t *testing.T) {
	ctx := context.Background()
	var errTransient = errors.New("dial tcp: i/o timeout")
	var errGone = errors.New("video deleted")

	// 只允许"确定性"错误进负缓存
	c := New[*video](time.Minute,
		WithNegativeTTL(time.Minute),
		WithNegativeFilter(func(err error) bool { return errors.Is(err, errGone) }))

	// 瞬时错误：不缓存 → 每次都要真打上游
	var transientCalls int32
	for i := 0; i < 3; i++ {
		_, hit, err := c.GetOrLoad(ctx, "t", func(context.Context) (*video, error) {
			atomic.AddInt32(&transientCalls, 1)
			return nil, errTransient
		})
		if hit {
			t.Fatalf("第 %d 次不应命中负缓存（瞬时错误不该被缓存）", i+1)
		}
		if !errors.Is(err, errTransient) {
			t.Fatalf("错误应为原始错误，得到 %v", err)
		}
	}
	if n := atomic.LoadInt32(&transientCalls); n != 3 {
		t.Fatalf("瞬时错误的 loader 调用次数 = %d, want 3（每次都应重试上游）", n)
	}

	// 确定性错误：缓存 → 第二次不再打上游
	var goneCalls int32
	for i := 0; i < 3; i++ {
		_, _, _ = c.GetOrLoad(ctx, "g", func(context.Context) (*video, error) {
			atomic.AddInt32(&goneCalls, 1)
			return nil, errGone
		})
	}
	if n := atomic.LoadInt32(&goneCalls); n != 1 {
		t.Fatalf("确定性错误的 loader 调用次数 = %d, want 1（应当被负缓存拦住）", n)
	}
}

// TestGetOrLoadCoalesces 确认 singleflight 仍然生效（改动没有破坏它）。
func TestGetOrLoadCoalesces(t *testing.T) {
	c := New[*video](time.Minute)
	var calls int32
	load := func(context.Context) (*video, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(30 * time.Millisecond)
		return &video{ID: "v"}, nil
	}

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if v, _, err := c.GetOrLoad(context.Background(), "same", load); err != nil || v == nil {
				t.Errorf("并发取回失败: v=%v err=%v", v, err)
			}
		}()
	}
	wg.Wait()

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("loader 被调用 %d 次，singleflight 未生效", n)
	}
}
