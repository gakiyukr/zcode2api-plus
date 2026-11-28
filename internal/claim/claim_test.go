// claim 包单测：对照 Python 主仓 tests/test_claim.py 的用例语义。
// billing 上游用注入的假 HTTPClient 模拟（离线稳定）；遥测端点覆写 EventReportURL。
package claim

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/fingerprint"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// fakeBilling 假计费上游：按注册路径返回响应体；记录每次收包。
type fakeBilling struct {
	responses map[string]func(call upstreamCall) (int, string)
	requests  []upstreamCall
}

type upstreamCall struct {
	Method  string
	Path    string
	Headers http.Header
	Body    []byte
}

func (f *fakeBilling) Do(r *http.Request) (*http.Response, error) {
	call := upstreamCall{Method: r.Method, Path: r.URL.Path, Headers: r.Header.Clone()}
	if r.Body != nil {
		call.Body, _ = io.ReadAll(r.Body)
	}
	f.requests = append(f.requests, call)
	// 先按完整路徑匹配，再回退末段匹配（/billing/preview、/billing/claim）
	if handler, ok := f.responses[call.Path]; ok {
		status, body := handler(call)
		return jsonResponse(status, body)
	}
	handler, ok := f.responses["/billing/"+lastSegment(call.Path)]
	if !ok {
		return jsonResponse(http.StatusNotFound, `{"code":3001}`)
	}
	status, body := handler(call)
	return jsonResponse(status, body)
}

