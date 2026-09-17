package publicq

import (
	"errors"
	"testing"
	"time"
)

// TestPerIPDailyAllowance：额度按 IP 隔离、按自然日重置。
func TestPerIPDailyAllowance(t *testing.T) {
	now := time.Date(2026, 1, 2, 23, 0, 0, 0, time.Local)
	clock := func() time.Time { return now }
	l := New(25, clock)

	// 甲乙两个 IP 各记各的
	if _, err := l.Charge("1.1.1.1", 20); err != nil {
		t.Fatalf("第一次计费应成功：%v", err)
	}
	if got := l.Snapshot("1.1.1.1").Remaining; got != 5 {
		t.Errorf("甲剩余 = %v，想要 5", got)
	}
	if got := l.Snapshot("2.2.2.2").Remaining; got != 25 {
		t.Errorf("乙不该被甲影响：剩余 %v", got)
	}
	// 超额：报错且不记账
	if _, err := l.Charge("1.1.1.1", 6); err == nil {
		t.Error("超过额度应报错")
	} else {
		var ex ErrDailyExhausted
		if !errors.As(err, &ex) || ex.Have != 5 {
			t.Errorf("错误里应带剩余额度，得到 %+v", ex)
		}
	}
	if got := l.Snapshot("1.1.1.1").Remaining; got != 5 {
		t.Errorf("失败的计费不该扣减：剩余 %v", got)
	}
	// 预授权
	if err := l.Check("1.1.1.1", 5); err != nil {
		t.Errorf("刚好够应通过：%v", err)
	}
	if err := l.Check("1.1.1.1", 5.01); err == nil {
		t.Error("差一点应被拒")
	}

	// 跨天重置
	now = time.Date(2026, 1, 3, 0, 1, 0, 0, time.Local)
	if got := l.Snapshot("1.1.1.1"); got.Used != 0 || got.Remaining != 25 {
		t.Errorf("跨天后应重置：%+v", got)
	}
	if got := l.Snapshot("1.1.1.1").ResetsAt; got.Day() != 4 || got.Hour() != 0 {
		t.Errorf("重置时间应是次日零点，得到 %v", got)
	}
	if n := l.Stats().IPsToday; n != 0 {
		t.Errorf("跨天后今天的 IP 数应为 0，得到 %d", n)
	}
}

// TestEvictsOldestWhenFull：超过条目上限时淘汰最久未出现的 IP，活跃的不受影响。
func TestEvictsOldestWhenFull(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.Local)
	clock := func() time.Time { return now }
	l := New(10, clock)

	for i := 0; i < maxIPs; i++ {
		ip := "10.0." + itoa(i/256) + "." + itoa(i%256)
		if _, err := l.Charge(ip, 1); err != nil {
			t.Fatalf("第 %d 个 IP 计费失败：%v", i, err)
		}
		now = now.Add(time.Millisecond) // 让 lastSeen 有先后
	}
	// 活跃一次，把自己刷新成最新
	active := "10.0.0.0"
	if _, err := l.Charge(active, 1); err != nil {
		t.Fatal(err)
	}
	// 再来一个新 IP，触发淘汰
	if _, err := l.Charge("192.168.1.1", 1); err != nil {
		t.Fatal(err)
	}
	if got := l.Snapshot(active).Used; got != 2 {
		t.Errorf("活跃 IP 不该被淘汰：used=%v", got)
	}
	if n := len(l.m); n > maxIPs {
		t.Errorf("条目数应被限制在 %d 以内，得到 %d", maxIPs, n)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
