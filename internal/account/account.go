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

	// PublicAccount 标记这是**公共账号**：Key 是公开的（如 vl_public），谁都能用。
	//
	// 因此它的配额不来自账本余额，而是"每 IP 每日限额"（见 internal/publicq）：
	// 共享余额会被一个人或所有人瞬间用光，对谁都不公平；而"每个 IP 每天
	// 100 配额"让单点滥用只影响自己。账本只累计它的用量与调用次数用于统计。
	//
	// 它**不在 Patch 里**：公共账号由服务启动时按配置创建，
	// 不允许把已经发出去的普通 Key 改成公共入口——那等于把客户凭据公开。
	PublicAccount bool `json:"public,omitempty"`

	// 注意：账号**没有**任何权限位。
	//
	// 管理面（/v1/admin/*）的凭据是一个独立于账本的固定 Key
	// （配置项 VIDLINK_ADMIN_KEY，见 internal/config），不是某个账号的属性。
	// 因此这里不存在 admin 字段：一个 Key 要么是账本里的普通账号，
	// 要么是服务级的管理 Key，二者互不蕴含、互不派生。

	// ID 是账号的**公开句柄**，由 Key 派生（见 Handle），不是独立秘密。
	//
	// 为什么需要它：管理面要在**不接触明文 Key** 的前提下定位账号。
	// 列表接口只回掩码（明文 Key 只在创建那一次返回），所以"改配额/改倍率/
	// 停用/删除"不能拿列表里的 Key 当路径参数；而把明文 Key 放进管理列表，
	// 又会让所有账号的凭据出现在浏览器存储、历史与运维截屏里。
	//
	// 性质：不可逆（Key 的 SHA-256 截断，反推需 2^256 次尝试）、
	// 稳定（纯派生，重启与账本重放都不变）、不敏感（拿到它只能定位账号，
	// 不能冒充账号调用任何接口）。
	ID string `json:"id,omitempty"`

	// Quota 是剩余配额。不允许为负——扣减前会检查。
	Quota float64 `json:"quota"`

	// Multiplier 是账号倍率，用于给单个账号做调整（与金额无关，
	// 只是配额消耗的折算比例）：
	//   1.0 标准   0.5 减半   0.0 不扣配额（仍记用量与调用次数）   2.0 加倍
	Multiplier float64 `json:"multiplier"`

	// DailyGrant 是**每日签到可领的配额**（默认 0 = 不开放签到）。
	//
	// 由用户自己调 POST /v1/checkin 领取，每天一次——不是后台自动发，
	// 也不是"用到就补"：签到这个动作本身有价值（用户知道自己有额度、
	// 也给了服务端一个自然的触达点）。
	DailyGrant float64 `json:"daily_grant,omitempty"`

	// GrantCap 是**配额停止增加界限**：补额后余额不超过它。
	//
	//	余额 = min(余额 + DailyGrant, GrantCap)
	//
	// 余额已经 ≥ 界限时当天不再补（记为"已结算"，避免同一天反复加）。
	// 为 0 表示不限（只加不封顶）——那是有意的写法，不是"关闭"，
	// 关闭请用 DailyGrant=0。
	GrantCap float64 `json:"grant_cap,omitempty"`

	// GrantDay 是最近一次**签到**的日期（本地时区 YYYY-MM-DD）。
	// 每天只能签一次就是靠它判断的；它随账号快照一起落盘，重启不丢。
	GrantDay string `json:"grant_day,omitempty"`

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
func (a Account) Masked() string { return Mask(a.Key) }

// Mask 把 Key 掩码成可外发的形式（账号视图与配额流水共用）。
func Mask(key string) string {
	if len(key) <= 10 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", 8) + key[len(key)-4:]
}

// Public 返回可安全外发的视图（掩码 Key + 公开句柄）。
func (a Account) Public() Account {
	c := a
	c.ID = Handle(a.Key)
	// 公共账号的 Key 是**公开信息**（页面与错误提示里都会写出来），
	// 掩码成 ********* 既不必要，也让管理员认不出这是哪个入口。
	if a.PublicAccount {
		return c
	}
	c.Key = a.Masked()
	return c
}