// lastSegment 取路径最后一段。
func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func jsonResponse(status int, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func newTestAccount(t *testing.T) *model.Account {
	t.Helper()
	oldDB, oldData := config.DBPath, config.DataDir
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	config.DataDir = t.TempDir()
	t.Cleanup(func() { config.DBPath, config.DataDir = oldDB, oldData })
	jwt := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"u123"}`)) + ".sig"
	return &model.Account{Provider: model.ProviderZai, Name: "t", Mode: "jwt", JWTToken: &jwt}
}

// stubSolver 验证码假求解器（按序列返回参数；用尽后固定 "ok"）。
type stubSolver struct{ params []string }

func (s *stubSolver) Solve(context.Context, captcha.Config) (string, error) {
	if len(s.params) > 1 {
		p := s.params[0]
		s.params = s.params[1:]
		return p, nil
	}
	if len(s.params) == 1 {
		return s.params[0], nil
	}
	return "ok", nil
}

func (s *stubSolver) Close() error { return nil }

// newSolvedManager 假求解 + 固定配置（离线稳定）的 manager。
func newSolvedManager(t *testing.T, params ...string) *captcha.Manager {
	t.Helper()
	old := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = old })
	cm := captcha.NewManager()
	cm.SetSolver(&stubSolver{params: params})
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, Prefix: "no8xfe", Region: "sgp", SceneID: "11xygtvd"}, nil
	})
	return cm
}

func TestParsePlan(t *testing.T) {
	plan := ParsePlan(map[string]any{
		"plan_id": "p1", "name": "新人套餐", "priority": float64(10),
		"entitlements": []any{
			map[string]any{"meter": "model_usage", "unit_type": "token",
				"show_name": "GLM-5.3", "grant_units": float64(1000000)},
			map[string]any{"meter": "other", "unit_type": "token", "show_name": "忽略"},
			map[string]any{"meter": "model_usage", "unit_type": "request", "show_name": "忽略2"},
		},
	})
	if plan == nil {
		t.Fatal("应解析出套餐")
	}
	if plan["plan_id"] != "p1" || plan["name"] != "新人套餐" {
		t.Fatalf("字段不符: %v", plan)
	}
	grants, _ := plan["grants"].([]any)
	if len(grants) != 1 {
		t.Fatalf("应 1 个 token 授权: %v", grants)
	}
	if ParsePlan(map[string]any{"name": "无 id"}) != nil {
		t.Fatal("缺 plan_id 应返回 nil")
	}
}

func TestClaimSuccessWith3007Retry(t *testing.T) {
	acc := newTestAccount(t)
	up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
		"/billing/preview": func(upstreamCall) (int, string) {
			return 200, `{"code":0,"data":{"plans":[{"plan_id":"p1","name":"A","priority":5}]}}`
		},
		"/billing/claim": func(call upstreamCall) (int, string) {
			if call.Headers.Get("X-Aliyun-Captcha-Verify-Param") == "first" {
				return 200, `{"code":3007,"msg":"captcha"}`
			}
			return 200, `{"code":0,"data":{}}`
		},
	}}
	svc := &Service{Captcha: newSolvedManager(t, "first", "second"), Client: up}

	result, err := svc.Claim(acc, "p1")
	if err != nil {
		t.Fatalf("3007 后换码重试应成功: %v", err)
	}
	if result["plan_id"] != "p1" {
		t.Fatalf("结果不符: %v", result)
	}
	if len(up.requests) != 2 { // 显式 plan_id 无需 preview：2 次 claim
		t.Fatalf("应 2 次上游请求: %d", len(up.requests))
	}
	// 最后一次 claim 应带换新后的验证码与区域头
	claimCall := up.requests[len(up.requests)-1]
	if claimCall.Headers.Get(captchaHeader) != "second" {
		t.Fatalf("换码重试应携带新验证码: %q", claimCall.Headers.Get(captchaHeader))
	}
	if claimCall.Headers.Get(captchaRegionHeader) != "sgp" {
		t.Fatalf("应携带验证码区域头: %q", claimCall.Headers.Get(captchaRegionHeader))
	}
}

func TestClaimBusinessCodes(t *testing.T) {
	cases := []struct {
		code    string
		wantSub string
	}{
		{"1003", "領取過"}, {"1005", "名額已用完"}, {"401", "登入"}, {"9999", "領取失敗"},
	}
	for _, c := range cases {
		t.Run(c.wantSub, func(t *testing.T) {
			acc := newTestAccount(t)
			up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
				"/billing/claim": func(upstreamCall) (int, string) {
					return 200, `{"code":` + c.code + `}`
				},
			}}
			svc := &Service{Captcha: newSolvedManager(t), Client: up}
			_, err := svc.Claim(acc, "p1")
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("code %s 应报 %q: %v", c.code, c.wantSub, err)
			}
		})
	}
}

func TestClaimRejectsAPIKeyAccount(t *testing.T) {
	acc := newTestAccount(t)
	key := "sk-x"
	acc.Mode = "apiKey"
	acc.JWTToken = nil
	acc.APIKey = &key
	svc := &Service{Captcha: newSolvedManager(t), Client: &fakeBilling{}}
	_, err := svc.Claim(acc, "p1")
	if err == nil || !strings.Contains(err.Error(), "JWT") {
		t.Fatalf("API Key 账号应拒绝: %v", err)
	}
}

func TestAutoClaimPriorityOrder(t *testing.T) {
	acc := newTestAccount(t)
	var claimed []string
	up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
		"/billing/preview": func(upstreamCall) (int, string) {
			return 200, `{"code":0,"data":{"plans":[
				{"plan_id":"low","name":"低","priority":1},
				{"plan_id":"high","name":"高","priority":9}]}}`
		},
		"/billing/claim": func(upstreamCall) (int, string) {
			claimed = append(claimed, "ok")
			return 200, `{"code":0}`
		},
	}}
	svc := &Service{Captcha: newSolvedManager(t), Client: up}
	outcomes := svc.AutoClaimAllPlans(acc)
	if len(outcomes) != 2 {
		t.Fatalf("应领取 2 个套餐: %v", outcomes)
	}
	for _, o := range outcomes {
		if ok, _ := o["ok"].(bool); !ok {
			t.Fatalf("全部应成功: %v", outcomes)
		}
	}
	// preview 排序：high 在前
	if len(claimed) != 2 {
		t.Fatalf("应 2 次 claim: %v", claimed)
	}
	first := outcomes[0]["plan_id"]
	if first != "high" {
		t.Fatalf("应先领高优先级套餐: %v", outcomes[0]["plan_id"])
	}
}

func TestActivationReportingTolerated(t *testing.T) {
	acc := newTestAccount(t)
	oldURL, oldDelays := EventReportURL, ActivationRetryDelays
	ActivationRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { EventReportURL, ActivationRetryDelays = oldURL, oldDelays })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	t.Cleanup(srv.Close)
	EventReportURL = srv.URL

	if err := ReportActivationEvents(acc); err != "" {
		t.Fatalf("激活上报应成功: %s", err)
	}

	// 失败上报仅返回文案，不 panic；preview 不被阻断由 AutoClaimAllPlans 覆盖
	EventReportURL = "http://127.0.0.1:1/nope"
	if err := ReportActivationEvents(acc); err == "" {
		t.Fatal("不可达端点应返回错误文案")
	}
}

func TestJWTUserID(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"u123"}`))
	if id := JWTUserID("h." + payload + ".sig"); id != "u123" {
		t.Fatalf("user_id 提取不符: %q", id)
	}
	if JWTUserID("not-a-jwt") != "" {
		t.Fatal("非 JWT 应返回空")
	}
}

