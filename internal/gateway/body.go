// 请求体整形。对应 Python 版 routes/gateway.py 的 _normalize_body。
package gateway

import (
	"maps"
	"strings"

	"zcode2api/internal/fingerprint"
	"zcode2api/internal/upstream"
)

// NormalizeBody 對請求體做三件事（注入語義對齊官方客戶端）：
//  1. 模型名去 "provider/" 前綴，並按 ModelNameMap 做大小寫映射（冪等）；
//  2. 字串 content 橋接為 [{type:"text"}]（不改寫原始訊息 map，冪等）；
//  3. needsZcodeSystem（JWT 帳號）時重建 system 為官方三塊形態並補齊
//     日期前綴 / cache_control / metadata——否則上游返回 405。
//
// 注意：system 重建不冪等，呼叫方須保證對同一 body 只注入一次。
func NormalizeBody(body map[string]any, needsZcodeSystem bool) map[string]any {
	return NormalizeBodyFor(body, needsZcodeSystem, "", "")
}

// NormalizeBodyFor 同 NormalizeBody，並支援傳入帳號專屬 device 與會話 ID，
// 使 metadata.user_id 與上游 x-session-id 一致。
func NormalizeBodyFor(body map[string]any, needsZcodeSystem bool, device, session string) map[string]any {
	return NormalizeBodyForSKU(body, needsZcodeSystem, device, session, fingerprint.For(nil))
}

// NormalizeBodyForSKU 與 NormalizeBodyFor 相同，但使用呼叫方提供的帳號 SKU，
// 確保 system environment 段與上游請求頭描述同一台桌面設備。
func NormalizeBodyForSKU(body map[string]any, needsZcodeSystem bool, device, session string, sku fingerprint.SKU) map[string]any {
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

	normalizeThinking(body)

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
		// 官方形態重建：三塊 system + 日期前綴 + cache_control + metadata.user_id。
		// 上游按官方客戶端的形態逐項校驗，外部 agent 的 system 文本併入第三塊。
		upstream.DressSystemForSKU(body, device, session, sku)
	} else if strings.TrimSpace(device) != "" {
		// API Key（Coding Plan）路徑不改動 system，僅於未帶 metadata.user_id 時補齊裝置與會話識別
		if strings.TrimSpace(session) == "" {
			session = upstream.SessionID(device)
		}
		origMeta, _ := body["metadata"].(map[string]any)
		if uid, _ := origMeta["user_id"].(string); strings.TrimSpace(uid) == "" {
			meta := maps.Clone(origMeta)
			if meta == nil {
				meta = map[string]any{}
			}
			meta["user_id"] = upstream.UserID(device, session)
			body["metadata"] = meta
		}
	}
	return body
}

// normalizeThinking 把 Anthropic/OpenAI 客戶端送來的思考參數歸一到上游接受的形態。
// 只調整已存在的參數，不主動為一般請求注入思考；Flash 模型關閉思考會讓上游 400，
// 因此把 thinking.type=disabled 改寫為最低檔 output_config.effort=low。
func normalizeThinking(body map[string]any) {
	modelName, _ := body["model"].(string)
	if output, ok := body["output_config"].(map[string]any); ok {
		if effort, ok := output["effort"].(string); ok {
			if normalized := NormalizeEffort(modelName, effort); normalized != effort {
				output["effort"] = normalized
			}
		}
	}
	if !IsFlashModel(modelName) {
		return
	}
	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		return
	}
	if typ, _ := thinking["type"].(string); strings.EqualFold(typ, "disabled") {
		delete(body, "thinking")
		setOutputEffort(body, "low")
	}
}

func setOutputEffort(body map[string]any, effort string) {
	if output, ok := body["output_config"].(map[string]any); ok {
		output["effort"] = effort
		return
	}
	body["output_config"] = map[string]any{"effort": effort}
}
