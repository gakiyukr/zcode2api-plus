// Package quota 额度 / 余额 / 用量查询与账号状态判定。
// 对应 Python 版 app/quota.py：在查询基础上提供「额度用完自动标记
// exhausted」的监控能力，并为后台刷新端点与网关错误路径提供接入。
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// QuotaCacheTTL 成功结果的复用窗口（对齐 _QUOTA_CACHE_TTL_SECONDS）。
const QuotaCacheTTL = 15 * time.Second

// HTTPClient 计费端点客户端抽象（*http.Client 满足；测试可注入假客户端）。
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Service 额度查询服务：inflight 去重 + 短缓存 + 账号状态回写。
type Service struct {
	Store  *store.Store
	Client HTTPClient // nil 时使用 20s 总超时的默认客户端（对齐 make_async_client(timeout=20)）

	mu       sync.Mutex
	inflight map[string]*inflightCall
	cache    map[string]cacheEntry

	now func() time.Time
}

// inflightCall 进行中的查询：done 用 close 广播完成（多个并发等待方共享
// 同一结果，对齐 Python 多协程 await 同一 task）；result 由 close 前的写入
// 与 <-done 建立 happens-before，读取无需加锁。
type inflightCall struct {
	done   chan struct{}
	result map[string]any
}

type cacheEntry struct {
	at     time.Time
	result map[string]any
}

// NewService 创建服务（inflight / cache 按账号 ID 索引，语义对齐模块级全局表）。
func NewService(st *store.Store) *Service {
	return &Service{
		Store:    st,
		inflight: map[string]*inflightCall{},
		cache:    map[string]cacheEntry{},
		now:      time.Now,
	}
}

// SetNow 注入时钟（测试用）。
func (s *Service) SetNow(fn func() time.Time) { s.now = fn }

// authHeaders 对齐 quota._auth_headers：额度端点必须携带完整设备信息。
func authHeaders(acc *model.Account) map[string]string {
	headers := map[string]string{
		"Content-Type":        "application/json",
		"User-Agent":          config.UserAgent,
		"X-ZCode-App-Version": config.ZcodeClientVersion,
		"X-Platform":          config.ZcodeClientPlatform,
		"X-Device-Mid":        config.DeviceMid(),
		"HTTP-Referer":        "https://zcode.z.ai/",
	}
	if acc.Mode == "jwt" && acc.JWTToken != nil {
		headers["Authorization"] = "Bearer " + *acc.JWTToken
	} else if acc.APIKey != nil {
		headers["x-api-key"] = *acc.APIKey
	}
	return headers
}

// ── 快照合并（对齐 _sum_units / _earliest_ts / _merge_quota_entry）──────────

var sumFields = []string{"total", "used", "remaining", "available"}

var earliestFields = []string{"period_start", "period_end", "expires_at"}

// sumUnits 数值额度合并：任一端缺值时保留另一端，避免 None 参与加总。
func sumUnits(current, incoming any) any {
	if current == nil {
		return incoming
	}
	if incoming == nil {
		return current
	}
	a, aok := asNumber(current)
	b, bok := asNumber(incoming)
	if aok && bok {
		return a + b
	}
	// 非数值类型不安全相加，保守保留当前值
	return current
}

// earliestTS 周期时间合并取最早者，作为额度恢复或到期的保守估计；
// 不可比较的类型（对齐 Python TypeError 分支）保留当前值。
func earliestTS(current, incoming any) any {
	if current == nil {
		return incoming
	}
	if incoming == nil {
		return current
	}
	a, aok := asNumber(current)
	b, bok := asNumber(incoming)
	if !aok || !bok {
		return current
	}
	if a <= b {
		return current
	}
	return incoming
}

func asNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// mergeQuotaEntry 同名模型出现在多个订阅时，合并为单一加总快照而非互相覆盖。
func mergeQuotaEntry(current map[string]any, incoming map[string]any) map[string]any {
	if current == nil {
		return incoming
	}
	for _, key := range sumFields {
		current[key] = sumUnits(current[key], incoming[key])
	}
	for _, key := range earliestFields {
		current[key] = earliestTS(current[key], incoming[key])
	}
	// period 去重后按字典序拼接，缺失时保持 null
	seen := map[string]bool{}
	var periods []string
	for _, raw := range []any{current["period"], incoming["period"]} {
		if s, ok := raw.(string); ok && s != "" && !seen[s] {
			seen[s] = true
			periods = append(periods, s)
		}
	}
	sort.Strings(periods)
	if len(periods) > 0 {
		current["period"] = strings.Join(periods, "+")
	} else {
		current["period"] = nil
	}
	return current
}

