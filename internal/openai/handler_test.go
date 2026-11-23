// 端到端测试：httptest mock 上游，覆盖非流式、流式、工具调用三种场景
// （M4 验收）以及鉴权、白名单、/v1/models 双兼容超集。
package openai

import (
	"context"
	"encoding/json"
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
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// upstreamBody 记录一次上游收包。
type upstreamBody struct {
	Header http.Header
	Body   map[string]any
}

// fixture 完整链路：openai handler + gateway engine + mock 上游。
// 账号用 api_key 模式（不触发验证码/zcode_system 注入，保持离线稳定；
// JWT 注入与验证码语义已由 gateway 包 e2e 覆盖）。
type fixture struct {
	srv      *httptest.Server
	upstream *httptest.Server
	st       *store.Store

	mu      sync.Mutex
	calls   []upstreamBody
	respond func(call int) (int, string, string) // status, contentType, body
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	oldDB, oldZai, oldData := config.DBPath, config.UpstreamZai, config.DataDir
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	config.DataDir = t.TempDir()
	t.Cleanup(func() { config.DBPath, config.UpstreamZai, config.DataDir = oldDB, oldZai, oldData })

	f := &fixture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		f.mu.Lock()
		f.calls = append(f.calls, upstreamBody{Header: r.Header.Clone(), Body: parsed})
		n := len(f.calls)
		respond := f.respond
		f.mu.Unlock()
		status, contentType, body := http.StatusBadGateway, "application/json", `{"error":"no script"}`
		if respond != nil {
			status, contentType, body = respond(n)
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	f.upstream = up
	t.Cleanup(up.Close)
	config.UpstreamZai = up.URL + "/zai"
	config.UpstreamZaiFallback = up.URL + "/fallback"

	st, err := store.New()
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f.st = st
	if _, err := st.AddAccount(model.ProviderZai, "e2e-acc", "sk-e2e-credential"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("gateway_key", "sk-test"); err != nil {
		t.Fatal(err)
	}

	cm := captcha.NewManager()
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, Prefix: "no8xfe", Region: "sgp", SceneID: "11xygtvd"}, nil
	})

	eng := gateway.NewEngine(st, cm, nil)
	eng.BusyRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	mux := http.NewServeMux()
	gw := gateway.Handler{Engine: eng, Auth: auth.New(st)}
	gw.Register(mux)
	New(eng, auth.New(st)).Register(mux)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// postChat 发起 OpenAI 请求并返回状态码与响应体。
func (f *fixture) postChat(t *testing.T, key, payload string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/chat/completions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// lastUpstream 取最近一次上游收包。
func (f *fixture) lastUpstream() upstreamBody {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func TestChatCompletionsNonStream(t *testing.T) {
	// 非流式：OpenAI 请求 → Anthropic 上游 → OpenAI 响应；上游收包应为
	// Anthropic 形态（system 归并、max_tokens 映射）
	f := newFixture(t)
	f.respond = func(int) (int, string, string) {
		return http.StatusOK, "application/json",
			`{"id":"msg_a","type":"message","role":"assistant","model":"GLM-5.3",
			  "stop_reason":"end_turn","usage":{"input_tokens":6,"output_tokens":3},
			  "content":[{"type":"text","text":"回答"}]}`
	}

	code, body := f.postChat(t, "sk-test", `{
		"model":"glm-5.3-flash","max_tokens":100,"stream":false,
		"messages":[
			{"role":"system","content":"系统指令"},
			{"role":"user","content":"问题"}]}`)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %s", code, body)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp["id"] != "chatcmpl-msg_a" || resp["object"] != "chat.completion" {
		t.Fatalf("OpenAI 响应形态不符: %v", resp)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "回答" || choice["finish_reason"] != "stop" {
		t.Fatalf("choices[0] 不符: %v", choice)
	}
	usage := resp["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(6) || usage["completion_tokens"] != float64(3) {
		t.Fatalf("usage 应由顶层映射: %v", usage)
	}

	// 上游收包形态
	up := f.lastUpstream()
	if up.Body["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens 应映射: %v", up.Body)
	}
	// api_key 账号无 zcode_system 注入，system 恰为归并的一个 text 块
	system, ok := up.Body["system"].([]any)
	if !ok || len(system) != 1 || system[0].(map[string]any)["text"] != "系统指令" {
		t.Fatalf("system 应为归并的单块: %v", up.Body["system"])
	}
}

func TestChatCompletionsStream(t *testing.T) {
	// 流式：SSE 重编码为 OpenAI chunk 流
	f := newFixture(t)
	f.respond = func(int) (int, string, string) {
		return http.StatusOK, "text/event-stream",
			"event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_s","model":"GLM-5.3","usage":{"input_tokens":4}}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"流式回答"}}` + "\n\n" +
				"event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n"
	}

	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3-flash","stream":true,
			"stream_options":{"include_usage":true},
			"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("应为 SSE: %s", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)

	// Go map 序列化按键字母序，delta 键序不保证，分键断言
	if !strings.Contains(text, `"role":"assistant"`) || !strings.Contains(text, `"content":""`) {
		t.Fatalf("首 chunk 应带 role: %s", text)
	}
	if !strings.Contains(text, `"content":"流式回答"`) {
		t.Fatalf("应有 content 增量: %s", text)
	}
	if !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("应有终止 chunk: %s", text)
	}
	// include_usage：prompt = 4（无缓存），completion = 2
	if !strings.Contains(text, `"prompt_tokens":4`) || !strings.Contains(text, `"completion_tokens":2`) {
		t.Fatalf("应有 usage chunk: %s", text)
	}
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("流应以 [DONE] 收尾: %q", text)
	}
}

func TestChatCompletionsToolCall(t *testing.T) {
	// 工具调用：tools/tool_choice 转换 + tool_use → tool_calls
	f := newFixture(t)
	f.respond = func(int) (int, string, string) {
		return http.StatusOK, "application/json",
			`{"id":"msg_t","type":"message","role":"assistant","model":"GLM-5.3",
			  "stop_reason":"tool_use","usage":{"input_tokens":9,"output_tokens":5},
			  "content":[{"type":"tool_use","id":"call_77","name":"get_weather",
			              "input":{"city":"北京"}}]}`
	}

	code, body := f.postChat(t, "sk-test", `{
		"model":"glm-5.3-flash","max_tokens":512,
		"tools":[{"type":"function","function":{"name":"get_weather",
			"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
		"tool_choice":"auto",
		"messages":[{"role":"user","content":"北京天气？"}]}`)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %s", code, body)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason 应为 tool_calls: %v", choice)
	}
	msg := choice["message"].(map[string]any)
	calls, _ := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("应有一条 tool_calls: %v", msg)
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_77" || call["type"] != "function" {
		t.Fatalf("tool_calls 项不符: %v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"北京"}` {
		t.Fatalf("function 不符: %v", fn)
	}

	// 上游收到 Anthropic tools 形态
	up := f.lastUpstream()
	tools, _ := up.Body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("上游应收到 1 个工具: %v", up.Body["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" {
		t.Fatalf("工具名应保留: %v", tool)
	}
	if _, ok := tool["input_schema"]; !ok {
		t.Fatal("parameters 应映射为 input_schema")
	}
}

func TestChatCompletionsAuthAndWhitelist(t *testing.T) {
	f := newFixture(t)

	// 鉴权 fail-closed
	code, _ := f.postChat(t, "", `{"model":"glm-5.3-flash","messages":[]}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("缺密钥应 401: %d", code)
	}
	code, _ = f.postChat(t, "sk-wrong", `{"model":"glm-5.3-flash","messages":[]}`)
	if code != http.StatusForbidden {
		t.Fatalf("错密钥应 403: %d", code)
	}

	// 白名单已移除：任意模型名直接转发上游（fixture 默认 respond 为 502，
	// 只验证请求确实出站、不再本地 400）
	code, body := f.postChat(t, "sk-test", `{"model":"gpt-4o","messages":[]}`)
	if code != http.StatusBadGateway {
		t.Fatalf("白名单移除后应转发上游: %d %s", code, body)
	}

	// 非法 JSON 400
	code, _ = f.postChat(t, "sk-test", "{bad")
	if code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应 400: %d", code)
	}
}

func TestModelsDualCompatibility(t *testing.T) {
	// /v1/models 双兼容超集：Anthropic 字段 + OpenAI 字段并存
	f := newFixture(t)
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	if payload["object"] != "list" {
		t.Fatalf("顶层 object 应为 list: %v", payload)
	}
	items := payload["data"].([]any)
	if len(items) == 0 {
		t.Fatal("应有模型清单")
	}
	first := items[0].(map[string]any)
	for _, key := range []string{"id", "type", "display_name", "created_at", "object", "created", "owned_by"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("模型项应含 %s: %v", key, first)
		}
	}
	if first["object"] != "model" || first["owned_by"] != "zcode2api" {
		t.Fatalf("OpenAI 侧字段不符: %v", first)
	}
}
