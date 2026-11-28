// 网关端到端测试：httptest mock 上游，覆盖鉴权、透传、错误分类链与账号状态迁移。
// 对应移植 Python 版 tests/test_gateway_captcha.py / test_model_routing.py 的路由级用例。
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestModelPassthroughNoWhitelist(t *testing.T) {
	// 白名单已移除：任何模型名都直接转发上游，由上游裁决准入。
	f := newFixture(t)
	f.respond = jsonResp(200, "ok")
	if _, err := f.st.AddAccount(model.ProviderZai, "k", "sk-abc"); err != nil {
		t.Fatal(err)
	}
	body := msgBody()
	body["model"] = "glm-5.2"
	status, raw := f.post(t, body, "sk-test")
	if status != 200 {
		t.Fatalf("白名单移除后应放行上游: %d %s", status, raw)
	}
	if f.callCount() != 1 {
		t.Fatalf("上游应被调用一次: %d", f.callCount())
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
	// 无账号：回退 AvailableModels 静态清单，保证端点永远可用
	if payload.Object != "list" || len(payload.Data) != 2 {
		t.Fatalf("模型清单不符: %s", raw)
	}
	if payload.Data[0].Type != "model" || payload.Data[0].DisplayName != payload.Data[0].ID {
		t.Fatalf("模型条目不符: %+v", payload.Data[0])
	}
}

func TestModelsEndpointFromQuotaSnapshot(t *testing.T) {
	// 有账号且额度快照非空：/v1/models 应聚合快照中的上游真实模型（去重排序），
	// 不再展示静态清单
	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "acc", "sk-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.Quota = map[string]map[string]any{
			"GLM-5.4":       {"model": "GLM-5.4", "remaining": float64(10)},
			"GLM-5.3":       {"model": "GLM-5.3", "remaining": float64(10)},
			"glm-5.3-flash": {"model": "glm-5.3-flash", "remaining": float64(10)},
			// 同名多订阅列（不同 key、相同 model）应去重
			"GLM-5.3 · 活动套餐": {"model": "GLM-5.3", "remaining": float64(5)},
		}
	}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", "sk-test")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(payload.Data))
	for _, d := range payload.Data {
		got = append(got, d.ID)
	}
	want := []string{"GLM-5.3", "GLM-5.4", "glm-5.3-flash"}
	if len(got) != len(want) {
		t.Fatalf("模型清单应去重为 %v: %v", want, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("模型清单应排序为 %v: %v", want, got)
		}
	}
}

type stubSolver struct{ token string }

func (s stubSolver) Solve(_ context.Context, _ captcha.Config) (string, error) {
	return s.token, nil
}

func (s stubSolver) Close() error { return nil }

func TestFlagshipGLM53AutoFallsBackToGLM54(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, okUpstreamJSON)
	acc, _ := f.st.AddAccount(model.ProviderZai, "v54", "sk-54")
	_ = f.st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.Quota = map[string]map[string]any{
			"GLM-5.4": {"model": "GLM-5.4", "remaining": float64(500)},
		}
	})

	// 舊客戶端仍送 GLM-5.3：因帳號池僅有 GLM-5.4 可用，應無縫改寫為 GLM-5.4 並命中帳號
	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 200 {
		t.Fatalf("應自動改寫 GLM-5.3 → GLM-5.4 成功: %d %s", status, raw)
	}
	if got := f.lastCall().Body["model"]; got != "GLM-5.4" {
		t.Fatalf("上游應收到改寫後的 GLM-5.4，實際: %v", got)
	}
}

