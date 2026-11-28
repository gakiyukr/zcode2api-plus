// Start Plan 雙層併發閘門與自適應收窄。
// 對齊 magpie zcode 插件 0.1.54~0.1.71 的併發治理策略：
//   - 單帳號 Start Plan 預設最多 3 個在途請求（遇同帳號在途 3008/3009/3010
//     於 1s 重試仍被拒時，自動收窄至 inFlight-1 維持 5 分鐘，並讓該請求歸還
//     槽位排回隊首重送，不直接報 429）；
//   - 同出口地址（直連或同代理）所有帳號共用最多 6 個在途 Start Plan 請求。
//
// 上游併發拒絕只縮小對應帳號的併發上限或延遲重試，不暫停整個出口，
// 避免把正常的 3008/3009/3010 放大成全域 429。
package gateway

import (
	"context"
	"fmt"
	"sync"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/egress"
	"zcode2api/internal/model"
)

const (
	defaultNarrowDuration = 5 * time.Minute
)

// GateWaitError 排隊等待槽位逾時，附帶等候診斷資訊（對齊 magpie 0.1.71）。
type GateWaitError struct {
	Stage           string        // "account" | "address"
	Cap             int           // 當前上限
	InFlight        int           // 該層當前在途數
	OldestInFlight  time.Duration // 最老在途請求已執行時間
	Queued          int           // 同層其餘排隊數
	NarrowRemaining time.Duration // 若處於自適應收窄期，剩餘收窄秒數
	WaitDuration    time.Duration
}

func (e *GateWaitError) Error() string {
	scope := "此帳號"
	if e.Stage == "address" {
		scope = "目前出口地址"
	}
	msg := fmt.Sprintf("ZCode Start Plan %s併發上限為 %d，請求已排隊等待 %ds 未發送（在途 %d",
		scope, e.Cap, int(e.WaitDuration.Seconds()), e.InFlight)
	if e.OldestInFlight > 0 {
		msg += fmt.Sprintf("，最長已執行 %ds", int(e.OldestInFlight.Seconds()))
	}
	if e.Queued > 0 {
		msg += fmt.Sprintf("，另有 %d 條排隊中", e.Queued)
	}
	if e.NarrowRemaining > 0 {
		msg += fmt.Sprintf("，帳號已因併發拒答收窄、剩餘 %ds", int(e.NarrowRemaining.Seconds()))
	}
	msg += "）"
	return msg
}

type gateWaiter struct {
	ch      chan struct{}
	granted bool
}

type laneState struct {
	inflight int
	starts   []time.Time
	waiters  []*gateWaiter
}

type narrowState struct {
	cap   int
	until time.Time
}

// StartPlanGate 管理 Start Plan（JWT）請求的雙層併發與自適應收窄。
type StartPlanGate struct {
	mu sync.Mutex

	accountCap     int
	addressCap     int
	waitTimeout    time.Duration
	narrowDuration time.Duration
	now            func() time.Time

	accountLanes map[string]*laneState
	addressLanes map[string]*laneState
	narrow       map[string]narrowState
}

// NewStartPlanGate 以目前 config 建立 Start Plan 併發閘門。
func NewStartPlanGate() *StartPlanGate {
	return &StartPlanGate{
		accountCap:     config.StartPlanAccountConcurrency,
		addressCap:     config.StartPlanAddressConcurrency,
		waitTimeout:    time.Duration(config.StartPlanGateWaitSeconds) * time.Second,
		narrowDuration: defaultNarrowDuration,
		now:            time.Now,
		accountLanes:   map[string]*laneState{},
		addressLanes:   map[string]*laneState{},
		narrow:         map[string]narrowState{},
	}
}

// DefaultStartGate 為 engine 與 asyncpool 共用的預設 Start Plan 併發閘門。
var DefaultStartGate = NewStartPlanGate()

// AddressKeyFor 取得帳號對應的出口地址鍵：有設定 proxy_url 則以代理為分桶，否則歸入 "direct"。
func AddressKeyFor(acc *model.Account) string {
	return egress.AccountKey(acc)
}

// GateLease 代表一次已取得的雙層槽位租約；使用完畢後必須呼叫 Release。
type GateLease struct {
	gate       *StartPlanGate
	accountKey string
	addrKey    string
	acctStart  time.Time
	addrStart  time.Time
	once       sync.Once
}

// Release 歸還雙層槽位（冪等）。
func (l *GateLease) Release() {
	if l == nil || l.gate == nil {
		return
	}
	l.once.Do(func() {
		l.gate.release(l.accountKey, l.addrKey, l.acctStart, l.addrStart)
	})
}

