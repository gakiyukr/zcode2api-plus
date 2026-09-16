// 网关端到端测试：httptest mock 上游，覆盖鉴权、透传、错误分类链与账号状态迁移。
// 对应移植 Python 版 tests/test_gateway_captcha.py / test_model_routing.py 的路由级用例。
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// upstreamCall 记录一次上游收包。
type upstreamCall struct {
	Path   string
	Header http.Header
	Body   map[string]any
}

// responder 由测试注入：第 n 次上游调用返回什么。
type responder func(call int, r *http.Request) (status int, header http.Header, body string)

type fixture struct {
	srv      *httptest.Server // 网关入口
	upstream *httptest.Server // mock 上游
	st       *store.Store
	cm       *captcha.Manager
	eng      *Engine

	mu      sync.Mutex
	calls   []upstreamCall
	respond responder
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		f.mu.Lock()
		f.calls = append(f.calls, upstreamCall{Path: r.URL.Path, Header: r.Header.Clone(), Body: parsed})
		n := len(f.calls)
		respond := f.respond
		f.mu.Unlock()
		if respond == nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		status, header, body := respond(n, r)
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	f.upstream = up
	t.Cleanup(up.Close)

	f.st = openStore(t)
	_ = f.st.SetSetting("gateway_key", "sk-test")
	f.cm = captcha.NewManager()
	f.eng = NewEngine(f.st, f.cm, nil)
	f.eng.BusyRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	h := &Handler{Engine: f.eng, Auth: auth.New(f.st)}
	mux := http.NewServeMux()
	h.Register(mux)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	oldZai, oldFB := config.UpstreamZai, config.UpstreamZaiFallback
	config.UpstreamZai = up.URL + "/zai"
	config.UpstreamZaiFallback = up.URL + "/fallback"
	t.Cleanup(func() { config.UpstreamZai, config.UpstreamZaiFallback = oldZai, oldFB })
	return f
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	old := config.DBPath
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	t.Cleanup(func() { config.DBPath = old })
	st, err := store.New()
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func (f *fixture) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fixture) lastCall() upstreamCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fixture) post(t *testing.T, body map[string]any, key string) (int, string) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/messages", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func msgBody() map[string]any {
	return map[string]any{
		"model":      "GLM-5.3",
		"max_tokens": 8,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	}
}

func jsonResp(status int, body string) responder {
	return func(int, *http.Request) (int, http.Header, string) {
		return status, http.Header{"Content-Type": []string{"application/json"}}, body
	}
}

const okUpstreamJSON = `{"id":"msg_1","usage":{"input_tokens":11,"output_tokens":22}}`

func TestAuthEnforced(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, okUpstreamJSON)

	status, body := f.post(t, msgBody(), "")
	if status != 401 || !strings.Contains(body, "缺少 API Key") {
		t.Fatalf("无密钥应 401: %d %s", status, body)
	}
	status, body = f.post(t, msgBody(), "sk-wrong")
	if status != 403 || !strings.Contains(body, "API Key 无效") {
		t.Fatalf("错密钥应 403: %d %s", status, body)
	}
	_ = f.st.SetSetting("gateway_key", "")
	status, body = f.post(t, msgBody(), "sk-test")
	if status != 503 || !strings.Contains(body, "网关未配置") {
		t.Fatalf("密钥被清空应 503 fail-closed: %d %s", status, body)
	}
	_ = f.st.SetSetting("gateway_key", "sk-test")
	// 鉴权通过后引擎需选中账号才能 200；Python 版只测 verify 函数，这里走全链路
	if _, err := f.st.AddAccount(model.ProviderZai, "a", "sk-1"); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.post(t, msgBody(), "sk-test"); status != 200 {
		t.Fatalf("正确密钥应 200: %d", status)
	}
}

func TestJSONPassthroughAndUsage(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, okUpstreamJSON)
	acc, _ := f.st.AddAccount(model.ProviderZai, "k", "sk-abc")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 200 || raw != okUpstreamJSON {
		t.Fatalf("应字节级透传: %d %q", status, raw)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.UseCount != 1 {
		t.Fatalf("use_count 应为 1: %d", got.UseCount)
	}
	if got.TotalInputTokens != 11 || got.TotalOutputTokens != 22 {
		t.Fatalf("usage 统计不符: %+v", got)
	}
	if f.callCount() != 1 {
		t.Fatalf("上游应被调用 1 次: %d", f.callCount())
	}
	call := f.lastCall()
	if call.Path != "/fallback" {
		t.Fatalf("apiKey 应走回退端点: %s", call.Path)
	}
	if call.Header.Get("x-api-key") != "sk-abc" {
		t.Fatalf("x-api-key 不符: %v", call.Header)
	}
	// apiKey 账号不注入 system，content 字符串已桥接
	if _, ok := call.Body["system"]; ok {
		t.Fatal("apiKey 不应注入 system")
	}
	msgs := call.Body["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("content 应桥接: %v", content)
	}
}

