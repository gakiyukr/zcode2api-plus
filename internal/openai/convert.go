// Package OpenAI（GPT）兼容端点的请求转换层：OpenAI Chat Completions →
// Anthropic Messages。契约见 PLAN.md §5.7（Go 版增量，Python 版无对应实现）。
package openai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// convertError 请求转换失败（对应 HTTP 400，OpenAI 错误形态）。
type convertError struct{ msg string }

func (e *convertError) Error() string { return e.msg }

// ConvertRequest 把 OpenAI Chat Completions 请求体转换为 Anthropic Messages
// 请求体（原地构造新 map，不修改入参）。模型白名单校验在 handler 侧完成。
func ConvertRequest(body map[string]any) (map[string]any, error) {
	if n, ok := body["n"]; ok {
		if v, isNum := n.(float64); isNum && v > 1 {
			return nil, &convertError{"仅支持 n=1"}
		}
	}

	out := map[string]any{}
	if m, ok := body["model"].(string); ok && m != "" {
		out["model"] = m
	}

	// max_tokens / max_completion_tokens → max_tokens（两者皆缺省 8192）
	if mt := body["max_tokens"]; mt != nil {
		out["max_tokens"] = mt
	} else if mct := body["max_completion_tokens"]; mct != nil {
		out["max_tokens"] = mct
	} else {
		out["max_tokens"] = float64(8192)
	}

	// temperature / top_p 透传（存在才带）
	for _, key := range []string{"temperature", "top_p"} {
		if v, ok := body[key]; ok && v != nil {
			out[key] = v
		}
	}

	// stop（string 或 array）→ stop_sequences
	switch stop := body["stop"].(type) {
	case string:
		if stop != "" {
			out["stop_sequences"] = []any{stop}
		}
	case []any:
		if len(stop) > 0 {
			out["stop_sequences"] = stop
		}
	}

	if stream, ok := body["stream"]; ok {
		out["stream"] = stream
	}
	// stream_options.include_usage 是 OpenAI 侧参数，Messages API 没有对应字段，
	// 故不写入上游请求体；handler 直接从原始 body 读取（见 handler.go）。

	// tools → Anthropic tools；tool_choice 同步映射
	if rawTools, ok := body["tools"].([]any); ok && len(rawTools) > 0 {
		tools := make([]any, 0, len(rawTools))
		for _, raw := range rawTools {
			fn, ok := toolFunction(raw)
			if !ok {
				continue
			}
			t := map[string]any{"name": fn["name"]}
			if d, ok := fn["description"].(string); ok && d != "" {
				t["description"] = d
			}
			if params := fn["parameters"]; params != nil {
				t["input_schema"] = params
			} else {
				t["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, t)
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	switch choice := body["tool_choice"].(type) {
	case string:
		switch choice {
		case "auto":
			out["tool_choice"] = map[string]any{"type": "auto"}
		case "none":
			// 上游为 Anthropic 兼容端点，none 即显式禁止工具调用
			out["tool_choice"] = map[string]any{"type": "none"}
		case "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		}
	case map[string]any:
		if fn, ok := choice["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				out["tool_choice"] = map[string]any{"type": "tool", "name": name}
			}
		}
	}

	// messages：system/developer 归并到顶层 system；user/assistant/tool 映射
	rawMessages, _ := body["messages"].([]any)
	messages := make([]any, 0, len(rawMessages))
	var systemBlocks []any
	for _, item := range rawMessages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system", "developer":
			blocks, err := contentToTextBlocks(msg["content"])
			if err != nil {
				return nil, err
			}
			systemBlocks = append(systemBlocks, blocks...)
		default:
			converted, err := convertMessage(msg, role)
			if err != nil {
				return nil, err
			}
			messages = append(messages, converted)
		}
	}
	if len(systemBlocks) > 0 {
		// 归并后的 system 交给引擎 NormalizeBody 注入 zcode_system 块时
		// 追加在其后（见 gateway/body.go：blocks 在前、existing 在后）
		out["system"] = systemBlocks
	}
	out["messages"] = messages
	return out, nil
}

// toolFunction 提取 OpenAI tools[] 项的 function 对象。
func toolFunction(raw any) (map[string]any, bool) {
	t, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	fn, ok := t["function"].(map[string]any)
	if !ok {
		return nil, false
	}
	name, _ := fn["name"].(string)
	if name == "" {
		return nil, false
	}
	return fn, true
}

// contentToTextBlocks 把 OpenAI content（字符串或 parts 数组）归并为
// Anthropic text blocks；空内容返回空切片。
func contentToTextBlocks(content any) ([]any, error) {
	switch c := content.(type) {
	case nil:
		return nil, nil
	case string:
		if c == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": "text", "text": c}}, nil
	case []any:
		var blocks []any
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if p["type"] != "text" {
				return nil, &convertError{fmt.Sprintf("system 消息不支持 content part 类型 %s", fmt.Sprint(p["type"]))}
			}
			text, _ := p["text"].(string)
			if text == "" {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		}
		return blocks, nil
	default:
		return nil, &convertError{"system 消息 content 形态无效"}
	}
}

// convertMessage 映射单条 user/assistant/tool 消息。
func convertMessage(msg map[string]any, role string) (map[string]any, error) {
	switch role {
	case "user", "assistant":
		blocks, err := contentBlocks(msg["content"])
		if err != nil {
			return nil, err
		}
		// assistant 的 tool_calls 追加 tool_use blocks（content 与工具调用可共存）
		if role == "assistant" {
			if rawCalls, ok := msg["tool_calls"].([]any); ok {
				for _, raw := range rawCalls {
					call, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					block, err := toolCallToUse(call)
					if err != nil {
						return nil, err
					}
					blocks = append(blocks, block)
				}
			}
		}
		out := map[string]any{"role": role}
		if len(blocks) > 0 {
			out["content"] = blocks
		} else {
			out["content"] = []any{}
		}
		return out, nil

	case "tool":
		// role=tool → user 消息 + tool_result block（tool_use_id 绑定）
		result := map[string]any{
			"type":        "tool_result",
			"tool_use_id": msg["tool_call_id"],
		}
		blocks, err := contentBlocks(msg["content"])
		if err != nil {
			return nil, err
		}
		if len(blocks) > 0 {
			result["content"] = blocks
		} else {
			result["content"] = []any{}
		}
		return map[string]any{"role": "user", "content": []any{result}}, nil
	}
	return nil, &convertError{fmt.Sprintf("不支持的消息角色 %s", role)}
}

// contentBlocks 把 OpenAI content（字符串或 parts 数组）转换为 Anthropic
// blocks：text part → text block；image_url part → image block。
func contentBlocks(content any) ([]any, error) {
	switch c := content.(type) {
	case nil:
		return nil, nil
	case string:
		if c == "" {
			return nil, nil
		}
		return []any{map[string]any{"type": "text", "text": c}}, nil
	case []any:
		blocks := make([]any, 0, len(c))
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch p["type"] {
			case "text":
				text, _ := p["text"].(string)
				if text == "" {
					continue
				}
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			case "image_url":
				block, err := imageURLToBlock(p)
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, block)
			default:
				return nil, &convertError{fmt.Sprintf("不支持 content part 类型 %s", fmt.Sprint(p["type"]))}
			}
		}
		return blocks, nil
	default:
		return nil, &convertError{"消息 content 形态无效"}
	}
}