func TestEmptyNonStreamReplyFailsOverAndRelaysLast(t *testing.T) {
	t.Run("首號空回覆自動換號成功", func(t *testing.T) {
		f := newFixture(t)
		_, _ = f.st.AddAccount(model.ProviderZai, "a1", "sk-1")
		_, _ = f.st.AddAccount(model.ProviderZai, "a2", "sk-2")
		f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
			if n == 1 {
				return 200, http.Header{"Content-Type": []string{"application/json"}},
					`{"id":"m_empty","content":[],"usage":{"input_tokens":5,"output_tokens":0}}`
			}
			return 200, http.Header{"Content-Type": []string{"application/json"}},
				`{"id":"m_ok","content":[{"type":"text","text":"正常回答"}],"usage":{"input_tokens":5,"output_tokens":3}}`
		}
		status, raw := f.post(t, msgBody(), "sk-test")
		if status != 200 || !strings.Contains(raw, "正常回答") {
			t.Fatalf("空回覆應自動換號並成功: %d %s", status, raw)
		}
		if f.callCount() != 2 {
			t.Fatalf("應嘗試 2 個帳號: %d", f.callCount())
		}
	})

	t.Run("全池皆空則原樣交付最後一次空回覆", func(t *testing.T) {
		f := newFixture(t)
		_, _ = f.st.AddAccount(model.ProviderZai, "a1", "sk-1")
		emptyBody := `{"id":"m_empty","content":[{"type":"text","text":"   "}],"usage":{"input_tokens":2,"output_tokens":0}}`
		f.respond = jsonResp(200, emptyBody)
		status, raw := f.post(t, msgBody(), "sk-test")
		if status != 200 || raw != emptyBody {
			t.Fatalf("全池皆空應誠實交付空回覆: %d %s", status, raw)
		}
	})
}

func TestToolSearchEndToEndRewriteAndRestore(t *testing.T) {
	f := newFixture(t)
	_, _ = f.st.AddAccount(model.ProviderZai, "k", "sk-abc")
	f.respond = jsonResp(200, `{"id":"m1","content":[{"type":"tool_use","id":"tu_1","name":"ToolSearchTool","input":{"query":"bash"}}],"usage":{"input_tokens":3,"output_tokens":2}}`)

	body := msgBody()
	body["tools"] = []any{
		map[string]any{"name": "ToolSearch", "description": "s"},
		map[string]any{"name": "Bash", "defer_loading": true},
	}
	status, raw := f.post(t, body, "sk-test")
	if status != 200 {
		t.Fatalf("應 200: %d %s", status, raw)
	}
	// 上游應收到 ToolSearchTool 且不含 defer_loading
	sentTools := f.lastCall().Body["tools"].([]any)
	if sentTools[0].(map[string]any)["name"] != "ToolSearchTool" {
		t.Fatalf("上游應收到改名後工具: %v", sentTools)
	}
	if _, has := sentTools[1].(map[string]any)["defer_loading"]; has {
		t.Fatal("上游不應收到 defer_loading")
	}
	// 客戶端應收到還原後的 ToolSearch
	if !strings.Contains(raw, `"name":"ToolSearch"`) || strings.Contains(raw, "ToolSearchTool") {
		t.Fatalf("回應應還原為 ToolSearch: %s", raw)
	}
}

func TestConcurrency3008RetriesWithoutCooling(t *testing.T) {
	f := newFixture(t)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")
	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		if n == 1 {
			return jsonResp(429, `{"code":3008,"msg":"user concurrency limit exceeded"}`)(n, nil)
		}
		return jsonResp(200, okUpstreamJSON)(n, nil)
	}
	status, _ := f.post(t, msgBody(), "sk-test")
	if status != 200 || f.callCount() != 2 {
		t.Fatalf("3008 應重試成功: status=%d calls=%d", status, f.callCount())
	}
	if got := f.st.Find(model.ProviderZai, acc.ID); got.Status != model.StatusActive {
		t.Fatalf("3008 不應標記帳號 cooling: %s", got.Status)
	}
}

func TestEngineBusyAttemptIndependentOfCaptcha(t *testing.T) {
	f := newFixture(t)
	oldBrowser := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = oldBrowser })
	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, SceneID: "s", Region: "cn", Prefix: "p"}, nil
	})
	f.cm.SetSolver(stubSolver{token: "tok"})

	acc, err := f.st.AddAccount(model.ProviderZai, "jwt-acc", "header.payload.sig")
	if err != nil {
		t.Fatal(err)
	}
	// 佔住第 2 條在途槽位：若 busyAttempt 被 captchaAttempt 污染為 1，首次 3010 就會立刻觸發收窄
	extra, err := f.eng.Gate.Acquire(context.Background(), acc.ID, "direct", false)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Release()

	// 第 1 次回 3007（驗證碼重試 → captchaAttempt 升至 1）；
	// 第 2 次回 3010（首次併發受限 → busyAttempt 應仍為 0，不應觸發收窄）；
	// 第 3 次成功 200。
	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		switch n {
		case 1:
			return jsonResp(400, `{"code":3007,"msg":"captcha rejected"}`)(n, nil)
		case 2:
			return jsonResp(429, `{"code":3010,"msg":"concurrency limit"}`)(n, nil)
		default:
			return jsonResp(200, okUpstreamJSON)(n, nil)
		}
	}

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 200 || !strings.Contains(raw, "msg_1") {
		t.Fatalf("3007 後接首次 3010 應重試成功: status=%d body=%s", status, raw)
	}
	if f.callCount() != 3 {
		t.Fatalf("應呼叫上游 3 次，實際: %d", f.callCount())
	}
	if gotCap := f.eng.Gate.AccountCap(acc.ID); gotCap != config.StartPlanAccountConcurrency {
		t.Fatalf("首次 3010（busyAttempt=0）不應被 captchaAttempt 污染而觸發收窄，實際上限: %d", gotCap)
	}
}

