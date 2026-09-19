package account

import (
	"errors"
	"strings"
	"testing"
)

// TestResetKeyKeepsEverythingExceptKey：重置 Key 是"换锁不换房子"——
// 配额、用量、倍率、签到状态、备注、创建时间全都保留，只有钥匙与句柄变。
func TestResetKeyKeepsEverythingExceptKey(t *testing.T) {
	s := newStore(t)
	a, err := s.Create(Account{
		Key: "vl_old", Name: "客户甲", Quota: 30, Multiplier: 2,
		DailyGrant: 5, GrantCap: 60, Note: "备注",
	})
	if err != nil {
		t.Fatal(err)
	}
	oldHandle := a.ID
	if _, err := s.Consume("vl_old", 4, "测试消耗"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ResetKey("vl_old", "vl_new")
	if err != nil {
		t.Fatalf("ResetKey: %v", err)
	}
	if got.Key != "vl_new" || got.ID != Handle("vl_new") {
		t.Errorf("Key/句柄没换：%+v", got)
	}
	if got.ID == oldHandle {
		t.Error("句柄由 Key 派生，换 Key 后必须变")
	}
	if got.Quota != 26 || got.Multiplier != 2 || got.DailyGrant != 5 || got.GrantCap != 60 || got.Note != "备注" {
		t.Errorf("除 Key 之外的字段都该保留：%+v", got)
	}
	if !got.CreatedAt.Equal(a.CreatedAt) {
		t.Error("创建时间不该变")
	}
	if !got.UpdatedAt.After(a.UpdatedAt) && !got.UpdatedAt.Equal(a.UpdatedAt) {
		t.Error("更新时间应被刷新")
	}
	if got.Used != 4 {
		t.Errorf("累计用量应保留：%v", got.Used)
	}

	// 旧 Key / 旧句柄立刻失效，新 Key / 新句柄立刻可用
	if _, ok := s.Get("vl_old"); ok {
		t.Error("旧 Key 必须立刻失效（同一把锁不能有两把钥匙）")
	}
	if _, ok := s.Resolve(oldHandle); ok {
		t.Error("旧句柄必须失效")
	}
	if _, ok := s.Get("vl_new"); !ok {
		t.Error("新 Key 应可用")
	}
	if _, ok := s.Resolve(got.ID); !ok {
		t.Error("新句柄应可用")
	}
}

// TestResetKeyLedger：重置要在账本里留一笔，且写明旧句柄——
// 否则"重置前的历史流水"就再也找不到入口了。
func TestResetKeyLedger(t *testing.T) {
	s := newStore(t)
	a, _ := s.Create(Account{Key: "vl_a", Name: "甲", Quota: 10})
	oldHandle := a.ID
	if _, err := s.Consume("vl_a", 2, "解析"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetKey("vl_a", "vl_b"); err != nil {
		t.Fatal(err)
	}

	entries, _ := s.Ledger(LedgerQuery{Key: "vl_b"})
	var found bool
	for _, e := range entries {
		if e.Type == EntrySet && strings.Contains(e.Detail, oldHandle) && strings.Contains(e.Detail, "重置 Key") {
			found = true
		}
	}
	if !found {
		t.Errorf("重置应留一笔 set 流水并写明旧句柄，实际：%+v", entries)
	}

	// 旧 Key 名下的历史流水仍在（追加写的账本不会丢）
	oldEntries, _ := s.Ledger(LedgerQuery{Key: "vl_a"})
	if len(oldEntries) < 2 {
		t.Errorf("旧 Key 的历史流水应仍可查（创建 + 消耗）：%+v", oldEntries)
	}
}

// TestResetKeyRejectsBadInput：空新 Key、已被占用的 Key、不存在的账号。
func TestResetKeyRejectsBadInput(t *testing.T) {
	s := newStore(t)
	if _, err := s.Create(Account{Key: "vl_a", Name: "甲"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(Account{Key: "vl_b", Name: "乙"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetKey("vl_a", ""); err == nil {
		t.Error("空新 Key 应报错")
	}
	if _, err := s.ResetKey("vl_a", "vl_b"); !errors.Is(err, ErrDuplicate) {
		t.Errorf("新 Key 已被占用应返回 ErrDuplicate，得到 %v", err)
	}
	if _, err := s.ResetKey("vl_a", "vl_a"); !errors.Is(err, ErrDuplicate) {
		t.Errorf("新旧相同等于“那个 Key 被占着”，应返回 ErrDuplicate，得到 %v", err)
	}
	if _, err := s.ResetKey("nope", "vl_c"); !errors.Is(err, ErrNotFound) {
		t.Errorf("账号不存在应返回 ErrNotFound，得到 %v", err)
	}
	// 失败不能改动任何状态
	if _, ok := s.Get("vl_a"); !ok {
		t.Error("失败的调用不该动原账号")
	}
}

// TestResetKeySurvivesReplay：重置必须落盘，回放后仍然是新 Key。
func TestResetKeySurvivesReplay(t *testing.T) {
	path := t.TempDir() + "/ledger.jsonl"
	s, err := New(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(Account{Key: "vl_a", Name: "甲", Quota: 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume("vl_a", 2, "解析"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetKey("vl_a", "vl_b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := New(Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	a, ok := s2.Get("vl_b")
	if !ok {
		t.Fatal("回放后新 Key 应存在")
	}
	if _, gone := s2.Get("vl_a"); gone {
		t.Error("回放后旧 Key 不该复活")
	}
	if a.Quota != 10 || a.Used != 2 {
		t.Errorf("回放后配额/用量应保留：%+v", a)
	}
	if _, ok := s2.Resolve(a.ID); !ok {
		t.Error("回放后新句柄索引应建立")
	}
}
