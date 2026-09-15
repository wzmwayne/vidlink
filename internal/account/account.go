// Package account 管理 API Key 账号、配额与用量。
//
// 为什么不用数据库
//
// 部署目标是 1GB 树莓派，服务常驻内存预算约 25MB。SQLite（即使纯 Go 实现）
// 会同时推高二进制体积与常驻内存，与"零第三方依赖 + 极轻量"的定位冲突。
// 而本场景的数据量很小（几百到几千个账号）且写入是低频的，
// 因此选择**内存账本 + 追加写 JSONL**：
//
//   - 读路径全在内存，无 IO；
//   - 写路径是一次 append + fsync，顺序追加，不会破坏已有数据；
//   - 崩溃只可能丢掉最后一次 fsync 之后的记录，而每条记录都是自包含的
//     全量快照，回放即可恢复；
//   - 文件是纯文本，出问题时可以直接看、直接改。
//
// 为什么记账与扣减配额要在一起
//
// "扣配额"必须是一个原子动作：先检查够不够、再扣。若拆成两步由调用方组合，
// 高并发下会出现配额被扣成负数（检查通过后、扣减之前有别的请求进来）。
// 因此 CheckAndCharge 在**同一把锁内**完成检查与扣减。
package account

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Account 是一个 API Key 账号。
//
// 注意 JSON 里**不包含 Key 之外的身份信息**，且 Key 本身用掩码输出，
// 见 Masked()。管理接口返回的是掩码，避免 Key 出现在日志与浏览器历史里。
type Account struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`

	// Admin 为 true 时可访问 /v1/admin/* 并管理其他账号。
	Admin bool `json:"admin,omitempty"`

	// Quota 是剩余配额。不允许为负——扣减前会检查。
	Quota float64 `json:"quota"`

	// Multiplier 是账号倍率，用于给单个账号做调整（与金额无关，
	// 只是配额消耗的折算比例）：
	//   1.0 标准   0.5 减半   0.0 不扣配额（仍记用量与调用次数）   2.0 加倍
	Multiplier float64 `json:"multiplier"`

	// Used 是累计消耗的 units（已按账号倍率折算后的实扣值）。
	Used float64 `json:"used"`
	// Calls 是累计成功配额计量的调用次数。
	Calls int64 `json:"calls"`

	Disabled bool `json:"disabled,omitempty"`
	// Deleted 是墓碑标记。删除不抹掉历史记录，而是追加一条 Deleted=true
	// 的快照——这样账本是只追加的，回放时据此移除账号，
	// 历史用量的汇总也不会因为删除而失真。
	Deleted   bool      `json:"deleted,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Note      string    `json:"note,omitempty"`
}

// Masked 返回 Key 的掩码形式，用于对外展示。
//
// 保留前 4 后 4 位便于辨认是哪把 Key，中间以固定宽度的 * 填充；
// 短 Key 全掩。
//
// 刻意**不保留原始长度**——长度本身就是可用于推断 Key 生成规则的信息。
// 代价是掩码长度与原 Key 无关，但这比泄露长度更划算。
func (a Account) Masked() string {
	k := a.Key
	if len(k) <= 10 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + strings.Repeat("*", 8) + k[len(k)-4:]
}

// Public 返回可安全外发的视图（掩码 Key）。
func (a Account) Public() Account {
	c := a
	c.Key = a.Masked()
	return c
}

// ErrQuotaExhausted 配额不足。
type ErrQuotaExhausted struct {
	Need float64 // 本次需要
	Have float64 // 当前剩余
}

func (e ErrQuotaExhausted) Error() string {
	return fmt.Sprintf("配额不足：本次需要 %.2f，当前剩余 %.2f", e.Need, e.Have)
}

// ErrNotFound 账号不存在。
var ErrNotFound = errors.New("账号不存在")

// ErrDuplicate 账号已存在。
var ErrDuplicate = errors.New("账号已存在")

// Store 是账号账本。
//
// 并发安全：所有读写都经过一把锁。看起来是粗粒度，但账本操作本身极短
// （几十纳秒的 map 读写 + 浮点运算），而 QPS 上限由上游解析能力决定，
// 远低于锁会成为瓶颈的量级。用细粒度锁换来的是更容易出错的原子性。
type Store struct {
	mu       sync.RWMutex
	accounts map[string]*Account

	path string // JSONL 落盘路径；空表示纯内存（测试用）
	f    *os.File
	fmu  sync.Mutex // 串行化 append，避免交错写坏行

	now func() time.Time
}