// Handle 由 Key 派生出公开句柄。
//
// 取 SHA-256 的前 8 字节（16 位十六进制）：几百到几千个账号下碰撞概率
// 可以忽略，而长度足够短，能放进 URL 路径、日志和运维对话里。
// 前缀 acc_ 让它与 vl_ 开头的 Key 一眼可分——句柄可以被冒用者当成 Key
// 去调用接口（会 403），但绝不会有人把它误当凭据保存。
//
// 派生逻辑只有 HandleID 一份实现（见 credential.go）：签名凭据要靠
// 「句柄 → Key」反过来找人，两边算法若各写一份，迟早会对不上。
func Handle(key string) string {
	return HandleID(HandlePrefix, key)
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

	// byHandle 是「句柄 → Key」的索引，只为**签名凭据**这条认证路径存在。
	//
	// 为什么不像 Resolve 那样线性扫描：签名认证在每个带凭据的请求上都要
	// 走一次，是热路径；而 Key 一旦创建不可改（Patch 里没有 Key 字段），
	// 所以索引只需要在回放/创建/删除三处同步，不会成为"要跟着改"的负担。
	//
	// 它是派生数据的缓存：内容永远等于 {Handle(a.Key): a.Key}，
	// 回放时全量重建，因此重启后不可能残留脏数据。
	byHandle map[string]string

	// 配额流水：常驻内存只保留最近 ledgerKeep 条，汇总则是全量增量累计。
	// 两者都在回放时重建，因此重启不丢。
	entries     []Entry
	totalsAll   map[EntryType]Totals
	totalsByKey map[string]map[EntryType]Totals

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
		accounts:    make(map[string]*Account, 64),
		byHandle:    make(map[string]string, 64),
		totalsAll:   make(map[EntryType]Totals, 8),
		totalsByKey: make(map[string]map[EntryType]Totals, 64),
		path:        opts.Path,
		now:         now,
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
		// 先看是不是流水行：两类记录共用一个文件，判别必须无歧义
		var wrapped struct {
			Tx *Entry `json:"tx"`
		}
		if err := json.Unmarshal([]byte(line), &wrapped); err == nil && wrapped.Tx != nil {
			s.replayEntry(*wrapped.Tx)
			continue
		}

		var a Account
		if err := json.Unmarshal([]byte(line), &a); err != nil || a.Key == "" {
			bad++ // 跳过损坏行，不让它毁掉整个账本
			continue
		}
		if a.Deleted {
			delete(s.accounts, a.Key)
			delete(s.byHandle, Handle(a.Key))
			continue
		}
		// 句柄是派生的：旧账本里没有这个字段，历史记录也无需迁移，
		// 回放时补上即可（因此重启不会让句柄变化）。
		a.ID = Handle(a.Key)
		cp := a
		s.accounts[a.Key] = &cp
		s.byHandle[a.ID] = a.Key
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("回放账本失败: %w", err)
	}
	return nil
}

// persist 追加一条快照。调用方必须已持有写锁（或确认独占）。
func (s *Store) persist(a *Account) error { return s.appendAccount(a, nil) }

// appendAccount 把账号快照与（可选的）流水**一次写入、一次 fsync**。
//
// 为什么要合并：树莓派上是 SD 卡，每次 fsync 都有实打实的代价；
// 而快照与流水本来就是同一次业务动作的两面，分两次写只会引入
// "一个成功一个失败"的中间态——那时账本与流水就对不上了。
func (s *Store) appendAccount(a *Account, e *Entry) error {
	if a == nil {
		return s.appendRecords(nil, e)
	}
	return s.appendRecords([]*Account{a}, e)
}

