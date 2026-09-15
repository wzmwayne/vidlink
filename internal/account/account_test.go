package account

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "ledger.jsonl")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateAndGet(t *testing.T) {
	s := newStore(t)
	a, err := s.Create(Account{Key: "k1", Name: "客户甲", Quota: 100, Multiplier: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
		t.Error("时间戳应被自动填充")
	}

	got, ok := s.Get("k1")
	if !ok {
		t.Fatal("应能取回")
	}
	if got.Quota != 100 || got.Name != "客户甲" {
		t.Fatalf("取回内容不对: %+v", got)
	}

	if _, err := s.Create(Account{Key: "k1"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("重复创建应报 ErrDuplicate，得到 %v", err)
	}
	if _, err := s.Create(Account{Key: ""}); err == nil {
		t.Fatal("空 Key 应被拒绝")
	}
}

// TestConsumeIsAtomicUnderConcurrency 是账本最重要的一条测试。
//
// "先读配额判断、再写扣减"的两步写法在并发下会把配额扣成负数。
// 这里用大量并发扣减配额验证：无论怎么并发，配额都不会被扣穿。
func TestConsumeIsAtomicUnderConcurrency(t *testing.T) {
	s := newStore(t)
	const balance = 100.0
	if _, err := s.Create(Account{Key: "k", Quota: balance, Multiplier: 1}); err != nil {
		t.Fatal(err)
	}

	const workers = 200
	var wg sync.WaitGroup
	var ok, insufficient int
	var mu sync.Mutex

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			_, err := s.Consume("k", 1.0)
			mu.Lock()
			switch {
			case err == nil:
				ok++
			case errors.As(err, &ErrQuotaExhausted{}):
				insufficient++
			default:
				t.Errorf("意外错误: %v", err)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if ok != int(balance) {
		t.Fatalf("成功次数 = %d, want %d（配额 100，每次扣 1）", ok, int(balance))
	}
	got, _ := s.Get("k")
	if got.Quota != 0 {
		t.Fatalf("配额 = %v, want 0（不能扣成负数）", got.Quota)
	}
	if got.Calls != int64(ok) {
		t.Fatalf("Calls = %d, want %d", got.Calls, ok)
	}
	if got.Used != float64(ok) {
		t.Fatalf("Used = %v, want %v", got.Used, float64(ok))
	}
}

// TestZeroQuotaAccountStillCountsUsage：倍率为 0 的免费账号不扣配额，但用量要记账。
func TestZeroQuotaAccountStillCountsUsage(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(Account{Key: "free", Quota: 0, Multiplier: 0}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Consume("free", 0); err != nil {
			t.Fatalf("免费账号扣 0 不应失败: %v", err)
		}
	}
	got, _ := s.Get("free")
	if got.Quota != 0 {
		t.Fatalf("免费账号配额应保持 0，得到 %v", got.Quota)
	}
	if got.Calls != 3 {
		t.Fatalf("免费账号也要记调用次数，得到 %d", got.Calls)
	}
}

func TestConsumeQuotaExhausted(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(Account{Key: "k", Quota: 0.5, Multiplier: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Consume("k", 1.0)
	var e ErrQuotaExhausted
	if !errors.As(err, &e) {
		t.Fatalf("应报 ErrQuotaExhausted，得到 %v", err)
	}
	if e.Need != 1.0 || e.Have != 0.5 {
		t.Fatalf("错误详情不对: %+v", e)
	}
	// 失败的扣减配额不能动配额
	got, _ := s.Get("k")
	if got.Quota != 0.5 || got.Calls != 0 {
		t.Fatalf("失败的扣减配额不应改变配额或用量的，得到 %+v", got)
	}
}

func TestConsumeUnknownKey(t *testing.T) {
	s := newStore(t)
	if _, err := s.Consume("nope", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("应报 ErrNotFound，得到 %v", err)
	}
}

func TestUpdatePatch(t *testing.T) {
	s := newStore(t)
	base := time.Now().Add(-time.Hour)
	if _, err := s.Create(Account{Key: "k", Quota: 10, Multiplier: 1, CreatedAt: base}); err != nil {
		t.Fatal(err)
	}

	// 促销：五折
	half := 0.5
	name := "促销客户"
	if _, err := s.Update("k", Patch{Multiplier: &half, Name: &name}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("k")
	if got.Multiplier != 0.5 || got.Name != "促销客户" {
		t.Fatalf("Patch 未生效: %+v", got)
	}
	if got.Quota != 10 {
		t.Fatalf("未提及的字段不该被改动，配额变成了 %v", got.Quota)
	}

	// 配额分配（增量）
	add := 90.0
	if _, err := s.Update("k", Patch{AddQuota: &add}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get("k")
	if got.Quota != 100 {
		t.Fatalf("增量配额分配后配额 = %v, want 100", got.Quota)
	}

	// 直接设值
	exact := 7.0
	if _, err := s.Update("k", Patch{Quota: &exact}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get("k")
	if got.Quota != 7 {
		t.Fatalf("设值后配额 = %v, want 7", got.Quota)
	}

	// 负倍率应被拒绝
	neg := -1.0
	if _, err := s.Update("k", Patch{Multiplier: &neg}); err == nil {
		t.Fatal("负倍率应被拒绝")
	}
}

// TestPersistenceReplay 是持久化的核心验证：写完重开，账本要一模一样。
func TestPersistenceReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")

	s, err := New(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(Account{Key: "a", Name: "甲", Quota: 100, Multiplier: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(Account{Key: "b", Quota: 50, Multiplier: 0.5}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume("a", 1.2); err != nil {
		t.Fatal(err)
	}
	hundred := 200.0
	if _, err := s.Update("b", Patch{AddQuota: &hundred}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("b"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// 重开
	s2, err := New(Options{Path: path})
	if err != nil {
		t.Fatalf("回放失败: %v", err)
	}
	defer func() { _ = s2.Close() }()

	a, ok := s2.Get("a")
	if !ok {
		t.Fatal("账号 a 应在回放后存在")
	}
	if a.Quota != 98.8 {
		t.Fatalf("账号 a 配额 = %v, want 98.8", a.Quota)
	}
	if a.Used != 1.2 || a.Calls != 1 {
		t.Fatalf("账号 a 用量未恢复: used=%v calls=%d", a.Used, a.Calls)
	}
	if a.Name != "甲" {
		t.Fatalf("账号 a 名称未恢复: %q", a.Name)
	}
	if _, ok := s2.Get("b"); ok {
		t.Fatal("账号 b 已删除，回放后不应存在（墓碑未生效）")
	}
}

// TestReplaySkipsCorruptLines：一行损坏不能毁掉整个账本。
func TestReplaySkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")
	content := `{"key":"a","quota":10,"multiplier":1}
这不是 JSON
{"key":"b","quota":20,"multiplier":1}
{"key":""}
{"key":"c","quota":30,"multiplier":1}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Path: path})
	if err != nil {
		t.Fatalf("有坏行也应能启动: %v", err)
	}
	defer func() { _ = s.Close() }()

	if s.Count() != 3 {
		t.Fatalf("应恢复 3 个账号，得到 %d", s.Count())
	}
	for _, k := range []string{"a", "b", "c"} {
		if _, ok := s.Get(k); !ok {
			t.Errorf("账号 %s 未恢复", k)
		}
	}
}

func TestMasked(t *testing.T) {
	long := Account{Key: "vl_1234567890abcdef"}
	m := long.Masked()
	if m == long.Key {
		t.Fatal("长 Key 必须被掩码")
	}
	if m[:4] != "vl_1" || m[len(m)-4:] != "cdef" {
		t.Errorf("掩码应保留前 4 后 4，得到 %q", m)
	}
	// 刻意**不**保留原始长度：长度本身也是信息（能推断 Key 的生成规则）
	if len(m) == len(long.Key) {
		t.Errorf("掩码不应泄露 Key 长度，得到 %q（原长 %d）", m, len(long.Key))
	}
	short := Account{Key: "abc"}
	if short.Masked() != "***" {
		t.Errorf("短 Key 应全掩，得到 %q", short.Masked())
	}

	// Public 不应泄露原始 Key
	p := long.Public()
	if p.Key == long.Key {
		t.Fatal("Public 泄露了原始 Key")
	}
}

func TestListIsMaskedAndSorted(t *testing.T) {
	s := newStore(t)
	for _, k := range []string{"k3", "k1", "k2"} {
		now := time.Now()
		if _, err := s.Create(Account{Key: k, Quota: 1, Multiplier: 1, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	list := s.List()
	if len(list) != 3 {
		t.Fatalf("应有 3 个账号，得到 %d", len(list))
	}
	for _, a := range list {
		if a.Key == "k1" || a.Key == "k2" || a.Key == "k3" {
			t.Fatalf("List 不应返回明文 Key: %q", a.Key)
		}
	}
	if !list[0].CreatedAt.Before(list[2].CreatedAt) && !list[0].CreatedAt.Equal(list[2].CreatedAt) {
		t.Error("List 应按创建时间升序")
	}
}

func TestStats(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(Account{Key: "a", Quota: 100, Multiplier: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(Account{Key: "b", Quota: 50, Multiplier: 1, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume("a", 10); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Accounts != 2 || st.Active != 1 {
		t.Fatalf("账号数/活跃数不对: %+v", st)
	}
	if st.UsedTotal != 10 || st.CallsTotal != 1 {
		t.Fatalf("用量汇总不对: %+v", st)
	}
	if st.QuotaTotal != 140 {
		t.Fatalf("配额汇总 = %v, want 140", st.QuotaTotal)
	}
}

// TestMemoryOnlyStore 验证 Path 为空时不落盘也能正常工作（测试与临时实例用）。
func TestMemoryOnlyStore(t *testing.T) {
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Create(Account{Key: "x", Quota: 5, Multiplier: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume("x", 5); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("x")
	if got.Quota != 0 {
		t.Fatalf("配额 = %v, want 0", got.Quota)
	}
}