// Options 是 Store 的配置。
type Options struct {
	// Path 是 JSONL 文件路径。为空则纯内存，重启即丢。
	Path string
	// Now 便于测试注入时钟。
	Now func() time.Time
}

// New 创建账本并回放已有记录。
func New(opts Options) (*Store, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Store{
		accounts: make(map[string]*Account, 64),
		path:     opts.Path,
		now:      now,
	}
	if opts.Path == "" {
		return s, nil
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	if err := s.replay(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(opts.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开账本失败: %w", err)
	}
	s.f = f
	return s, nil
}

// Close 关闭账本文件。
func (s *Store) Close() error {
	if s == nil || s.f == nil {
		return nil
	}
	return s.f.Close()
}

// replay 逐行回放 JSONL，重建内存账本。
//
// 每条记录都是一份**全量快照**（而不是增量），所以回放就是不断覆盖同一个
// Key 的当前值，最后一条即最新。这让格式足够健壮：任何一行损坏都只影响
// 那一个账号的历史，跳过即可，不会让整个账本读不出来。
func (s *Store) replay() error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取账本失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	// 单行可能较长（name/note 是用户输入），放宽上限
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var bad int
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var a Account
		if err := json.Unmarshal([]byte(line), &a); err != nil || a.Key == "" {
			bad++ // 跳过损坏行，不让它毁掉整个账本
			continue
		}
		if a.Deleted {
			delete(s.accounts, a.Key)
			continue
		}
		cp := a
		s.accounts[a.Key] = &cp
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("回放账本失败: %w", err)
	}
	return nil
}

// persist 追加一条快照。调用方必须已持有写锁（或确认独占）。
func (s *Store) persist(a *Account) error {
	if s.f == nil {
		return nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	s.fmu.Lock()
	defer s.fmu.Unlock()
	if _, err := s.f.Write(b); err != nil {
		return err
	}
	// 账本必须真正落盘：配额数据丢了就是真金白银的争议。
	return s.f.Sync()
}

// --- 读 ---

// Get 按 Key 取账号（返回内部指针的副本，调用方不能改到账本）。
func (s *Store) Get(key string) (Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[key]
	if !ok {
		return Account{}, false
	}
	return *a, true
}

// Count 返回账号数。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.accounts)
}

// List 返回所有账号的**掩码视图**，按创建时间排序。
func (s *Store) List() []Account {
	s.mu.RLock()
	out := make([]Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a.Public())
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// --- 写（管理面）---

// Create 新建账号。
//
// ⚠️ Multiplier 的零值是 0，而 0 表示**免费**（合法设置）。所以调用方
// 想要"正常消耗配额"就必须显式传 1.0；不要依赖零值。管理接口层负责补这个默认。
func (s *Store) Create(a Account) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if a.Key == "" {
		return Account{}, errors.New("Key 不能为空")
	}
	if _, dup := s.accounts[a.Key]; dup {
		return Account{}, ErrDuplicate
	}
	now := s.now()
	a.CreatedAt, a.UpdatedAt = now, now
	if a.Multiplier < 0 {
		a.Multiplier = 0
	}
	cp := a
	s.accounts[a.Key] = &cp
	if err := s.persist(&cp); err != nil {
		delete(s.accounts, a.Key) // 落盘失败就回滚，避免内存与磁盘不一致
		return Account{}, err
	}
	return cp, nil
}

// Patch 描述一次局部修改。nil 表示该字段不动。
type Patch struct {
	Name       *string
	Quota      *float64
	Multiplier *float64
	Disabled   *bool
	Note       *string
	Admin      *bool
	// AddQuota 是**增量**调整（正数增加、负数扣减）。
	// 与 Quota 的区别：Quota 是设成某值，AddQuota 是在现有值上加减。
	AddQuota *float64
}

