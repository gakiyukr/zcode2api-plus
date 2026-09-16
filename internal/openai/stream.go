// 流式重编码：上游 Anthropic SSE 事件流 → OpenAI chat.completion.chunk 流。
// 不是透传：逐事件解析后按 §5.7 重新编码（ping 丢弃、[DONE] 收尾）。
package openai

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// scanSSE 逐行解析上游 SSE，把每个完整事件（event 名 + data 正文）交给 dispatch。
// 事件边界是空行；流以事件收尾而非空行时同样分发。注释行（: keepalive）忽略。
//
// /v1/chat/completions 与 /v1/responses 共用这段骨架，仅 dispatch 的实现不同。
func scanSSE(body io.Reader, dispatch func(event, data string) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	event := ""
	var data strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if err := dispatch(event, data.String()); err != nil {
				return err
			}
			event, data = "", strings.Builder{}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if event != "" || data.Len() > 0 {
		return dispatch(event, data.String())
	}
	return nil
}

// reencodeSSE 读取上游 SSE 流并写出 OpenAI chunk 流；includeUsage 为 true 时
// 终止前附 usage chunk。write 只接收 `data: ...\n\n` 形态的完整事件。
// 返回 write 或读取的错误（客户端中断由调用方经 write 错误感知）。
func reencodeSSE(body io.Reader, includeUsage bool, write func(string) error) error {
	enc := &sseEncoder{write: write, includeUsage: includeUsage}
	return scanSSE(body, enc.dispatch)
}


// sseEncoder 持有跨事件的流状态（id/model/tool 序号/usage）。
type sseEncoder struct {
	write func(string) error

	id           string
	model        string
	toolIndexes  map[int]float64 // Anthropic block index → OpenAI tool_calls index
	toolCount    float64
	inputUsage   map[string]any // message_start 的 usage（input 系）
	outputUsage  map[string]any // message_delta 的 usage（output）
	includeUsage bool
}

// dispatch 分发一个已解析的上游事件。
func (e *sseEncoder) dispatch(event, data string) error {
	if data == "" || data == "[DONE]" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return nil // 非 JSON 数据跳过（对齐引擎透传容错）
	}

	switch event {
	case "message_start":
		return e.onMessageStart(payload)
	case "content_block_start":
		return e.onContentBlockStart(payload)
	case "content_block_delta":
		return e.onContentBlockDelta(payload)
	case "message_delta":
		return e.onMessageDelta(payload)
	case "message_stop":
		return e.onMessageStop()
	case "error":
		// payload 是 Anthropic 形态 {"type":"error","error":{...}}；OpenAI 要求
		// error.message / error.type 直接位于 error 之下，照抄会多一层嵌套，
		// 客户端读 error.message 得到 null。
		return e.emit(map[string]any{"error": openAIErrorObject(payload)})
	default:
		// ping / content_block_stop 等事件丢弃
		return nil
	}
}

func (e *sseEncoder) onMessageStart(payload map[string]any) error {
	message, _ := payload["message"].(map[string]any)
	if message == nil {
		return nil
	}
	e.id = newChunkID(stringOr(message["id"], "unknown"))
	e.model = stringOr(message["model"], "")
	if u, ok := message["usage"].(map[string]any); ok {
		e.inputUsage = u
	}
	// 首 chunk：delta 带 role（OpenAI 惯例 content 以空串开场）
	return e.emit(chunk(e.id, e.model, map[string]any{"role": "assistant", "content": ""}, nil))
}

func (e *sseEncoder) onContentBlockStart(payload map[string]any) error {
	block, _ := payload["content_block"].(map[string]any)
	if block == nil || block["type"] != "tool_use" {
		return nil
	}
	index, _ := payload["index"].(float64)
	toolIndex := e.toolCount
	e.toolCount++
	if e.toolIndexes == nil {
		e.toolIndexes = map[int]float64{}
	}
	e.toolIndexes[int(index)] = toolIndex
	delta := map[string]any{"tool_calls": []any{map[string]any{
		"index": toolIndex,
		"id":    block["id"],
		"type":  "function",
		"function": map[string]any{
			"name":      block["name"],
			"arguments": "",
		},
	}}}
	return e.emit(chunk(e.id, e.model, delta, nil))
}

