// 流式重编码：上游 Anthropic SSE 事件流 → OpenAI chat.completion.chunk 流。
// 不是透传：逐事件解析后按 §5.7 重新编码（ping 丢弃、[DONE] 收尾）。
package openai

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// reencodeSSE 读取上游 SSE 流并写出 OpenAI chunk 流；includeUsage 为 true 时
// 终止前附 usage chunk。write 只接收 `data: ...\n\n` 形态的完整事件。
// 返回 write 或读取的错误（客户端中断由调用方经 write 错误感知）。
func reencodeSSE(body io.Reader, includeUsage bool, write func(string) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	enc := &sseEncoder{write: write, includeUsage: includeUsage}
	event := ""
	var data strings.Builder

	flush := func() error { return enc.dispatch(event, data.String()) }
	reset := func() { event, data = "", strings.Builder{} }

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
			if err := flush(); err != nil {
				return err
			}
			reset()
		}
		// 注释行（: keepalive）与其他行忽略
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// 流以事件收尾而非空行时同样分发
	if event != "" || data.Len() > 0 {
		if err := flush(); err != nil {
			return err
		}
	}
	return nil
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
		return e.emit(map[string]any{"error": payload})
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
func (e *sseEncoder) emit(payload map[string]any) error {
	data, err := marshalCompact(payload)
	if err != nil {
		return err
	}
	return e.write("data: " + data + "\n\n")
}

// mergeUsage 合并 message_start（input 系）与 message_delta（output）的 usage。
func mergeUsage(input, output map[string]any) map[string]any {
	merged := map[string]any{}
	for k, v := range input {
		merged[k] = v
	}
	for k, v := range output {
		merged[k] = v
	}
	return mapUsage(merged)
}

// stringOr 取字符串值，nil 或空时回退。
func stringOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}
