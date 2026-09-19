package account

import (
	"sort"
	"time"
)

// 配额流水（对外叫"账单"）。
//
// 为什么要有它：账本本身只保存**当前状态**（余额、累计用量），
// 管理员加了 100 又减了 50，从余额上是看不出来的。流水记录的是
// "每一次变化是什么、由谁造成、变化后余额多少"，用于对账与用户自查。
//
// 存储策略：与账号快照共用同一个 JSONL 文件，每条流水用一层 `tx` 包起来
// （见 ledgerLine）。这样一次业务动作仍然只写一次、只 fsync 一次——
// 部署目标是 SD 卡上的树莓派，每次 fsync 都有实打实的代价；
// 而"快照 + 流水"本来就是同一次动作的两面，拆成两个文件只会引入
// "一个成功一个失败"的中间态。

// EntryType 是流水类型。
type EntryType string

const (
	// EntryConsume 使用：解析或代理消耗配额（units 为负）。
	EntryConsume EntryType = "consume"
	// EntryAdd 管理员增加配额。
	EntryAdd EntryType = "add"
	// EntryReduce 管理员减少配额。
	EntryReduce EntryType = "reduce"
	// EntrySet 管理员设置：配额设为某值、倍率、名称、备注、停用/启用。
	EntrySet EntryType = "set"
	// EntryCreate 建号。
	EntryCreate EntryType = "create"
	// EntryDelete 删号。
	EntryDelete EntryType = "delete"
)

// AllEntryTypes 是全部流水类型（便于遍历、校验与文档）。
var AllEntryTypes = []EntryType{
	EntryConsume, EntryAdd, EntryReduce, EntrySet, EntryCreate, EntryDelete,
}

// Valid 报告这个类型是否是已知类型。
func (t EntryType) Valid() bool {
	for _, x := range AllEntryTypes {
		if t == x {
			return true
		}
	}
	return false
}

