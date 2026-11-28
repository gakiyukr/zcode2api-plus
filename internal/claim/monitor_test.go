package claim

import (
	"testing"
	"time"
)

func TestMonitor_StopBeforeStart(t *testing.T) {
	mon := NewMonitor(nil, nil, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		mon.Stop()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("未曾呼叫 Start 時呼叫 Stop 不應永久阻塞")
	}
}

func TestMonitorBackoffLadderRespectsRetryAfter(t *testing.T) {
	mon := NewMonitor(nil, nil, nil)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, beijingZone)

	mon.recordBackoff("a", 429, 0, now)
	if !mon.backoffActive("a", now.Add(29*time.Minute)) {
		t.Fatal("首次 429 應退避 30 分鐘")
	}
	if mon.backoffActive("a", now.Add(30*time.Minute)) {
		t.Fatal("30 分鐘後應解除退避")
	}

	mon.recordBackoff("a", 3002, 0, now.Add(30*time.Minute))
	if !mon.backoffActive("a", now.Add(30*time.Minute+59*time.Minute)) {
		t.Fatal("第二次頻控應升級為 1 小時")
	}
	if mon.backoffActive("a", now.Add(30*time.Minute+time.Hour)) {
		t.Fatal("升級後的 1 小時應到期")
	}

	mon.recordBackoff("b", 429, 5*time.Hour, now)
	if !mon.backoffActive("b", now.Add(4*time.Hour)) {
		t.Fatal("Retry-After 長於階梯時應以 Retry-After 為準")
	}
}

func TestShouldSettleCodeIncludesTerminalCodes(t *testing.T) {
	for _, code := range []int{1002, 1003, 1005, 3007} {
		if !ShouldSettleCode(code) {
			t.Fatalf("%d 應視為當日終態", code)
		}
	}
	if ShouldSettleCode(1004) {
		t.Fatal("1004 未具資格仍應下輪重試")
	}
}

func TestMonitorRollDay(t *testing.T) {
	mon := NewMonitor(nil, nil, nil)
	day1 := time.Date(2026, 1, 1, 23, 0, 0, 0, beijingZone)
	day2 := time.Date(2026, 1, 2, 0, 1, 0, 0, beijingZone)
	if !mon.rollDay(day1) {
		t.Fatal("首輪應視為日界")
	}
	if mon.rollDay(day1) {
		t.Fatal("同一北京日不應重複觸發喚醒")
	}
	if !mon.rollDay(day2) {
		t.Fatal("跨北京午夜應觸發喚醒")
	}
}

func TestUntilBeijingMidnight(t *testing.T) {
	now := time.Date(2026, 1, 1, 23, 30, 0, 0, beijingZone)
	if got := untilBeijingMidnight(now); got != 30*time.Minute {
		t.Fatalf("距午夜應為 30 分鐘: %s", got)
	}
}
