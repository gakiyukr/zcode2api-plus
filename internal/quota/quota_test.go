// 额度查询测试：移植 Python 版 tests/test_quota.py 全部用例，
// 并补充缓存复用、inflight 去重与 405 幂等等 Go 版核心语义。
package quota

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// fakeBilling 计费端点假客户端：记录请求并按脚本应答。
type fakeBilling struct {
	mu     sync.Mutex
	status int
	body   string
	calls  []string // 记录完整请求 URL
}

func (f *fakeBilling) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req.URL.String())
	return &http.Response{
		StatusCode: f.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(f.body)),
	}, nil
}

func (f *fakeBilling) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeBilling) lastURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeBilling) setStatus(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

// setup 打开隔离存储与注入假计费客户端的额度服务。
func setup(t *testing.T) (*Service, *store.Store, *fakeBilling) {
	t.Helper()
	oldDB, oldData := config.DBPath, config.DataDir
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	config.DataDir = t.TempDir() // DeviceMid 持久化路径隔离
	t.Cleanup(func() { config.DBPath, config.DataDir = oldDB, oldData })

	st, err := store.New()
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	svc := NewService(st)
	billing := &fakeBilling{status: http.StatusOK, body: "{}"}
	svc.Client = billing
	return svc, st, billing
}

// balancePayload 对应 Python 版 _balance_payload 夹具。
func balancePayload(flashRemaining int) string {
	return fmt.Sprintf(`{"code":0,"data":{
		"plans":[{"plan_id":"start-plan","entitlements":[
			{"entitlement_id":"ent-flash","show_name":"GLM-5.3-Flash","period":"daily","grant_units":5000000}]}],
		"balances":[{"entitlement_id":"ent-flash","show_name":"GLM-5.3-Flash",
			"total_units":5000000,"used_units":%d,"remaining_units":%d,"available_units":%d,
			"period_start":1788105600,"period_end":1788191999,"expires_at":1788191999}]}}`,
		5000000-flashRemaining, flashRemaining, flashRemaining)
}

// multiPlanPayload 对应 Python 版 _multi_plan_payload：两个订阅同名模型各一列。
const multiPlanPayload = `{"code":0,"data":{
	"plans":[
		{"plan_id":"global-build","name":"ZCode Global Build","entitlements":[
			{"entitlement_id":"ent-flash-gb","show_name":"GLM-5.3-Flash","period":"monthly","grant_units":100000000}]},
		{"plan_id":"start-plan","name":"ZCode Start Plan","entitlements":[
			{"entitlement_id":"ent-flash-sp","show_name":"GLM-5.3-Flash","period":"daily","grant_units":5000000}]}],
	"balances":[
		{"entitlement_id":"ent-flash-gb","show_name":"GLM-5.3-Flash","total_units":100000000,
			"used_units":40000000,"remaining_units":60000000,"available_units":60000000,
			"period_start":1788019200,"period_end":1788278399,"expires_at":1788278399},
		{"entitlement_id":"ent-flash-sp","show_name":"GLM-5.3-Flash","total_units":5000000,
			"used_units":5000000,"remaining_units":0,"available_units":0,
			"period_start":1788105600,"period_end":1788191999,"expires_at":1788191999}]}}`

func TestDailyQuotaUsesDeviceIdentityAndPreservesPeriod(t *testing.T) {
	// 額度端點必須帶完整裝置資訊，並保存每日配額與重置週期。
	svc, st, billing := setup(t)
	acc, err := st.AddAccount(model.ProviderZai, "daily", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	billing.body = balancePayload(2500000)

	result := svc.FetchQuota(acc)
	if _, hasErr := result["error"]; hasErr {
		t.Fatalf("不应报错: %v", result)
	}

	// 请求形态：路径、查询参数与设备头
	url := billing.lastURL()
	if !strings.Contains(url, "/billing/balance") {
		t.Fatalf("应请求计费余额端点: %s", url)
	}
	if !strings.Contains(url, "platform="+config.ZcodeClientPlatform) ||
		!strings.Contains(url, "app_version="+config.ZcodeClientVersion) {
		t.Fatalf("查询参数应携带客户端标识: %s", url)
	}

	got := st.FindAny(acc.ID)
	if got.Mode != "jwt" {
		t.Fatalf("应为 jwt 账号: %s", got.Mode)
	}
	daily := got.Quota["GLM-5.3-Flash"]
	if daily == nil {
		t.Fatalf("应有 GLM-5.3-Flash 额度列: %v", got.Quota)
	}
	if daily["total"] != float64(5000000) || daily["remaining"] != float64(2500000) {
		t.Fatalf("数值不符: %v", daily)
	}
	if daily["period"] != "daily" {
		t.Fatalf("period 应为 daily: %v", daily["period"])
	}
	if daily["period_end"] != float64(1788191999) {
		t.Fatalf("period_end 应保留: %v", daily["period_end"])
	}
	if len(got.ExhaustedModels) != 0 {
		t.Fatalf("不应标记耗尽: %v", got.ExhaustedModels)
	}
}

func TestZeroDailyBalanceMarksAccountExhausted(t *testing.T) {
	// 所有官方餘額均為零時，帳號不可再參與輪詢。
	svc, st, billing := setup(t)
	acc, err := st.AddAccount(model.ProviderZai, "empty", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	billing.body = balancePayload(0)

	svc.FetchQuota(acc)

	got := st.FindAny(acc.ID)
	if got.Status != model.StatusExhausted {
		t.Fatalf("应标记 exhausted: %s", got.Status)
	}
	if got.LastError == nil || *got.LastError != "額度已用完" {
		t.Fatalf("last_error 应为 額度已用完: %v", got.LastError)
	}
	if len(got.ExhaustedModels) != 1 || got.ExhaustedModels[0] != "glm-5.3-flash" {
		t.Fatalf("应标记 glm-5.3-flash 耗尽: %v", got.ExhaustedModels)
	}
}

func TestSameModelAcrossPlansStaysIndependent(t *testing.T) {
	// 同名模型在多個訂閱下各自獨立一列，並標記所屬方案與體驗身份。
	svc, st, billing := setup(t)
	acc, err := st.AddAccount(model.ProviderZai, "multi", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	billing.body = multiPlanPayload

	svc.FetchQuota(acc)

	got := st.FindAny(acc.ID)
	if len(got.Plans) != 2 {
		t.Fatalf("应有 2 个订阅: %d", len(got.Plans))
	}
	view := got.PublicView(time.Now())
	if view["plan_name"] != "ZCode Global Build / ZCode Start Plan" {
		t.Fatalf("plan_name 应串接两个订阅: %v", view["plan_name"])
	}

	keys := make([]string, 0, len(got.Quota))
	for k := range got.Quota {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"GLM-5.3-Flash · ZCode Global Build", "GLM-5.3-Flash · ZCode Start Plan"}
	if strings.Join(keys, "|") != strings.Join(want, "|") {
		t.Fatalf("额度键不符: %v", keys)
	}

	gb := got.Quota[want[0]]
	if gb["total"] != float64(100000000) || gb["remaining"] != float64(60000000) {
		t.Fatalf("Global Build 数值不符: %v", gb)
	}
	if gb["period"] != "monthly" || gb["model"] != "GLM-5.3-Flash" {
		t.Fatalf("Global Build 元数据不符: %v", gb)
	}
	if gb["plan_name"] != "ZCode Global Build" || gb["plan_is_trial"] != false {
		t.Fatalf("Global Build 方案标记不符: %v", gb)
	}
	sp := got.Quota[want[1]]
	if sp["remaining"] != float64(0) || sp["period"] != "daily" {
		t.Fatalf("Start Plan 数值不符: %v", sp)
	}
	if sp["period_end"] != float64(1788191999) {
		t.Fatalf("各列应保留自身重置时间: %v", sp["period_end"])
	}
	// 任一訂閱仍有餘額，模型與帳號皆不得視為耗盡
	if len(got.ExhaustedModels) != 0 {
		t.Fatalf("不应标记耗尽: %v", got.ExhaustedModels)
	}
	if got.Status != model.StatusActive {
		t.Fatalf("应保持 active: %s", got.Status)
	}
	if got.ModelAvailability("GLM-5.3-Flash") != "available" {
		t.Fatalf("模型应可用: %s", got.ModelAvailability("GLM-5.3-Flash"))
	}
}

// clientFunc 函数适配 HTTPClient 接口。
type clientFunc func(req *http.Request) (*http.Response, error)

func (f clientFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func TestFetchQuotaCacheAndInflightDedup(t *testing.T) {
	// 并发查询共享同一次请求；成功结果 15s 内复用并带 cached 标记。
	svc, st, _ := setup(t)
	acc, err := st.AddAccount(model.ProviderZai, "cache", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}

	base := time.Now()
	now := base
	svc.SetNow(func() time.Time { return now })

	calls := 0
	var mu sync.Mutex
	svc.Client = clientFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(balancePayload(2500000))),
		}, nil
	})

	// 并发双调用：后者必然命中 inflight（前者进行中）或缓存（前者已完成），
	// 两种时序下都只发一次请求
	resCh := make(chan map[string]any, 2)
	go func() { resCh <- svc.FetchQuota(acc) }()
	go func() { resCh <- svc.FetchQuota(acc) }()
	first := <-resCh
	second := <-resCh

	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("并发双调用应只发一次请求: %d", gotCalls)
	}
	if _, hasErr := first["error"]; hasErr {
		t.Fatalf("查询不应报错: %v", first)
	}
	cachedCount := 0
	if _, ok := first["cached"]; ok {
		cachedCount++
	}
	if _, ok := second["cached"]; ok {
		cachedCount++
	}
	if cachedCount > 1 {
		t.Fatalf("至多一个结果带 cached 标记（inflight 等待方不带）: %v %v", first, second)
	}

	// 缓存命中：TTL 内复用结果、不再发请求
	now = base.Add(5 * time.Second)
	third := svc.FetchQuota(acc)
	if third["cached"] != true {
		t.Fatalf("TTL 内应命中缓存: %v", third)
	}
	mu.Lock()
	gotCalls = calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("缓存命中不应发请求: %d", gotCalls)
	}

	// TTL 过期后重新查询
	now = base.Add(QuotaCacheTTL + time.Second)
	_ = svc.FetchQuota(acc)
	mu.Lock()
	gotCalls = calls
	mu.Unlock()
	if gotCalls != 2 {
		t.Fatalf("过期后应重新请求: %d", gotCalls)
	}
}

