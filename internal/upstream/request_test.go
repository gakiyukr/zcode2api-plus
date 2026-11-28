package upstream

import (
	"encoding/json"
	"maps"
	"strings"
	"sync"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/fingerprint"
	"zcode2api/internal/model"
)

func jwtAccount() *model.Account  { return model.Create(model.ProviderZai, "t", "header.payload.sig") }
func keyAccount() *model.Account  { return model.Create(model.ProviderZai, "t", "sk-secret") }
func bareAccount() *model.Account { return &model.Account{Provider: model.ProviderZai, Mode: "jwt"} }

func TestBuildRequestJWT(t *testing.T) {
	acc := jwtAccount()
	req, err := BuildRequest(acc, "server-token", "sgp", nil)
	if err != nil {
		t.Fatal(err)
	}
	sku := fingerprint.For(acc)
	if req.URL != config.UpstreamZai {
		t.Fatalf("JWT 应走主端点: %s", req.URL)
	}
	h := req.Headers
	if h["Authorization"] != "Bearer header.payload.sig" {
		t.Fatalf("Authorization 不符: %q", h["Authorization"])
	}
	if h["Content-Type"] != "application/json" || h["Anthropic-Version"] != "2023-06-01" {
		t.Fatalf("固定头不符: %v", h)
	}
	if h["X-ZCode-App-Version"] != config.ZcodeClientVersion || h["X-ZCode-Agent"] != "glm" {
		t.Fatalf("ZCode 标识头不符: %v", h)
	}
	// 官方 3.17.2 客户端的 messages 请求不带 X-Device-Mid（仅账号管理 API 携带）
	if _, ok := h["X-Device-Mid"]; ok {
		t.Fatal("messages 请求不应携带 X-Device-Mid")
	}
	// 官方新增的客户端特征头：必须与账号 SKU 同源，而非部署宿主
	if h["X-Platform"] != sku.Platform || h["X-Os-Category"] != sku.OsCategory ||
		h["X-Os-Version"] != sku.OsVersion || h["X-Client-Language"] != sku.Language ||
		h["X-Client-Timezone"] != sku.Timezone {
		t.Fatalf("平台头与账号 SKU 不符: %v vs %+v", h, sku)
	}
	if h["X-Title"] != "Z Code@electron" || h["X-Release-Channel"] != "production" {
		t.Fatalf("客户端标识头不符: %v", h)
	}
	if h["x-zcode-query-source"] != "main_turn" || h["x-zcode-session-type"] != "main" {
		t.Fatalf("3.16 attribution 头不符: %v", h)
	}
	if h["x-request-id"] == "" || h["x-zcode-trace-id"] == "" || h["x-session-id"] == "" {
		t.Fatal("请求/追踪/会话 id 应生成")
	}
	if h["User-Agent"] != config.UserAgent {
		t.Fatalf("UA 不符: %q", h["User-Agent"])
	}
	if h["X-Aliyun-Captcha-Verify-Param"] != "server-token" ||
		h["X-Aliyun-Captcha-Verify-Region"] != "sgp" {
		t.Fatalf("验证码头不符: %v", h)
	}
}