func TestPreviewPlansSortedAndBusinessError(t *testing.T) {
	acc := newTestAccount(t)
	up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
		"/billing/preview": func(upstreamCall) (int, string) {
			return 200, `{"code":0,"data":{"plans":[
				{"plan_id":"b","priority":2},{"plan_id":"a","priority":2},
				{"plan_id":"c","priority":5}]}}`
		},
	}}
	svc := &Service{Captcha: newSolvedManager(t), Client: up}
	plans, err := svc.PreviewPlans(acc)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 || plans[0]["plan_id"] != "c" || plans[1]["plan_id"] != "a" {
		t.Fatalf("应按 priority 降序、plan_id 升序: %v", plans)
	}

	// 业务码非 0 → ClaimError 文案
	up.responses["/billing/preview"] = func(upstreamCall) (int, string) {
		return 200, `{"code":1002}`
	}
	_, err = svc.PreviewPlans(acc)
	if err == nil || !strings.Contains(err.Error(), "活動已結束") {
		t.Fatalf("1002 应翻译为文案: %v", err)
	}
}

func TestParseTouchPlansAndMergePreview(t *testing.T) {
	acc := newTestAccount(t)
	up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
		"/billing/preview": func(upstreamCall) (int, string) {
			return 200, `{"code":0,"data":{"plans":[{"plan_id":"p-preview","name":"常駐方案","priority":5}]}}`
		},
		"/api/v1/marketing/touch": func(call upstreamCall) (int, string) {
			if call.Headers.Get("X-Device-Mid") != acc.DeviceMid() {
				t.Errorf("應帶帳號專屬 device_mid: %q", call.Headers.Get("X-Device-Mid"))
			}
			return 200, `{"code":0,"data":{"deliveries":[{"banner":{
				"background":{"args":{"zcode_plan":{"name":"ZCODE WEEKEND BUILD"}}},
				"buttons":[
					{"action":{"type":"claim_zcode_plan","args":{"plan_id":"p-weekend"}}},
					{"action":{"type":"claim_zcode_plan","args":{"plan_id":"p-preview"}}}
				]
			}}]}}`
		},
	}}
	svc := &Service{Captcha: newSolvedManager(t), Client: up, GetRetryDelays: []time.Duration{}}
	plans, err := svc.PreviewPlans(acc)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("應合併去重為 2 個方案: %v", plans)
	}
	if plans[0]["plan_id"] != "p-preview" || plans[1]["plan_id"] != "p-weekend" || plans[1]["name"] != "ZCODE WEEKEND BUILD" {
		t.Fatalf("合併結果不符: %v", plans)
	}
}

type flakyClient struct {
	failBefore int32
	calls      atomic.Int32
}

func (f *flakyClient) Do(r *http.Request) (*http.Response, error) {
	n := f.calls.Add(1)
	if n <= f.failBefore {
		return nil, errors.New("transient reset")
	}
	return jsonResponse(http.StatusOK, `{"code":0,"data":{"plans":[]}}`)
}

func TestGetTransientRetryAndPostSingleShot(t *testing.T) {
	acc := newTestAccount(t)

	// GET 遇瞬態錯誤應重試直到成功
	fc := &flakyClient{failBefore: 2}
	svc := &Service{
		Captcha:        newSolvedManager(t),
		Client:         fc,
		GetRetryDelays: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
	}
	if _, err := svc.TouchPlans(acc); err != nil {
		t.Fatalf("GET 瞬態重試應成功: %v", err)
	}
	if fc.calls.Load() != 3 {
		t.Fatalf("GET 應嘗試 3 次: %d", fc.calls.Load())
	}

	// POST claim 嚴格保持單發，絕不重試
	postFC := &flakyClient{failBefore: 5}
	svc.Client = postFC
	if _, err := svc.Claim(acc, "p1"); err == nil {
		t.Fatal("POST 失敗應直接返回錯誤")
	}
	if postFC.calls.Load() != 1 {
		t.Fatalf("POST 必須單發防 1003 重發: %d", postFC.calls.Load())
	}
}