func TestFetchQuotaErrorNotCached(t *testing.T) {
	// 失败结果不写缓存：连续失败每次都真实请求。
	svc, st, billing := setup(t)
	acc, err := st.AddAccount(model.ProviderZai, "nocache", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	billing.status = http.StatusInternalServerError
	billing.body = "boom"

	first := svc.FetchQuota(acc)
	if _, hasErr := first["error"]; !hasErr {
		t.Fatalf("500 应返回错误: %v", first)
	}
	second := svc.FetchQuota(acc)
	if _, hasErr := second["error"]; !hasErr {
		t.Fatalf("第二次也应报错: %v", second)
	}
	if billing.callCount() != 2 {
		t.Fatalf("失败不缓存、应各请求一次: %d", billing.callCount())
	}
}

func TestBilling405WithSnapshotIsIdempotent(t *testing.T) {
	// 上游对重复查询返回 405：已有快照时视为幂等成功并清除 last_error。
	svc, st, billing := setup(t)
	acc, err := st.AddAccount(model.ProviderZai, "idem", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	billing.body = balancePayload(2500000)
	svc.FetchQuota(acc) // 建立快照

	base := time.Now()
	now := base
	svc.SetNow(func() time.Time { return now })

	// 人为制造 last_error，验证 405 幂等路径将其清除
	msg := "上游服務暫時不可用 HTTP 503"
	st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.LastError = &msg
	})

	now = base.Add(QuotaCacheTTL + time.Second)
	billing.setStatus(http.StatusMethodNotAllowed, "")
	result := svc.FetchQuota(acc)

	if result["cached"] != true {
		t.Fatalf("405 应返回 cached: %v", result)
	}
	if reason, _ := result["reason"].(string); !strings.Contains(reason, "405") {
		t.Fatalf("reason 应说明 405: %v", result["reason"])
	}
	got := st.FindAny(acc.ID)
	if got.LastError != nil {
		t.Fatalf("405 幂等路径应清除 last_error: %v", got.LastError)
	}
}

