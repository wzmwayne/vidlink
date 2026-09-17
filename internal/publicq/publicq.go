// Package publicq 实现公共 Key 的"每 IP 每日配额"。
//
// 为什么不用账本余额：公共 Key 是**共享**的（谁都能拿到），一个共享余额
// 会在几分钟内被一个人用光，或者被所有人互相用光——两种都不公平，而且
// 前端展示"剩余额度"时也没有个人意义。
//
// "每个 IP 每天 N 配额"才是公共入口该有的口径：
//
//   - 单点滥用只影响自己（刷爆了也只是那一个 IP 当天没得用）；
//   - 正常用户每天都有稳定的免费额度，不需要注册；
//   - 对服务端来说，成本上限是可计算的：IP 数 × N。
//
// 计数只在内存里，**重启即清零**。这是刻意的取舍：持久化意味着每次计费
// 都要写一次 SD 卡（树莓派上不可接受），而"重启能重置额度"在这里不构成
// 实际风险——重启权在运维手里，且额度的量级决定了它不值得被攻击。
package publicq

import (
	"fmt"
	"sync"
	"time"
)

// DefaultDaily 是每 IP 每日的默认配额（可用 VIDLINK_PUBLIC_DAILY_QUOTA 覆盖）。
//
// 25 是配合"代理 0.2 配额/MiB"定的：约等于每天 25 条直链，
// 或约 125 MiB 代理流量——够试完一条短视频，又不至于变成免费带宽。
const DefaultDaily = 25

// maxIPs 是常驻内存的 IP 数量上限。
//
// 每个条目只有几十字节，4096 个约 200 KB——符合服务 25 MB 的内存预算。
// 超限时淘汰"最久没出现"的条目：那多半是昨天的临时访客，
// 而活跃用户每次请求都会刷新时间戳，不会被误伤。
const maxIPs = 4096

// ErrDailyExhausted 表示该 IP 今天的额度用完了。
type ErrDailyExhausted struct {
	Need     float64
	Have     float64
	ResetsAt time.Time
}

func (e ErrDailyExhausted) Error() string {
	return fmt.Sprintf("今日免费额度已用完：本次需要 %.2f，剩余 %.2f；%s 后重置",
		e.Need, e.Have, time.Until(e.ResetsAt).Round(time.Minute))
}

// Snapshot 是某个 IP 当下的额度快照。Remaining 可能为负（不会超扣，
// 只是"欠着"的部分在下一次请求时会被拒）。
type Snapshot struct {
	Limit     float64   `json:"daily_limit"`
	Used      float64   `json:"used_today"`
	Remaining float64   `json:"remaining"`
	ResetsAt  time.Time `json:"resets_at"`
}

// Stats 是整体观测值。
type Stats struct {
	Daily    float64 `json:"daily_limit"`
	IPsToday int     `json:"ips_today"`
}

type bucket struct {
	used     float64
	lastSeen time.Time
}

// Limiter 是每 IP 每日额度的记账器。并发安全（一把锁，操作都是常数级）。
type Limiter struct {
	daily float64

	mu  sync.Mutex
	day string
	m   map[string]*bucket

	now func() time.Time
}

// New 构造一个限额器；daily<=0 时用 DefaultDaily。now 为 nil 时用 time.Now。
func New(daily float64, now func() time.Time) *Limiter {
	if daily <= 0 {
		daily = DefaultDaily
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		daily: daily,
		day:   dayKey(now()),
		m:     make(map[string]*bucket, 64),
		now:   now,
	}
}

// Limit 返回每日额度。
func (l *Limiter) Limit() float64 { return l.daily }

// dayKey 用**本地日期**做分界：用户看到的"每天"就是自己所在地的每天。
func dayKey(t time.Time) string { return t.Format("2006-01-02") }

// resetsAt 返回当天 24:00（本地时区）。
func resetsAt(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
}

// rollLocked 在跨天时清空计数。调用方必须持锁。
func (l *Limiter) rollLocked(now time.Time) {
	if day := dayKey(now); day != l.day {
		l.day = day
		l.m = make(map[string]*bucket, 64)
	}
}

// Snapshot 返回某 IP 今天的额度情况（只读，不产生计数）。
func (l *Limiter) Snapshot(ip string) Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.rollLocked(now)
	used := 0.0
	if b := l.m[ip]; b != nil {
		used = b.used
	}
	return Snapshot{
		Limit: l.daily, Used: used,
		Remaining: l.daily - used, ResetsAt: resetsAt(now),
	}
}

// Check 只校验额度够不够（预授权），不计数。
func (l *Limiter) Check(ip string, need float64) error {
	s := l.Snapshot(ip)
	if need > s.Remaining {
		return ErrDailyExhausted{Need: need, Have: s.Remaining, ResetsAt: s.ResetsAt}
	}
	return nil
}

// Charge 记账并返回扣减后的快照。额度不足时返回 ErrDailyExhausted，且不记账。
func (l *Limiter) Charge(ip string, units float64) (Snapshot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.rollLocked(now)

	b := l.m[ip]
	if b == nil {
		l.evictLocked(now)
		b = &bucket{}
		l.m[ip] = b
	}
	if units > l.daily-b.used {
		return Snapshot{
			Limit: l.daily, Used: b.used,
			Remaining: l.daily - b.used, ResetsAt: resetsAt(now),
		}, ErrDailyExhausted{Need: units, Have: l.daily - b.used, ResetsAt: resetsAt(now)}
	}
	b.used += units
	b.lastSeen = now
	return Snapshot{
		Limit: l.daily, Used: b.used,
		Remaining: l.daily - b.used, ResetsAt: resetsAt(now),
	}, nil
}

// evictLocked 在超过上限时淘汰最久未出现的条目。调用方必须持锁。
func (l *Limiter) evictLocked(now time.Time) {
	if len(l.m) < maxIPs {
		return
	}
	var oldestIP string
	var oldest time.Time
	first := true
	for ip, b := range l.m {
		if first || b.lastSeen.Before(oldest) {
			oldestIP, oldest, first = ip, b.lastSeen, false
		}
	}
	if oldestIP != "" {
		delete(l.m, oldestIP)
	}
}

// Stats 返回观测值（有多少 IP 今天用过免费额度）。
func (l *Limiter) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollLocked(l.now())
	return Stats{Daily: l.daily, IPsToday: len(l.m)}
}
