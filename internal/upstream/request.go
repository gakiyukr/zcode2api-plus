// Package upstream 构造对 ZCode 上游的请求。
// 对应 Python 版 app/agent.py：按账号凭证选端点、组装请求头。
// 实际发送与流式透传在 internal/gateway。
//
// 请求形态对齐 ZCode 3.17.2 官方客户端（移植自 magpie 0.1.52 的 zcode 插件，
// MIT License, Copyright (c) 2026 yetone；prompt 文本取自 ZCode 3.17.2 官方
// 源码 a9fe401）：上游对 Start Plan 请求做 prompt 文本与请求头的逐项校验，
// 不合形态的请求返回 405/3012 "request has been blocked due to unusual
// activity"，而官方客户端同机同账号畅通。
package upstream

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/util"
)

//go:embed zcode_system.json
var zcodeSystemJSON []byte

var (
	zcodeOnce sync.Once
	zcodeText *zcodePrompts
)

// zcodePrompts 是 zcode_system.json 的结构：ZCode 3.17.2 官方客户端的
// system 提示词段文本。上游按 byte-for-byte 校验前两块；第三块按官方
// ContextBuilder 的顺序拼接动态段（行为、技能指引、环境、模型、上下文管理、
// 调用方 system），外部 agent 的 system 文本并入第三块末尾。
type zcodePrompts struct {
	CliPrefix         string   `json:"cliPrefix"`
	StableSections    []string `json:"stableSections"`
	DynamicBehavior   string   `json:"dynamicBehavior"`
	SessionGuidance   string   `json:"sessionGuidance"`
	ContextManagement string   `json:"contextManagement"`
	PoweredBy         string   `json:"poweredBy"`
	Environment       struct {
		Heading        string `json:"heading"`
		InvokedLine    string `json:"invokedLine"`
		CwdLabel       string `json:"cwdLabel"`
		GitLabel       string `json:"gitLabel"`
		GitYes         string `json:"gitYes"`
		GitNo          string `json:"gitNo"`
		PlatformLabel  string `json:"platformLabel"`
		ShellLabel     string `json:"shellLabel"`
		OsVersionLabel string `json:"osVersionLabel"`
		UntrustedNote  string `json:"untrustedNote"`
	} `json:"environment"`
	Context struct {
		CurrentDateLine string `json:"currentDateLine"`
	} `json:"context"`
}

// ZcodePrompts 返回官方提示词段（JSON 解析失败时为 nil，调用方跳过注入）。
func ZcodePrompts() *zcodePrompts {
	zcodeOnce.Do(func() {
		var p zcodePrompts
		if err := json.Unmarshal(zcodeSystemJSON, &p); err != nil || p.CliPrefix == "" {
			return
		}
		zcodeText = &p
	})
	return zcodeText
}

// dropHeaders 透传客户端 header 时需要剔除的字段。官方客户端的 messages
// 请求只带自己的头（magpie 0.1.22 验证：Claude Code 的 anthropic-beta、
// x-app、x-stainless-* 等一概不发送，混入即触发风控），因此这里按黑名单
// 加前缀双保险剔除 agent 特征头。
var dropHeaders = map[string]bool{
	"host":                           true,
	"content-length":                 true,
	"x-api-key":                      true,
	"authorization":                  true,
	"user-agent":                     true,
	"http-referer":                   true,
	"accept":                         true,
	"accept-encoding":                true,
	"connection":                     true,
	"cookie":                         true,
	"x-aliyun-captcha-verify-param":  true,
	"x-aliyun-captcha-verify-region": true,
	"anthropic-beta":                 true,
	"x-app":                          true,
	"x-request-id":                   true,
	"x-session-id":                   true,
	"x-conversation-id":              true,
	// 官方客户端的 messages 请求不携带 X-Device-Mid：客户端伪造同样剔除，
	// 设备身份经由 body 的 metadata.user_id 传递（见 DressSystem）
	"x-device-mid": true,
	"anthropic-dangerous-direct-browser-access": true,
	"x-query-id":          true,
	"x-forwarded-for":     true,
	"x-real-ip":           true,
	"forwarded":           true,
	"via":                 true,
	"cf-connecting-ip":    true,
	"cf-ray":              true,
	"cf-ipcountry":        true,
	"true-client-ip":      true,
	"origin":              true,
	"proxy-authorization": true,
}

