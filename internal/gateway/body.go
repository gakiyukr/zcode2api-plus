// 请求体整形。对应 Python 版 routes/gateway.py 的 _normalize_body。
package gateway

import (
	"maps"
	"strings"

	"zcode2api/internal/upstream"
)

// NormalizeBody 对请求体做三件事（注入语义对齐 ZCode 3.17.2 官方客户端，
// 移植自 magpie 的 dress）：
//  1. 模型名去 "provider/" 前缀，并按 MODEL_NAME_MAP 做大小写映射（幂等）；
//  2. 字符串 content 桥接为 [{type:"text"}]（不改写原始消息 map，幂等）；
//  3. needsZcodeSystem（JWT 账号）时重建 system 为官方三块形态并补齐
//     日期前缀 / cache_control / metadata——否则上游返回 405。
//
// 注意：system 重建**不幂等**（重复调用会重复追加日期前缀），调用方须保证
// 对同一个 body 只注入一次（Python 版同样是入口一次 + 账号副本一次）。
func NormalizeBody(body map[string]any, needsZcodeSystem bool) map[string]any {
	if raw, ok := body["model"].(string); ok {
		m := raw
		if strings.Contains(m, "/") {
			parts := strings.Split(m, "/")
			m = strings.Join(parts[1:], "/")
		}
		if mapped, ok := ModelNameMap[strings.ToLower(m)]; ok {
			m = mapped
		}
		body["model"] = m
	}

	if messages, ok := body["messages"].([]any); ok {
		bridged := make([]any, 0, len(messages))
		for _, item := range messages {
			msg, ok := item.(map[string]any)
			if !ok {
				bridged = append(bridged, item)
				continue
			}
			text, ok := msg["content"].(string)
			if !ok {
				bridged = append(bridged, msg)
				continue
			}
			clone := maps.Clone(msg)
			clone["content"] = []any{map[string]any{"type": "text", "text": text}}
			bridged = append(bridged, clone)
		}
		body["messages"] = bridged
	}

	if needsZcodeSystem {
		// 官方形态重建：三块 system + 日期前缀 + cache_control + metadata.user_id。
		// 上游按官方客户端的形态逐项校验，外部 agent 的 system 文本并入第三块。
		upstream.DressSystem(body)
	}
	return body
}