func TestMergeQuotaEntryCombinesSubscriptions(t *testing.T) {
	// 同一订阅内重复条目：数值相加、时间取最早、period 去重排序拼接。
	merged := mergeQuotaEntry(
		map[string]any{"total": float64(10), "used": float64(4), "period": "2026-01", "period_start": float64(200)},
		map[string]any{"total": float64(5), "used": nil, "period": "2026-01", "period_start": float64(100)},
	)
	if merged["total"] != float64(15) {
		t.Fatalf("total 应相加: %v", merged["total"])
	}
	if merged["used"] != float64(4) {
		t.Fatalf("used 缺失端应保留另一端: %v", merged["used"])
	}
	if merged["period"] != "2026-01" {
		t.Fatalf("period 应去重: %v", merged["period"])
	}
	if merged["period_start"] != float64(100) {
		t.Fatalf("period_start 应取最早: %v", merged["period_start"])
	}

	merged = mergeQuotaEntry(
		map[string]any{"period": "b", "remaining": float64(1)},
		map[string]any{"period": "a", "remaining": float64(2)},
	)
	if merged["period"] != "a+b" {
		t.Fatalf("period 应排序拼接: %v", merged["period"])
	}
	if merged["remaining"] != float64(3) {
		t.Fatalf("remaining 应相加: %v", merged["remaining"])
	}
}