// wallNow 剝離單調時鐘讀數，確保系統休眠喚醒後熔斷、收窄與逾時仍按真實牆鐘即時生效。
func (g *StartPlanGate) wallNow() time.Time {
	return g.now().Round(0)
}

// AccountInFlight 回傳該帳號目前持有槽位的在途請求數。
func (g *StartPlanGate) AccountInFlight(accountKey string) int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if l := g.accountLanes[accountKey]; l != nil {
		return l.inflight
	}
	return 0
}

// AccountCap 回傳該帳號當前的有效併發上限（考慮自適應收窄）。
func (g *StartPlanGate) AccountCap(accountKey string) int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	c, _ := g.accountCapLocked(accountKey, g.wallNow())
	return c
}

// Acquire 依序取得帳號槽位與出口槽位（共用單一等待截止時間）。
// front=true 時插入帳號等待佇列隊首（供自適應收窄觸發的重排請求使用）。
func (g *StartPlanGate) Acquire(ctx context.Context, accountKey, addrKey string, front bool) (*GateLease, error) {
	if g == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	startWait := g.wallNow()
	deadline := startWait.Add(g.waitTimeout)

	acctStart, err := g.acquireLane(ctx, true, accountKey, addrKey, front, deadline, startWait)
	if err != nil {
		return nil, err
	}
	addrStart, err := g.acquireLane(ctx, false, addrKey, addrKey, false, deadline, startWait)
	if err != nil {
		// 出口層取槽失敗時必須立即歸還帳號層槽位，避免洩漏
		g.mu.Lock()
		g.releaseLaneLocked(true, accountKey, acctStart, g.wallNow())
		g.mu.Unlock()
		return nil, err
	}
	return &GateLease{
		gate:       g,
		accountKey: accountKey,
		addrKey:    addrKey,
		acctStart:  acctStart,
		addrStart:  addrStart,
	}, nil
}

func (g *StartPlanGate) acquireLane(
	ctx context.Context,
	isAccount bool,
	key string,
	addrKey string,
	front bool,
	deadline time.Time,
	waitOrigin time.Time,
) (time.Time, error) {
	g.mu.Lock()
	now := g.wallNow()
	lanes := g.addressLanes
	if isAccount {
		lanes = g.accountLanes
	}
	lane := lanes[key]
	if lane == nil {
		lane = &laneState{}
		lanes[key] = lane
	}
	capNow, _ := g.laneCapLocked(isAccount, key, now)
	if lane.inflight < capNow && len(lane.waiters) == 0 {
		lane.inflight++
		lane.starts = append(lane.starts, now)
		g.mu.Unlock()
		return now, nil
	}
	w := &gateWaiter{ch: make(chan struct{})}
	if front {
		lane.waiters = append([]*gateWaiter{w}, lane.waiters...)
	} else {
		lane.waiters = append(lane.waiters, w)
	}
	g.mu.Unlock()

	rem := deadline.Sub(g.wallNow())
	if rem < 0 {
		rem = 0
	}
	timer := time.NewTimer(rem)
	defer timer.Stop()

	select {
	case <-w.ch:
		g.mu.Lock()
		defer g.mu.Unlock()
		if w.granted {
			ts := g.wallNow()
			currentLanes := g.addressLanes
			if isAccount {
				currentLanes = g.accountLanes
			}
			// 寫入 starts 必須操作 map 中的當前 lane（原 lane 可能已被替換）
			if cur := currentLanes[key]; cur != nil {
				cur.starts = append(cur.starts, ts)
			}
			return ts, nil
		}
		return time.Time{}, ctx.Err()
	case <-ctx.Done():
		g.cancelWaiter(isAccount, key, w)
		return time.Time{}, ctx.Err()
	case <-timer.C:
		g.mu.Lock()
		if w.granted {
			// 恰好在逾時瞬間拿到槽位，直接歸還
			g.releaseLaneLocked(isAccount, key, time.Time{}, g.wallNow())
			g.mu.Unlock()
		} else {
			removeWaiter(lane, w)
			g.cleanupLaneLocked(isAccount, key, lane)
			g.mu.Unlock()
		}
		return time.Time{}, g.buildWaitError(isAccount, key, g.wallNow().Sub(waitOrigin))
	}
}

func (g *StartPlanGate) cancelWaiter(isAccount bool, key string, w *gateWaiter) {
	g.mu.Lock()
	defer g.mu.Unlock()
	lanes := g.addressLanes
	if isAccount {
		lanes = g.accountLanes
	}
	lane := lanes[key]
	if w.granted {
		g.releaseLaneLocked(isAccount, key, time.Time{}, g.wallNow())
		return
	}
	if lane != nil {
		removeWaiter(lane, w)
		g.cleanupLaneLocked(isAccount, key, lane)
	}
}