// imageURLToBlock 把 OpenAI image_url part 转换为 Anthropic image block。
// data URL → base64 source（media_type 从 URL 解析）；http[s] URL → url source
// （上游支持度未知，失败按上游错误原样透传）。
func imageURLToBlock(part map[string]any) (map[string]any, error) {
	inner, ok := part["image_url"].(map[string]any)
	if !ok {
		return nil, &convertError{"image_url part 缺少 image_url 对象"}
	}
	u, _ := inner["url"].(string)
	switch {
	case strings.HasPrefix(u, "data:"):
		mediaType, data, err := parseDataURL(u)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": mediaType, "data": data,
		}}, nil
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "url", "url": u,
		}}, nil
	default:
		return nil, &convertError{"image_url 仅支持 data URL 或 http(s) URL"}
	}
}

// parseDataURL 解析 data:<media_type>;base64,<data> 形态。
func parseDataURL(u string) (string, string, error) {
	rest := strings.TrimPrefix(u, "data:")
	sep := strings.Index(rest, ",")
	if sep < 0 {
		return "", "", &convertError{"data URL 格式无效（缺少逗号）"}
	}
	meta := rest[:sep]
	const marker = ";base64"
	if !strings.HasSuffix(meta, marker) {
		return "", "", &convertError{"data URL 仅支持 base64 编码"}
	}
	mediaType := strings.TrimSuffix(meta, marker)
	if mediaType == "" {
		return "", "", &convertError{"data URL 缺少 media_type"}
	}
	data := rest[sep+1:]
	if data == "" {
		return "", "", &convertError{"data URL 缺少数据段"}
	}
	return mediaType, data, nil
}

// toolCallToUse 把 OpenAI tool_calls[] 项转换为 Anthropic tool_use block；
// arguments JSON 字符串解析为 input（解析失败按空对象，模型侧已生成即认可）。
func toolCallToUse(call map[string]any) (map[string]any, error) {
	fn, ok := call["function"].(map[string]any)
	if !ok {
		return nil, &convertError{"tool_calls 项缺少 function 对象"}
	}
	name, _ := fn["name"].(string)
	if name == "" {
		return nil, &convertError{"tool_calls 项缺少 function.name"}
	}
	input := map[string]any{}
	if raw, _ := fn["arguments"].(string); strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			var arr any
			if err2 := json.Unmarshal([]byte(raw), &arr); err2 != nil {
				return nil, &convertError{fmt.Sprintf("tool_calls 的 arguments 不是合法 JSON: %v", err)}
			}
			input = map[string]any{}
			_ = arr // arguments 为数组等非对象形态：保留空对象，上游按 schema 校验
		}
	}
	return map[string]any{
		"type":  "tool_use",
		"id":    call["id"],
		"name":  name,
		"input": input,
	}, nil
}