type cancelInjectSolver struct {
	calls int
	onRun func()
}

func (s *cancelInjectSolver) Solve(ctx context.Context, _ captcha.Config) (string, error) {
	s.calls++
	if s.onRun != nil {
		s.onRun()
	}
	return "", context.Canceled
}

func (s *cancelInjectSolver) Close() error { return nil }

func TestCaptchaInvalidateNotCalledOnCancel(t *testing.T) {
	f := newFixture(t)
	oldBrowser := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = oldBrowser })
	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, SceneID: "s", Region: "cn", Prefix: "p"}, nil
	})

	solver := &cancelInjectSolver{
		onRun: func() {
			// 模擬其他並發來源同時補入有效手動令牌：客戶端取消不得觸發 Invalidate 清空它
			_ = f.cm.SetManualParam("manual-keep-token", "cn")
		},
	}
	f.cm.SetSolver(solver)

	if _, err := f.st.AddAccount(model.ProviderZai, "jwt-acc", "header.payload.sig"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := f.eng.RunMessages(ctx, msgBody(), nil, func(d Delivery) error { return nil })
	if res.Delivered {
		t.Fatal("已取消的請求不應交付成功")
	}
	if solver.calls != 1 {
		t.Fatalf("取消時應立即中止而不重試驗證碼，實際呼叫次數: %d", solver.calls)
	}
	// 驗證手動令牌仍保留在快取中（未被誤呼叫 Invalidate）
	tok, err := f.cm.GetVerifyParam(context.Background())
	if err != nil || tok == nil || tok.VerifyParam != "manual-keep-token" {
		t.Fatalf("客戶端取消不應清空全域驗證碼快取，實際: tok=%+v err=%v", tok, err)
	}
}

func TestIsEmptyAnthropicReply_Thinking(t *testing.T) {
	thinkingOnly := []byte(`{"id":"msg_t","content":[{"type":"thinking","thinking":"思考過程"}]}`)
	if isEmptyAnthropicReply(thinkingOnly) {
		t.Fatal("僅含非空 thinking block 的回覆不應誤判為空回覆")
	}
	redactedOnly := []byte(`{"id":"msg_r","content":[{"type":"redacted_thinking","data":"abc"}]}`)
	if isEmptyAnthropicReply(redactedOnly) {
		t.Fatal("僅含 redacted_thinking block 的回覆不應誤判為空回覆")
	}
	emptyContent := []byte(`{"id":"msg_e","content":[]}`)
	if !isEmptyAnthropicReply(emptyContent) {
		t.Fatal("空 content 陣列應判為空回覆")
	}
}

func TestIsEmptyAnthropicReply_EmptyThinking(t *testing.T) {
	emptyThinking := []byte(`{"id":"msg_et","content":[{"type":"thinking","thinking":""}]}`)
	if !isEmptyAnthropicReply(emptyThinking) {
		t.Fatal("空白 thinking block 僅為佔位結構，應判為空回覆")
	}
	whitespaceThinking := []byte(`{"id":"msg_wt","content":[{"type":"thinking","thinking":"   \n\t  "}]}`)
	if !isEmptyAnthropicReply(whitespaceThinking) {
		t.Fatal("純空白 thinking block 應判為空回覆")
	}
}