// Entry 是一条配额流水。
//
// Key 是明文（与账号快照一样落在同一个文件里），但**对外一律掩码**：
// 接口层用 MaskedKey() 输出。ID 是公开句柄，管理面按它筛账号。
type Entry struct {
	Time    time.Time `json:"time"`
	Type    EntryType `json:"type"`
	Key     string    `json:"key,omitempty"`
	ID      string    `json:"id,omitempty"`
	Name    string    `json:"name,omitempty"`
	Units   float64   `json:"units"`   // 变化量：+ 增加 / - 减少（消耗为负）
	Balance float64   `json:"balance"` // 变化后的剩余配额
	Used    float64   `json:"used,omitempty"`
	Calls   int64     `json:"calls,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

// MaskedKey 返回可外发的 Key 形式。
func (e Entry) MaskedKey() string { return Mask(e.Key) }

// Totals 是某一类流水的汇总。
//
// Units 是**带符号**的净变化：消耗为负、管理员增加为正。
// 展示成"累计消耗"时取反即可（-totals[consume].units）。
type Totals struct {
	Count int64   `json:"count"`
	Units float64 `json:"units"`
}

// ledgerLine 是流水在账本文件里的封装。
//
// 顶层包一层 tx 而不是把字段摊平：回放时先看有没有 tx，
// 再看是不是账号快照——两类记录共用一个文件的前提是"永远能一眼分清"，
// 摊平后只靠字段有无来判断，迟早会踩到同名字段。
type ledgerLine struct {
	Tx Entry `json:"tx"`
}

// LedgerQuery 是读取流水的条件。
type LedgerQuery struct {
	// Key 为空表示全部账号（管理面的总账单）。
	Key string
	// Type 为空表示不限类型。
	Type EntryType
	// Limit 是返回条数上限；<=0 时用 DefaultLedgerLimit。
	Limit int
}

// DefaultLedgerLimit / MaxLedgerLimit 控制单次读取的条数。
const (
	DefaultLedgerLimit = 50
	MaxLedgerLimit     = 500
)

// ledgerKeep 是常驻内存的流水条数上限。
//
// 只保留最近这么多条（约 200 字节/条 → 1 MB 上下）：服务的内存预算只有
// 25 MB，而流水会随调用无限增长。更早的记录仍然在文件里，需要时可以直接
// 读文件——接口只承诺"最近 N 条 + 分类汇总"。
//
// 汇总（totals）**不受这个上限影响**：它在回放与写入时增量累计，
// 所以"累计消耗"永远是全量口径。
const ledgerKeep = 5000

// Ledger 返回流水（新的在前）与分类汇总。
func (s *Store) Ledger(q LedgerQuery) ([]Entry, map[EntryType]Totals) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ledgerLocked(q)
}

func (s *Store) ledgerLocked(q LedgerQuery) ([]Entry, map[EntryType]Totals) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLedgerLimit
	}
	if limit > MaxLedgerLimit {
		limit = MaxLedgerLimit
	}
	keys := s.accountKeys(q.Key)
	out := make([]Entry, 0, min(limit, 64))
	for i := len(s.entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := s.entries[i]
		if len(keys) > 0 && !containsKey(keys, e.Key) {
			continue
		}
		if q.Type != "" && e.Type != q.Type {
			continue
		}
		out = append(out, e)
	}
	return out, s.totalsLocked(q)
}

func (s *Store) totalsLocked(q LedgerQuery) map[EntryType]Totals {
	// 汇总同样要跨"这个账号用过的所有 Key"合并：
	// 账号换过钥匙之后，累计消耗仍然是这个账号的，不能凭空变少。
	var srcs []map[EntryType]Totals
	if keys := s.accountKeys(q.Key); len(keys) > 0 {
		for _, k := range keys {
			if t := s.totalsByKey[k]; t != nil {
				srcs = append(srcs, t)
			}
		}
	} else {
		srcs = []map[EntryType]Totals{s.totalsAll}
	}
	out := make(map[EntryType]Totals, 8)
	for _, src := range srcs {
		for k, v := range src {
			if q.Type != "" && k != q.Type {
				continue
			}
			cur := out[k]
			cur.Count += v.Count
			cur.Units += v.Units
			out[k] = cur
		}
	}
	return out
}

// accountKeys 返回"这次查询该匹配哪些 Key"：
//
//	q.Key == ""            → nil（不过滤，总账单）
//	账号存在                → 当前 Key + KeyHistory（账单跟着账号走）
//	账号不存在（已删除等）  → 只按传入的 Key 匹配，历史流水仍可按旧 Key 查到
//
// 调用方必须已持有读锁或写锁（它会读 s.accounts）。
func (s *Store) accountKeys(key string) []string {
	if key == "" {
		return nil
	}
	out := []string{key}
	if a, ok := s.accounts[key]; ok {
		for _, k := range a.KeyHistory {
			if k != "" && k != key {
				out = append(out, k)
			}
		}
	}
	return out
}

// containsKey 判断 Key 是否在集合里（历史长度有上限，线性查找足够）。
func containsKey(keys []string, key string) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// --- 内部：写入与回放 ---

// ledgerAdd 把一条流水放进内存并累计汇总。调用方必须已持有写锁。
func (s *Store) ledgerAdd(e Entry) {
	if e.ID == "" && e.Key != "" {
		e.ID = Handle(e.Key)
	}
	s.entries = append(s.entries, e)
	if n := len(s.entries) - ledgerKeep; n > 0 {
		// 逐条丢弃最旧的：汇总已经累计过，不受影响
		s.entries = append(s.entries[:0], s.entries[n:]...)
	}
	s.bumpTotals(s.totalsAll, e)
	if e.Key != "" {
		m := s.totalsByKey[e.Key]
		if m == nil {
			m = make(map[EntryType]Totals, 4)
			s.totalsByKey[e.Key] = m
		}
		s.bumpTotals(m, e)
	}
}

// ledgerDrop 撤销最后一次 ledgerAdd（落盘失败时的回滚）。调用方持写锁。
//
// 只可能撤销刚追加的那一条，所以直接砍尾巴、再把汇总减回去。
func (s *Store) ledgerDrop() {
	if len(s.entries) == 0 {
		return
	}
	e := s.entries[len(s.entries)-1]
	s.entries = s.entries[:len(s.entries)-1]
	unbumpTotals(s.totalsAll, e)
	if e.Key != "" {
		if m := s.totalsByKey[e.Key]; m != nil {
			unbumpTotals(m, e)
		}
	}
}

func (s *Store) bumpTotals(m map[EntryType]Totals, e Entry) {
	t := m[e.Type]
	t.Count++
	t.Units += e.Units
	m[e.Type] = t
}

func unbumpTotals(m map[EntryType]Totals, e Entry) {
	t, ok := m[e.Type]
	if !ok {
		return
	}
	t.Count--
	t.Units -= e.Units
	if t.Count <= 0 {
		delete(m, e.Type)
		return
	}
	m[e.Type] = t
}

// replayEntry 回放文件里的一条流水。调用方持有写锁（回放期间独占）。
func (s *Store) replayEntry(e Entry) { s.ledgerAdd(e) }

// LedgerTypes 返回已知类型的排序副本（用于接口自描述）。
func LedgerTypes() []EntryType {
	out := append([]EntryType(nil), AllEntryTypes...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
