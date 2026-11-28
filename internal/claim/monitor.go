// 背景套餐補領監控與北京自然日去重器（對齊 magpie 0.1.69 與 D3-vin/Zcode2Api）。
//
// ZCode 會在一天的不同時段（例如北京時間 19:00 與 19:30）向活動橫幅分批投放新套餐。
// 本模組預設每 30 分鐘（ZCODE_CLAIM_REFRESH_INTERVAL=1800）對全部可用 JWT 帳號進行
// 一次補檢，並以北京自然日記憶當日已領取、1003（已領過）與 3007（驗證碼拒答）的 plan_id，
// 避免重複提交，同時讓 1004（暫未具資格）與稍後掉落的新方案能在下一輪自動補領。
package claim

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

const (
	// BatchMaxWorkers 全量領取／背景補領的並發 worker 上限，避免打垮出站代理池。
	BatchMaxWorkers = 4
	// BatchAccountPause 每個 worker 處理完一個帳號後的節流間隔。
	BatchAccountPause = 400 * time.Millisecond
)

var beijingZone = time.FixedZone("CST", 8*3600)

// BeijingDay 回傳指定時刻對應的北京時間自然日字串（YYYY-MM-DD）。
func BeijingDay(t time.Time) string {
	return t.In(beijingZone).Format("2006-01-02")
}

// DayTracker 依「帳號 + 北京自然日」記憶當日已確定結果的 plan_id，避免重複提交。
type DayTracker struct {
	mu      sync.Mutex
	day     string
	settled map[string]map[string]string // accountID -> planID -> outcome ("claimed"|"1003"|"3007")
	now     func() time.Time
}

// NewDayTracker 建立日內去重器。
func NewDayTracker() *DayTracker {
	return &DayTracker{
		settled: map[string]map[string]string{},
		now:     time.Now,
	}
}

// SetNow 注入時鐘（測試用）。
func (d *DayTracker) SetNow(fn func() time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.now = fn
}

func (d *DayTracker) rollLocked() {
	today := BeijingDay(d.now())
	if d.day != today {
		d.day = today
		d.settled = map[string]map[string]string{}
	}
}

// IsSettled 判斷該帳號的指定 plan_id 在北京時間今日是否已有確定結果（不應再重送）。
func (d *DayTracker) IsSettled(accountID, planID string) bool {
	if d == nil || accountID == "" || planID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rollLocked()
	_, ok := d.settled[accountID][planID]
	return ok
}

// MarkSettled 記錄今日已確定結果的方案（claimed / 1003 / 3007）。
func (d *DayTracker) MarkSettled(accountID, planID, outcome string) {
	if d == nil || accountID == "" || planID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rollLocked()
	if d.settled[accountID] == nil {
		d.settled[accountID] = map[string]string{}
	}
	d.settled[accountID][planID] = outcome
}

// ShouldSettleCode 判斷領取業務碼是否屬於「當日不再重送」的終態（1003 已領、3007 驗證碼拒答）。
// 1004（未具資格）與網路錯誤回傳 false，留待下一輪 30 分鐘重檢。
func ShouldSettleCode(code int) bool {
	switch code {
	case 1002, 1003, 1005, 3007:
		// 1002 活動結束、1005 今日名額用完、1003 已領、3007 驗證碼拒答：
		// 當日不再重送，避免每 30 分鐘白刷一次驗證碼。
		return true
	}
	return false
}

// claimBackoffLadder 帳號級頻控退避階梯（429/3002）；成功一次即清空。
var claimBackoffLadder = []time.Duration{
	30 * time.Minute,
	time.Hour,
	2 * time.Hour,
	6 * time.Hour,
}

type accountBackoff struct {
	attempts int
	next     time.Time
	lastCode int
}

// Monitor 背景週期性套餐補領器。
type Monitor struct {
	Store   *store.Store
	Captcha *captcha.Manager
	Quota   *quota.Service
	Service *Service
	Tracker *DayTracker

	// Pause 單帳號處理完畢的節流間隔（預設 BatchAccountPause，測試可設 0）。
	Pause time.Duration

	backoffMu sync.Mutex
	backoff   map[string]accountBackoff
	lastDay   string
	nowFn     func() time.Time

	stop       chan struct{}
	done       chan struct{}
	start      sync.Once
	started    atomic.Bool
	stopCtx    context.Context
	stopCancel context.CancelFunc
}

