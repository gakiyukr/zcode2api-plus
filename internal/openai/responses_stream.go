// Responses 流式重编码：上游 Anthropic SSE → OpenAI Responses 的 response.* 事件。
// 事件序列：response.created →（output_item.added / output_text.delta /
// function_call_arguments.delta）→ response.completed。
package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// reencodeResponsesSSE 读取上游 SSE 并写出 Responses 事件流。
func reencodeResponsesSSE(body io.Reader, write func(string) error) error {
	enc := &responsesEncoder{write: write, startedAt: time.Now()}
	return scanSSE(body, enc.dispatch)
}

// responsesEncoder 跨事件流状态。
type responsesEncoder struct {
	write     func(string) error
	startedAt time.Time

	respID      string
	model       string
	created     float64
	finished    bool
	inputUsage  map[string]any
	outputUsage map[string]any
	textParts   []string
	// messageAnnounced 记录 message item 是否已通过 output_item.added 宣告。
	// Codex 端只用 OutputItemAdded 设置 active_item，而 OutputTextDelta 在
	// active_item 为空时走 error_or_panic（turn.rs），故文字增量前必须先宣告。
	messageAnnounced bool
	functionCalls    []any // 完整 function_call item（completed 时回填 output）
	toolBlockMap     []int // 上游 content block index → functionCalls 陣列索引映射
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

	// 上游或代理省略 event: 行時，回退從 JSON payload 的 "type" 欄位推斷事件名稱
	if event == "" {
		if t, ok := payload["type"].(string); ok {
			event = t
		}
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
			e.outputUsage = mergeUsageMax(e.outputUsage, u)
		}
		return nil
	case "message_stop":
		return e.onMessageStop()
	case "error":
		// 上游串流報錯：發出 response.failed 事件並返回錯誤中止串流
		errPayload := openAIErrorObject(payload)
		_ = e.emit("response.failed", map[string]any{
			"response": e.responseEnvelope("failed", nil),
		})
		msg, _ := errPayload["message"].(string)
		if msg == "" {
			msg = "上游串流報錯"
		}
		return fmt.Errorf("upstream error: %s", msg)
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
	if indexVal, ok := payload["index"].(float64); ok && int(indexVal) >= 0 {
		idx := int(indexVal)
		for len(e.toolBlockMap) <= idx {
			e.toolBlockMap = append(e.toolBlockMap, -1)
		}
		e.toolBlockMap[idx] = len(e.functionCalls) - 1
	}
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
		// 必须先宣告 message item：Codex 只用 output_item.added 设置
		// active_item，而 output_text.delta 在 active_item 为空时会被丢弃
		// （debug 构建下 error_or_panic）。只宣告一次。
		if err := e.announceMessageItem(); err != nil {
			return err
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
		// 按上游 payload["index"] 路由到對應的 function_call item，未命中時回退末項
		idx := len(e.functionCalls) - 1
		if indexVal, ok := payload["index"].(float64); ok {
			mapped := int(indexVal)
			if mapped >= 0 && mapped < len(e.toolBlockMap) && e.toolBlockMap[mapped] >= 0 {
				idx = e.toolBlockMap[mapped]
			}
		}
		itemID := ""
		if idx >= 0 && idx < len(e.functionCalls) {
			if item, ok := e.functionCalls[idx].(map[string]any); ok {
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

	// 每个 item 都要发 output_item.done：Codex 只在这个事件里排入工具任务并
	// 设置 needs_follow_up（turn.rs 的 OutputItemDone 分支是唯一入口）。
	// 缺了它，模型请求的工具永不执行，会话在第一次工具调用处中断。
	if err := e.announceMessageItem(); err != nil {
		return err
	}
	if err := e.emit("response.output_item.done", map[string]any{
		"output_index": float64(0),
		"item":         messageItem,
	}); err != nil {
		return err
	}
	for i, raw := range e.functionCalls {
		if err := e.emit("response.output_item.done", map[string]any{
			"output_index": float64(i + 1), // message item 占 index 0
			"item":         raw,
		}); err != nil {
			return err
		}
	}

	output := append([]any{messageItem}, e.functionCalls...)
	return e.emit("response.completed", map[string]any{
		"response": e.responseEnvelope("completed", output),
	})
}

// announceMessageItem 首次需要时宣告 message item（幂等）。
// 无文字输出时也宣告一次：Codex 需要 OutputItemDone 才会排入工具任务。
func (e *responsesEncoder) announceMessageItem() error {
	if e.messageAnnounced {
		return nil
	}
	e.messageAnnounced = true
	return e.emit("response.output_item.added", map[string]any{
		"output_index": float64(0),
		"item": map[string]any{
			"type":    "message",
			"id":      "msg_" + e.respID,
			"role":    "assistant",
			"status":  "in_progress",
			"content": []any{},
		},
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
	return mergeUsageMax(input, output)
}