func TestStreamPassthroughAndUsage(t *testing.T) {
	f := newFixture(t)
	sse := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\ndata: [DONE]\n\n"
	f.respond = func(int, *http.Request) (int, http.Header, string) {
		return 200, http.Header{"Content-Type": []string{"text/event-stream"}}, sse
	}
	acc, _ := f.st.AddAccount(model.ProviderZai, "k", "sk-abc")

	body := msgBody()
	body["stream"] = true
	status, raw := f.post(t, body, "sk-test")
	if status != 200 || raw != sse {
		t.Fatalf("SSE 应字节级透传: %d %q", status, raw)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.TotalInputTokens != 9 || got.TotalOutputTokens != 42 {
		t.Fatalf("流式 usage 统计不符: %+v", got)
	}
}

func TestModelNotAllowed(t *testing.T) {
	f := newFixture(t)
	f.respond = func(int, *http.Request) (int, http.Header, string) {
		t.Fatal("白名单外模型不应触达上游")
		return 200, nil, ""
	}
	body := msgBody()
	body["model"] = "glm-5.2"
	status, raw := f.post(t, body, "sk-test")
	if status != 400 || !strings.Contains(raw, "model_not_allowed") {
		t.Fatalf("应 400 model_not_allowed: %d %s", status, raw)
	}
	if f.callCount() != 0 {
		t.Fatal("上游不应被调用")
	}
}

func Test401MarksInvalidAndSwitches(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(401, "")
	a1, _ := f.st.AddAccount(model.ProviderZai, "a1", "sk-1")
	a2, _ := f.st.AddAccount(model.ProviderZai, "a2", "sk-2")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 503 || !strings.Contains(raw, "no_available_account") {
		t.Fatalf("全部失效应 503: %d %s", status, raw)
	}
	for _, id := range []string{a1.ID, a2.ID} {
		if got := f.st.Find(model.ProviderZai, id); got.Status != model.StatusInvalid {
			t.Fatalf("账号 %s 应 invalid: %s", id, got.Status)
		}
	}
	if f.callCount() != 2 {
		t.Fatalf("应各试一次: %d", f.callCount())
	}
}

func Test402MarksModelExhausted(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(402, `{"error":{"message":"payment required"}}`)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 503 || !strings.Contains(raw, "no_available_account") {
		t.Fatalf("唯一账号耗尽应 503: %d %s", status, raw)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("仅单模型耗尽时账号应保持 active: %s", got.Status)
	}
	if len(got.ExhaustedModels) != 1 || got.ExhaustedModels[0] != "glm-5.3" {
		t.Fatalf("应只标记请求模型: %v", got.ExhaustedModels)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "額度已用完") {
		t.Fatalf("last_error 不符: %v", got.LastError)
	}
}

// TestMarkModelExhaustedDoesNotClobberStrongerStatus 额度信号不得覆盖
// invalid/cooling/disabled——它们由凭据校验或上游限流直接判定。
//
// Store.Select 不做占位保留，同一账号可被并发请求同时选中：A 被上游 401
// 标 invalid 后，B 的 402 额度信号曾无条件把状态刷回 active，失效账号立刻
// 回到轮询池，每次选中都白耗一次上游调用。cooling 同理会提前解除。
func TestMarkModelExhaustedDoesNotClobberStrongerStatus(t *testing.T) {
	cases := []struct {
		name   string
		status string
	}{
		{"invalid 不被覆盖", model.StatusInvalid},
		{"cooling 不被覆盖", model.StatusCooling},
		{"disabled 不被覆盖", model.StatusDisabled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			acc, err := f.st.AddAccount(model.ProviderZai, "acc", "sk-1")
			if err != nil {
				t.Fatal(err)
			}
			until := float64(time.Now().Add(time.Minute).UnixNano()) / 1e9
			prevMsg := "先前状态"
			f.st.Update(acc.Provider, acc.ID, func(a *model.Account) {
				a.Status = c.status
				if c.status == model.StatusCooling {
					a.CoolingUntil = &until
				}
				a.LastError = &prevMsg
			})

			MarkModelExhausted(f.st, acc.Provider, acc.ID, "GLM-5.3", "額度已用完")

			got := f.st.Find(model.ProviderZai, acc.ID)
			if got.Status != c.status {
				t.Fatalf("状态不应被额度信号改写: %s -> %s", c.status, got.Status)
			}
			if c.status == model.StatusCooling {
				if got.CoolingUntil == nil {
					t.Fatal("cooling 的截止时间不应被清空")
				}
			}
			// 模型级标记仍要生效，否则该模型不会被摘出轮询
			found := false
			for _, m := range got.ExhaustedModels {
				if m == "glm-5.3" {
					found = true
				}
			}
			if !found {
				t.Fatalf("模型应仍被标记耗尽: %v", got.ExhaustedModels)
			}
		})
	}
}

