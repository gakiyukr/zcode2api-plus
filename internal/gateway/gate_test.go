package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStartPlanGateAccountAndAddressLimits(t *testing.T) {
	g := NewStartPlanGate()
	g.accountCap = 2
	g.addressCap = 3
	g.waitTimeout = 60 * time.Millisecond

	// 帳號 A 先佔 2 格（達帳號上限）
	l1, err := g.Acquire(context.Background(), "acc-a", "direct", false)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := g.Acquire(context.Background(), "acc-a", "direct", false)
	if err != nil {
		t.Fatal(err)
	}
	// 帳號 A 第 3 條逾時並帶有診斷資訊
	_, err = g.Acquire(context.Background(), "acc-a", "direct", false)
	var waitErr *GateWaitError
	if !errors.As(err, &waitErr) || waitErr.Stage != "account" || waitErr.InFlight != 2 {
		t.Fatalf("預期帳號層排隊逾時診斷錯誤，實際: %v", err)
	}

	// 帳號 B 仍可取 1 格（此時 direct 出口達上限 3）
	l3, err := g.Acquire(context.Background(), "acc-b", "direct", false)
	if err != nil {
		t.Fatal(err)
	}
	// 帳號 C 共用 direct 出口 → 在出口層受限（並自動歸還帳號 C 的槽位）
	_, err = g.Acquire(context.Background(), "acc-c", "direct", false)
	if !errors.As(err, &waitErr) || waitErr.Stage != "address" {
		t.Fatalf("預期出口層排隊逾時，實際: %v", err)
	}
	if g.AccountInFlight("acc-c") != 0 {
		t.Fatalf("出口層失敗時帳號槽位不得洩漏: %d", g.AccountInFlight("acc-c"))
	}

	// 帳號 C 換用獨立代理出口不受 direct 限制
	l4, err := g.Acquire(context.Background(), "acc-c", "http://127.0.0.1:7890", false)
	if err != nil {
		t.Fatalf("獨立代理出口不應受 direct 限制: %v", err)
	}
	l4.Release()
	l3.Release()
	l2.Release()
	l1.Release()
}

func TestStartPlanGateNarrowingAndFrontRequeue(t *testing.T) {
	g := NewStartPlanGate()
	g.accountCap = 3
	g.addressCap = 6
	g.waitTimeout = 2 * time.Second

	l1, _ := g.Acquire(context.Background(), "acc-1", "direct", false)
	l2, _ := g.Acquire(context.Background(), "acc-1", "direct", false)

	// retryAttempt == 0 不觸發收窄
	if g.RecordConcurrencyRefusal("acc-1", "direct", 0) {
		t.Fatal("首次失敗不應立刻收窄")
	}
	// retryAttempt == 1 且在途 2 條 → 收窄為 1 並要求隊首重排
	if !g.RecordConcurrencyRefusal("acc-1", "direct", 1) {
		t.Fatal("預期觸發自適應收窄")
	}
	if gotCap := g.AccountCap("acc-1"); gotCap != 1 {
		t.Fatalf("上限應收窄為 1: %d", gotCap)
	}

	// 歸還 l2 後排回隊首，應優先於普通排隊者取得槽位
	l2.Release()
	order := make(chan string, 2)
	readyBack := make(chan struct{})
	go func() {
		close(readyBack)
		l, err := g.Acquire(context.Background(), "acc-1", "direct", false)
		if err == nil {
			order <- "back"
			l.Release()
		}
	}()
	<-readyBack
	time.Sleep(15 * time.Millisecond)

	readyFront := make(chan struct{})
	go func() {
		close(readyFront)
		l, err := g.Acquire(context.Background(), "acc-1", "direct", true)
		if err == nil {
			order <- "front"
			l.Release()
		}
	}()
	<-readyFront
	time.Sleep(15 * time.Millisecond)

	l1.Release()
	first := <-order
	second := <-order
	if first != "front" || second != "back" {
		t.Fatalf("隊首重排應優先取得槽位: %s -> %s", first, second)
	}
}