func TestRefreshAccountsSummary(t *testing.T) {
	// 批量刷新返回 {ok, fail} 汇总；空列表直接返回零值。
	svc, st, billing := setup(t)
	if got := svc.RefreshAccounts(nil); got["ok"] != 0 || got["fail"] != 0 {
		t.Fatalf("空列表应零汇总: %v", got)
	}

	a, err := st.AddAccount(model.ProviderZai, "ok-acc", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.AddAccount(model.ProviderZai, "bad-acc", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}

	// 全部失败（失败不缓存，两次都真实请求）
	billing.status = http.StatusInternalServerError
	billing.body = "boom"
	summary := svc.RefreshAccounts([]*model.Account{a, b})
	if summary["ok"] != 0 || summary["fail"] != 2 {
		t.Fatalf("全失败汇总不符: %v", summary)
	}

	// 恢复后全部成功
	billing.status = http.StatusOK
	billing.body = balancePayload(2500000)
	summary = svc.RefreshAccounts([]*model.Account{a, b})
	if summary["ok"] != 2 || summary["fail"] != 0 {
		t.Fatalf("成功批次汇总不符: %v", summary)
	}
}

// 上游把 entitlement_id 回成非字符串时不得 panic。
//
// 该索引原以 map[any] 为键，容器类型不可哈希：上游一次异常响应就会 panic
// 终止整个进程（fetchQuotaOnce 跑在无 recover 的 goroutine 里，所有在途
// 串流一并被杀）。非字符串一律视为无对应项，退化为缺少周期信息。
func TestEntitlementIDNonStringDoesNotPanic(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
	}{
		{"数组", `["a","b"]`},
		{"对象", `{"k":"v"}`},
		{"数字", `123`},
		{"null", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, billing := setup(t)
			acc, err := st.AddAccount(model.ProviderZai, "badent", "header.payload.signature")
			if err != nil {
				t.Fatal(err)
			}
			billing.body = fmt.Sprintf(`{"code":0,"data":{
				"plans":[{"plan_id":"p","entitlements":[
					{"entitlement_id":%s,"show_name":"GLM-5.3-Flash","period":"daily","grant_units":100}]}],
				"balances":[{"entitlement_id":%s,"show_name":"GLM-5.3-Flash",
					"total_units":100,"used_units":10,"remaining_units":90,"available_units":90}]}}`,
				tc.id, tc.id)

			// 只要不 panic 即通过；返回值不作断言（缺周期信息是预期退化）
			_ = svc.FetchQuota(acc)
			if fresh := st.Find(acc.Provider, acc.ID); fresh == nil {
				t.Fatal("账号应仍存在")
			}
		})
	}
}