// ── 查询主流程 ──────────────────────────────────────────────────────────────

// fetchQuotaOnce 拉取官方客户端使用的套餐与模型余额，写回账号状态并持久化。
// 返回结构与 Python 版一致：成功 {"balance": payload}；失败 {"error": ...}。
func (s *Service) fetchQuotaOnce(acc *model.Account) map[string]any {
	// 调用方传入的对象可能是 Select/ListAccounts 的返回值，与 Store 内部对象
	// 共享或在锁外被并发读写；这里改用它自己的一份深拷贝做只读计算，
	// 状态写入一律经 Store.Update 在锁内完成。
	snap := s.Store.Find(acc.Provider, acc.ID)
	if snap == nil {
		return map[string]any{"error": "账号已不存在"}
	}
	acc = snap

	query := url.Values{}
	query.Set("app_version", config.ZcodeClientVersion)
	query.Set("platform", config.ZcodeClientPlatform)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		strings.TrimRight(config.ZcodeBillingBase, "/")+"/billing/balance?"+query.Encode(), nil)
	if err == nil {
		for k, v := range authHeaders(acc) {
			req.Header.Set(k, v)
		}
		var resp *http.Response
		resp, err = s.clientFor(acc).Do(req)
		if err == nil {
			return s.handleBillingResponse(acc, resp)
		}
	}
	msg := "额度查询网络错误: " + err.Error()
	s.setLastError(acc, msg)
	return map[string]any{"error": msg}
}

// setLastError 在锁内设置账号错误信息（顺带刷新 LastCheckedAt）。
func (s *Service) setLastError(acc *model.Account, msg string) {
	checkedAt := float64(s.now().UnixNano()) / 1e9
	s.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.LastCheckedAt = &checkedAt
		a.LastError = &msg
	})
}

