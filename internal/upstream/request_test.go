package upstream

import (
	"encoding/json"
	"strings"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func jwtAccount() *model.Account  { return model.Create(model.ProviderZai, "t", "header.payload.sig") }
func keyAccount() *model.Account  { return model.Create(model.ProviderZai, "t", "sk-secret") }
func bareAccount() *model.Account { return &model.Account{Provider: model.ProviderZai, Mode: "jwt"} }

func TestBuildRequestJWT(t *testing.T) {
	req, err := BuildRequest(jwtAccount(), "server-token", "sgp", nil)
	if err != nil {
		t.Fatal(err)
	}
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
	// 官方新增的客户端特征头
	if h["X-Platform"] != config.ClientPlatform() || h["X-Os-Category"] != config.OsCategory() {
		t.Fatalf("平台头不符: %v", h)
	}
	if h["X-Title"] != "Z Code@cli" || h["X-Release-Channel"] != "production" {
		t.Fatalf("客户端标识头不符: %v", h)
	}
	if h["x-zcode-query-source"] != "main_turn" || h["x-zcode-session-type"] != "main" {
		t.Fatalf("3.16 attribution 头不符: %v", h)
	}
	if h["x-request-id"] == "" || h["x-zcode-trace-id"] == "" {
		t.Fatal("请求/追踪 id 应每次生成")
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
	if h["X-Custom-Trace"] != "keep-me" {
		t.Fatalf("普通透传头应保留: %v", h)
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

func strPtr(s string) *string { return &s }