// dropHeaderPrefixes 按前缀剔除的 agent、反代與瀏覽器特徵頭（x-stainless-* 是官方 SDK
// 的遙測頭族；x-claude-code-* / x-magpie-* 是 agent 網關的會話頭；
// x-forwarded-* / sec-* / cf-* 為反向代理或瀏覽器頭）。
var dropHeaderPrefixes = []string{
	"x-stainless-", "x-claude-code-", "x-magpie-",
	"x-forwarded-", "sec-", "cf-",
}

// Request 构造结果：目标 URL 与请求头。
type Request struct {
	URL     string
	Headers map[string]string
}

// BuildRequest 对应 Python 版 build_request：
// JWT 账号走 zcode.z.ai 主端点（Bearer），API Key 账号走 api.z.ai 回退端点（x-api-key）。
// verifyParam/verifyRegion 为验证码令牌（仅 JWT 账号）；incomingHeaders 为客户端透传头。
//
// 固定头对齐官方 3.17.2 客户端的 messages 请求（magpie startHeaders）：
// 不携带 X-Device-Mid（该头只出现在官方的账号管理 API 上，messages 请求
// 没有它）；设备身份经由 body 的 metadata.user_id 传递（见 DressSystem）。
func BuildRequest(acc *model.Account, verifyParam, verifyRegion string, incomingHeaders map[string]string) (Request, error) {
	var targetURL, authHeader, authValue string
	if acc.Provider == model.ProviderZai {
		if acc.Mode == "jwt" && acc.JWTToken != nil {
			targetURL = config.UpstreamZai
			authHeader, authValue = "Authorization", "Bearer "+*acc.JWTToken
		} else if acc.APIKey != nil {
			targetURL = config.UpstreamZaiFallback
			authHeader, authValue = "x-api-key", *acc.APIKey
		} else {
			return Request{}, errors.New("账号缺少有效凭证")
		}
	} else {
		return Request{}, fmt.Errorf("未知提供商: %s", acc.Provider)
	}

	device := accountDeviceMid(acc)
	sessionID := AccountSessionID(incomingHeaders, device)

	// 固定頭一律用 Go 的規範化形式（或明確小寫鍵），並於透傳客戶端頭之後寫回。
	// X-Title 對齊 ZCode 桌面端（Z Code@electron，magpie 0.1.55 實證）；
	// x-session-id 供上游 prompt cache 定位快取節點（不帶時長對話快取命中率極低）。
	fixed := map[string]string{
		"Content-Type":         "application/json",
		authHeader:             authValue,
		"Anthropic-Version":    "2023-06-01",
		"User-Agent":           config.UserAgent,
		"X-ZCode-App-Version":  config.ZcodeClientVersion,
		"X-ZCode-Agent":        "glm",
		"HTTP-Referer":         "https://zcode.z.ai",
		"X-Title":              "Z Code@electron",
		"X-Release-Channel":    "production",
		"X-Client-Language":    config.ClientLanguage,
		"X-Client-Timezone":    config.ClientTimezone,
		"X-Platform":           config.ClientPlatform(),
		"X-Os-Category":        config.OsCategory(),
		"x-request-id":         util.NewUUID(),
		"x-session-id":         sessionID,
		"x-zcode-session-type": "main",
		"x-zcode-trace-id":     util.NewUUID(),
		"x-zcode-query-source": "main_turn",
	}
	if osRel := config.OsRelease(); osRel != "" {
		fixed["X-Os-Version"] = osRel
	}
	if verifyParam != "" {
		fixed["X-Aliyun-Captcha-Verify-Param"] = verifyParam
		if verifyRegion != "" {
			fixed["X-Aliyun-Captcha-Verify-Region"] = verifyRegion
		}
	}

	// 先透传客户端头（剔除 agent 特征/敏感/连接类），再强制写回固定头。
	// 顺序不能颠倒：固定头必须在最后覆盖同名透传头。
	headers := map[string]string{}
	for key, value := range incomingHeaders {
		lower := strings.ToLower(key)
		if dropHeaders[lower] || strings.HasPrefix(lower, "x-zcode") {
			continue
		}
		dropped := false
		for _, prefix := range dropHeaderPrefixes {
			if strings.HasPrefix(lower, prefix) {
				dropped = true
				break
			}
		}
		if dropped {
			continue
		}
		headers[key] = value
	}
	for key, value := range fixed {
		headers[key] = value
	}

	return Request{URL: targetURL, Headers: headers}, nil
}

