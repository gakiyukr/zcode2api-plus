// /v1/responses（OpenAI Responses API，服务 Codex CLI 生态）：
// 请求转换 → 复用 chat/completions 的引擎交付与流式解析，输出重编码为
// response.* 事件。契约见 PLAN.md §5.8；previous_response_id（状态化）v1 明确 400。
package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"zcode2api/internal/gateway"
)

// ConvertResponsesRequest 把 Responses 请求体转换为 Anthropic Messages 请求体。
func ConvertResponsesRequest(body map[string]any) (map[string]any, error) {
	if prev := strings.TrimSpace(stringOf(body["previous_response_id"])); prev != "" {
		return nil, &convertError{"previous_response_id 暫不支持（無狀態模式：每輪請求攜帶完整歷史）"}
	}

	out := map[string]any{}
	if m, ok := body["model"].(string); ok && m != "" {
		out["model"] = m
	}

	// max_output_tokens → max_tokens（缺省 8192，对齐 completions）
	if mt := body["max_output_tokens"]; mt != nil {
		out["max_tokens"] = mt
	} else {
		out["max_tokens"] = float64(8192)
	}

	for _, key := range []string{"temperature", "top_p"} {
		if v, ok := body[key]; ok && v != nil {
			out[key] = v
		}
	}
	if stream, ok := body["stream"]; ok {
		out["stream"] = stream
	}

	// instructions → system 文本块（归并交给引擎 NormalizeBody）
	if inst, ok := body["instructions"].(string); ok && inst != "" {
		out["system"] = []any{map[string]any{"type": "text", "text": inst}}
	}

	// reasoning.effort → output_config.effort（§5.8 契约）
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" {
			out["output_config"] = map[string]any{"effort": effort}
		}
	}

	// 扁平 tools（Responses 形态：type/name/description/parameters 直列）
	if rawTools, ok := body["tools"].([]any); ok && len(rawTools) > 0 {
		tools := make([]any, 0, len(rawTools))
		for _, raw := range rawTools {
			t, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := t["name"].(string)
			if name == "" {
				continue
			}
			entry := map[string]any{"name": name}
			if d, ok := t["description"].(string); ok && d != "" {
				entry["description"] = d
			}
			if params := t["parameters"]; params != nil {
				entry["input_schema"] = params
			} else {
				entry["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, entry)
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}

	messages, err := convertResponsesInput(body["input"])
	if err != nil {
		return nil, err
	}
	out["messages"] = messages
	return out, nil
}

// convertResponsesInput 把 Responses input（字符串或 item 数组）转换为
// Anthropic messages。item 类型：message / function_call / function_call_output；
// 其余类型（reasoning 等推理器自有 item）按忽略处理（Codex 默认不回传）。
func convertResponsesInput(input any) ([]any, error) {
	switch v := input.(type) {
	case nil:
		return []any{}, nil
	case string:
		if v == "" {
			return []any{}, nil
		}
		return []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": v},
		}}}, nil
	case []any:
		messages := make([]any, 0, len(v))
		var pendingBlocks []any // 当前 user 消息累积（function_call_output 归并）
		flushUser := func() {
			if len(pendingBlocks) > 0 {
				messages = append(messages, map[string]any{"role": "user", "content": pendingBlocks})
				pendingBlocks = nil
			}
		}
		for _, raw := range v {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch itype, _ := item["type"].(string); itype {
			case "", "message":
				role, _ := item["role"].(string)
				if role != "assistant" {
					role = "user"
				}
				blocks, err := responsesContentToBlocks(item["content"])
				if err != nil {
					return nil, err
				}
				flushUser()
				messages = append(messages, map[string]any{"role": role, "content": blocks})
			case "function_call":
				// assistant 的历史工具调用 → tool_use block（独立 assistant 消息）
				input := map[string]any{}
				if rawArgs, _ := item["arguments"].(string); strings.TrimSpace(rawArgs) != "" {
					_ = json.Unmarshal([]byte(rawArgs), &input)
				}
				flushUser()
				messages = append(messages, map[string]any{
					"role": "assistant",
					"content": []any{map[string]any{
						"type": "tool_use", "id": item["call_id"],
						"name": item["name"], "input": input,
					}},
				})
			case "function_call_output":
				pendingBlocks = append(pendingBlocks, map[string]any{
					"type": "tool_result", "tool_use_id": item["call_id"],
					"content": functionCallOutputContent(item["output"]),
				})
			default:
				// reasoning 等 item 忽略
			}
		}
		flushUser()
		return messages, nil
	default:
		return nil, &convertError{fmt.Sprintf("input 形态无效: %T", input)}
	}
}

// responsesContentToBlocks 把 message item 的 content（字符串或 parts）转 blocks。
func responsesContentToBlocks(content any) ([]any, error) {
	switch c := content.(type) {
	case nil:
		return []any{}, nil
	case string:
		if c == "" {
			return []any{}, nil
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
			case "", "input_text", "output_text":
				text, _ := p["text"].(string)
				if text == "" {
					continue
				}
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			case "input_image":
				block, err := imageURLToBlock(map[string]any{"image_url": map[string]any{"url": p["image_url"]}})
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, block)
			default:
				return nil, &convertError{fmt.Sprintf("不支持的 content part 类型 %s", fmt.Sprint(p["type"]))}
			}
		}
		return blocks, nil
	default:
		return nil, &convertError{"message content 形态无效"}
	}
}

// stringOf 安全取字符串。
func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// ── 响应转换（非流式）────────────────────────────────────────────────────────