func Test429QuotaFamilyExhaustsAndRateLimitCools(t *testing.T) {
	t.Run("1310 用量上限族→模型耗尽", func(t *testing.T) {
		f := newFixture(t)
		f.respond = jsonResp(429, `{"code":1310,"msg":"usage cap reached"}`)
		acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")
		f.post(t, msgBody(), "sk-test")
		got := f.st.Find(model.ProviderZai, acc.ID)
		if got.Status != model.StatusActive || len(got.ExhaustedModels) != 1 {
			t.Fatalf("上限族应标记模型耗尽而保留账号: %+v", got)
		}
	})
	t.Run("1302 瞬时限流→cooling", func(t *testing.T) {
		f := newFixture(t)
		f.respond = jsonResp(429, `{"code":1302,"msg":"rate limited"}`)
		acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")
		f.post(t, msgBody(), "sk-test")
		got := f.st.Find(model.ProviderZai, acc.ID)
		if got.Status != model.StatusCooling || got.CoolingUntil == nil {
			t.Fatalf("瞬时限流应 cooling: %+v", got)
		}
		if len(got.ExhaustedModels) != 0 {
			t.Fatalf("瞬时限流不应标记耗尽: %v", got.ExhaustedModels)
		}
	})
}

func Test3010RetriesThenSucceeds(t *testing.T) {
	f := newFixture(t)
	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		if n <= 2 {
			return jsonResp(429, `{"code":3010,"msg":"model admission concurrency limit exceeded"}`)(n, nil)
		}
		return jsonResp(200, okUpstreamJSON)(n, nil)
	}
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 200 || !strings.Contains(raw, "msg_1") {
		t.Fatalf("3010 两次后应成功: %d %s", status, raw)
	}
	if f.callCount() != 3 {
		t.Fatalf("应重试到第 3 次: %d", f.callCount())
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("3010 不应改变账号状态: %s", got.Status)
	}
}

func Test500PassthroughVerbatim(t *testing.T) {
	f := newFixture(t)
	upstreamBody := `{"error":{"message":"boom","quota_hint":"yes"}}`
	f.respond = jsonResp(500, upstreamBody)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 500 || raw != upstreamBody {
		t.Fatalf("未识别错误应原样透传: %d %q", status, raw)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive || len(got.ExhaustedModels) != 0 {
		t.Fatalf("未识别错误不应推断状态: %+v", got)
	}
	if got.FailCount != 1 {
		t.Fatalf("失败计数应 +1: %d", got.FailCount)
	}
}

func TestBusinessCode1005ExhaustsDailyQuota(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, `{"code":1005,"msg":"exceed quota limit"}`)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, _ := f.post(t, msgBody(), "sk-test")
	if status != 503 {
		t.Fatalf("业务码 1005 应换号并 503: %d", status)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if len(got.ExhaustedModels) != 1 || got.ExhaustedModels[0] != "glm-5.3" {
		t.Fatalf("1005 应标记该模型耗尽: %v", got.ExhaustedModels)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "每日額度已用完") {
		t.Fatalf("last_error 不符: %v", got.LastError)
	}
}

func TestJWTInjectsSystemAndCaptcha(t *testing.T) {
	f := newFixture(t)
	oldBrowser := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = oldBrowser })
	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.DefaultConfig, nil // 避免测试触网
	})
	f.cm.SetSolver(stubSolver{token: "token-1"})

	f.respond = jsonResp(200, okUpstreamJSON)
	acc, _ := f.st.AddAccount(model.ProviderZai, "j", "header.payload.sig")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 200 {
		t.Fatalf("JWT 应成功: %d %s", status, raw)
	}
	call := f.lastCall()
	if call.Path != "/zai" {
		t.Fatalf("JWT 应走主端点: %s", call.Path)
	}
	if call.Header.Get("Authorization") != "Bearer header.payload.sig" {
		t.Fatalf("Authorization 不符: %v", call.Header)
	}
	if call.Header.Get("X-Aliyun-Captcha-Verify-Param") != "token-1" {
		t.Fatalf("验证码头不符: %v", call.Header)
	}
	sys, ok := call.Body["system"].([]any)
	if !ok || len(sys) == 0 {
		t.Fatalf("JWT 必须注入 system: %v", call.Body["system"])
	}
	if got := f.st.Find(model.ProviderZai, acc.ID); got.UseCount != 1 {
		t.Fatalf("use_count 应为 1")
	}
}