func TestCleanupLaneDoesNotDeleteReplacedLane(t *testing.T) {
	g := NewStartPlanGate()
	g.accountCap = 1
	g.addressCap = 6
	g.waitTimeout = 150 * time.Millisecond

	// 佔滿帳號槽位並取得舊 lane 指標
	l1, err := g.Acquire(context.Background(), "acc-race", "direct", false)
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	oldAcctLane := g.accountLanes["acc-race"]
	oldAddrLane := g.addressLanes["direct"]
	g.mu.Unlock()
	if oldAcctLane == nil || oldAddrLane == nil {
		t.Fatal("預期已建立舊的 account 與 address lane")
	}

	// 模擬舊 lane 清空並被移出 map（例如經 abortWaitersForAddrLocked 與歸還後），
	// 隨後同 key 建立新的活躍 lane
	l1.Release()
	lNew, err := g.Acquire(context.Background(), "acc-race", "direct", false)
	if err != nil {
		t.Fatal(err)
	}
	defer lNew.Release()

	g.mu.Lock()
	newAcctLane := g.accountLanes["acc-race"]
	newAddrLane := g.addressLanes["direct"]
	if newAcctLane == nil || newAcctLane == oldAcctLane {
		g.mu.Unlock()
		t.Fatal("預期同 key 已重建新的 account lane")
	}
	// 模擬舊 waiter 逾時後對舊 lane 指標呼叫 cleanupLaneLocked
	g.cleanupLaneLocked(true, "acc-race", oldAcctLane)
	g.cleanupLaneLocked(false, "direct", oldAddrLane)
	gotAcctLane := g.accountLanes["acc-race"]
	gotAddrLane := g.addressLanes["direct"]
	g.mu.Unlock()

	if gotAcctLane != newAcctLane {
		t.Fatal("cleanupLaneLocked 誤刪了重建後的活躍 account lane")
	}
	if gotAddrLane != newAddrLane {
		t.Fatal("cleanupLaneLocked 誤刪了重建後的活躍 address lane")
	}
	if g.AccountInFlight("acc-race") != 1 {
		t.Fatalf("新 lane 的 inflight 應維持為 1，實際: %d", g.AccountInFlight("acc-race"))
	}
}

func TestReleaseLaneWakesMultipleAfterNarrowExpiry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	g := NewStartPlanGate()
	g.accountCap = 3
	g.addressCap = 6
	g.waitTimeout = 2 * time.Second
	g.now = func() time.Time { return now }

	// 先佔 2 個槽位，觸發收窄至 cap=1，再歸還其中 1 個使 inflight=1（達到收窄上限）
	l1, err := g.Acquire(context.Background(), "acc-narrow", "direct", false)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := g.Acquire(context.Background(), "acc-narrow", "direct", false)
	if err != nil {
		t.Fatal(err)
	}
	if !g.RecordConcurrencyRefusal("acc-narrow", "direct", 1) {
		t.Fatal("預期觸發自適應收窄")
	}
	if got := g.AccountCap("acc-narrow"); got != 1 {
		t.Fatalf("收窄上限應為 1，實際: %d", got)
	}
	l2.Release()

	// 排入 3 個 waiter（此時 cap=1、inflight=1，全數排隊）
	woken := make(chan *GateLease, 3)
	for range 3 {
		go func() {
			l, err := g.Acquire(context.Background(), "acc-narrow", "direct", false)
			if err == nil {
				woken <- l
			}
		}()
	}
	// 等待 3 個 waiter 全部進入佇列
	deadline := time.Now().Add(time.Second)
	for {
		g.mu.Lock()
		q := 0
		if lane := g.accountLanes["acc-narrow"]; lane != nil {
			q = len(lane.waiters)
		}
		g.mu.Unlock()
		if q == 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 推進時間讓 5 分鐘收窄到期（上限回升至 3），釋放 l1 應一次喚醒全部 3 個排隊者
	now = now.Add(6 * time.Minute)
	l1.Release()

	var leases []*GateLease
	for i := 0; i < 3; i++ {
		select {
		case l := <-woken:
			leases = append(leases, l)
		case <-time.After(time.Second):
			t.Fatalf("收窄到期後一次釋放應批次喚醒全部排隊者至上限 3，只喚醒了 %d 個", len(leases))
		}
	}
	if got := g.AccountInFlight("acc-narrow"); got != 3 {
		t.Fatalf("批次喚醒後 inflight 應為 3，實際: %d", got)
	}
	for _, l := range leases {
		l.Release()
	}
}