// clientFor 返回账号出站客户端；配置了代理时走代理传输（20s 超时，短请求）。
// 代理无效时回退直连并记日志（对齐 claim 包的同名行为）。
func (s *Service) clientFor(acc *model.Account) HTTPClient {
	if s.Client != nil {
		return s.Client
	}
	if acc != nil && acc.ProxyURL != nil && *acc.ProxyURL != "" {
		client, err := proxy.ClientFor(*acc.ProxyURL, 20*time.Second)
		if err == nil {
			return client
		}
		web.Warn("quota", fmt.Sprintf("账号 %s 代理无效，回退直连: %v", acc.Name, err))
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// handleBillingResponse 处理计费端点响应：错误分类、快照解析与状态回写。
func (s *Service) handleBillingResponse(acc *model.Account, resp *http.Response) map[string]any {
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		msg := fmt.Sprintf("鉴权失败 HTTP %d", resp.StatusCode)
		s.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
			a.Status = model.StatusInvalid
			a.LastError = &msg
		})
		return map[string]any{"error": msg}
	}
	if resp.StatusCode != http.StatusOK {
		// 上游对重复查询返回 405：已有快照时视为幂等成功（清错误、不重建状态）
		if resp.StatusCode == http.StatusMethodNotAllowed && len(acc.Quota) > 0 {
			s.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
				a.LastError = nil
			})
			return map[string]any{"cached": true, "reason": "上游额度接口拒绝了重复查询（HTTP 405）"}
		}
		msg := fmt.Sprintf("额度查询失败 HTTP %d", resp.StatusCode)
		s.setLastError(acc, msg)
		return map[string]any{"error": msg}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		msg := "额度查询网络错误: " + err.Error()
		s.setLastError(acc, msg)
		return map[string]any{"error": msg}
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		msg := "额度查询返回了无效 JSON"
		s.setLastError(acc, msg)
		return map[string]any{"error": msg}
	}
	if code := payload["code"]; code != nil && !isZeroNumber(code) {
		msg, _ := payload["msg"].(string)
		if msg == "" {
			msg = fmt.Sprintf("额度查询失败 code=%s", fmt.Sprint(code))
		}
		msg = strings.TrimSpace(msg)
		s.setLastError(acc, msg)
		return map[string]any{"balance": payload, "error": msg}
	}

	// 对齐 payload.get("data") or {}：缺失或类型异常时按空快照处理
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}

	rawPlans, _ := data["plans"].([]any)
	plans := []map[string]any{}
	for _, item := range rawPlans {
		if p, ok := item.(map[string]any); ok {
			plans = append(plans, p)
		}
	}
	// 下面全部是「响应 → 新快照」的纯计算，在 acc（深拷贝）上进行；
	// 真正落库统一放到末尾的 Store.Update 中，避免用陈旧快照覆盖并发状态变更。
	acc.Plans = plans

	// balance 仅提供当期数值；周期与所属方案需由 entitlement 对应回来
	entitlements := map[any]map[string]any{}
	entitlementPlans := map[any]map[string]any{}
	for _, plan := range acc.Plans {
		rawEnts, _ := plan["entitlements"].([]any)
		for _, raw := range rawEnts {
			ent, ok := raw.(map[string]any)
			if !ok || !truthyAny(ent["entitlement_id"]) {
				continue
			}
			entitlements[ent["entitlement_id"]] = ent
			entitlementPlans[ent["entitlement_id"]] = plan
		}
	}

	// 同名模型在多个订阅各自独立一列（每日刷新的体验套餐与限时活动套餐不得混合）；
	// 仅同一订阅内的重复项目才合并加总。
	multiPlan := len(acc.Plans) > 1
	rawBalances, _ := data["balances"].([]any)
	quotaMap := map[string]map[string]any{}
	for _, raw := range rawBalances {
		balance, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := firstNonEmptyString(balance["show_name"], balance["model"], "model")
		entID := balance["entitlement_id"]
		ent := entitlements[entID]
		plan := entitlementPlans[entID]
		planName, planIsTrial := "", false
		if multiPlan && len(plan) > 0 {
			planName = model.PlanText(plan)
			planIsTrial = model.IsTrialPlan(plan)
		}
		key := name
		if planName != "" {
			key = name + " · " + planName
		}
		entry := map[string]any{
			"total":         balance["total_units"],
			"used":          balance["used_units"],
			"remaining":     balance["remaining_units"],
			"available":     balance["available_units"],
			"period":        mapValueOrNil(ent, "period"),
			"period_start":  balance["period_start"],
			"period_end":    balance["period_end"],
			"expires_at":    balance["expires_at"],
			"model":         name,
			"plan_name":     planName,
			"plan_is_trial": planIsTrial,
		}
		quotaMap[key] = mergeQuotaEntry(quotaMap[key], entry)
	}

	if len(quotaMap) == 0 {
		msg := "账号未返回可用套餐额度"
		s.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
			a.Quota = map[string]map[string]any{}
			a.LastError = &msg
		})
		return map[string]any{"balance": payload, "error": msg}
	}

	// 任一列 remaining 缺失时跳过该列；全部已列 remaining <= 0 才判耗尽
	var remainings []float64
	for _, q := range quotaMap {
		if q["remaining"] == nil {
			continue
		}
		if v, ok := asNumber(q["remaining"]); ok {
			remainings = append(remainings, v)
		}
	}
	allEmpty := len(remainings) > 0
	for _, v := range remainings {
		if v > 0 {
			allEmpty = false
			break
		}
	}

	now := float64(s.now().UnixNano()) / 1e9
	s.Store.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.Plans = plans
		if len(plans) > 0 {
			a.Plan = plans[0]
		} else {
			a.Plan = map[string]any{}
		}
		a.Quota = quotaMap
		a.SyncExhaustedModels()

		if allEmpty {
			a.Status = model.StatusExhausted
			msg := "額度已用完"
			a.LastError = &msg
			return
		}
		// 状态机读的是锁内当前值：并发请求可能刚把账号标成 invalid/cooling，
		// 用陈旧快照判断会把它们错误地刷回 active。
		switch a.Status {
		case model.StatusExhausted, model.StatusInvalid:
			a.Status = model.StatusActive
			a.CoolingUntil = nil
		case model.StatusCooling:
			if a.CoolingUntil != nil && *a.CoolingUntil <= now {
				a.Status = model.StatusActive
				a.CoolingUntil = nil
			}
		}
		if a.Status != model.StatusCooling {
			a.LastError = nil
		}
	})
	return map[string]any{"balance": payload}
}