// appendRecords 是 appendAccount 的多记录版本。
//
// 为什么需要"一次写多条"：重置 Key 落盘时要同时写**旧 Key 的墓碑**与
// **新 Key 的快照**。分两次写就会留下"只写了墓碑"或"只写了快照"的中间态，
// 回放出来要么账号凭空消失，要么两把钥匙都能开——账本必须一次落定。
func (s *Store) appendRecords(as []*Account, e *Entry) error {
	if s.f == nil {
		return nil
	}
	var buf []byte
	for _, a := range as {
		if a == nil {
			continue
		}
		b, err := json.Marshal(a)
		if err != nil {
			return err
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	if e != nil {
		b, err := json.Marshal(ledgerLine{Tx: *e})
		if err != nil {
			return err
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	if len(buf) == 0 {
		return nil
	}

	s.fmu.Lock()
	defer s.fmu.Unlock()
	if _, err := s.f.Write(buf); err != nil {
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

// EnsurePublic 确保公共账号存在，并返回它。
//
// 已存在时**什么都不改**（含 Disabled）：管理员把公共账号停用就是
// "关闭公共入口"的开关，启动时重新启用会让这个开关形同虚设。
func (s *Store) EnsurePublic(key, name string) (Account, bool, error) {
	if strings.TrimSpace(key) == "" {
		return Account{}, false, errors.New("公共 Key 不能为空")
	}
	if a, ok := s.Get(key); ok {
		return a, false, nil
	}
	a, err := s.Create(Account{
		Key: key, Name: name, PublicAccount: true, Quota: 0, Multiplier: 1,
		Note: "公共账号：Key 公开，配额按「每 IP 每日限额」，不使用账本余额",
	})
	if err != nil {
		if errors.Is(err, ErrDuplicate) { // 并发下别人刚建好
			if got, ok := s.Get(key); ok {
				return got, false, nil
			}
		}
		return Account{}, false, err
	}
	return a, true, nil
}

// RecordUsage 只记账、不扣配额。
//
// 公共账号需要它：配额由每 IP 每日限额控制（publicq），但用量与调用次数
// 仍要累计，否则管理面完全看不到公共入口被用了多少。
func (s *Store) RecordUsage(key string, units float64, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.accounts[key]
	if !ok {
		return ErrNotFound
	}
	before := *a
	a.Used += units
	a.Calls++
	a.UpdatedAt = s.now()

	e := Entry{
		Time: a.UpdatedAt, Type: EntryConsume, Key: a.Key, ID: a.ID, Name: a.Name,
		Units: -units, Balance: a.Quota, Used: a.Used, Calls: a.Calls, Detail: detail,
	}
	s.ledgerAdd(e)
	if err := s.appendAccount(a, &e); err != nil {
		*a = before
		s.ledgerDrop()
		return err
	}
	return nil
}

// DayKey 把时间点规约成"本地日期"（每日补额的分界）。
func DayKey(t time.Time) string { return t.Format("2006-01-02") }

// CheckInResult 是一次签到的结果。
type CheckInResult struct {
	// Granted 是本次真正加上的配额（已达上限时为 0）。
	Granted float64 `json:"granted"`
	// Balance 是签到后的余额。
	Balance float64 `json:"balance"`
	// Already 表示今天已经签过（本次什么都没做）。
	Already bool `json:"already_checked_in"`
	// AtCap 表示余额已经在"停止增加界限"上（本次不消耗签到机会）。
	AtCap bool `json:"at_cap"`
	// NextAt 是下次可签到的时间（今天已签时才有意义）。
	NextAt time.Time `json:"next_checkin_at"`
}

// CheckIn 执行每日签到：余额 = min(余额 + DailyGrant, GrantCap)。
//
//	GrantCap 为 0 表示不封顶；DailyGrant 为 0 表示这个账号不开放签到。
//
// 三条规则都是有意的：
//
//   - **每天一次**：靠 GrantDay（本地日期）判断，随快照落盘，重启不丢；
//   - **到界限就停**：余额已经 ≥ GrantCap 时不再增加（"配额停止增加界限"）；
//   - **到界限不消耗当天机会**：签到的意义就是"需要时补一点"，余额满了
//     把它作废对用户没有好处，所以这次不算签过——花掉一些之后当天仍可签。
func (s *Store) CheckIn(key string) (Account, CheckInResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.accounts[key]
	if !ok {
		return Account{}, CheckInResult{}, ErrNotFound
	}
	now := s.now()
	today := DayKey(now)
	next := nextDay(now)

	res := CheckInResult{Balance: a.Quota, NextAt: next}
	if a.DailyGrant <= 0 {
		return *a, res, ErrCheckInDisabled
	}
	if a.Disabled {
		return *a, res, ErrDisabled
	}
	if a.GrantDay == today {
		res.Already = true
		return *a, res, nil
	}
	if a.GrantCap > 0 && a.Quota >= a.GrantCap {
		// 已在界限上：不增加、也不占用今天的签到机会
		res.AtCap = true
		return *a, res, nil
	}

	before := *a
	units := a.DailyGrant
	if a.GrantCap > 0 && a.Quota+units > a.GrantCap {
		units = a.GrantCap - a.Quota
	}
	a.Quota += units
	a.GrantDay = today
	a.UpdatedAt = now

	e := Entry{
		Time: now, Type: EntryAdd, Key: a.Key, ID: a.ID, Name: a.Name,
		Units: units, Balance: a.Quota, Used: a.Used, Calls: a.Calls,
		Detail: fmt.Sprintf("每日签到 +%.4g → %.4g", units, a.Quota) +
			func() string {
				if a.GrantCap > 0 {
					return fmt.Sprintf("（上限 %.4g）", a.GrantCap)
				}
				return ""
			}(),
	}
	s.ledgerAdd(e)
	if err := s.appendAccount(a, &e); err != nil {
		*a = before
		s.ledgerDrop()
		return before, CheckInResult{Balance: before.Quota, NextAt: next}, err
	}
	res.Granted = units
	res.Balance = a.Quota
	return *a, res, nil
}

// ErrCheckInDisabled 表示这个账号没有开放每日签到（DailyGrant 为 0）。
var ErrCheckInDisabled = errors.New("该账号未开放每日签到")

// nextDay 返回次日零点（本地时区）。
func nextDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
}

// ClearCheckInDay 清掉"今天已签到"的标记（仅供测试模拟跨天）。
func (s *Store) ClearCheckInDay(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.accounts[key]; ok {
		a.GrantDay = ""
	}
}

// Resolve 按"账号 Key 或公开句柄"取账号，供管理面使用。
//
// 两种寻址方式并存的原因：管理面板手上只有句柄（它拿不到明文 Key），
// 而运维用 curl 时手上往往就是明文 Key，没必要先换算一遍。
//
// 顺序是"精确 Key → 句柄索引 → 兜底扫描"：Key 优先，因为一个账号的 Key
// 有可能恰好长得像另一个账号的句柄（极端但合法）；最后的扫描只是兜底，
// 正常路径不会走到——签名认证走的是更严格的 ByHandle（只认句柄、只走索引）。
func (s *Store) Resolve(ref string) (Account, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a, ok := s.accounts[ref]; ok {
		return *a, true
	}
	if ref == "" {
		return Account{}, false
	}
	if key, ok := s.byHandle[ref]; ok {
		if a, ok := s.accounts[key]; ok {
			return *a, true
		}
	}
	for _, a := range s.accounts {
		if a.ID == ref {
			return *a, true
		}
	}
	return Account{}, false
}

// ByHandle 按句柄取账号（签名凭据的认证路径）。
//
// 与 Resolve 的区别：这里**只认句柄**、且只走索引——调用方拿到的一定是
// 20 字符的句柄形态，没有"也可能是个 Key"的歧义；而 Resolve 是给运维用的
// 宽松入口（明文 Key 或句柄都行），保留了兜底扫描。
//
// 索引由 replay/Create/Delete 维护，内容恒等于 {Handle(Key): Key}，
// 所以这里不需要再扫一遍：扫一遍等于承认索引可能不全，那才是真正的问题。
func (s *Store) ByHandle(handle string) (Account, bool) {
	if handle == "" {
		return Account{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.byHandle[handle]
	if !ok {
		return Account{}, false
	}
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
	// 句柄由 Key 派生，调用方传什么都不作数（保持"ID == Handle(Key)"这条不变量）
	a.ID = Handle(a.Key)
	now := s.now()
	a.CreatedAt, a.UpdatedAt = now, now
	if a.Multiplier < 0 {
		a.Multiplier = 0
	}
	cp := a
	s.accounts[a.Key] = &cp
	s.byHandle[a.ID] = a.Key
	e := Entry{
		Time: now, Type: EntryCreate, Key: a.Key, ID: a.ID, Name: a.Name,
		Units: a.Quota, Balance: a.Quota, Used: a.Used, Calls: a.Calls,
		Detail: fmt.Sprintf("创建账号（初始配额 %.4g）", a.Quota),
	}
	s.ledgerAdd(e)
	if err := s.appendAccount(&cp, &e); err != nil {
		delete(s.accounts, a.Key) // 落盘失败就回滚，避免内存与磁盘不一致
		delete(s.byHandle, a.ID)
		s.ledgerDrop()
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
	// DailyGrant / GrantCap 是每日签到可领的配额与它的封顶（见 Account 上的说明）。
	DailyGrant *float64
	GrantCap   *float64
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
	// 配额不允许为负：Consume 的检查只保证"扣不穿"，但管理面的
	// "设为 X" 与 "在现值上加减" 同样能把它写成负数（实测减配额减出过 -99），
	// 而负余额意味着这个账号此后连一次调用都发不出去，只能靠管理员再改回来。
	if p.Quota != nil {
		if *p.Quota < 0 {
			return Account{}, errors.New("配额不能为负")
		}
		a.Quota = *p.Quota
	}
	if p.AddQuota != nil {
		if a.Quota+*p.AddQuota < 0 {
			return Account{}, fmt.Errorf("当前配额 %.2f，减去 %.2f 会变成负数；"+
				"要把余额清零就用 quota:0", a.Quota, -*p.AddQuota)
		}
		a.Quota += *p.AddQuota
	}
	if p.Disabled != nil {
		a.Disabled = *p.Disabled
	}
	if p.DailyGrant != nil {
		if *p.DailyGrant < 0 {
			return Account{}, errors.New("签到额度不能为负")
		}
		a.DailyGrant = *p.DailyGrant
	}
	if p.GrantCap != nil {
		if *p.GrantCap < 0 {
			return Account{}, errors.New("补额上限不能为负")
		}
		a.GrantCap = *p.GrantCap
	}
	a.UpdatedAt = s.now()

	// 流水：把这次改了什么写成一句人能读的话。类型上只分四类业务含义，
	// 具体改了什么放进 detail——枚举越细，客户端的分支就越多。
	etype, units, parts := EntrySet, 0.0, make([]string, 0, 4)
	if p.Name != nil && *p.Name != before.Name {
		parts = append(parts, "名称 → "+*p.Name)
	}
	if p.Note != nil && *p.Note != before.Note {
		parts = append(parts, "备注已更新")
	}
	if p.Multiplier != nil && *p.Multiplier != before.Multiplier {
		parts = append(parts, fmt.Sprintf("账号倍率 %g → %g", before.Multiplier, *p.Multiplier))
	}
	if p.Disabled != nil && *p.Disabled != before.Disabled {
		if *p.Disabled {
			parts = append(parts, "停用账号")
		} else {
			parts = append(parts, "启用账号")
		}
	}
	if p.Quota != nil && *p.Quota != before.Quota {
		units = *p.Quota - before.Quota
		parts = append(parts, fmt.Sprintf("配额设为 %.4g（%+.4g）", *p.Quota, units))
	}
	if p.DailyGrant != nil && *p.DailyGrant != before.DailyGrant {
		parts = append(parts, fmt.Sprintf("每日签到额度 %.4g → %.4g", before.DailyGrant, *p.DailyGrant))
	}
	if p.GrantCap != nil && *p.GrantCap != before.GrantCap {
		parts = append(parts, fmt.Sprintf("补额上限 %.4g → %.4g", before.GrantCap, *p.GrantCap))
	}
	if p.AddQuota != nil && *p.AddQuota != 0 {
		units += *p.AddQuota
		if *p.AddQuota > 0 {
			etype = EntryAdd
			parts = append(parts, fmt.Sprintf("管理员增加 %.4g 配额", *p.AddQuota))
		} else {
			etype = EntryReduce
			parts = append(parts, fmt.Sprintf("管理员减少 %.4g 配额", -*p.AddQuota))
		}
	}

	var e *Entry
	if len(parts) > 0 {
		entry := Entry{
			Time: a.UpdatedAt, Type: etype, Key: a.Key, ID: a.ID, Name: a.Name,
			Units: units, Balance: a.Quota, Used: a.Used, Calls: a.Calls,
			Detail: strings.Join(parts, "；"),
		}
		e = &entry
		s.ledgerAdd(entry)
	}
	if err := s.appendAccount(a, e); err != nil {
		*a = before // 落盘失败回滚内存
		if e != nil {
			s.ledgerDrop()
		}
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
	a := s.accounts[key]
	delete(s.accounts, key)
	delete(s.byHandle, a.ID)
	// 追加一条墓碑记录，回放时据此移除。保留原账号的 Used/Calls
	// 之外的信息没有意义，但把它标成 Deleted 就足以让回放跳过它。
	now := s.now()
	e := Entry{
		Time: now, Type: EntryDelete, Key: key, ID: a.ID, Name: a.Name,
		Units: 0, Balance: 0, Used: a.Used, Calls: a.Calls,
		Detail: fmt.Sprintf("删除账号（删除时余额 %.4g，累计消耗 %.4g）", a.Quota, a.Used),
	}
	s.ledgerAdd(e)
	if err := s.appendAccount(&Account{
		Key: key, Deleted: true, Disabled: true,
		CreatedAt: now, UpdatedAt: now,
	}, &e); err != nil {
		s.accounts[key] = a // 落盘失败回滚：账号不能"删了但文件里没有"
		s.byHandle[a.ID] = key
		s.ledgerDrop()
		return err
	}
	return nil
}

// ResetKey 把账号的 Key 换成新值（newKey 不能为空，由调用方决定是手填还是随机）。
//
// 语义是**换锁不换房子**：配额、用量、倍率、签到状态、备注、创建时间全部保留，
// 只有"哪把钥匙能开"变了；旧 Key 在同一个锁内立刻失效——同一把锁不能有两把钥匙。
//
// 句柄（ID）由 Key 派生（见 Handle），所以重置后句柄也会变，并随返回值一起给出。
// 这带来一个必须讲清楚的后果：**账本里的历史流水挂在旧 Key/旧句柄下**，
// 新句柄只从这次重置之后的记录开始累积。重置那一笔写明了旧句柄，因此
// 管理员仍可按旧句柄查回历史（账本本来就是追加写的，不会丢）。
func (s *Store) ResetKey(oldKey, newKey string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.accounts[oldKey]
	if !ok {
		return Account{}, ErrNotFound
	}
	if newKey == "" {
		return Account{}, errors.New("新 Key 不能为空")
	}
	if _, taken := s.accounts[newKey]; taken {
		// 包含"新旧相同"这种情况：那个 Key 已经被占着。
		return Account{}, ErrDuplicate
	}

	before := *a
	oldHandle := a.ID
	cp := *a
	cp.Key = newKey
	cp.ID = Handle(newKey)
	cp.UpdatedAt = s.now()

	delete(s.accounts, oldKey)
	delete(s.byHandle, oldHandle)
	s.accounts[newKey] = &cp
	s.byHandle[cp.ID] = newKey

	e := Entry{
		Time: cp.UpdatedAt, Type: EntrySet, Key: newKey, ID: cp.ID, Name: cp.Name,
		Units: 0, Balance: cp.Quota, Used: cp.Used, Calls: cp.Calls,
		Detail: fmt.Sprintf("重置 Key（旧句柄 %s → 新句柄 %s；配额与用量保留）", oldHandle, cp.ID),
	}
	s.ledgerAdd(e)
	// 一次性写两条：旧 Key 的墓碑（回放时删掉它）+ 新 Key 的快照。
	tombstone := &Account{Key: oldKey, Deleted: true, Disabled: true,
		CreatedAt: before.CreatedAt, UpdatedAt: cp.UpdatedAt}
	if err := s.appendRecords([]*Account{tombstone, &cp}, &e); err != nil {
		// 落盘失败回滚：宁可"没重置"，也不能出现"内存里换了、文件里没换"
		delete(s.accounts, newKey)
		delete(s.byHandle, cp.ID)
		s.accounts[oldKey] = &before
		s.byHandle[oldHandle] = oldKey
		s.ledgerDrop()
		return Account{}, err
	}
	return cp, nil
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
func (s *Store) Consume(key string, units float64, detail string) (Consumption, error) {
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

	e := Entry{
		Time: a.UpdatedAt, Type: EntryConsume, Key: a.Key, ID: a.ID, Name: a.Name,
		Units: -units, Balance: a.Quota, Used: a.Used, Calls: a.Calls, Detail: detail,
	}
	s.ledgerAdd(e)
	if err := s.appendAccount(a, &e); err != nil {
		*a = before
		s.ledgerDrop()
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