// DressSystem 按 ZCode 官方客戶端形態重建請求體的 system 與相關欄位（使用全域 device_mid）。
// 若呼叫方已有帳號與會話資訊，應優先使用 DressSystemFor。
func DressSystem(body map[string]any) {
	dev := config.DeviceMid()
	DressSystemFor(body, dev, SessionID(dev))
}

// DressSystemFor 以指定的帳號裝置指紋與會話 ID 重建請求體。
// 不冪等（重複呼叫會重複追加日期前綴），呼叫方須保證對同一 body 只呼叫一次。
//
// 官方形態要點（magpie dress 語義，風控按此校驗）：
//  1. 頂層 system 恰好三塊：cliPrefix、兩段 stable 合併、動態段（官方文本 +
//     呼叫方 system 文本併入塊尾）——上游拒絕第四塊（405/3012）；
//  2. 首個 user turn 之前插入日期 <system-reminder>（官方把當天日期放在
//     user turn 而非 system）；
//  3. cache_control 只出現在 system 三塊與最後一條非 system 訊息的末塊上，
//     訊息中間塊與 tools 上一律清除；
//  4. metadata.user_id 標記裝置與會話（須與 x-session-id 一致，否則命中不了快取）。
func DressSystemFor(body map[string]any, device, session string) {
	p := ZcodePrompts()
	if p == nil || body == nil {
		return
	}
	if strings.TrimSpace(device) == "" {
		device = config.DeviceMid()
	}
	if strings.TrimSpace(session) == "" {
		session = SessionID(device)
	}

	// 调用方原有 system 文本：字串或 blocks 数组，一律并入第三块
	var own []string
	switch existing := body["system"].(type) {
	case string:
		if strings.TrimSpace(existing) != "" {
			own = append(own, existing)
		}
	case []any:
		for _, item := range existing {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t != "text" {
				continue
			}
			text, _ := block["text"].(string)
			if strings.TrimSpace(text) != "" {
				own = append(own, text)
			}
		}
	}

	// 第三块动态段顺序对齐官方 ContextBuilder assembleSystemMessages：
	// 行为段、（有 Skill 工具时的）技能指引、环境、模型、上下文管理、调用方文本
	var dynamic []string
	dynamic = append(dynamic, p.DynamicBehavior)
	if hasSkillTool(body) {
		dynamic = append(dynamic, p.SessionGuidance)
	}
	dynamic = append(dynamic, environment(p))
	if modelName, _ := body["model"].(string); modelName != "" {
		line := strings.ReplaceAll(p.PoweredBy, "{provider}", "zai-api")
		dynamic = append(dynamic, strings.ReplaceAll(line, "{model}", modelName))
	}
	dynamic = append(dynamic, p.ContextManagement)
	dynamic = append(dynamic, own...)

	cached := func(text string) map[string]any {
		return map[string]any{
			"type":          "text",
			"text":          text,
			"cache_control": map[string]any{"type": "ephemeral"},
		}
	}
	body["system"] = []any{
		cached(p.CliPrefix),
		cached(strings.Join(p.StableSections, "\n\n")),
		cached("\n\n" + strings.Join(dynamic, "\n\n")),
	}

	// 日期前綴與 cache_control 重排：複製訊息與區塊 map，避免就地修改呼叫方共享的 body 結構
	dateLine := strings.ReplaceAll(p.Context.CurrentDateLine, "{date}", time.Now().Format("2006-01-02"))
	prefix := map[string]any{
		"role": "user",
		"content": []any{map[string]any{
			"type": "text",
			"text": "<system-reminder>" + dateLine + "</system-reminder>",
		}},
	}
	origMessages, _ := body["messages"].([]any)
	clonedMessages := make([]any, 0, len(origMessages)+1)
	clonedMessages = append(clonedMessages, prefix)
	var last map[string]any
	for _, item := range origMessages {
		origMsg, ok := item.(map[string]any)
		if !ok {
			clonedMessages = append(clonedMessages, item)
			continue
		}
		msg := maps.Clone(origMsg)
		if role, _ := msg["role"].(string); role != "system" {
			last = msg
			if blocks, ok := msg["content"].([]any); ok {
				newBlocks := make([]any, len(blocks))
				for i, b := range blocks {
					if m, ok := b.(map[string]any); ok {
						mc := maps.Clone(m)
						delete(mc, "cache_control")
						newBlocks[i] = mc
					} else {
						newBlocks[i] = b
					}
				}
				msg["content"] = newBlocks
			}
		}
		clonedMessages = append(clonedMessages, msg)
	}
	if last == nil {
		last = prefix
	}
	body["messages"] = clonedMessages
	if last != nil {
		switch content := last["content"].(type) {
		case string:
			last["content"] = []any{cached(content)}
		case []any:
			if len(content) > 0 {
				if m, ok := content[len(content)-1].(map[string]any); ok {
					m["cache_control"] = map[string]any{"type": "ephemeral"}
				}
			}
		}
	}
	if origTools, ok := body["tools"].([]any); ok {
		clonedTools := make([]any, len(origTools))
		for i, item := range origTools {
			if t, ok := item.(map[string]any); ok {
				tc := maps.Clone(t)
				delete(tc, "cache_control")
				clonedTools[i] = tc
			} else {
				clonedTools[i] = item
			}
		}
		body["tools"] = clonedTools
	}

	// metadata.user_id：官方在每個請求上標記裝置與會話（選快取節點用）；先複製再寫入防污染原 map
	origMeta, _ := body["metadata"].(map[string]any)
	meta := maps.Clone(origMeta)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["user_id"] = UserID(device, session)
	body["metadata"] = meta
}