func (e *sseEncoder) onContentBlockDelta(payload map[string]any) error {
	deltaObj, _ := payload["delta"].(map[string]any)
	if deltaObj == nil {
		return nil
	}
	switch deltaObj["type"] {
	case "text_delta":
		text, _ := deltaObj["text"].(string)
		if text == "" {
			return nil
		}
		return e.emit(chunk(e.id, e.model, map[string]any{"content": text}, nil))
	case "input_json_delta":
		index, _ := payload["index"].(float64)
		toolIndex := e.toolIndexes[int(index)]
		partial, _ := deltaObj["partial_json"].(string)
		if partial == "" {
			return nil
		}
		delta := map[string]any{"tool_calls": []any{map[string]any{
			"index":    toolIndex,
			"function": map[string]any{"arguments": partial},
		}}}
		return e.emit(chunk(e.id, e.model, delta, nil))
	default:
		// thinking_delta 等未知增量忽略
		return nil
	}
}

func (e *sseEncoder) onMessageDelta(payload map[string]any) error {
	if u, ok := payload["usage"].(map[string]any); ok {
		e.outputUsage = u
	}
	deltaObj, _ := payload["delta"].(map[string]any)
	stopReason := any(nil)
	if deltaObj != nil {
		stopReason = mapStopReason(deltaObj["stop_reason"])
	}
	return e.emit(chunk(e.id, e.model, map[string]any{}, stopReason))
}

func (e *sseEncoder) onMessageStop() error {
	if e.includeUsage {
		merged := mergeUsage(e.inputUsage, e.outputUsage)
		data, err := marshalCompact(usageChunk(e.id, e.model, merged))
		if err != nil {
			return err
		}
		if err := e.write("data: " + data + "\n\n"); err != nil {
			return err
		}
	}
	return e.write("data: [DONE]\n\n")
}

// emit 序列化并写出一个 chunk 事件。
// openAIErrorObject 把 Anthropic 错误对象压平为 OpenAI 形态。
// 输入形如 {"type":"error","error":{"type":"...","message":"..."}}。
func openAIErrorObject(payload map[string]any) map[string]any {
	if inner, ok := payload["error"].(map[string]any); ok {
		out := map[string]any{}
		for k, v := range inner {
			out[k] = v
		}
		return out
	}
	// 上游直接给了扁平错误体：原样透传，只保证有 message 字段
	out := map[string]any{}
	for k, v := range payload {
		if k == "type" {
			continue
		}
		out[k] = v
	}
	if _, ok := out["message"]; !ok {
		out["message"] = "上游返回错误"
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "upstream_error"
	}
	return out
}

func (e *sseEncoder) emit(payload map[string]any) error {
	data, err := marshalCompact(payload)
	if err != nil {
		return err
	}
	return e.write("data: " + data + "\n\n")
}

// mergeUsage 合并 message_start（input 系）与 message_delta（output）的 usage。
func mergeUsage(input, output map[string]any) map[string]any {
	return mapUsage(mergeUsageMax(input, output))
}

// mergeUsageMax 合并两段 Anthropic usage，数值键取最大。
//
// message_start 带 input 系字段、message_delta 带 output 系字段，但上游
// 可能在 message_delta 里重复携带 input 键（例如 input_tokens: 0）。
// 直接覆盖会让真实的输入量被归零，客户端计费与上下文统计随之出错。
// gateway/usage.go 对同一问题已采用 max，这里保持一致。
func mergeUsageMax(input, output map[string]any) map[string]any {
	merged := map[string]any{}
	for k, v := range input {
		merged[k] = v
	}
	for k, v := range output {
		if prev, ok := merged[k]; ok && numericGreater(prev, v) {
			continue
		}
		merged[k] = v
	}
	return merged
}

// numericGreater 判断 a 是否为大于 b 的数值（非数值键返回 false，走覆盖）。
func numericGreater(a, b any) bool {
	af, aok := toFloatOK(a)
	bf, bok := toFloatOK(b)
	if !aok || !bok {
		return false
	}
	return af > bf
}

func toFloatOK(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// stringOr 取字符串值，nil 或空时回退。
func stringOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}
