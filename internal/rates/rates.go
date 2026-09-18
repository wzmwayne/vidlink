// Package rates 管"可编辑的计费倍率"：加载、校验、落盘、审计。
//
// 为什么要有这一层：倍率是**运营参数**，会随上游成本反复调整。以前它写在
// internal/quota 的 DefaultTable() 里，改一次要走代码评审 + 重新部署；
// 现在出厂默认仍在代码里（可复现、可重置），而"改过的差值"作为**覆盖层**
// 存在这里，由管理员通过管理接口改，重启不丢。
//
// 为什么是一个 JSON 文件而不是账本那样的追加式日志：
//
//   - 倍率是**配置**，不是流水：它只关心"现在是多少"，不关心历史每一条；
//   - 一次改动就是一次整体替换，原子写（临时文件 → fsync → rename）即可，
//     不需要每次扣费都落盘（账本才需要，那是钱袋子的账）；
//   - 文件小到可以整体重写，读的时候也能一次校验完，坏文件直接拒绝启动，
//     避免带着半个价目表跑起来。
//
// 措辞约定与其余部分一致：只讲"配额/系数/倍率"，不出现价格、金额、付费。
package rates

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"vidlink/internal/core"
	"vidlink/internal/quota"
)

// fileVersion 是配置文件格式版本。读到不认识的版本直接报错，
// 而不是"尽力解析"——价目表读错一个数字，扣的就是错的量。
const fileVersion = 1

// HistoryKeep 是保留的变更历史条数。
const HistoryKeep = 50

// 审计动作。
const (
	ActionUpdate = "update"
	ActionReset  = "reset"
)

// Actor 是审计里的操作者。
//
// 管理面只有一个固定 Key（VIDLINK_ADMIN_KEY），没有多管理员身份，
// 所以这里只能记到"管理面"这一层——文档里也这么写，避免误解成审计到人。
const Actor = "admin"

// Entry 是一条变更记录。
type Entry struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Actor  string    `json:"actor"`
	Detail string    `json:"detail,omitempty"`
}

// Options 是 Store 的配置。
type Options struct {
	// Path 是倍率文件路径；为空表示纯内存（测试用，改动不落盘）。
	Path string
	// Now 便于测试注入时钟。
	Now func() time.Time
	// Seed 是**文件不存在时**代理费率的种子（来自 VIDLINK_PROXY_RATE）。
	// <=0 表示用内置默认。
	//
	// 只在首次生效是刻意的：否则管理员在面板上改的值会被下次重启
	// 用环境变量悄悄覆盖回去，"改了不生效"最难查。
	Seed float64
	// Table 可注入（默认 quota.DefaultTable()）。
	Table *quota.Table
}

// Store 是倍率表 + 落盘 + 审计。
//
// 并发：Apply/Reset/View 用一把互斥锁串行化（管理操作是低频人工动作），
// 而**计量路径不经过这里**——server 直接读 quota.Table 的读锁，
// 所以改价不会给每次请求加一层锁。
type Store struct {
	mu    sync.Mutex
	path  string
	now   func() time.Time
	table *quota.Table
	seed  float64

	source    string // defaults | file
	updatedAt time.Time
	history   []Entry
}

// fileFormat 是倍率文件的结构。
//
// Rates 只存**覆盖层**（被改过的格子），出厂默认值不写进文件——
// 这样代码里的默认值改了之后，没被覆盖的格子会自动跟着走。
type fileFormat struct {
	Version   int                           `json:"version"`
	UpdatedAt time.Time                     `json:"updated_at"`
	ProxyRate *float64                      `json:"proxy_rate,omitempty"`
	Rates     map[string]map[string]float64 `json:"rates,omitempty"`
	History   []Entry                       `json:"history,omitempty"`
}

// New 构造 Store：文件存在则加载（校验失败即报错），否则用内置默认 + 种子。
func New(o Options) (*Store, error) {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	s := &Store{
		path:   o.Path,
		now:    now,
		table:  o.Table,
		seed:   o.Seed,
		source: "defaults",
	}
	if s.table == nil {
		s.table = quota.DefaultTable()
	}
	if s.path == "" {
		s.applySeed()
		return s, nil
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Table 返回底层倍率表（计量路径直接用它）。
func (s *Store) Table() *quota.Table { return s.table }

// Path 返回文件路径（可能为空）。
func (s *Store) Path() string { return s.path }

// applySeed 在没有文件时决定代理费率的初值。
func (s *Store) applySeed() {
	if s.seed > 0 {
		s.table.SetProxyRate(s.seed)
	}
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		s.applySeed()
		return nil
	}
	if err != nil {
		return fmt.Errorf("rates: 读取 %s 失败: %w", s.path, err)
	}

	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var f fileFormat
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("rates: 解析 %s 失败（文件损坏？）: %w", s.path, err)
	}
	if f.Version != fileVersion {
		return fmt.Errorf("rates: %s 的版本是 %d，本程序只认识 %d",
			s.path, f.Version, fileVersion)
	}

	overrides, err := decodeCells(f.Rates)
	if err != nil {
		return fmt.Errorf("rates: %s 内容非法: %w", s.path, err)
	}
	if f.ProxyRate != nil {
		if err := ValidateProxyRate(*f.ProxyRate); err != nil {
			return fmt.Errorf("rates: %s 内容非法: %w", s.path, err)
		}
		s.table.SetProxyRate(*f.ProxyRate)
	} else {
		s.applySeed()
	}
	s.table.ReplaceOverrides(overrides)

	s.source = "file"
	s.updatedAt = f.UpdatedAt
	s.history = capHistory(f.History)
	return nil
}