func TestDayTrackerBeijingResetAndMonitorDedup(t *testing.T) {
	tracker := NewDayTracker()
	day1 := time.Date(2026, 11, 27, 23, 50, 0, 0, beijingZone)
	now := day1
	tracker.SetNow(func() time.Time { return now })

	tracker.MarkSettled("acc-1", "p-1900", "claimed")
	if !tracker.IsSettled("acc-1", "p-1900") {
		t.Fatal("當日已領方案應標記為 settled")
	}
	if tracker.IsSettled("acc-1", "p-1930") {
		t.Fatal("新掉落的 19:30 方案不應被去重")
	}
	// 跨北京午夜自動重置
	now = day1.Add(20 * time.Minute)
	if tracker.IsSettled("acc-1", "p-1900") {
		t.Fatal("跨北京自然日後應清空當日去重記錄")
	}

	// 端對端驗證 Monitor.RunOnce：第 1 輪領 p-1900，第 2 輪出現 p-1930 時只領 p-1930（不重送 p-1900）
	oldDB, oldData, oldURL, oldDelays := config.DBPath, config.DataDir, EventReportURL, ActivationRetryDelays
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	config.DataDir = t.TempDir()
	ActivationRetryDelays = []time.Duration{}
	t.Cleanup(func() {
		config.DBPath, config.DataDir, EventReportURL, ActivationRetryDelays = oldDB, oldData, oldURL, oldDelays
	})
	evtSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	t.Cleanup(evtSrv.Close)
	EventReportURL = evtSrv.URL

	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	jwt := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"u99"}`)) + ".sig"
	if _, err := st.AddAccount(model.ProviderZai, "mon-acc", jwt); err != nil {
		t.Fatal(err)
	}

	round := 1
	var claimedIDs []string
	up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
		"/billing/preview": func(upstreamCall) (int, string) {
			if round == 1 {
				return 200, `{"code":0,"data":{"plans":[{"plan_id":"p-1900","name":"19點","priority":5}]}}`
			}
			return 200, `{"code":0,"data":{"plans":[{"plan_id":"p-1900","name":"19點","priority":5},{"plan_id":"p-1930","name":"1930點","priority":4}]}}`
		},
		"/billing/claim": func(call upstreamCall) (int, string) {
			claimedIDs = append(claimedIDs, string(call.Body))
			return 200, `{"code":0}`
		},
	}}
	cm := newSolvedManager(t)
	mon := NewMonitor(st, cm, nil)
	mon.Service = &Service{Captcha: cm, Client: up, GetRetryDelays: []time.Duration{}}
	mon.Pause = 0

	mon.RunOnce()
	round = 2
	mon.RunOnce()

	if len(claimedIDs) != 2 || !strings.Contains(claimedIDs[0], "p-1900") || !strings.Contains(claimedIDs[1], "p-1930") {
		t.Fatalf("第 2 輪應跳過 p-1900 僅補領 p-1930，實際: %v", claimedIDs)
	}
}

func TestClaimOutcome_LocalVerifyParam_CodeZero(t *testing.T) {
	acc := newTestAccount(t)
	// 建立未設 solver 且未啟用瀏覽器的 Manager → GetVerifyParam 返回 ErrUnavailable
	cm := captcha.NewManager()
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, SceneID: "sc", Region: "cn", Prefix: "px"}, nil
	})
	svc := &Service{Captcha: cm, GetRetryDelays: []time.Duration{}}

	_, err := svc.Claim(acc, "p-test")
	var ce *ClaimError
	if !errors.As(err, &ce) {
		t.Fatalf("預期回傳 *ClaimError，實際: %v", err)
	}
	if ce.Code != 0 {
		t.Fatalf("本地驗證碼求解失敗錯誤碼應為 0（不得冒充上游 3007），實際: %d", ce.Code)
	}
	if ShouldSettleCode(ce.Code) {
		t.Fatal("錯誤碼 0 不應被 ShouldSettleCode 視為當日終態")
	}
}

func TestBillingRequest_CancelableRetry(t *testing.T) {
	acc := newTestAccount(t)
	fc := &flakyClient{failBefore: 10}
	svc := &Service{
		Captcha:        newSolvedManager(t),
		Client:         fc,
		GetRetryDelays: []time.Duration{5 * time.Second, 5 * time.Second},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := svc.billingRequestCtx(ctx, acc, http.MethodGet, "/billing/preview", authHeaders(acc), nil)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("重試退避期間取消應返回 ctx.Err()，實際: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("重試等待未被即時取消，耗時 %s", elapsed)
	}
}

func TestActivationEventUsesAccountSKU(t *testing.T) {
	acc := model.Create(model.ProviderZai, "telemetry", "a.b.c")
	sku := fingerprint.For(acc)
	body := BuildActivationEventBodyFor("app_launch", "u1", acc.DeviceMid(), sku)
	if body["client_timezone"] != sku.Timezone ||
		body["client_language"] != sku.Language ||
		body["screen_resolution"] != sku.Screen ||
		body["device_os_category"] != sku.OsCategory ||
		body["device_os_version"] != sku.OsVersion {
		t.Fatalf("遥测未使用帳號 SKU: %v vs %+v", body, sku)
	}
}