// NewMonitor 建立背景補領監控器。
func NewMonitor(st *store.Store, cm *captcha.Manager, qs *quota.Service) *Monitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Monitor{
		Store:      st,
		Captcha:    cm,
		Quota:      qs,
		Service:    NewService(cm),
		Tracker:    NewDayTracker(),
		Pause:      BatchAccountPause,
		backoff:    map[string]accountBackoff{},
		nowFn:      time.Now,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		stopCtx:    ctx,
		stopCancel: cancel,
	}
}

// Start 啟動背景補領迴圈（冪等）。
func (m *Monitor) Start() {
	m.start.Do(func() {
		m.started.Store(true)
		go m.loop()
	})
}

// Stop 停止背景補領並等待收尾；未曾啟動時直接返回，避免永久阻塞。
func (m *Monitor) Stop() {
	if m.stopCancel != nil {
		m.stopCancel()
	}
	if !m.started.Load() {
		return
	}
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
}

// SetNow 注入時鐘（測試用）。
func (m *Monitor) SetNow(fn func() time.Time) {
	m.backoffMu.Lock()
	m.nowFn = fn
	m.backoffMu.Unlock()
}

func (m *Monitor) now() time.Time {
	m.backoffMu.Lock()
	fn := m.nowFn
	m.backoffMu.Unlock()
	if fn == nil {
		return time.Now()
	}
	return fn()
}

// rollDay 回報北京自然日是否切換；日切換時對 exhausted 帳號優先喚醒。
func (m *Monitor) rollDay(now time.Time) bool {
	today := BeijingDay(now)
	m.backoffMu.Lock()
	defer m.backoffMu.Unlock()
	if m.lastDay == today {
		return false
	}
	m.lastDay = today
	return true
}

func (m *Monitor) backoffActive(accountID string, now time.Time) bool {
	m.backoffMu.Lock()
	defer m.backoffMu.Unlock()
	state, ok := m.backoff[accountID]
	if !ok {
		return false
	}
	if state.next.After(now) {
		return true
	}
	// 已到期但保留 attempts：再次失敗時應升級到下一階退避。
	return false
}

func (m *Monitor) recordBackoff(accountID string, code int, retryAfter time.Duration, now time.Time) {
	m.backoffMu.Lock()
	defer m.backoffMu.Unlock()
	state := m.backoff[accountID]
	idx := state.attempts
	if idx >= len(claimBackoffLadder) {
		idx = len(claimBackoffLadder) - 1
	}
	delay := claimBackoffLadder[idx]
	if retryAfter > delay {
		delay = retryAfter
	}
	state.attempts++
	state.lastCode = code
	state.next = now.Add(delay)
	m.backoff[accountID] = state
	web.Warn("claim", fmt.Sprintf("帳號 %s 領取頻控（code=%d），%s 後再試", accountID, code, delay))
}

func (m *Monitor) clearBackoff(accountID string) {
	m.backoffMu.Lock()
	delete(m.backoff, accountID)
	m.backoffMu.Unlock()
}

func isClaimRateLimit(code int) bool {
	return code == http.StatusTooManyRequests || code == 3002
}

// RunOnce 執行一輪全池補檢（含 panic 隔離與並發節流）。
func (m *Monitor) RunOnce() {
	defer func() {
		if r := recover(); r != nil {
			web.Warn("claim", fmt.Sprintf("背景補領發生異常（已隔離，下一輪繼續）: %v", r))
		}
	}()
	if m.Store == nil || m.Service == nil {
		return
	}
	now := m.now()
	dayRolled := m.rollDay(now)
	var targets []*model.Account
	for _, a := range m.Store.ListAccounts(model.ProviderZai) {
		if a.ArchivedAt != nil || a.Status == model.StatusDisabled || a.Status == model.StatusInvalid {
			continue
		}
		if a.Mode != "jwt" || a.JWTToken == nil || *a.JWTToken == "" {
			continue
		}
		// 冷卻中帳號跳過（避免風控期產生額外寫流量）
		if !a.IsSelectable(now) && a.Status == model.StatusCooling {
			continue
		}
		if m.backoffActive(a.ID, now) {
			continue
		}
		targets = append(targets, a)
	}
	if len(targets) == 0 {
		return
	}
	// 跨日後優先喚醒 exhausted：先補激活與領取，再刷新額度恢復調度。
	sort.SliceStable(targets, func(i, j int) bool {
		return targets[i].Status == model.StatusExhausted && targets[j].Status != model.StatusExhausted
	})

	workers := min(BatchMaxWorkers, len(targets))
	jobs := make(chan *model.Account, len(targets))
	for _, acc := range targets {
		jobs <- acc
	}
	close(jobs)

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for acc := range jobs {
				select {
				case <-m.stop:
					return
				default:
				}
				m.checkAccount(acc, dayRolled)
				if m.Pause > 0 {
					select {
					case <-m.stop:
						return
					case <-time.After(m.Pause):
					}
				}
			}
		}()
	}
	wg.Wait()
}