// accountDeviceMid 取得帳號專屬裝置指紋；帳號為空或無憑證時回退全域裝置指紋。
func accountDeviceMid(acc *model.Account) string {
	if acc != nil {
		if mid := acc.DeviceMid(); mid != "" {
			return mid
		}
	}
	return config.DeviceMid()
}

// AccountDeviceMid 供跨包（gateway / asyncpool）取得與請求標頭一致的裝置指紋。
func AccountDeviceMid(acc *model.Account) string {
	return accountDeviceMid(acc)
}

// ExtractClientSessionKey 依優先順序（x-session-id → x-claude-code-session-id → x-conversation-id）
// 大小寫不敏感提取客戶端傳入的原始會話鍵；皆無則回傳空串。
func ExtractClientSessionKey(incomingHeaders map[string]string) string {
	var s1, s2, s3 string
	for k, v := range incomingHeaders {
		val := strings.TrimSpace(v)
		if val == "" {
			continue
		}
		switch strings.ToLower(k) {
		case "x-session-id":
			if s1 == "" {
				s1 = val
			}
		case "x-claude-code-session-id":
			if s2 == "" {
				s2 = val
			}
		case "x-conversation-id":
			if s3 == "" {
				s3 = val
			}
		}
	}
	if s1 != "" {
		return s1
	}
	if s2 != "" {
		return s2
	}
	return s3
}

// AccountSessionID 以帳號裝置指紋與客戶端會話鍵組合派生專屬 UUID v5：
// 同一帳號多輪對話維持相同 x-session-id 以命中快取，跨帳號故障轉移則派生全新 UUID 避免關聯。
func AccountSessionID(incomingHeaders map[string]string, device string) string {
	if strings.TrimSpace(device) == "" {
		device = config.DeviceMid()
	}
	if raw := ExtractClientSessionKey(incomingHeaders); raw != "" {
		return util.UUIDv5("zcode-account-session", device+"\x00"+strings.ToLower(raw))
	}
	return SessionID(device)
}