func TestJWTWithoutSolverReturnsCaptchaRequired(t *testing.T) {
	f := newFixture(t)
	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.DefaultConfig, nil // 避免测试触网
	})
	f.respond = func(int, *http.Request) (int, http.Header, string) {
		t.Fatal("无求解器时不应触达上游")
		return 200, nil, ""
	}
	if _, err := f.st.AddAccount(model.ProviderZai, "j", "header.payload.sig"); err != nil {
		t.Fatal(err)
	}

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 503 || !strings.Contains(raw, "captcha_required") {
		t.Fatalf("应 503 captcha_required: %d %s", status, raw)
	}
	if f.callCount() != 0 {
		t.Fatal("上游不应被调用")
	}
}

// TestConcurrentRequestsAccountState 并发压测：多个请求同时打同一账号池，
// 验证账号状态更新（use_count / token 统计）不丢失。
//
// 每个成功请求恰好累计一次，并发下丢更新会直接反映为数值偏小——这是
// 「状态写入必须经 Store.Update 在锁内完成」的行为契约。
func TestConcurrentRequestsAccountState(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, okUpstreamJSON)

	const accounts = 4
	const requests = 40
	for i := range accounts {
		acc, err := f.st.AddAccount(model.ProviderZai, fmt.Sprintf("acc-%d", i), fmt.Sprintf("sk-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		// quota 需在锁内写入，Select 才会认为该模型可用
		f.st.Update(acc.Provider, acc.ID, func(a *model.Account) {
			a.Quota = map[string]map[string]any{
				"GLM-5.3": {"remaining": float64(1000), "model": "GLM-5.3"},
			}
		})
	}

	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 第三个参数是网关密钥（所有请求相同）；账号由引擎轮询选中
			status, _ := f.post(t, msgBody(), "sk-test")
			if status != 200 {
				t.Errorf("请求 %d 应 200: %d", i, status)
			}
		}(i)
	}
	wg.Wait()

	// 每个成功请求恰好累计一次：use_count 与 token 统计都必须精确，
	// 并发下丢更新会直接反映为数值偏小。
	totalUse, totalIn, totalOut := 0, 0, 0
	for _, a := range f.st.ListAccounts(model.ProviderZai) {
		totalUse += a.UseCount
		totalIn += a.TotalInputTokens
		totalOut += a.TotalOutputTokens
	}
	if totalUse != requests {
		t.Fatalf("use_count 合计应为 %d（并发丢更新？）: %d", requests, totalUse)
	}
	if totalIn != requests*11 || totalOut != requests*22 {
		t.Fatalf("token 统计不符: in=%d out=%d（期望 %d/%d）",
			totalIn, totalOut, requests*11, requests*22)
	}
}