func (g *StartPlanGate) release(accountKey, addrKey string, acctStart, addrStart time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.wallNow()
	g.releaseLaneLocked(false, addrKey, addrStart, now)
	g.releaseLaneLocked(true, accountKey, acctStart, now)
}

func (g *StartPlanGate) releaseLaneLocked(isAccount bool, key string, startTS time.Time, now time.Time) {
	lanes := g.addressLanes
	if isAccount {
		lanes = g.accountLanes
	}
	lane := lanes[key]
	if lane == nil {
		return
	}
	if !startTS.IsZero() {
		removeTimestamp(&lane.starts, startTS)
	}
	// 先歸還槽位
	if lane.inflight > 0 {
		lane.inflight--
	}
	// 以迴圈喚醒排隊者至當前上限（收窄到期回升時可一次喚醒多人）
	capNow, _ := g.laneCapLocked(isAccount, key, now)
	for lane.inflight < capNow && len(lane.waiters) > 0 {
		next := lane.waiters[0]
		lane.waiters = lane.waiters[1:]
		next.granted = true
		lane.inflight++
		close(next.ch)
	}
	g.cleanupLaneLocked(isAccount, key, lane)
}

func (g *StartPlanGate) cleanupLaneLocked(isAccount bool, key string, lane *laneState) {
	if lane.inflight == 0 && len(lane.waiters) == 0 {
		lanes := g.addressLanes
		if isAccount {
			lanes = g.accountLanes
		}
		// 只有當 map 中仍是同一個 lane 指標時才刪除，避免誤刪重建後的新 lane
		if lanes[key] == lane {
			delete(lanes, key)
		}
	}
}

func (g *StartPlanGate) accountCapLocked(accountKey string, now time.Time) (int, time.Duration) {
	if ns, ok := g.narrow[accountKey]; ok {
		if now.Before(ns.until) {
			return ns.cap, ns.until.Sub(now)
		}
		delete(g.narrow, accountKey)
	}
	return g.accountCap, 0
}

func (g *StartPlanGate) laneCapLocked(isAccount bool, key string, now time.Time) (int, time.Duration) {
	if isAccount {
		return g.accountCapLocked(key, now)
	}
	return g.addressCap, 0
}

// RecordConcurrencyRefusal 在上游回傳 3008/3009/3010 時呼叫。
// 僅在 retryAttempt == 1（即 1s 重試仍被拒答）且同帳號在途數 > 1 時，
// 自動收窄該帳號上限為 max(1, inFlight-1) 維持 5 分鐘，並回傳 requeue=true
// （呼叫方釋放槽位並排回隊首重送）。不再因連續拒答暫停整個出口。
func (g *StartPlanGate) RecordConcurrencyRefusal(accountKey, _ string, retryAttempt int) bool {
	if g == nil || retryAttempt != 1 {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.wallNow()

	inflight := 0
	if lane := g.accountLanes[accountKey]; lane != nil {
		inflight = lane.inflight
	}
	if inflight <= 1 {
		return false
	}
	newCap := max(1, inflight-1)
	if prev, ok := g.narrow[accountKey]; ok && now.Before(prev.until) && prev.cap < newCap {
		newCap = prev.cap
	}
	g.narrow[accountKey] = narrowState{cap: newCap, until: now.Add(g.narrowDuration)}
	return true
}

func (g *StartPlanGate) buildWaitError(isAccount bool, key string, waited time.Duration) *GateWaitError {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.wallNow()
	stage := "address"
	lanes := g.addressLanes
	if isAccount {
		stage = "account"
		lanes = g.accountLanes
	}
	capNow, narrowRem := g.laneCapLocked(isAccount, key, now)
	e := &GateWaitError{
		Stage:           stage,
		Cap:             capNow,
		NarrowRemaining: narrowRem,
		WaitDuration:    waited,
	}
	if lane := lanes[key]; lane != nil {
		e.InFlight = lane.inflight
		e.Queued = len(lane.waiters)
		for _, ts := range lane.starts {
			if d := now.Sub(ts); d > e.OldestInFlight {
				e.OldestInFlight = d
			}
		}
	}
	return e
}

func removeWaiter(lane *laneState, target *gateWaiter) {
	for i, w := range lane.waiters {
		if w == target {
			lane.waiters = append(lane.waiters[:i], lane.waiters[i+1:]...)
			return
		}
	}
}

func removeTimestamp(starts *[]time.Time, target time.Time) {
	s := *starts
	for i, ts := range s {
		if ts.Equal(target) {
			*starts = append(s[:i], s[i+1:]...)
			return
		}
	}
	// 找不到精確時間戳時移除最舊的一筆，防止切片殘留
	if len(s) > 0 {
		*starts = s[1:]
	}
}