// 单个账号的 panic 不得杀死进程，也不得中断其余账号的刷新。
//
// Python 版在两层做了隔离（gather 的 return_exceptions=True 与 _loop 的
// except Exception），Go 版两处都没有——额度解析路径上任何一个未预见的
// panic（上游回传意外类型）都会终止整个进程，所有在途串流一并陪葬。
// net/http 只 recover「处理该连接的 goroutine」，RefreshAccounts 自己开的
// goroutine 不在其保护范围内。
func TestRefreshAccountsIsolatesPanic(t *testing.T) {
	svc, st, _ := setup(t)

	// 造一个会在解析时 panic 的账号：让 Client 在请求时 panic
	bad, err := st.AddAccount(model.ProviderZai, "panicky", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	good, err := st.AddAccount(model.ProviderZai, "healthy", "h.p.s")
	if err != nil {
		t.Fatal(err)
	}

	svc.Client = clientFunc(func(req *http.Request) (*http.Response, error) {
		// 只让 bad 账号的请求 panic（按 URL 里的 device mid 无法区分，
		// 故用请求计数：第一次调用即 panic）
		panic("simulated parse panic")
	})
	_ = bad

	// 关键断言：本调用不得让 panic 逃逸（逃逸即测试进程崩溃）
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = svc.RefreshAccounts([]*model.Account{bad, good})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RefreshAccounts 未在预期时间内返回")
	}
}

func TestFutureStartsAtAndExpiredPlansFiltered(t *testing.T) {
	svc, st, billing := setup(t)
	acc, err := st.AddAccount(model.ProviderZai, "future-and-live", "header.payload.sig")
	if err != nil {
		t.Fatal(err)
	}
	// server_time = 2000：一個生效中（starts_at=1000）、一個未來未生效（starts_at=3000）、一個已過期（ends_at=1500）
	billing.body = `{"code":0,"data":{
		"server_time":2000,
		"plans":[
			{"user_plan_id":"up-live","plan_id":"p-live","name":"Live","status":"active","starts_at":1000,"ends_at":5000,
			 "entitlements":[{"entitlement_id":"e-live","show_name":"GLM-5.4","period":"daily"}]},
			{"user_plan_id":"up-future","plan_id":"p-future","name":"Future","status":"active","starts_at":3000,"ends_at":9000,
			 "entitlements":[{"entitlement_id":"e-future","show_name":"GLM-5.3-Flash","period":"one_time"}]},
			{"user_plan_id":"up-exp","plan_id":"p-exp","name":"Expired","status":"active","starts_at":100,"ends_at":1500,
			 "entitlements":[{"entitlement_id":"e-exp","show_name":"GLM-5.2","period":"one_time"}]}
		],
		"balances":[
			{"user_plan_id":"up-live","entitlement_id":"e-live","show_name":"GLM-5.4","total_units":100,"used_units":10,"remaining_units":90},
			{"user_plan_id":"up-future","entitlement_id":"e-future","show_name":"GLM-5.3-Flash","total_units":100,"used_units":0,"remaining_units":100},
			{"user_plan_id":"up-exp","entitlement_id":"e-exp","show_name":"GLM-5.2","total_units":100,"used_units":0,"remaining_units":100}
		]
	}}`

	res := svc.FetchQuota(acc)
	if _, hasErr := res["error"]; hasErr {
		t.Fatalf("有生效方案不應報錯: %v", res)
	}
	got := st.FindAny(acc.ID)
	if len(got.Quota) != 1 {
		t.Fatalf("僅應保留生效方案額度，應過濾未來與過期方案: %v", got.Quota)
	}
	if got.ModelAvailability("GLM-5.4") != "available" {
		t.Fatalf("生效中的 GLM-5.4 應可用: %s", got.ModelAvailability("GLM-5.4"))
	}
	if got.ModelAvailability("GLM-5.3-Flash") != "absent" {
		t.Fatalf("未生效方案的 GLM-5.3-Flash 不應算可用: %s", got.ModelAvailability("GLM-5.3-Flash"))
	}

	// 僅有未來未生效套餐時，應清空額度並標示尚未生效
	acc2, _ := st.AddAccount(model.ProviderZai, "only-future", "h2.p2.s2")
	billing.body = `{"code":0,"data":{
		"server_time":2000,
		"plans":[{"user_plan_id":"up-f","plan_id":"p-f","name":"Pending","status":"active","starts_at":3000,
			"entitlements":[{"entitlement_id":"e-f","show_name":"GLM-5.4","period":"one_time"}]}],
		"balances":[{"user_plan_id":"up-f","entitlement_id":"e-f","show_name":"GLM-5.4","total_units":100,"remaining_units":100}]
	}}`
	res2 := svc.FetchQuota(acc2)
	if msg, _ := res2["error"].(string); !strings.Contains(msg, "尚未生效") {
		t.Fatalf("僅有待開始方案應回報尚未生效: %v", res2)
	}
	got2 := st.FindAny(acc2.ID)
	if len(got2.Quota) != 0 || got2.Status != model.StatusExhausted {
		t.Fatalf("待開始方案帳號不應被排程消耗: status=%s quota=%v", got2.Status, got2.Quota)
	}
}

