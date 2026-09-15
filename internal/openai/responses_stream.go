// Responses 流式重编码：上游 Anthropic SSE → OpenAI Responses 的 response.* 事件。
// 事件序列：response.created →（output_item.added / output_text.delta /
// function_call_arguments.delta）→ response.completed。
package openai

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// reencodeResponsesSSE 读取上游 SSE 并写出 Responses 事件流。
func reencodeResponsesSSE(body io.Reader, write func(string) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	enc := &responsesEncoder{write: write, startedAt: time.Now()}
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
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if event != "" || data.Len() > 0 {
		return flush()
	}
	return nil
}

// responsesEncoder 跨事件流状态。
type responsesEncoder struct {
	write     func(string) error
	startedAt time.Time

	respID        string
	model         string
	created       float64
	finished      bool
	inputUsage    map[string]any
	outputUsage   map[string]any
	textParts     []string
	functionCalls []any // 完整 function_call item（completed 时回填 output）
}

// emit 写出一个 `event: X\ndata: {...}\n\n` 事件。
func (e *responsesEncoder) emit(name string, payload map[string]any) error {
	payload["type"] = name
	data, err := marshalCompact(payload)
	if err != nil {
		return err
	}
	return e.write("event: " + name + "\ndata: " + data + "\n\n")
}

func (e *responsesEncoder) dispatch(event, data string) error {
	if data == "" || data == "[DONE]" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return nil
	}

	switch event {
	case "message_start":
		return e.onMessageStart(payload)
	case "content_block_start":
		return e.onContentBlockStart(payload)
	case "content_block_delta":
		return e.onContentBlockDelta(payload)
	case "message_delta":
		if u, ok := payload["usage"].(map[string]any); ok {
			e.outputUsage = u
		}
		return nil
	case "message_stop":
		return e.onMessageStop()
	default:
		return nil
	}
}

func (e *responsesEncoder) onMessageStart(payload map[string]any) error {
	message, _ := payload["message"].(map[string]any)
	if message == nil {
		return nil
	}
	e.respID = stringOr(message["id"], "unknown")
	e.model = stringOr(message["model"], "")
	e.created = float64(e.startedAt.Unix())
	if u, ok := message["usage"].(map[string]any); ok {
		e.inputUsage = u
	}
	return e.emit("response.created", map[string]any{
		"response": e.responseEnvelope("in_progress", nil),
	})
}

func (e *responsesEncoder) onContentBlockStart(payload map[string]any) error {
	block, _ := payload["content_block"].(map[string]any)
	if block == nil || block["type"] != "tool_use" {
		return nil
	}
	item := map[string]any{
		"type":      "function_call",
		"id":        "fc_" + stringOf(block["id"]),
		"call_id":   block["id"],
		"name":      block["name"],
		"arguments": "",
		"status":    "in_progress",
	}
	e.functionCalls = append(e.functionCalls, item)
	return e.emit("response.output_item.added", map[string]any{
		"output_index": float64(len(e.functionCalls)), // message item 占 index 0
		"item":         item,
	})
}

func (e *responsesEncoder) onContentBlockDelta(payload map[string]any) error {
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
		e.textParts = append(e.textParts, text)
		return e.emit("response.output_text.delta", map[string]any{
			"item_id": "msg_" + e.respID,
			"delta":   text,
		})
	case "input_json_delta":
		partial, _ := deltaObj["partial_json"].(string)
		if partial == "" {
			return nil
		}
		itemID := ""
		if len(e.functionCalls) > 0 {
			if item, ok := e.functionCalls[len(e.functionCalls)-1].(map[string]any); ok {
				item["arguments"] = stringOf(item["arguments"]) + partial
				// item_id 必须与 output_item.added 一致（同一 function_call item），
				// 否则客户端无法把增量关联到对应工具调用。
				itemID = stringOf(item["id"])
			}
		}
		return e.emit("response.function_call_arguments.delta", map[string]any{
			"item_id": itemID,
			"delta":   partial,
		})
	default:
		return nil
	}
}

func (e *responsesEncoder) onMessageStop() error {
	if e.finished {
		return nil
	}
	e.finished = true
	// 组装完整 output：message item 在前、function_call items 在后
	messageItem := map[string]any{
		"type":   "message",
		"id":     "msg_" + e.respID,
		"role":   "assistant",
		"status": "completed",
		"content": []any{map[string]any{
			"type":        "output_text",
			"text":        strings.Join(e.textParts, ""),
			"annotations": []any{},
		}},
	}
	for _, raw := range e.functionCalls {
		if item, ok := raw.(map[string]any); ok {
			item["status"] = "completed"
		}
	}
	output := append([]any{messageItem}, e.functionCalls...)
	return e.emit("response.completed", map[string]any{
		"response": e.responseEnvelope("completed", output),
	})
}

// responseEnvelope 组装 response 对象（created/completed 事件共用）。
func (e *responsesEncoder) responseEnvelope(status string, output []any) map[string]any {
	if output == nil {
		output = []any{}
	}
	return map[string]any{
		"id":         "resp_" + e.respID,
		"object":     "response",
		"created_at": e.created,
		"model":      e.model,
		"status":     status,
		"output":     output,
		"usage":      responsesUsage(mergeRawUsage(e.inputUsage, e.outputUsage)),
	}
}

// mergeRawUsage 合并 message_start（input 系）与 message_delta（output）的
// Anthropic 形态 usage（键保持 input_tokens/output_tokens）。
func mergeRawUsage(input, output map[string]any) map[string]any {
	merged := map[string]any{}
	for k, v := range input {
		merged[k] = v
	}
	for k, v := range output {
		merged[k] = v
	}
	return merged
}