// FetchQuota 合并同账号并发查询，并短暂复用成功结果以避免触发上游限流。
// 返回 {"balance": ...} 或 {"error": ...}；缓存命中附带 cached=true。
// 查询在独立 goroutine 中执行并与调用方解耦（对齐 Python create_task +
// shield：调用方放弃不影响查询落库），重复调用共享同一进行中的查询。
func (s *Service) FetchQuota(acc *model.Account) map[string]any {
	s.mu.Lock()
	if call, ok := s.inflight[acc.ID]; ok {
		s.mu.Unlock()
		<-call.done
		return call.result
	}
	if entry, ok := s.cache[acc.ID]; ok {
		if s.now().Sub(entry.at) < QuotaCacheTTL {
			s.mu.Unlock()
			out := maps.Clone(entry.result) // 对齐 {**result, "cached": True}
			out["cached"] = true
			return out
		}
		delete(s.cache, acc.ID)
	}
	call := &inflightCall{done: make(chan struct{})}
	s.inflight[acc.ID] = call
	s.mu.Unlock()

	go func() {
		result := s.fetchQuotaOnce(acc)
		if _, hasErr := result["error"]; !hasErr {
			s.mu.Lock()
			s.cache[acc.ID] = cacheEntry{at: s.now(), result: result}
			s.mu.Unlock()
		}
		// 仅当仍是本查询时才清理（对齐 done_callback 的身份校验）
		s.mu.Lock()
		if cur, ok := s.inflight[acc.ID]; ok && cur == call {
			delete(s.inflight, acc.ID)
		}
		s.mu.Unlock()
		call.result = result
		close(call.done)
	}()
	<-call.done
	return call.result
}

// pruneCache 清理已删除账号的额度缓存，避免缓存无界增长（对齐 _prune_quota_cache）。
func (s *Service) pruneCache() {
	live := map[string]bool{}
	for _, a := range s.Store.ListAccounts("") {
		live[a.ID] = true
	}
	s.mu.Lock()
	for key := range s.cache {
		if !live[key] {
			delete(s.cache, key)
		}
	}
	s.mu.Unlock()
}

// RefreshAccounts 并发刷新一批账号（信号量并发 8，对齐 refresh_accounts），返回汇总。
func (s *Service) RefreshAccounts(accounts []*model.Account) map[string]any {
	s.pruneCache()
	if len(accounts) == 0 {
		return map[string]any{"ok": 0, "fail": 0}
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount := 0
	for _, acc := range accounts {
		wg.Add(1)
		go func(acc *model.Account) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := s.FetchQuota(acc)
			if _, hasErr := res["error"]; !hasErr {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}(acc)
	}
	wg.Wait()
	return map[string]any{"ok": okCount, "fail": len(accounts) - okCount}
}

// ── 后台监控 ────────────────────────────────────────────────────────────────

// Monitor 后台周期性刷新可管理账号的额度，实现实时用量监控。
type Monitor struct {
	svc   *Service
	stop  chan struct{}
	done  chan struct{}
	start sync.Once
}

// NewMonitor 创建后台监控（调用 Start 启动）。
func (s *Service) NewMonitor() *Monitor {
	return &Monitor{svc: s, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start 启动后台循环；重复调用无操作。
func (m *Monitor) Start() {
	m.start.Do(func() { go m.loop() })
}

// loop 对齐 QuotaMonitor._loop：启动先等 5s 避让服务启动；刷新间隔实时读取
// 设置、改后即生效；间隔<=0 视为关闭，但仍每 30s 回看设置便于随时启用。
func (m *Monitor) loop() {
	defer close(m.done)
	select {
	case <-m.stop:
		return
	case <-time.After(5 * time.Second):
	}
	for {
		interval := m.svc.Store.QuotaRefreshInterval()
		if interval > 0 {
			var targets []*model.Account
			for _, a := range m.svc.Store.ListAccounts(model.ProviderZai) {
				if a.ArchivedAt != nil {
					continue // 已归档账号不再刷新额度
				}
				if a.Mode == "jwt" && a.Status != model.StatusDisabled {
					targets = append(targets, a)
				}
			}
			if len(targets) > 0 {
				m.svc.RefreshAccounts(targets)
			}
		}
		wait := interval
		if wait <= 0 {
			wait = 30
		}
		select {
		case <-m.stop:
			return
		case <-time.After(time.Duration(wait) * time.Second):
		}
	}
}

// Stop 停止后台循环并等待 goroutine 退出（对齐 monitor.stop 的 gather 语义）。
func (m *Monitor) Stop() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

func isZeroNumber(v any) bool {
	n, ok := asNumber(v)
	return ok && n == 0
}

// truthyAny 对齐 Python 的真值判定（entitlement_id 缺失 / 空串 / 0 均视为无效）。
func truthyAny(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case float64:
		return x != 0
	case int:
		return x != 0
	}
	return true
}

// firstNonEmptyString 依次取第一个非空字符串项（对齐 or 链），全空时取末位兜底。
func firstNonEmptyString(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// mapValueOrNil 从映射取值，映射缺失时返回 nil（对齐 dict.get(key)）。
func mapValueOrNil(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}