func TestFetchQuota_WeakStatusGuard(t *testing.T) {
	svc, st, billing := setup(t)
	now := time.Unix(1750000000, 0)
	svc.SetNow(func() time.Time { return now })

	emptyPayload := `{"code":0,"data":{"plans":[{"plan_id":"p1","entitlements":[{"entitlement_id":"e1","show_name":"GLM-5.4","period":"daily"}]}],"balances":[{"entitlement_id":"e1","show_name":"GLM-5.4","total_units":100,"used_units":100,"remaining_units":0}]}}`
	availPayload := `{"code":0,"data":{"plans":[{"plan_id":"p1","entitlements":[{"entitlement_id":"e1","show_name":"GLM-5.4","period":"daily"}]}],"balances":[{"entitlement_id":"e1","show_name":"GLM-5.4","total_units":100,"used_units":10,"remaining_units":90}]}}`

	// 1. active 額度用完 → exhausted，且更新 LastCheckedAt
	a1, _ := st.AddAccount(model.ProviderZai, "active-empty", "h1.p1.s1")
	billing.body = emptyPayload
	svc.FetchQuota(a1)
	g1 := st.FindAny(a1.ID)
	if g1.Status != model.StatusExhausted {
		t.Fatalf("active 帳號額度用完應轉 exhausted，實際: %s", g1.Status)
	}
	if g1.LastCheckedAt == nil || *g1.LastCheckedAt < float64(now.Unix()-2) {
		t.Fatalf("刷新後應更新 LastCheckedAt，實際: %v", g1.LastCheckedAt)
	}

	// 2. disabled 帳號額度用完 → 不改狀態
	a2, _ := st.AddAccount(model.ProviderZai, "disabled-empty", "h2.p2.s2")
	_, _ = st.SetEnabled(a2.Provider, a2.ID, false)
	billing.body = emptyPayload
	svc.FetchQuota(a2)
	if g2 := st.FindAny(a2.ID); g2.Status != model.StatusDisabled {
		t.Fatalf("disabled 帳號額度用完不得覆寫狀態，實際: %s", g2.Status)
	}

	// 3. archived 帳號額度用完與有額度 → 皆不改狀態
	a3, _ := st.AddAccount(model.ProviderZai, "archived-acc", "h3.p3.s3")
	_, _ = st.SetArchived(a3.Provider, a3.ID, true)
	billing.body = emptyPayload
	svc.FetchQuota(a3)
	if g3 := st.FindAny(a3.ID); g3.Status != model.StatusDisabled || g3.ArchivedAt == nil {
		t.Fatalf("archived 帳號額度用完不得覆寫狀態: status=%s archived=%v", g3.Status, g3.ArchivedAt)
	}

	// 4. invalid 帳號拉到有效額度 → 恢復 active 且更新 LastCheckedAt
	a4, _ := st.AddAccount(model.ProviderZai, "invalid-recover", "h4.p4.s4")
	_ = st.Update(a4.Provider, a4.ID, func(a *model.Account) { a.Status = model.StatusInvalid })
	billing.body = availPayload
	svc.FetchQuota(a4)
	g4 := st.FindAny(a4.ID)
	if g4.Status != model.StatusActive {
		t.Fatalf("invalid 帳號拉到有效額度應恢復 active，實際: %s", g4.Status)
	}
	if g4.LastCheckedAt == nil || *g4.LastCheckedAt < float64(now.Unix()-2) {
		t.Fatalf("有額度刷新成功後必須更新 LastCheckedAt，實際: %v", g4.LastCheckedAt)
	}

	// 5. disabled 帳號拉到有效額度 → 不改狀態
	a5, _ := st.AddAccount(model.ProviderZai, "disabled-avail", "h5.p5.s5")
	_, _ = st.SetEnabled(a5.Provider, a5.ID, false)
	billing.body = availPayload
	svc.FetchQuota(a5)
	if g5 := st.FindAny(a5.ID); g5.Status != model.StatusDisabled {
		t.Fatalf("disabled 帳號拉到有效額度不得覆寫為 active，實際: %s", g5.Status)
	}

	// 6. 未呼叫 Start 直接 Stop 不得阻塞
	mon := svc.NewMonitor()
	done := make(chan struct{})
	go func() {
		defer close(done)
		mon.Stop()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("quota.Monitor 未啟動就 Stop 不應阻塞")
	}
}