func TestBuildRequestAPIKey(t *testing.T) {
	req, err := BuildRequest(keyAccount(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL != config.UpstreamZaiFallback {
		t.Fatalf("API Key 应走回退端点: %s", req.URL)
	}
	if req.Headers["x-api-key"] != "sk-secret" {
		t.Fatalf("x-api-key 不符: %v", req.Headers)
	}
	if _, ok := req.Headers["X-Aliyun-Captcha-Verify-Param"]; ok {
		t.Fatal("无验证码令牌时不应携带验证码头")
	}
	if _, ok := req.Headers["X-Aliyun-Captcha-Verify-Region"]; ok {
		t.Fatal("无 region 时不应携带 region 头")
	}
}

func TestBuildRequestNoCredentials(t *testing.T) {
	if _, err := BuildRequest(bareAccount(), "", "", nil); err == nil {
		t.Fatal("缺少凭证应报错")
	}
	foreign := &model.Account{Provider: "unknown", Mode: "apiKey", APIKey: strPtr("k")}
	if _, err := BuildRequest(foreign, "", "", nil); err == nil {
		t.Fatal("未知提供商应报错")
	}
}

func TestClientHeadersFiltered(t *testing.T) {
	incoming := map[string]string{
		"Cookie":                         "session=1",
		"Authorization":                  "Bearer client-key",
		"X-ZCode-Agent":                  "spoof",
		"x-zcode-app-version":            "9.9.9",
		"x-aliyun-captcha-verify-param":  "client-token",
		"X-Aliyun-Captcha-Verify-Region": "client-region",
		"X-Custom-Trace":                 "keep-me",
		// agent 特征头：官方客户端从不发送，混入即触发风控（magpie 0.1.22）
		"Anthropic-Beta":      "claude-code-20250219",
		"X-App":               "cli",
		"X-Stainless-Timeout": "30",
		"X-Claude-Code-Run":   "abc",
		// 设备指纹已从 messages 请求移除（官方不发送）：客户端伪造也不应复活它
		"X-Device-Mid":      "spoofed-fingerprint",
		"Content-Type":      "text/plain",
		"Anthropic-Version": "1999-01-01",
	}
	req, err := BuildRequest(jwtAccount(), "server-token", "sgp", incoming)
	if err != nil {
		t.Fatal(err)
	}
	h := req.Headers
	if _, ok := h["Cookie"]; ok {
		t.Fatal("cookie 应被剔除")
	}
	if h["Authorization"] != "Bearer header.payload.sig" {
		t.Fatal("客户端不得覆盖鉴权头")
	}
	if _, ok := h["X-ZCode-Agent"]; !ok || h["X-ZCode-Agent"] == "spoof" {
		t.Fatal("x-zcode* 透传应被剔除")
	}
	if _, ok := h["x-zcode-app-version"]; ok {
		t.Fatal("x-zcode*（小写）透传应被剔除")
	}
	if h["X-Aliyun-Captcha-Verify-Param"] != "server-token" {
		t.Fatal("客户端不得覆盖验证码头")
	}
	if h["X-Aliyun-Captcha-Verify-Region"] != "sgp" {
		t.Fatal("客户端不得覆盖验证码 region 头")
	}
	// agent 特征头一律剔除
	for _, name := range []string{"Anthropic-Beta", "X-App", "X-Stainless-Timeout", "X-Claude-Code-Run"} {
		if _, ok := h[name]; ok {
			t.Fatalf("agent 特征头 %s 应被剔除", name)
		}
	}
	if _, ok := h["X-Device-Mid"]; ok {
		t.Fatalf("messages 请求不应复活设备指纹头: %q", h["X-Device-Mid"])
	}
	if h["Content-Type"] != "application/json" {
		t.Fatalf("客户端不得覆盖 Content-Type: %q", h["Content-Type"])
	}
	if h["Anthropic-Version"] != "2023-06-01" {
		t.Fatalf("客户端不得覆盖 Anthropic-Version: %q", h["Anthropic-Version"])
	}
	if _, ok := h["X-Custom-Trace"]; ok {
		t.Fatalf("未知私有头不得透传（严格白名单）: %v", h)
	}
}

func TestZcodePrompts(t *testing.T) {
	p := ZcodePrompts()
	if p == nil {
		t.Fatal("zcode_system.json 应解析出官方提示词段")
	}
	if !strings.HasPrefix(p.CliPrefix, "You are ZCode") {
		t.Fatalf("cliPrefix 不符: %q", p.CliPrefix)
	}
	if len(p.StableSections) != 2 {
		t.Fatalf("官方应有两段 stable: %d", len(p.StableSections))
	}
	again := ZcodePrompts()
	if again != p {
		t.Fatal("重复调用应返回同一份缓存")
	}
}

func TestDressSystemOfficialShape(t *testing.T) {
	body := map[string]any{
		"model": "GLM-5.3",
		"system": []any{
			map[string]any{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude."},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"metadata": map[string]any{"user_id": "user_x_account_y_session_z"},
	}
	DressSystem(body)

	sys, ok := body["system"].([]any)
	if !ok || len(sys) != 3 {
		t.Fatalf("system 应恰好三块: %v", body["system"])
	}
	// 调用方 metadata 被官方形态覆盖
	meta := body["metadata"].(map[string]any)
	var uid map[string]any
	if err := json.Unmarshal([]byte(meta["user_id"].(string)), &uid); err != nil {
		t.Fatalf("user_id 应为 JSON: %v", err)
	}
	if uid["account_uuid"] != "" {
		t.Fatalf("官方形态 account_uuid 为空: %v", uid)
	}
	if uid["device_id"] == "" {
		t.Fatal("device_id 不应为空")
	}
}

func TestSessionIDStable(t *testing.T) {
	a := SessionID("device-1")
	b := SessionID("device-1")
	c := SessionID("device-2")
	if a != b {
		t.Fatal("同一 device 的会话 id 应稳定")
	}
	if a == c {
		t.Fatal("不同 device 的会话 id 应不同")
	}
	if len(a) != 36 || strings.Count(a, "-") != 4 {
		t.Fatalf("应为 UUID 形态: %q", a)
	}
}

func TestSessionIDFromHeadersAndQueryID(t *testing.T) {
	// 客戶端已是標準 UUID → 原樣保留（小寫）
	wantUUID := "11111111-2222-3333-4444-555555555555"
	if got := SessionIDFromHeaders(map[string]string{"X-Session-Id": strings.ToUpper(wantUUID)}, "dev-1"); got != wantUUID {
		t.Fatalf("標準 UUID 應直接沿用: %q", got)
	}
	// 非 UUID 字串 → 穩定派生 UUID v5
	s1 := SessionIDFromHeaders(map[string]string{"x-claude-code-session-id": "conv-abc"}, "dev-1")
	s2 := SessionIDFromHeaders(map[string]string{"x-claude-code-session-id": "conv-abc"}, "dev-2")
	if s1 != s2 || len(s1) != 36 {
		t.Fatalf("相同外部會話 ID 應派生相同 UUID: %q vs %q", s1, s2)
	}

	// QueryID：同一輪含 tool_result 不變，下一輪新使用者訊息自動換新
	bodyTurn1 := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "查天氣"},
		},
	}
	bodyTurn1Tool := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "查天氣"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "t1"}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "晴"}}},
		},
	}
	bodyTurn2 := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "查天氣"},
			map[string]any{"role": "assistant", "content": "晴天"},
			map[string]any{"role": "user", "content": "明天呢？"},
		},
	}
	q1 := QueryID(bodyTurn1, s1)
	q1Tool := QueryID(bodyTurn1Tool, s1)
	q2 := QueryID(bodyTurn2, s1)
	if q1 == "" || q1 != q1Tool {
		t.Fatalf("同輪工具結果不應變更 x-query-id: %q vs %q", q1, q1Tool)
	}
	if q2 == "" || q2 == q1 {
		t.Fatalf("下一輪使用者提問應產生新 x-query-id: %q vs %q", q1, q2)
	}
}