func TestEdgeHold_PerAddrKey(t *testing.T) {
	now := time.Now()
	proxyA := "http://proxy-a.invalid:8080"
	proxyB := "http://proxy-b.invalid:8080"
	HoldEdgeFor(proxyA, 5*time.Minute, now)
	t.Cleanup(func() {
		edgeHoldMap.Delete(proxyA)
		edgeHoldMap.Delete(proxyB)
	})

	if !EdgeHeldFor(proxyA, now.Add(time.Minute)) {
		t.Fatal("proxy-a 應處於熔斷期")
	}
	if EdgeHeldFor("direct", now.Add(time.Minute)) {
		t.Fatal("proxy-a 熔斷不應影響 direct 出口")
	}
	if EdgeHeldFor(proxyB, now.Add(time.Minute)) {
		t.Fatal("proxy-a 熔斷不應影響 proxy-b 出口")
	}
	if EdgeHeldFor(proxyA, now.Add(6*time.Minute)) {
		t.Fatal("超過熔斷時長後 proxy-a 應自動恢復")
	}
}

func TestNilBodyReturns400(t *testing.T) {
	f := newFixture(t)
	h := f.srv.Config.Handler
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("null"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("body=null 應回傳 400，實際: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"invalid_request_error"`) {
		t.Fatalf("錯誤型別應為 invalid_request_error，實際: %s", rec.Body.String())
	}

	// 超過 MaxBytesReader 應回傳 413 request_too_large
	bigReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"GLM-5.4","messages":[{"role":"user","content":"0123456789abcdef"}]}`))
	bigReq.Header.Set("Content-Type", "application/json")
	bigReq.Header.Set("x-api-key", "sk-test")
	bigRec := httptest.NewRecorder()
	bigReq.Body = http.MaxBytesReader(bigRec, bigReq.Body, 16)
	h.ServeHTTP(bigRec, bigReq)
	if bigRec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超過上限應回傳 413，實際: %d %s", bigRec.Code, bigRec.Body.String())
	}
	if !strings.Contains(bigRec.Body.String(), `"request_too_large"`) {
		t.Fatalf("413 錯誤型別應為 request_too_large，實際: %s", bigRec.Body.String())
	}
}

func TestIsRefusedEnvelopeAndEngineIntercept(t *testing.T) {
	f := newFixture(t)
	a1, _ := f.st.AddAccount(model.ProviderZai, "a1", "sk-1")
	_, _ = f.st.AddAccount(model.ProviderZai, "a2", "sk-2")
	f.respond = jsonResp(http.StatusOK, `{"code":0,"data":null,"success":true}`)

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusBadGateway {
		t.Fatalf("空信封應回傳 502，實際: %d %s", status, raw)
	}
	if !strings.Contains(raw, `"upstream_version_refused"`) {
		t.Fatalf("錯誤型別應為 upstream_version_refused，實際: %s", raw)
	}
	if f.callCount() != 1 {
		t.Fatalf("空信封不應盲目換號重試，實際呼叫次數: %d", f.callCount())
	}
	got := f.st.Find(model.ProviderZai, a1.ID)
	if got.Status != model.StatusActive || got.FailCount != 0 {
		t.Fatalf("空信封不應汙染帳號狀態或增加 FailCount: status=%s fail=%d", got.Status, got.FailCount)
	}
}

func TestBare429CircuitBreakerProtectsPool(t *testing.T) {
	t.Run("連續兩號裸 429 觸發止損並平反首號", func(t *testing.T) {
		f := newFixture(t)
		for i := range 4 {
			if _, err := f.st.AddAccount(model.ProviderZai, fmt.Sprintf("a%d", i+1), fmt.Sprintf("sk-%d", i+1)); err != nil {
				t.Fatal(err)
			}
		}
		f.respond = func(int, *http.Request) (int, http.Header, string) {
			return http.StatusTooManyRequests, http.Header{"Content-Type": []string{"text/plain"}}, "Too Many Requests"
		}

		status, _ := f.post(t, msgBody(), "sk-test")
		if status != http.StatusTooManyRequests {
			t.Fatalf("連續裸 429 應止損回傳 429，實際: %d", status)
		}
		if f.callCount() != 2 {
			t.Fatalf("應在第 2 號裸 429 時立即止損，實際呼叫: %d", f.callCount())
		}
		for _, acc := range f.st.ListAccounts(model.ProviderZai) {
			if acc.Status != model.StatusActive || acc.CoolingUntil != nil {
				t.Fatalf("帳號 %s 應保留 active（首號須已平反），實際 status=%s cooling=%v", acc.Name, acc.Status, acc.CoolingUntil)
			}
		}
	})

	t.Run("連續兩號空正文裸 429 止損並回傳非空文案", func(t *testing.T) {
		f := newFixture(t)
		for i := range 3 {
			if _, err := f.st.AddAccount(model.ProviderZai, fmt.Sprintf("e%d", i+1), fmt.Sprintf("sk-e-%d", i+1)); err != nil {
				t.Fatal(err)
			}
		}
		f.respond = func(int, *http.Request) (int, http.Header, string) {
			return http.StatusTooManyRequests, http.Header{"Content-Type": []string{"text/plain"}}, ""
		}

		status, raw := f.post(t, msgBody(), "sk-test")
		if status != http.StatusTooManyRequests {
			t.Fatalf("空正文裸 429 應止損回傳 429，實際: %d %s", status, raw)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatalf("回傳應為合法 JSON: %v %s", err, raw)
		}
		errObj, _ := body["error"].(map[string]any)
		if errObj == nil || strings.TrimSpace(fmt.Sprint(errObj["message"])) == "" {
			t.Fatalf("空正文裸 429 回傳的 error.message 不得為空: %s", raw)
		}
	})

	t.Run("帶業務碼 1302 的 429 正常冷卻並換號", func(t *testing.T) {
		f := newFixture(t)
		a1, _ := f.st.AddAccount(model.ProviderZai, "a1", "sk-1")
		_, _ = f.st.AddAccount(model.ProviderZai, "a2", "sk-2")
		f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
			if n == 1 {
				return jsonResp(http.StatusTooManyRequests, `{"code":1302,"msg":"rate limited"}`)(n, nil)
			}
			return jsonResp(http.StatusOK, okUpstreamJSON)(n, nil)
		}

		status, _ := f.post(t, msgBody(), "sk-test")
		if status != http.StatusOK {
			t.Fatalf("第二號應接手成功 200，實際: %d", status)
		}
		if got := f.st.Find(model.ProviderZai, a1.ID); got.Status != model.StatusCooling {
			t.Fatalf("帶 1302 業務碼的帳號應標記 cooling，實際: %s", got.Status)
		}
	})
}

func TestModelSkipFallbackOn3007(t *testing.T) {
	f := newFixture(t)
	oldBrowser := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = oldBrowser })

	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{
			Enabled:          true,
			SkipModelRequest: true,
			Prefix:           "p",
			Region:           "cn",
			SceneID:          "s",
		}, nil
	})
	f.cm.SetSolver(stubSolver{token: "recovered-token"})
	if _, err := f.st.AddAccount(model.ProviderZai, "jwt-skip", "header.payload.sig"); err != nil {
		t.Fatal(err)
	}

	f.respond = func(n int, r *http.Request) (int, http.Header, string) {
		if n == 1 {
			if got := r.Header.Get("X-Aliyun-Captcha-Verify-Param"); got != "" {
				return http.StatusInternalServerError, nil, "首發免驗不應帶驗證碼標頭"
			}
			return jsonResp(http.StatusBadRequest, `{"code":3007,"msg":"captcha required"}`)(n, nil)
		}
		if got := r.Header.Get("X-Aliyun-Captcha-Verify-Param"); got != "recovered-token" {
			return http.StatusInternalServerError, nil, "補驗未攜帶求解令牌: " + got
		}
		return jsonResp(http.StatusOK, okUpstreamJSON)(n, nil)
	}

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusOK || !strings.Contains(raw, "msg_1") {
		t.Fatalf("免驗遇 3007 應自動補驗成功 200: %d %s", status, raw)
	}
	if f.callCount() != 2 {
		t.Fatalf("應剛好呼叫上游 2 次（首發免驗 + 補驗），實際: %d", f.callCount())
	}
}

func TestEdgeBlocked405SwitchesProxyExit(t *testing.T) {
	f := newFixture(t)
	t.Cleanup(func() {
		edgeHoldMap.Delete("direct")
	})

	// 建立本機測試 HTTP 代理並附加標記頭，讓 mock 上游區分直連與代理出口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				outReq := req.Clone(context.Background())
				outReq.RequestURI = ""
				if outReq.URL.Host == "" {
					outReq.URL.Host = req.Host
				}
				outReq.Header.Set("X-Test-Proxy-Exit", "proxy-1")
				resp, err := http.DefaultTransport.RoundTrip(outReq)
				if err != nil {
					return
				}
				defer resp.Body.Close()
				_ = resp.Write(c)
			}(conn)
		}
	}()

	// a1 為直連（給予臨期一次性優惠使其在同組中必定先被選中）；a2 走代理出口（一般日常額度）
	a1, _ := f.st.AddAccount(model.ProviderZai, "direct-acc", "sk-direct")
	soon := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	_ = f.st.Update(a1.Provider, a1.ID, func(a *model.Account) {
		a.Quota = map[string]map[string]any{
			"GLM-5.3": {"model": "GLM-5.3", "remaining": float64(50), "period": "one_time", "expires_at": soon},
		}
	})
	a2, _ := f.st.AddAccount(model.ProviderZai, "proxy-acc", "sk-proxy")
	proxyURL := "http://" + ln.Addr().String()
	_ = f.st.Update(a2.Provider, a2.ID, func(a *model.Account) {
		a.ProxyURL = &proxyURL
		a.Quota = map[string]map[string]any{
			"GLM-5.3": {"model": "GLM-5.3", "remaining": float64(50), "period": "daily"},
		}
	})
	a2Updated := f.st.Find(model.ProviderZai, a2.ID)
	t.Cleanup(func() {
		edgeHoldMap.Delete(AddressKeyFor(a2Updated))
	})

	f.respond = func(_ int, r *http.Request) (int, http.Header, string) {
		if r.Header.Get("X-Test-Proxy-Exit") == "" {
			return http.StatusMethodNotAllowed, http.Header{"Content-Type": []string{"text/html"}},
				`<html><body>errors.aliyun.com blocked</body></html>`
		}
		return http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, okUpstreamJSON
	}

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusOK || !strings.Contains(raw, "msg_1") {
		t.Fatalf("直連 405 應自動熔斷並切換代理出口成功 200: %d %s", status, raw)
	}
	if f.callCount() != 2 {
		t.Fatalf("應先試直連再切代理共 2 次，實際: %d", f.callCount())
	}
	if !EdgeHeldFor("direct", time.Now()) {
		t.Fatal("direct 出口應已進入邊緣熔斷")
	}
	if EdgeHeldFor(AddressKeyFor(a2Updated), time.Now()) {
		t.Fatal("代理出口不應被熔斷")
	}
}

func TestTransparentReplayOnBrokenConnDoesNotCoolAccount(t *testing.T) {
	f := newFixture(t)
	f.eng.ReplayDelay = 10 * time.Millisecond
	tr := &http.Transport{MaxIdleConnsPerHost: 4}
	t.Cleanup(tr.CloseIdleConnections)
	f.eng.Client = &http.Client{Transport: tr}

	// 自建可劫持關閉複用連線的上游伺服器
	var hits atomic.Int32
	brokenUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 2 {
			// 第 2 次命中複用連線時，不回傳任何位元組直接斷開底層 TCP 觸發 EOF
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okUpstreamJSON))
	}))
	t.Cleanup(brokenUp.Close)

	oldFB := config.UpstreamZaiFallback
	config.UpstreamZaiFallback = brokenUp.URL + "/fallback"
	t.Cleanup(func() { config.UpstreamZaiFallback = oldFB })

	acc, _ := f.st.AddAccount(model.ProviderZai, "replay-acc", "sk-replay")

	// 第 1 次請求：建立 keep-alive 閒置連線
	if status, raw := f.post(t, msgBody(), "sk-test"); status != http.StatusOK {
		t.Fatalf("暖連線請求應 200: %d %s", status, raw)
	}
	// 第 2 次請求：複用連線遭對端無聲關閉 → 透明重放 1 次成功 200
	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusOK || !strings.Contains(raw, "msg_1") {
		t.Fatalf("複用斷連應透明重放成功 200: %d %s", status, raw)
	}
	if hits.Load() != 3 {
		t.Fatalf("上游應共收到 3 次（暖連線 + 斷開 + 重放），實際: %d", hits.Load())
	}
	if got := f.st.Find(model.ProviderZai, acc.ID); got.Status != model.StatusActive {
		t.Fatalf("透明重放成功後帳號必須維持 active，實際: %s", got.Status)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestUploadStallGuardCancelsHungStream(t *testing.T) {
	f := newFixture(t)
	f.eng.UploadStallTimeout = 40 * time.Millisecond

	a1, _ := f.st.AddAccount(model.ProviderZai, "hung-acc", "sk-1")
	_, _ = f.st.AddAccount(model.ProviderZai, "healthy-acc", "sk-2")

	var calls atomic.Int32
	f.eng.Client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			n := calls.Add(1)
			if n == 1 {
				// 刻意不讀 r.Body，模擬 H2 上傳流控視窗歸零掛死
				<-r.Context().Done()
				return nil, context.Cause(r.Context())
			}
			_, _ = io.Copy(io.Discard, r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(okUpstreamJSON)),
			}, nil
		}),
	}

	start := time.Now()
	status, raw := f.post(t, msgBody(), "sk-test")
	elapsed := time.Since(start)
	if status != http.StatusOK || !strings.Contains(raw, "msg_1") {
		t.Fatalf("上傳停滯逾時後應切換健康帳號成功 200: %d %s", status, raw)
	}
	if calls.Load() != 2 {
		t.Fatalf("應嘗試 2 次（首號停滯中斷 + 次號成功），實際: %d", calls.Load())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("上傳停滯看門狗未準時觸發，耗時過長: %v", elapsed)
	}
	if got := f.st.Find(model.ProviderZai, a1.ID); got.Status != model.StatusActive {
		t.Fatalf("上傳流控停滯不應將首號誤標 cooling，實際: %s", got.Status)
	}
}

func TestConcurrencyRetryReleasesGateLeaseDuringSleep(t *testing.T) {
	f := newFixture(t)
	oldBrowser, oldCap := config.CaptchaBrowserEnabled, config.StartPlanAccountConcurrency
	config.CaptchaBrowserEnabled = true
	config.StartPlanAccountConcurrency = 1
	t.Cleanup(func() {
		config.CaptchaBrowserEnabled = oldBrowser
		config.StartPlanAccountConcurrency = oldCap
	})

	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, Prefix: "p", Region: "cn", SceneID: "s"}, nil
	})
	f.cm.SetSolver(stubSolver{token: "tok-gate"})
	f.eng.Gate = NewStartPlanGate()
	f.eng.BusyRetryDelays = []time.Duration{150 * time.Millisecond}

	if _, err := f.st.AddAccount(model.ProviderZai, "solo-jwt", "header.payload.sig"); err != nil {
		t.Fatal(err)
	}

	aGot3008 := make(chan struct{})
	bSucceeded := make(chan struct{})
	var reqACall atomic.Int32

	f.respond = func(_ int, r *http.Request) (int, http.Header, string) {
		var payload map[string]any
		// 透過 x-client-tag 區分請求 A 與請求 B
		if r.Header.Get("X-Test-Caller") == "A" {
			if reqACall.Add(1) == 1 {
				close(aGot3008)
				return http.StatusTooManyRequests, http.Header{"Content-Type": []string{"application/json"}},
					`{"code":3008,"msg":"user concurrency limit exceeded"}`
			}
			return http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, okUpstreamJSON
		}
		_ = payload
		select {
		case <-bSucceeded:
		default:
			close(bSucceeded)
		}
		return http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, okUpstreamJSON
	}

	postWithCaller := func(caller string) (int, string) {
		data, _ := json.Marshal(msgBody())
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/messages", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "sk-test")
		req.Header.Set("X-Test-Caller", caller)
		resp, err := f.srv.Client().Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	aDone := make(chan int, 1)
	go func() {
		st, _ := postWithCaller("A")
		aDone <- st
	}()

	select {
	case <-aGot3008:
	case <-time.After(2 * time.Second):
		t.Fatal("等待請求 A 收到 3008 逾時")
	}

	// 請求 A 正處於 150ms 退避睡眠中：此時發起請求 B，必須能在 A 醒來前立刻取得唯一槽位完成
	bStart := time.Now()
	bStatus, bRaw := postWithCaller("B")
	bElapsed := time.Since(bStart)
	if bStatus != http.StatusOK {
		t.Fatalf("請求 B 在 A 睡眠期間應立即成功 200: %d %s", bStatus, bRaw)
	}
	if bElapsed >= 120*time.Millisecond {
		t.Fatalf("請求 B 疑遭 A 睡眠佔槽阻塞，耗時 %v（應遠小於 150ms）", bElapsed)
	}

	select {
	case aStatus := <-aDone:
		if aStatus != http.StatusOK {
			t.Fatalf("請求 A 醒來隊首重取槽位後應成功 200，實際: %d", aStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待請求 A 完成逾時")
	}
}
