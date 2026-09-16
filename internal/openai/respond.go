// 响应转换层：Anthropic Messages → OpenAI Chat Completions。
// 契约见 PLAN.md §5.7「响应转换」。
package openai

import (
	"encoding/json"
	"time"
)

// ConvertResponse 把上游 Anthropic Messages 非流式 JSON 响应转换为 OpenAI
// chat.completion 形态。上游响应即标准 Messages 形态：顶层 id/content/
// stop_reason/usage（UsageCollector 同样按顶层解析，见 gateway/usage.go）。
// 响应非法（缺 id/content）时返回 nil。
func ConvertResponse(payload map[string]any) map[string]any {
	id, _ := payload["id"].(string)
	if _, ok := payload["content"].([]any); !ok || id == "" {
		// 上游 200 JSON 也可能是错误包装（引擎已过滤业务码，此处兜底）
		return nil
	}

	content, toolCalls := messageToOpenAI(payload)

	choice := map[string]any{
		"index":         float64(0),
		"message":       content,
		"finish_reason": mapStopReason(payload["stop_reason"]),
	}
	out := map[string]any{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion",
		"created": float64(time.Now().Unix()),
		"model":   payload["model"],
		"choices": []any{choice},
		"usage":   mapUsage(payload["usage"]),
	}
	if len(toolCalls) > 0 {
		content["tool_calls"] = toolCalls // OpenAI 标准：tool_calls 在 message 内
	}
	return out
}

// messageToOpenAI 把 Anthropic message 内容转换为 OpenAI message：
// text blocks 拼接为 content；tool_use → tool_calls（arguments 序列化为
// JSON 字符串）。两者共存时 content 保留、tool_calls 并列。
func messageToOpenAI(message map[string]any) (map[string]any, []any) {
	texts := ""
	var toolCalls []any
	if rawBlocks, ok := message["content"].([]any); ok {
		for _, raw := range rawBlocks {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				t, _ := block["text"].(string)
				texts += t
			case "tool_use":
				args, err := marshalCompact(block["input"])
				if err != nil {
					args = "{}"
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   block["id"],
					"type": "function",
					"function": map[string]any{
						"name":      block["name"],
						"arguments": args,
					},
				})
			}
		}
	}

	out := map[string]any{"role": "assistant"}
	if texts != "" || len(toolCalls) == 0 {
		if texts == "" && len(toolCalls) == 0 {
			out["content"] = nil
		} else {
			out["content"] = texts
		}
	} else {
		// 仅工具调用：OpenAI 惯例 content 为 null
		out["content"] = nil
	}
	return out, toolCalls
}

// mapStopReason 映射 Anthropic stop_reason → OpenAI finish_reason：
// end_turn/stop_sequence → stop、max_tokens → length、tool_use → tool_calls、
// refusal → content_filter；未列出的值按 stop 处理。
func mapStopReason(v any) string {
	switch s, _ := v.(string); s {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		// end_turn / stop_sequence / 其他（pause_turn 等）
		return "stop"
	}
}

// mapUsage 把 Anthropic usage 转换为 OpenAI usage：
// prompt_tokens = input + cache_read + cache_creation；completion_tokens = output；
// 缓存细节放 prompt_tokens_details.cached_tokens。
func mapUsage(raw any) map[string]any {
	u, _ := raw.(map[string]any)
	num := func(key string) float64 {
		v, _ := u[key].(float64)
		return v
	}
	cacheRead := num("cache_read_input_tokens")
	cacheCreation := num("cache_creation_input_tokens")
	prompt := num("input_tokens") + cacheRead + cacheCreation
	completion := num("output_tokens")
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      prompt + completion,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": cacheRead,
		},
	}
}

// marshalCompact 紧凑序列化（tool_use input → arguments JSON 字符串）。
func marshalCompact(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// newChunkID 上游 id → OpenAI chunk id。
func newChunkID(upstreamID string) string {
	return "chatcmpl-" + upstreamID
}

// chunk 构造一个 OpenAI 流式 chunk（choices 单元素）。
func chunk(id, model string, delta map[string]any, finishReason any) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": float64(time.Now().Unix()),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         float64(0),
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
}

// usageChunk 构造 include_usage 终止前附带的 usage chunk（choices 为空数组）。
func usageChunk(id, model string, usage map[string]any) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": float64(time.Now().Unix()),
		"model":   model,
		"choices": []any{},
		"usage":   usage,
	}
}
