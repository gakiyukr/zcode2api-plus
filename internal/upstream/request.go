// Package upstream 构造对 ZCode 上游的请求。
// 对应 Python 版 app/agent.py：按账号凭证选端点、组装请求头。
// 实际发送与流式透传在 internal/gateway。
package upstream

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

//go:embed zcode_system.json
var zcodeSystemJSON []byte

var (
	zcodeOnce   sync.Once
	zcodeBlocks []any
)

// ZcodeSystemBlocks 返回 ZCode 官方系统提示词块（JSON 解析失败时为空，对齐 Python 版）。
// JWT 账号请求必须注入到顶层 system，否则上游返回 405。
func ZcodeSystemBlocks() []any {
	zcodeOnce.Do(func() {
		var blocks []any
		if err := json.Unmarshal(zcodeSystemJSON, &blocks); err != nil {
			blocks = []any{}
		}
		zcodeBlocks = blocks
	})
	return zcodeBlocks
}

// dropHeaders 透传客户端 header 时需要剔除的字段（对齐 agent.py _DROP_HEADERS）。
var dropHeaders = map[string]bool{
	"host":                           true,
	"content-length":                 true,
	"x-api-key":                      true,
	"authorization":                  true,
	"user-agent":                     true,
	"http-referer":                   true,
	"accept-encoding":                true,
	"connection":                     true,
	"cookie":                         true,
	"x-aliyun-captcha-verify-param":  true,
	"x-aliyun-captcha-verify-region": true,
}

// Request 构造结果：目标 URL 与请求头。
type Request struct {
	URL     string
	Headers map[string]string
}

// BuildRequest 对应 Python 版 build_request：
// JWT 账号走 zcode.z.ai 主端点（Bearer），API Key 账号走 api.z.ai 回退端点（x-api-key）。
// verifyParam/verifyRegion 为验证码令牌（仅 JWT 账号）；incomingHeaders 为客户端透传头。
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
		"Content-Type":        "application/json",
		authHeader:            authValue,
		"Anthropic-Version":   "2023-06-01",
		"User-Agent":          config.UserAgent,
		"X-ZCode-App-Version": config.ZcodeClientVersion,
		"X-ZCode-Agent":       "glm",
		"HTTP-Referer":        "https://zcode.z.ai/",
		"X-Device-Mid":        config.DeviceMid(),
	}
	if verifyParam != "" {
		fixed["X-Aliyun-Captcha-Verify-Param"] = verifyParam
		if verifyRegion != "" {
			fixed["X-Aliyun-Captcha-Verify-Region"] = verifyRegion
		}
	}

	// 先透传客户端头（剔除敏感/连接类），再强制写回固定头。
	// 顺序不能颠倒：客户端可送 x-device-mid（它不在 dropHeaders 内，也不是
	// x-zcode 前缀），若在其后合并就会覆盖掉设备指纹——而该指纹是刻意固定
	// 以避免被风控的。同理 Content-Type / Anthropic-Version 也不得被改写。
	headers := map[string]string{}
	for key, value := range incomingHeaders {
		lower := strings.ToLower(key)
		if dropHeaders[lower] || strings.HasPrefix(lower, "x-zcode") {
			continue
		}
		headers[key] = value
	}
	for key, value := range fixed {
		headers[key] = value
	}

	return Request{URL: targetURL, Headers: headers}, nil
}