func (m *Monitor) checkAccount(acc *model.Account, wakeExhausted bool) {
	defer func() {
		if r := recover(); r != nil {
			web.Warn("claim", fmt.Sprintf("帳號 %s 背景補領異常（已隔離）: %v", acc.Name, r))
		}
	}()
	ctx := m.stopCtx
	if ctx == nil {
		ctx = context.Background()
	}
	// 日切換後先刷新額度：昨天耗盡的帳號可能已恢復，先讓調度可見 active，
	// 再進行激活上報與套餐補領。
	if wakeExhausted && acc.Status == model.StatusExhausted && m.Quota != nil {
		m.Quota.RefreshAccounts([]*model.Account{acc})
	}
	_ = ReportActivationEventsContext(ctx, acc)
	plans, err := m.Service.PreviewPlansContext(ctx, acc)
	if err != nil {
		var ce *ClaimError
		if errors.As(err, &ce) && isClaimRateLimit(ce.Code) {
			m.recordBackoff(acc.ID, ce.Code, ce.RetryAfter, m.now())
		}
		return
	}
	m.clearBackoff(acc.ID)
	if len(plans) == 0 {
		return
	}
	claimedAny := false
	for _, p := range plans {
		if ctx.Err() != nil {
			return
		}
		planID, _ := p["plan_id"].(string)
		if planID == "" || m.Tracker.IsSettled(acc.ID, planID) {
			continue
		}
		res, err := m.Service.ClaimContext(ctx, acc, planID)
		if err != nil {
			var ce *ClaimError
			if errors.As(err, &ce) {
				if isClaimRateLimit(ce.Code) {
					m.recordBackoff(acc.ID, ce.Code, ce.RetryAfter, m.now())
					return
				}
				if ShouldSettleCode(ce.Code) {
					m.Tracker.MarkSettled(acc.ID, planID, fmt.Sprintf("%d", ce.Code))
				}
			}
			web.Warn("claim", fmt.Sprintf("帳號 %s 背景補領 %s 失敗: %v", acc.Name, planID, err))
			continue
		}
		m.Tracker.MarkSettled(acc.ID, planID, "claimed")
		claimedAny = true
		planName, _ := res["plan_name"].(string)
		if planName == "" {
			if name, _ := p["name"].(string); name != "" {
				planName = name
			} else {
				planName = planID
			}
		}
		web.Ok("claim", fmt.Sprintf("帳號 %s 背景補領成功: %s", acc.Name, planName))
	}
	if claimedAny && m.Quota != nil {
		m.Quota.RefreshAccounts([]*model.Account{acc})
	}
}

func (m *Monitor) loop() {
	defer close(m.done)
	// 啟動後延遲 30 秒避讓服務初始化與首輪額度刷新
	select {
	case <-m.stop:
		return
	case <-time.After(30 * time.Second):
	}
	for {
		interval := config.ClaimRefreshInterval
		if interval > 0 {
			m.RunOnce()
		}
		wait := interval
		if wait <= 0 {
			wait = 1800
		}
		delay := time.Duration(wait) * time.Second
		if interval > 0 {
			// 睡到最近一次北京午夜，讓跨日喚醒不必等完整輪詢間隔。
			if until := untilBeijingMidnight(m.now()); until > 0 && until < delay {
				delay = until
			}
		}
		select {
		case <-m.stop:
			return
		case <-time.After(delay):
		}
	}
}

// untilBeijingMidnight 返回距離下一個北京自然日 00:00 的時長。
func untilBeijingMidnight(now time.Time) time.Duration {
	b := now.In(beijingZone)
	next := time.Date(b.Year(), b.Month(), b.Day()+1, 0, 0, 0, 0, beijingZone)
	return next.Sub(now)
}
