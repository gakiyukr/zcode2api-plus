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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	CliPrefix        string   `json:"cliPrefix"`
	StableSections   []string `json:"stableSections"`
	DynamicBehavior  string   `json:"dynamicBehavior"`
	SessionGuidance  string   `json:"sessionGuidance"`
	ContextManagement string  `json:"contextManagement"`
	PoweredBy        string   `json:"poweredBy"`
	Environment      struct {
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
	// 官方客户端的 messages 请求不携带 X-Device-Mid：客户端伪造同样剔除，
	// 设备身份经由 body 的 metadata.user_id 传递（见 DressSystem）
	"x-device-mid":                   true,
	"anthropic-dangerous-direct-browser-access": true,
}

// dropHeaderPrefixes 按前缀剔除的 agent 特征头（x-stainless-* 是官方 SDK
// 的遥测头族；x-claude-code-* / x-magpie-* 是 agent 网关的会话头）。
var dropHeaderPrefixes = []string{"x-stainless-", "x-claude-code-", "x-magpie-"}

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

	// 固定头一律用 Go 的规范化形式（textproto canonical），与客户端头的 key
	// 形式保持一致——否则同一逻辑头会以两个不同 key 存在于 map 中，
	// 最终值取决于 map 迭代顺序（随机）。
	fixed := map[string]string{
		"Content-Type":         "application/json",
		authHeader:             authValue,
		"Anthropic-Version":    "2023-06-01",
		"User-Agent":           config.UserAgent,
		"X-ZCode-App-Version":  config.ZcodeClientVersion,
		"X-ZCode-Agent":        "glm",
		"HTTP-Referer":         "https://zcode.z.ai",
		"X-Title":              "Z Code@cli",
		"X-Release-Channel":    "production",
		"X-Client-Language":    config.ClientLanguage,
		"X-Client-Timezone":    config.ClientTimezone,
		"X-Platform":           config.ClientPlatform(),
		"X-Os-Category":        config.OsCategory(),
		"x-request-id":         util.NewUUID(),
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

// DressSystem 按 ZCode 3.17.2 官方客户端的形态重建请求体的 system 与相关字段。
// 不幂等（重复调用会重复追加日期前缀），调用方须保证对同一 body 只调用一次。
//
// 官方形态要点（magpie dress 语义，风控按此校验）：
//  1. 顶层 system 恰好三块：cliPrefix、两段 stable 合并、动态段（官方文本 +
//     调用方 system 文本并入块尾）——上游拒绝第四块（405/3012）；
//  2. 首个 user turn 之前插入日期 <system-reminder>（官方把当天日期放在
//     user turn 而非 system）；
//  3. cache_control 只出现在 system 三块与最后一条非 system 消息的末块上，
//     消息中间块与 tools 上一律清除；
//  4. metadata.user_id 标记设备（官方按它选缓存节点，缺失会命中不了缓存）。
func DressSystem(body map[string]any) {
	p := ZcodePrompts()
	if p == nil || body == nil {
		return
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

	// 日期前缀：官方以 <system-reminder> 包裹当天日期放在首个 user turn
	dateLine := strings.ReplaceAll(p.Context.CurrentDateLine, "{date}", time.Now().Format("2006-01-02"))
	prefix := map[string]any{
		"role": "user",
		"content": []any{map[string]any{
			"type": "text",
			"text": "<system-reminder>" + dateLine + "</system-reminder>",
		}},
	}
	messages, _ := body["messages"].([]any)
	body["messages"] = append([]any{prefix}, messages...)

	// cache_control 重排：消息内所有块清除，仅最后一条非 system 消息的末块保留
	var last map[string]any
	for _, item := range body["messages"].([]any) {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role == "system" {
			continue
		}
		last = msg
		if blocks, ok := msg["content"].([]any); ok {
			for _, b := range blocks {
				if m, ok := b.(map[string]any); ok {
					delete(m, "cache_control")
				}
			}
		}
	}
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
	if tools, ok := body["tools"].([]any); ok {
		for _, item := range tools {
			if t, ok := item.(map[string]any); ok {
				delete(t, "cache_control")
			}
		}
	}

	// metadata.user_id：官方在每个请求上标记设备（选缓存节点用）
	meta, _ := body["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["user_id"] = UserID(config.DeviceMid(), SessionID(config.DeviceMid()))
	body["metadata"] = meta
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

// SessionID 把会话标识归一为官方形态：UUID 直接用；非 UUID 的会话（或缺失）
// 以 device 为种子派生稳定 UUID（对齐 magpie sessionOf，uuid v5 形态）。
func SessionID(device string) string {
	sum := sha256.Sum256([]byte("zcode-session-device\x00" + device))
	u := sum[:16]
	u[6] = (u[6] & 0x0f) | 0x50
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
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