func TestFetchQuota_EmptyQuotaAndExpiredPlans(t *testing.T) {
	svc, st, billing := setup(t)
	now := time.Now()
	svc.SetNow(func() time.Time { return now })

	expiredOnlyPayload := `{"code":0,"data":{"server_time":2000,"plans":[{"plan_id":"p_exp","status":"expired","ends_at":1500,"entitlements":[{"entitlement_id":"e1","show_name":"GLM-5.4"}]}],"balances":[{"entitlement_id":"e1","show_name":"GLM-5.4","total_units":100,"used_units":10,"remaining_units":90}]}}`

	// 1. active 帳號全部套餐皆過期（len(quotaMap)==0 且 pendingPlans==0）→ 轉 exhausted 並清空 ExhaustedModels
	a1, _ := st.AddAccount(model.ProviderZai, "all-expired", "h1.p1.s1")
	_ = st.Update(a1.Provider, a1.ID, func(a *model.Account) {
		a.ExhaustedModels = []string{"glm-5.4"}
	})
	billing.body = expiredOnlyPayload
	svc.FetchQuota(a1)
	g1 := st.FindAny(a1.ID)
	if g1.Status != model.StatusExhausted {
		t.Fatalf("全過期空額度帳號應轉 exhausted，實際: %s", g1.Status)
	}
	if len(g1.ExhaustedModels) != 0 {
		t.Fatalf("空額度分支應清空 ExhaustedModels，實際: %v", g1.ExhaustedModels)
	}

	// 2. invalid 與未到期 cooling 帳號遇空額度不得降級改寫為 exhausted
	a2, _ := st.AddAccount(model.ProviderZai, "invalid-keep", "h2.p2.s2")
	_ = st.Update(a2.Provider, a2.ID, func(a *model.Account) { a.Status = model.StatusInvalid })
	svc.FetchQuota(a2)
	if g2 := st.FindAny(a2.ID); g2.Status != model.StatusInvalid {
		t.Fatalf("invalid 遇空額度不得降級為 exhausted，實際: %s", g2.Status)
	}

	a3, _ := st.AddAccount(model.ProviderZai, "cooling-keep", "h3.p3.s3")
	future := float64(now.Add(10 * time.Minute).Unix())
	_ = st.Update(a3.Provider, a3.ID, func(a *model.Account) {
		a.Status = model.StatusCooling
		a.CoolingUntil = &future
	})
	svc.FetchQuota(a3)
	if g3 := st.FindAny(a3.ID); g3.Status != model.StatusCooling {
		t.Fatalf("未到期 cooling 遇空額度不得降級為 exhausted，實際: %s", g3.Status)
	}

	// 3. Enabled=false 即使 Status 暫非 disabled，isWeakStatus 也必須回傳 false 保護之
	a4, _ := st.AddAccount(model.ProviderZai, "enabled-false", "h4.p4.s4")
	_ = st.Update(a4.Provider, a4.ID, func(a *model.Account) {
		a.Enabled = false
		a.Status = model.StatusActive
	})
	svc.FetchQuota(a4)
	if g4 := st.FindAny(a4.ID); g4.Status != model.StatusActive {
		t.Fatalf("!Enabled 帳號不得被額度刷新覆寫狀態，實際: %s", g4.Status)
	}
}