func TestPerAccountDeviceMid(t *testing.T) {
	a1 := model.Create(model.ProviderZai, "u1", "h1.p1.s1")
	a2 := model.Create(model.ProviderZai, "u2", "h2.p2.s2")
	if a1.DeviceMid() == "" || a1.DeviceMid() == a2.DeviceMid() {
		t.Fatalf("不同帳號應有各自獨立且穩定的 device_mid: %q vs %q", a1.DeviceMid(), a2.DeviceMid())
	}
	body := map[string]any{"model": "GLM-5.4", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	DressSystemFor(body, a1.DeviceMid(), "11111111-2222-3333-4444-555555555555")
	meta := body["metadata"].(map[string]any)
	var uid map[string]any
	if err := json.Unmarshal([]byte(meta["user_id"].(string)), &uid); err != nil {
		t.Fatal(err)
	}
	if uid["device_id"] != a1.DeviceMid() || uid["session_id"] != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("metadata.user_id 應與帳號 device_mid 及 session_id 一致: %v", uid)
	}
}

func TestDropHeadersIncludesReverseProxyHeaders(t *testing.T) {
	incoming := map[string]string{
		"X-Forwarded-For":     "1.2.3.4",
		"X-Forwarded-Proto":   "https",
		"X-Real-IP":           "1.2.3.4",
		"Forwarded":           "for=1.2.3.4",
		"Via":                 "1.1 proxy",
		"CF-Connecting-IP":    "1.2.3.4",
		"CF-Ray":              "abc-123",
		"CF-Visitor":          `{"scheme":"https"}`,
		"True-Client-IP":      "1.2.3.4",
		"Origin":              "https://evil.example",
		"Proxy-Authorization": "Basic secret",
		"Sec-Fetch-Mode":      "cors",
		"X-Query-Id":          "spoof-query-id",
		"X-Custom-Allowed":    "ok",
	}
	req, err := BuildRequest(jwtAccount(), "tok", "cn", incoming)
	if err != nil {
		t.Fatal(err)
	}
	for _, blocked := range []string{
		"X-Forwarded-For", "X-Forwarded-Proto", "X-Real-IP", "Forwarded", "Via",
		"CF-Connecting-IP", "CF-Ray", "CF-Visitor", "True-Client-IP",
		"Origin", "Proxy-Authorization", "Sec-Fetch-Mode", "X-Query-Id",
	} {
		if _, ok := req.Headers[blocked]; ok {
			t.Fatalf("反代/瀏覽器/偽造標頭 %s 應被剔除", blocked)
		}
	}
	if _, ok := req.Headers["X-Custom-Allowed"]; ok {
		t.Fatalf("未知自訂標頭不得透傳: %v", req.Headers)
	}

	// SetQueryID 應清理大小寫變體並統一寫入小寫 x-query-id
	req.Headers["X-Query-Id"] = "legacy-upper"
	SetQueryID(&req, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if _, exists := req.Headers["X-Query-Id"]; exists {
		t.Fatal("SetQueryID 應刪除大寫 X-Query-Id 變體")
	}
	if req.Headers["x-query-id"] == "" {
		t.Fatal("SetQueryID 應寫入小寫 x-query-id")
	}
}

func TestDressSystemFor_NoSharedMapMutation(t *testing.T) {
	origMeta := map[string]any{"trace": "keep"}
	origBlock := map[string]any{"type": "text", "text": "hello", "cache_control": map[string]any{"type": "ephemeral"}}
	origMsg := map[string]any{"role": "user", "content": []any{origBlock}}
	origTool := map[string]any{"name": "bash", "cache_control": map[string]any{"type": "ephemeral"}}
	base := map[string]any{
		"model":    "GLM-5.4",
		"metadata": origMeta,
		"messages": []any{origMsg},
		"tools":    []any{origTool},
	}

	// 單次呼叫不得改寫原 metadata / message / content block / tool map
	copy1 := maps.Clone(base)
	DressSystemFor(copy1, "dev-1", "sess-1")
	if _, mutated := origMeta["user_id"]; mutated {
		t.Fatalf("原 metadata map 不應被就地修改: %v", origMeta)
	}
	if _, ok := origBlock["cache_control"]; !ok {
		t.Fatal("原 message content block 的 cache_control 不應被就地刪除")
	}
	if _, ok := origTool["cache_control"]; !ok {
		t.Fatal("原 tool 的 cache_control 不應被就地刪除")
	}

	// 多 goroutine 併發共享同一份原始子 map 應無競態且各自獨立
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := maps.Clone(base)
			dev := "dev-" + string(rune('a'+idx))
			sess := "sess-" + string(rune('a'+idx))
			DressSystemFor(req, dev, sess)
			meta := req["metadata"].(map[string]any)
			if !strings.Contains(meta["user_id"].(string), dev) {
				t.Errorf("併發請求 metadata.user_id 遭污染: %v", meta["user_id"])
			}
		}(i)
	}
	wg.Wait()
}