// TestClientCancelDoesNotCoolAccounts 客户端中断不得污染账号状态。
//
// 中断会让 ctx 取消，Do 随即返回 context.Canceled；若把它当成「连接失败」，
// 重试循环会把每个被选中的账号各标一次冷却（默认 300s）并落库——小账号池
// 几次 Ctrl-C 就全池不可用，所有请求 503。中断与账号健康无关，必须不写状态。
func TestClientCancelDoesNotCoolAccounts(t *testing.T) {
	f := newFixture(t)

	// 上游阻塞到 ctx 取消为止，确保取消发生在请求进行中
	blocked := make(chan struct{})
	f.respond = func(int, *http.Request) (int, http.Header, string) {
		<-blocked
		return 200, http.Header{"Content-Type": []string{"application/json"}}, okUpstreamJSON
	}
	defer close(blocked)

	const accounts = 3
	for i := range accounts {
		if _, err := f.st.AddAccount(model.ProviderZai, fmt.Sprintf("acc-%d", i), fmt.Sprintf("sk-%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan runResult, 1)
	go func() {
		done <- f.eng.RunMessages(ctx, msgBody(), map[string]string{}, func(Delivery) error { return nil })
	}()

	// 等上游真的被调用后再取消，模拟客户端中途断开
	deadline := time.After(5 * time.Second)
	for f.callCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("上游未被调用")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("取消后引擎未返回")
	}

	// 核心断言：没有任何账号被标记为冷却或其他异常状态
	for _, a := range f.st.ListAccounts(model.ProviderZai) {
		if a.Status != model.StatusActive {
			t.Fatalf("账号 %s 状态被中断污染: %s (%v)", a.Name, a.Status, a.LastError)
		}
		if a.CoolingUntil != nil {
			t.Fatalf("账号 %s 不应有冷却截止时间", a.Name)
		}
	}
}

// TestAccount 只测指定账号、不走选号轮询，且不把响应交付给客户端。
//
// 用途是访客提交账号时的「实测通过才入池」：OAuth 授权只证明提交者持有
// 账号，不证明它当下可用。这里验证成功路径判定与失败路径的原因提取。
func TestTestAccount(t *testing.T) {
	t.Run("上游 200 判定为可用", func(t *testing.T) {
		f := newFixture(t)
		f.respond = jsonResp(200, okUpstreamJSON)
		acc, _ := f.st.AddAccount(model.ProviderZai, "probe", "sk-1")

		res := f.eng.TestAccount(context.Background(), acc, "GLM-5.3")
		if !res.OK || res.Status != http.StatusOK {
			t.Fatalf("应判定可用: %+v", res)
		}
		if f.callCount() != 1 {
			t.Fatalf("应恰好请求上游一次: %d", f.callCount())
		}
		// 请求体必须是最小的：max_tokens=1，避免为了一次探测消耗额度
		call := f.lastCall()
		if call.Body["max_tokens"] != float64(1) {
			t.Fatalf("探测请求应使用 max_tokens=1: %v", call.Body["max_tokens"])
		}
		if _, ok := call.Body["stream"]; ok {
			t.Fatalf("探测应为非流式: %v", call.Body)
		}
	})

	t.Run("上游 401 判定为不可用", func(t *testing.T) {
		f := newFixture(t)
		f.respond = jsonResp(401, "")
		acc, _ := f.st.AddAccount(model.ProviderZai, "probe", "sk-1")

		res := f.eng.TestAccount(context.Background(), acc, "GLM-5.3")
		if res.OK {
			t.Fatalf("401 不应判定可用: %+v", res)
		}
		if res.Reason == "" {
			t.Fatal("失败应给出原因")
		}
		// 状态如实反映：401 会把账号标为 invalid，这正是期望行为
		if got := f.st.Find(model.ProviderZai, acc.ID); got.Status != model.StatusInvalid {
			t.Fatalf("401 应把账号标为 invalid: %s", got.Status)
		}
	})

	t.Run("nil 账号安全拒绝", func(t *testing.T) {
		f := newFixture(t)
		res := f.eng.TestAccount(context.Background(), nil, "GLM-5.3")
		if res.OK {
			t.Fatal("nil 账号不应判定可用")
		}
		if f.callCount() != 0 {
			t.Fatal("nil 账号不应发起上游请求")
		}
	})

	t.Run("空模型名回退到可用清单", func(t *testing.T) {
		f := newFixture(t)
		f.respond = jsonResp(200, okUpstreamJSON)
		acc, _ := f.st.AddAccount(model.ProviderZai, "probe", "sk-1")

		res := f.eng.TestAccount(context.Background(), acc, "")
		if !res.OK {
			t.Fatalf("空模型名应回退并成功: %+v", res)
		}
		if f.callCount() != 1 {
			t.Fatalf("应请求上游一次: %d", f.callCount())
		}
	})
}

func TestModelsEndpoint(t *testing.T) {
	f := newFixture(t)
	// Python 版 /v1/models 带 Depends(verify_gateway_key)，同样需要鉴权
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", "sk-test")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("应 200: %d %s", resp.StatusCode, raw)
	}
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Object != "list" || len(payload.Data) != 2 {
		t.Fatalf("模型清单不符: %s", raw)
	}
	if payload.Data[0].Type != "model" || payload.Data[0].DisplayName != payload.Data[0].ID {
		t.Fatalf("模型条目不符: %+v", payload.Data[0])
	}
}

type stubSolver struct{ token string }

func (s stubSolver) Solve(_ context.Context, _ captcha.Config) (string, error) {
	return s.token, nil
}

func (s stubSolver) Close() error { return nil }