// Update 局部修改账号。
func (s *Store) Update(key string, p Patch) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.accounts[key]
	if !ok {
		return Account{}, ErrNotFound
	}
	before := *a

	if p.Name != nil {
		a.Name = *p.Name
	}
	if p.Note != nil {
		a.Note = *p.Note
	}
	if p.Multiplier != nil {
		if *p.Multiplier < 0 {
			return Account{}, errors.New("倍率不能为负")
		}
		a.Multiplier = *p.Multiplier
	}
	if p.Quota != nil {
		a.Quota = *p.Quota
	}
	if p.AddQuota != nil {
		a.Quota += *p.AddQuota
	}
	if p.Disabled != nil {
		a.Disabled = *p.Disabled
	}
	if p.Admin != nil {
		a.Admin = *p.Admin
	}
	a.UpdatedAt = s.now()

	if err := s.persist(a); err != nil {
		*a = before // 落盘失败回滚内存
		return Account{}, err
	}
	return *a, nil
}

// Delete 删除账号。
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[key]; !ok {
		return ErrNotFound
	}
	delete(s.accounts, key)
	// 追加一条墓碑记录，回放时据此移除。保留原账号的 Used/Calls
	// 之外的信息没有意义，但把它标成 Deleted 就足以让回放跳过它。
	return s.persist(&Account{
		Key: key, Deleted: true, Disabled: true,
		CreatedAt: s.now(), UpdatedAt: s.now(),
	})
}

// --- 配额计量 ---

// Receipt 是一次扣减配额的凭据，用于响应头与日志。
type Consumption struct {
	Units      float64 `json:"units"`      // 本次实扣
	Quota      float64 `json:"quota"`      // 扣后配额
	Multiplier float64 `json:"multiplier"` // 生效的账号倍率
}

// Charge 在**同一把锁内**完成"检查配额 + 扣减 + 落盘"。
//
// 这是账本唯一允许减少配额的入口。把它做成原子操作，是因为
// "先读配额判断、再写扣减"这种两步写法在并发下会把配额扣成负数。
//
// units<=0（例如账号倍率为 0 的免费账号）时不校验配额，
// 但仍然累计 Used 与 Calls ——用量统计不该因为免费而缺失。
func (s *Store) Consume(key string, units float64) (Consumption, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.accounts[key]
	if !ok {
		return Consumption{}, ErrNotFound
	}
	if units > a.Quota {
		return Consumption{}, ErrQuotaExhausted{Need: units, Have: a.Quota}
	}
	before := *a
	a.Quota -= units
	a.Used += units
	a.Calls++
	a.UpdatedAt = s.now()

	if err := s.persist(a); err != nil {
		*a = before
		return Consumption{}, err
	}
	return Consumption{Units: units, Quota: a.Quota, Multiplier: a.Multiplier}, nil
}

// Snapshot 是账本的汇总，用于 /v1/admin/stats。
type Snapshot struct {
	Accounts   int     `json:"accounts"`
	Active     int     `json:"active"`
	UsedTotal  float64 `json:"used_total"`
	CallsTotal int64   `json:"calls_total"`
	QuotaTotal float64 `json:"quota_total"`
}

// Stats 汇总所有账号。
func (s *Store) Stats() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out Snapshot
	for _, a := range s.accounts {
		out.Accounts++
		if !a.Disabled {
			out.Active++
		}
		out.UsedTotal += a.Used
		out.CallsTotal += a.Calls
		out.QuotaTotal += a.Quota
	}
	return out
}

// Check 只做配额校验，不扣减。
//
// 它服务于**预授权**：解析是一件有上游成本的事，配额为空的客户不应该
// 让我们白跑一趟。所以先按"最贵可能的档位"检查一次，解析成功后再按
// 实际平台结算。两者之间配额可能被其他并发请求消耗掉，
// 所以 Charge 仍会独立校验——Check 只是第一道门。
func (s *Store) Check(key string, need float64) error {
	s.mu.RLock()
	a, ok := s.accounts[key]
	s.mu.RUnlock()
	if !ok {
		return ErrNotFound
	}
	if a.Disabled {
		return ErrDisabled
	}
	if a.Quota < need {
		return ErrQuotaExhausted{Need: need, Have: a.Quota}
	}
	return nil
}

// ErrDisabled 账号已被停用。
var ErrDisabled = errors.New("账号已被停用")