func TestAccountSessionID_SameAccountStableCrossAccountIsolated(t *testing.T) {
	headers := map[string]string{"X-Session-Id": "conv-shared-999"}
	s1a := AccountSessionID(headers, "dev-account-1")
	s1b := AccountSessionID(map[string]string{"x-session-id": "CONV-SHARED-999"}, "dev-account-1")
	s2 := AccountSessionID(headers, "dev-account-2")

	if s1a == "" || s1a != s1b {
		t.Fatalf("同一帳號對同一客戶端會話鍵應派生穩定 UUIDv5: %q vs %q", s1a, s1b)
	}
	if s1a == s2 {
		t.Fatalf("不同帳號對同一客戶端會話鍵必須派生不同 UUIDv5 防跨號關聯: %q", s1a)
	}
	if s1a == "conv-shared-999" || strings.Count(s1a, "-") != 4 {
		t.Fatalf("結果應為標準 UUID 形態: %q", s1a)
	}

	// BuildRequest 應剃除 x-conversation-id 並使用跨號隔離後的 x-session-id
	a1 := model.Create(model.ProviderZai, "u1", "h1.p1.s1")
	a2 := model.Create(model.ProviderZai, "u2", "h2.p2.s2")
	in := map[string]string{"X-Conversation-Id": "conv-abc"}
	r1, err := BuildRequest(a1, "tok", "cn", in)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := BuildRequest(a2, "tok", "cn", in)
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := r1.Headers["X-Conversation-Id"]; leaked {
		t.Fatal("x-conversation-id 不得原樣洩漏給上游")
	}
	if r1.Headers["x-session-id"] == r2.Headers["x-session-id"] {
		t.Fatalf("不同帳號的 x-session-id 應互相隔離: %q", r1.Headers["x-session-id"])
	}
}