// View 是对外的只读快照。
type View struct {
	Source    string
	Path      string
	UpdatedAt time.Time
	ProxyRate float64
	// Overrides / Defaults 都是 **平台 → 端点 → 倍率**，键用对外名字
	// （通用档是 "default"），方便直接塞进 JSON 与前端表格。
	Overrides map[string]map[string]float64
	Defaults  map[string]map[string]float64
	History   []Entry
}

// View 返回当前快照。
func (s *Store) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewLocked()
}

func (s *Store) viewLocked() View {
	return View{
		Source:    s.source,
		Path:      s.path,
		UpdatedAt: s.updatedAt,
		ProxyRate: s.table.ProxyRate(),
		Overrides: encodeCells(s.table.Overrides()),
		Defaults:  encodeCells(s.table.Defaults()),
		History:   append([]Entry(nil), s.history...),
	}
}

// Apply 应用一次修改。
//
// cells 是 **平台 → 端点 → 倍率或 nil**：nil 表示**清除**该格的自定义
// （回到内置默认），缺省的格子不动。proxyRate 为 nil 时不动代理费率。
//
// 语义是**逐格合并**而不是整体替换：管理面板只提交被改动的格子，
// 整体替换会让"改一格"顺手把别的自定义抹掉，那是最难查的一类事故。
//
// 任何一格非法 → 整体拒绝（400），绝不部分生效；落盘失败 → 回滚内存。
func (s *Store) Apply(cells map[string]map[string]*float64, proxyRate *float64) (View, error) {
	targets, err := decodeCellPointers(cells)
	if err != nil {
		return View{}, err
	}
	if proxyRate != nil {
		if err := ValidateProxyRate(*proxyRate); err != nil {
			return View{}, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	prev := s.table.Overrides()
	prevProxy := s.table.ProxyRate()
	prevHistory, prevUpdated := s.history, s.updatedAt

	// 审计里的"旧值"要在改动**之前**取：一次 Apply 可能同时改通用档与某个
	// 平台，若边改边取，douyin 的旧值会变成刚写进去的通用档值，
	// 看起来像级联改动，对不上操作者实际做了什么。
	before := make([]float64, len(targets))
	for i, t := range targets {
		before[i], _ = s.table.Coefficient(t.Endpoint, t.Platform)
	}

	var parts []string
	for i, t := range targets {
		old := before[i]
		switch {
		case t.Value == nil:
			s.table.SetOverride(t.Endpoint, t.Platform, nil)
			parts = append(parts, fmt.Sprintf("%s/%s: %g → 默认",
				quota.PlatformName(t.Platform), t.Endpoint, old))
		default:
			s.table.SetOverride(t.Endpoint, t.Platform, t.Value)
			parts = append(parts, fmt.Sprintf("%s/%s: %g → %g",
				quota.PlatformName(t.Platform), t.Endpoint, old, *t.Value))
		}
	}
	if proxyRate != nil {
		parts = append(parts, fmt.Sprintf("proxy_rate: %g → %g", prevProxy, *proxyRate))
		s.table.SetProxyRate(*proxyRate)
	}

	s.updatedAt = s.now()
	s.pushHistoryLocked(Entry{
		At: s.updatedAt, Action: ActionUpdate, Actor: Actor,
		Detail: strings.Join(parts, "；"),
	})

	if err := s.saveLocked(); err != nil {
		s.table.ReplaceOverrides(prev)
		s.table.SetProxyRate(prevProxy)
		s.history, s.updatedAt = prevHistory, prevUpdated
		return View{}, err
	}
	s.source = "file"
	return s.viewLocked(), nil
}

// Reset 清空全部自定义（含代理费率），回到内置默认。
func (s *Store) Reset() (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	before := s.table.Overrides()
	beforeProxy := s.table.ProxyRate()
	beforeHistory, beforeUpdated := s.history, s.updatedAt

	s.table.ReplaceOverrides(nil)
	if s.seed > 0 {
		s.table.SetProxyRate(s.seed)
	} else {
		s.table.SetProxyRate(quota.ProxyRate)
	}
	s.updatedAt = s.now()
	s.pushHistoryLocked(Entry{
		At: s.updatedAt, Action: ActionReset, Actor: Actor,
		Detail: "恢复内置默认倍率",
	})
	if err := s.saveLocked(); err != nil {
		s.table.ReplaceOverrides(before)
		s.table.SetProxyRate(beforeProxy)
		s.history, s.updatedAt = beforeHistory, beforeUpdated
		return View{}, err
	}
	s.source = "file"
	return s.viewLocked(), nil
}

func (s *Store) pushHistoryLocked(e Entry) {
	s.history = append(s.history, e)
	s.history = capHistory(s.history)
}

func capHistory(in []Entry) []Entry {
	if len(in) <= HistoryKeep {
		return append([]Entry(nil), in...)
	}
	return append([]Entry(nil), in[len(in)-HistoryKeep:]...)
}

// saveLocked 原子落盘。调用方必须持有锁。
//
// 原子写：同目录临时文件 → fsync → rename → fsync 目录。
// 任何一步失败都不会让旧文件变成半截内容——价目表读坏了就是扣错量。
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("rates: 创建目录 %s 失败: %w", dir, err)
	}
	proxy := s.table.ProxyRate()
	f := fileFormat{
		Version:   fileVersion,
		UpdatedAt: s.updatedAt,
		ProxyRate: &proxy,
		Rates:     encodeCells(s.table.Overrides()),
		History:   s.history,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".rates-*.tmp")
	if err != nil {
		return fmt.Errorf("rates: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后这里是空操作

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rates: 写入 %s 失败: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rates: fsync %s 失败: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("rates: 关闭 %s 失败: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("rates: 替换 %s 失败: %w", s.path, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // 目录项也要落盘，否则 rename 可能丢
		_ = d.Close()
	}
	return nil
}

// --- 内部：名字 ↔ 内部键，以及校验 ---

type target struct {
	Endpoint quota.Endpoint
	Platform core.Platform
	Value    *float64
}

func decodeCellPointers(cells map[string]map[string]*float64) ([]target, error) {
	out := make([]target, 0, len(cells)*len(quota.AllEndpoints))
	// 排序保证错误信息与审计顺序稳定
	plats := make([]string, 0, len(cells))
	for p := range cells {
		plats = append(plats, p)
	}
	sort.Strings(plats)
	for _, pname := range plats {
		platform, ok := quota.NormalizePlatform(pname)
		if !ok {
			return nil, fmt.Errorf("未知平台 %q（可用：%s）", pname, platformList())
		}
		eps := cells[pname]
		epNames := make([]string, 0, len(eps))
		for e := range eps {
			epNames = append(epNames, e)
		}
		sort.Strings(epNames)
		for _, ename := range epNames {
			ep := quota.Endpoint(ename)
			v := eps[ename]
			if v != nil {
				if err := quota.ValidateCell(ep, platform, *v); err != nil {
					return nil, err
				}
			}
			out = append(out, target{Endpoint: ep, Platform: platform, Value: v})
		}
	}
	return out, nil
}

// ValidateProxyRate 校验代理费率。
//
// 允许 0（代理不扣配额）——带宽白送是运营选择；上限与其它系数一致。
func ValidateProxyRate(v float64) error {
	if v != v || v < 0 || v > quota.MaxRate { // v != v 拦 NaN
		return fmt.Errorf("代理费率 %v 超出允许范围 [0, %v]", v, quota.MaxRate)
	}
	return nil
}

func platformList() string {
	names := []string{quota.PlatformDefault}
	for _, p := range quota.AllPlatforms {
		names = append(names, string(p))
	}
	return strings.Join(names, " / ")
}

// decodeCells 把文件里的"平台 → 端点 → 值"转成内部键并校验。
func decodeCells(in map[string]map[string]float64) (map[quota.Endpoint]map[core.Platform]float64, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[quota.Endpoint]map[core.Platform]float64, len(quota.AllEndpoints))
	for pname, eps := range in {
		platform, ok := quota.NormalizePlatform(pname)
		if !ok {
			return nil, fmt.Errorf("未知平台 %q（可用：%s）", pname, platformList())
		}
		for ename, v := range eps {
			ep := quota.Endpoint(ename)
			if err := quota.ValidateCell(ep, platform, v); err != nil {
				return nil, err
			}
			if out[ep] == nil {
				out[ep] = make(map[core.Platform]float64, len(quota.AllPlatforms)+1)
			}
			out[ep][platform] = v
		}
	}
	return out, nil
}

// encodeCells 把内部键转成"平台 → 端点 → 值"，并对齐顺序便于人读。
func encodeCells(in map[quota.Endpoint]map[core.Platform]float64) map[string]map[string]float64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]map[string]float64, len(quota.AllPlatforms)+1)
	for ep, byPlatform := range in {
		for platform, v := range byPlatform {
			name := quota.PlatformName(platform)
			if out[name] == nil {
				out[name] = make(map[string]float64, len(quota.AllEndpoints))
			}
			out[name][string(ep)] = v
		}
	}
	return out
}
