package account

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLedgerRecordsEachKind：四类业务动作都要留下可读的流水。
//
// 这是"账单"能回答"这笔钱花在哪、是谁加的"的前提：只存余额与累计用量
// 是答不出来的（管理员加了 100 又减了 50，余额上看不出发生过什么）。
func TestLedgerRecordsEachKind(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, err := New(Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.Create(Account{Key: "vl_a", Name: "甲", Quota: 100, Multiplier: 1}); err != nil {
		t.Fatal(err)
	}
	// 使用：消耗 1.2
	if _, err := s.Consume("vl_a", 1.2, "links/bilibili ×1"); err != nil {
		t.Fatal(err)
	}
	// 管理员增加 / 减少 / 设置倍率
	add, minus, mult := 50.0, -20.0, 0.5
	if _, err := s.Update("vl_a", Patch{AddQuota: &add}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update("vl_a", Patch{AddQuota: &minus}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update("vl_a", Patch{Multiplier: &mult}); err != nil {
		t.Fatal(err)
	}

	entries, totals := s.Ledger(LedgerQuery{Key: "vl_a", Limit: 20})
	if len(entries) != 5 {
		t.Fatalf("应有 5 条流水，得到 %d：%+v", len(entries), entries)
	}
	// 新的在前
	kinds := make([]EntryType, 0, len(entries))
	for _, e := range entries {
		kinds = append(kinds, e.Type)
	}
	want := []EntryType{EntrySet, EntryReduce, EntryAdd, EntryConsume, EntryCreate}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("第 %d 条类型 = %q，想要 %q（完整：%v）", i, kinds[i], want[i], kinds)
		}
	}
	// 消耗记的是负的变化量；余额逐条对得上
	for _, e := range entries {
		if e.Type == EntryConsume && e.Units != -1.2 {
			t.Errorf("消耗流水 units = %v，想要 -1.2", e.Units)
		}
	}
	if got := entries[3].Balance; got != 98.8 {
		t.Errorf("消耗后余额 = %v，想要 98.8", got)
	}
	if got := entries[0].Detail; !strings.HasPrefix(got, "账号倍率 1 → 0.5") {
		t.Errorf("设置倍率的说明 = %q", got)
	}
	// 汇总：消耗取负、增加为正，互为独立口径
	if tot := totals[EntryConsume]; tot.Count != 1 || tot.Units != -1.2 {
		t.Errorf("consume 汇总 = %+v", tot)
	}
	if tot := totals[EntryAdd]; tot.Units != 50 {
		t.Errorf("add 汇总 = %+v", tot)
	}
	if tot := totals[EntryReduce]; tot.Units != -20 {
		t.Errorf("reduce 汇总 = %+v", tot)
	}
	if _, ok := totals[EntrySet]; !ok {
		t.Error("set 汇总缺失")
	}
}

// TestLedgerIsMaskedAndScoped：流水按账号隔离，且明文 Key 不出现在视图里。
func TestLedgerIsMaskedAndScoped(t *testing.T) {
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, k := range []string{"vl_aaaaaaaaaaaa", "vl_bbbbbbbbbbbb"} {
		if _, err := s.Create(Account{Key: k, Quota: 10, Multiplier: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Consume("vl_aaaaaaaaaaaa", 1, "links/bilibili ×1"); err != nil {
		t.Fatal(err)
	}

	mine, _ := s.Ledger(LedgerQuery{Key: "vl_aaaaaaaaaaaa", Limit: 10})
	for _, e := range mine {
		if e.Key != "vl_aaaaaaaaaaaa" {
			t.Errorf("自己的流水里混进了别的账号：%q", e.Key)
		}
		if got := e.MaskedKey(); got == e.Key || got == "" {
			t.Errorf("对外必须掩码，得到 %q", got)
		}
	}
	all, allTotals := s.Ledger(LedgerQuery{Limit: 10})
	if len(all) != 3 { // 两个 create + 一次 consume
		t.Errorf("总账单应有 3 条，得到 %d", len(all))
	}
	if allTotals[EntryConsume].Count != 1 {
		t.Errorf("总账单的消耗计数 = %d", allTotals[EntryConsume].Count)
	}
}

// TestLedgerSurvivesReplay：流水与账号快照共用一个文件，重启后都要回来。
func TestLedgerSurvivesReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.jsonl")
	s, err := New(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(Account{Key: "vl_x", Name: "甲", Quota: 100, Multiplier: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume("vl_x", 2.5, "detail/douyin ×1"); err != nil {
		t.Fatal(err)
	}
	add := 7.0
	if _, err := s.Update("vl_x", Patch{AddQuota: &add}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s2, err := New(Options{Path: path})
	if err != nil {
		t.Fatalf("回放失败: %v", err)
	}
	defer func() { _ = s2.Close() }()

	entries, totals := s2.Ledger(LedgerQuery{Key: "vl_x", Limit: 10})
	if len(entries) != 3 {
		t.Fatalf("回放后应有 3 条流水，得到 %d：%+v", len(entries), entries)
	}
	if entries[0].Type != EntryAdd || entries[1].Type != EntryConsume {
		t.Errorf("回放顺序不对：%v", entries)
	}
	if totals[EntryConsume].Units != -2.5 || totals[EntryAdd].Units != 7 {
		t.Errorf("回放后汇总不对：%+v", totals)
	}
	// 账号本身也要正常回放
	if a, ok := s2.Get("vl_x"); !ok || a.Quota != 104.5 {
		t.Errorf("账号回放 = %+v（ok=%v），想要余额 104.5", a, ok)
	}
}

// TestLedgerTypeFilterAndLimit：按类型筛选与条数上限。
func TestLedgerTypeFilterAndLimit(t *testing.T) {
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Create(Account{Key: "vl_y", Quota: 1000, Multiplier: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.Consume("vl_y", 1, "links/bilibili ×1"); err != nil {
			t.Fatal(err)
		}
	}
	only, totals := s.Ledger(LedgerQuery{Key: "vl_y", Type: EntryConsume, Limit: 2})
	if len(only) != 2 {
		t.Errorf("limit 没生效：%d 条", len(only))
	}
	for _, e := range only {
		if e.Type != EntryConsume {
			t.Errorf("类型筛选没生效：%q", e.Type)
		}
	}
	if len(totals) != 1 || totals[EntryConsume].Count != 5 {
		t.Errorf("筛选后的汇总应只含 consume 且计数为全量 5：%+v", totals)
	}
}

// TestLedgerDeleteKeepsHistory：删号之后流水仍在（那正是对账最需要的时候）。
func TestLedgerDeleteKeepsHistory(t *testing.T) {
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Create(Account{Key: "vl_z", Name: "乙", Quota: 10, Multiplier: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume("vl_z", 3, "links/bilibili ×1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("vl_z"); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.Ledger(LedgerQuery{Key: "vl_z", Limit: 10})
	if len(entries) != 3 || entries[0].Type != EntryDelete {
		t.Fatalf("删号后流水应保留且带 delete，得到 %+v", entries)
	}
	if entries[0].Detail == "" {
		t.Error("删号流水应有说明")
	}
}

// TestPublicAccount：公共账号的创建与"只记账不扣配额"。
func TestPublicAccount(t *testing.T) {
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// 创建：带 public 标记、余额为 0（配额由每 IP 日限额管，与余额无关）
	a, created, err := s.EnsurePublic("vl_public", "公共账号")
	if err != nil || !created {
		t.Fatalf("应新建公共账号：created=%v err=%v", created, err)
	}
	if !a.PublicAccount || a.Quota != 0 {
		t.Errorf("公共账号应 public 且余额 0：%+v", a)
	}
	// 幂等：再次调用不改动任何东西
	a2, created2, err := s.EnsurePublic("vl_public", "公共账号")
	if err != nil || created2 || a2.Key != a.Key {
		t.Errorf("重复调用应无副作用：created=%v err=%v", created2, err)
	}
	// 管理员停用后，启动流程不能再把它悄悄启用（那是关闭公共入口的开关）
	off := true
	if _, err := s.Update("vl_public", Patch{Disabled: &off}); err != nil {
		t.Fatal(err)
	}
	a3, _, err := s.EnsurePublic("vl_public", "公共账号")
	if err != nil {
		t.Fatal(err)
	}
	if !a3.Disabled {
		t.Error("EnsurePublic 不该重新启用被停用的公共账号")
	}

	// RecordUsage：只加用量与次数，不动余额；流水照记
	if err := s.RecordUsage("vl_public", 1.5, "public@1.2.3.4 links/bilibili ×1"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("vl_public")
	if got.Used != 1.5 || got.Calls != 1 || got.Quota != 0 {
		t.Errorf("用量/次数/余额 = %v/%v/%v，想要 1.5/1/0", got.Used, got.Calls, got.Quota)
	}
	entries, totals := s.Ledger(LedgerQuery{Key: "vl_public", Limit: 10})
	if len(entries) != 3 || entries[0].Type != EntryConsume || entries[0].Units != -1.5 {
		t.Fatalf("流水应有一条 consume：%+v", entries)
	}
	if !strings.Contains(entries[0].Detail, "public@1.2.3.4") {
		t.Errorf("公共账号的流水说明应带来源 IP：%q", entries[0].Detail)
	}
	if totals[EntryConsume].Count != 1 {
		t.Errorf("汇总计数 = %d", totals[EntryConsume].Count)
	}
}