func TestUpstreamQueryIsStripped(t *testing.T) {
	old := config.UpstreamZai
	config.UpstreamZai = old + "?beta=true&trace=1"
	t.Cleanup(func() { config.UpstreamZai = old })

	req, err := BuildRequest(jwtAccount(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(req.URL, "?") {
		t.Fatalf("messages 端點不應保留 query: %s", req.URL)
	}
}

func TestDressSystemSanitizesCallerBillingLines(t *testing.T) {
	acc := jwtAccount()
	body := map[string]any{
		"model": "GLM-5.4",
		"system": "x-anthropic-billing-header: secret\n" +
			"cc_version=9.9.9\n" +
			"keep this business instruction",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	DressSystemForSKU(body, acc.DeviceMid(), "11111111-2222-3333-4444-555555555555", fingerprint.For(acc))
	system := body["system"].([]any)
	text := system[2].(map[string]any)["text"].(string)
	if strings.Contains(text, "billing-header") || strings.Contains(text, "cc_version=") {
		t.Fatalf("第三方計費特徵行未清洗: %q", text)
	}
	if !strings.Contains(text, "keep this business instruction") {
		t.Fatalf("正常呼叫方文本被誤刪: %q", text)
	}
}

func TestRequestAndSystemShareSKU(t *testing.T) {
	acc := jwtAccount()
	sku := fingerprint.For(acc)
	req, err := BuildRequest(acc, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Headers["X-Platform"] != sku.Platform || req.Headers["X-Os-Version"] != sku.OsVersion {
		t.Fatalf("請求頭與 SKU 不一致: %v vs %+v", req.Headers, sku)
	}

	body := map[string]any{"model": "GLM-5.4", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	DressSystemForSKU(body, acc.DeviceMid(), "11111111-2222-3333-4444-555555555555", sku)
	system := body["system"].([]any)
	text := system[2].(map[string]any)["text"].(string)
	if !strings.Contains(text, sku.PlatformName+" "+sku.OsVersion+" "+sku.Arch) {
		t.Fatalf("system environment 與請求頭 SKU 不一致: %q", text)
	}
}

func strPtr(s string) *string { return &s }