// SessionIDFromHeaders 依客戶端會話頭（x-session-id / x-claude-code-session-id）
// 歸一化為標準 UUID；若客戶端未攜帶則以帳號 device 派生穩定 UUID。
// 確保同一對話的每輪請求維持相同 x-session-id，讓上游 Prompt Cache 穩定命中。
func SessionIDFromHeaders(incomingHeaders map[string]string, device string) string {
	var raw string
	for k, v := range incomingHeaders {
		lower := strings.ToLower(k)
		if lower == "x-session-id" && strings.TrimSpace(v) != "" {
			raw = strings.TrimSpace(v)
			break
		}
		if lower == "x-claude-code-session-id" && strings.TrimSpace(v) != "" && raw == "" {
			raw = strings.TrimSpace(v)
		}
	}
	if raw != "" {
		if util.IsUUID(raw) {
			return strings.ToLower(raw)
		}
		return util.UUIDv5("zcode-session", raw)
	}
	return SessionID(device)
}

// QueryID 依會話 ID 與請求體中「含非 tool_result 的使用者訊息數」派生每輪查詢 UUID。
// 對齊 ZCode 官方 createModelRequestAttributionHeaders（magpie 0.1.55 queryOf）：
// 同一輪工具呼叫迴圈維持不變，下一次使用者提問自動換新。
func QueryID(body map[string]any, session string) string {
	if body == nil || strings.TrimSpace(session) == "" {
		return ""
	}
	msgs, ok := body["messages"].([]any)
	if !ok {
		return ""
	}
	turns := 0
	for _, item := range msgs {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "user" {
			continue
		}
		if userMessageHasNonToolResult(msg["content"]) {
			turns++
		}
	}
	if turns == 0 {
		return ""
	}
	return util.UUIDv5("zcode-query", fmt.Sprintf("%s\x00%d", session, turns))
}

func userMessageHasNonToolResult(content any) bool {
	switch c := content.(type) {
	case string:
		return strings.TrimSpace(c) != ""
	case []any:
		for _, part := range c {
			block, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := block["type"].(string); t != "tool_result" {
				return true
			}
		}
	}
	return false
}

// SetQueryID 依已組裝請求的 x-session-id 與原始請求體寫入 x-query-id 標頭。
func SetQueryID(req *Request, body map[string]any) {
	if req == nil || req.Headers == nil {
		return
	}
	// 防止大小寫變體與此處寫入的小寫鍵共存
	delete(req.Headers, "X-Query-Id")
	delete(req.Headers, "X-Query-ID")
	if qid := QueryID(body, req.Headers["x-session-id"]); qid != "" {
		req.Headers["x-query-id"] = qid
	}
}

// userIDPayload 字段顺序对齐官方（device_id、account_uuid、session_id）；
// struct 序列化保证键序稳定，map 会按字典序重排。
type userIDPayload struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

// UserID 是 metadata.user_id 的官方形态：JSON 编码的设备标识。
func UserID(device, session string) string {
	payload, err := json.Marshal(userIDPayload{DeviceID: device, SessionID: session})
	if err != nil {
		return ""
	}
	return string(payload)
}

// SessionID 把会话标识归一为官方形态：以 device 为种子派生稳定 UUID v5。
func SessionID(device string) string {
	return util.UUIDv5("zcode-session-device", device)
}

// hasSkillTool 请求是否携带 Skill 工具（官方在有该工具时追加技能指引段）。
func hasSkillTool(body map[string]any) bool {
	tools, _ := body["tools"].([]any)
	for _, item := range tools {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := t["name"].(string); name == "Skill" {
			return true
		}
	}
	return false
}

// environment 渲染官方 environment 段。网关不知道调用方的工作目录，
// 保持官方的无目录分支：平台、系统版本与不可信数据提示。
// platform/osVersion 是网关本机的值——magpie 插件同样如此（插件跑在
// magpie 进程而非 agent 目录），已验证可通过风控。
func environment(p *zcodePrompts) string {
	e := p.Environment
	osLine := config.OsRelease()
	if osLine == "" {
		osLine = config.PlatformName()
	} else {
		// 官方形态 "linux 6.1.0-13-amd64 x64"（Node: platform release arch）
		osLine = config.PlatformName() + " " + osLine + " " + config.ArchName()
	}
	lines := []string{
		e.Heading,
		e.InvokedLine,
		" - " + e.PlatformLabel + ": " + config.PlatformName(),
		" - " + e.OsVersionLabel + ": " + osLine,
		" - " + e.UntrustedNote,
	}
	return strings.Join(lines, "\n")
}