// ConvertResponsesResponse 把上游 Anthropic Messages JSON 转换为 Responses 形态。
func ConvertResponsesResponse(payload map[string]any) map[string]any {
	// 与 respond.go 的 chat/completions 路径同语义：id 缺失或 content 形态
	// 不对都视为「不是合法 Messages 响应」。原写法用嵌套 if 等价于「两者
	// 同时缺失才拒绝」，会产出 "resp_" 这种空 id。
	id, _ := payload["id"].(string)
	if _, ok := payload["content"].([]any); !ok || id == "" {
		return nil
	}
	output, _ := messageToResponsesOutput(payload)
	return map[string]any{
		"id":                  "resp_" + id,
		"object":              "response",
		"created_at":          float64(time.Now().Unix()),
		"model":               payload["model"],
		"status":              "completed",
		"output":              output,
		"parallel_tool_calls": true,
		"usage":               responsesUsage(payload["usage"]),
	}
}

// messageToResponsesOutput 把 Anthropic content 转换为 output item 数组：
// text block → message item（content[0] 为 output_text）；tool_use → function_call item。
func messageToResponsesOutput(message map[string]any) ([]any, bool) {
	var output []any
	texts := []string{}
	rawBlocks, _ := message["content"].([]any)
	for _, raw := range rawBlocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			t, _ := block["text"].(string)
			texts = append(texts, t)
		case "tool_use":
			args, err := marshalCompact(block["input"])
			if err != nil {
				args = "{}"
			}
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        "fc_" + stringOf(block["id"]),
				"call_id":   block["id"],
				"name":      block["name"],
				"arguments": args,
				"status":    "completed",
			})
		}
	}
	messageItem := map[string]any{
		"type":   "message",
		"id":     "msg_" + stringOf(message["id"]),
		"role":   "assistant",
		"status": "completed",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        strings.Join(texts, ""),
			"annotations": []any{},
		}},
	}
	// message item 在前、function_call 在后（Responses 惯例）
	return append([]any{messageItem}, output...), true
}

// responsesUsage Anthropic usage → Responses usage（直接 token 计数）。
// functionCallOutputContent 归一 function_call_output 的 output 字段。
//
// Responses 契约允许它要么是字符串，要么是 content items 数组——Codex 在
// 工具结果无 structured_content 时固定发数组（models.rs 的
// FunctionCallOutputBody::ContentItems）。只做字符串断言会把整段工具输出
// 静默变成空串，模型随即失去工具上下文。
func functionCallOutputContent(raw any) []any {
	switch v := raw.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": v}}
	case []any:
		blocks := make([]any, 0, len(v))
		for _, entry := range v {
			item, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := item["text"].(string); ok {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
				continue
			}
			// 非文本项（图片等）无法映射到 Anthropic tool_result，降级为
			// 其 JSON 字面量，至少不丢信息。
			if encoded, err := marshalCompact(item); err == nil {
				blocks = append(blocks, map[string]any{"type": "text", "text": encoded})
			}
		}
		return blocks
	default:
		return []any{map[string]any{"type": "text", "text": ""}}
	}
}

func responsesUsage(raw any) map[string]any {
	u, _ := raw.(map[string]any)
	num := func(key string) float64 {
		switch v := u[key].(type) {
		case float64:
			return v
		case int:
			return float64(v)
		default:
			return 0
		}
	}
	// Anthropic 的 input_tokens 不含缓存读写，两者独立计数；Codex 从
	// input_tokens_details.cached_tokens 读取缓存命中并据此判断压缩阈值，
	// 漏算会让上下文用量被严重低估。
	cacheRead := num("cache_read_input_tokens")
	cacheCreation := num("cache_creation_input_tokens")
	input := num("input_tokens") + cacheRead + cacheCreation
	output := num("output_tokens")
	return map[string]any{
		"input_tokens":  input,
		"output_tokens": output,
		"total_tokens":  input + output,
		"input_tokens_details": map[string]any{
			"cached_tokens":      cacheRead,
			"cache_write_tokens": cacheCreation,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": float64(0),
		},
	}
}

// ── HTTP 层 ──────────────────────────────────────────────────────────────────

func (h *Handler) handleResponses(w http.ResponseWriter, r *http.Request) {
	if e := h.Auth.VerifyGatewayKey(r); e != nil {
		gateway.WriteAuthError(w, e)
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON", "invalid_request_error")
		return
	}

	anthropicReq, err := ConvertResponsesRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if !gateway.ModelAllowed(anthropicReq["model"]) {
		writeError(w, http.StatusBadRequest,
			"模型 "+gateway.AnyToString(anthropicReq["model"])+" 不在可用清單內，僅支持 "+
				strings.Join(gateway.AvailableModels, ", "), "invalid_request_error")
		return
	}
	gateway.NormalizeBody(anthropicReq, false)

	stream, _ := body["stream"].(bool)
	result := h.Engine.RunMessages(r.Context(), anthropicReq, gateway.IncomingHeaders(r),
		func(d gateway.Delivery) error {
			if stream {
				return deliverResponsesStream(w, d)
			}
			return deliverResponsesJSON(w, d)
		})
	if result.Delivered {
		return
	}
	gateway.WriteJSON(w, result.Status, result.Body)
}

// deliverResponsesJSON 非流式交付。
func deliverResponsesJSON(w http.ResponseWriter, d gateway.Delivery) error {
	return deliverJSONWith(w, d, ConvertResponsesResponse)
}

// deliverResponsesStream 流式交付：上游 SSE → response.* 事件流。
func deliverResponsesStream(w http.ResponseWriter, d gateway.Delivery) error {
	return deliverSSEWith(w, d, reencodeResponsesSSE)
}
