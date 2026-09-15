// /v1/responses 测试：请求转换、响应转换、流式重编码、previous_response_id 400、
// 以及复用 fixture 的端到端链路。
package openai

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestResponsesConvertStringInput(t *testing.T) {
	got, err := ConvertResponsesRequest(map[string]any{
		"model":             "glm-5.3-flash",
		"instructions":      "你是助手",
		"input":             "你好",
		"max_output_tokens": float64(512),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(512) {
		t.Fatalf("max_output_tokens 未映射: %v", got["max_tokens"])
	}
	sys, _ := got["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["text"] != "你是助手" {
		t.Fatalf("instructions 未转 system: %v", got["system"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("应 1 条消息: %v", msgs)
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Fatalf("角色应为 user: %v", msg)
	}
}

func TestResponsesConvertItems(t *testing.T) {
	got, err := ConvertResponsesRequest(map[string]any{
		"model": "glm-5.3-flash",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "查天气"},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"台北"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "晴 28 度"},
			map[string]any{"type": "reasoning", "summary": "略"}, // 忽略
		},
		"tools": []any{map[string]any{
			"type": "function", "name": "get_weather", "description": "查天气",
			"parameters": map[string]any{"type": "object"},
		}},
		"reasoning": map[string]any{"effort": "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("扁平 tools 未转换: %v", got["tools"])
	}
	oc, _ := got["output_config"].(map[string]any)
	if oc["effort"] != "high" {
		t.Fatalf("reasoning.effort 未映射: %v", got["output_config"])
	}
	msgs, _ := got["messages"].([]any)
	// user → assistant(tool_use) → user(tool_result) 共 3 条
	if len(msgs) != 3 {
		t.Fatalf("应 3 条消息: %v", msgs)
	}
	if msgs[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("function_call 应成 assistant 消息: %v", msgs[1])
	}
	last := msgs[2].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("function_call_output 应成 user 消息: %v", last)
	}
	block := last["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_result" || block["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result 绑定不符: %v", block)
	}
}

func TestResponsesPreviousResponseIDRejected(t *testing.T) {
	_, err := ConvertResponsesRequest(map[string]any{
		"model": "glm-5.3-flash", "previous_response_id": "resp_x", "input": "hi",
	})
	if err == nil || !strings.Contains(err.Error(), "previous_response_id") {
		t.Fatalf("previous_response_id 应 400: %v", err)
	}
}

func TestResponsesConvertResponseShape(t *testing.T) {
	got := ConvertResponsesResponse(map[string]any{
		"id": "msg_a", "model": "GLM-5.3", "stop_reason": "tool_use",
		"usage": map[string]any{"input_tokens": 6, "output_tokens": 3},
		"content": []any{
			map[string]any{"type": "text", "text": "部分"},
			map[string]any{"type": "tool_use", "id": "call_1", "name": "f", "input": map[string]any{"a": 1}},
		},
	})
	if got == nil {
		t.Fatal("不应返回 nil")
	}
	if got["object"] != "response" || got["status"] != "completed" {
		t.Fatalf("顶层形态不符: %v", got)
	}
	output, _ := got["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("应 2 个 output item: %v", output)
	}
	msgItem := output[0].(map[string]any)
	if msgItem["type"] != "message" {
		t.Fatalf("首 item 应为 message: %v", msgItem)
	}
	text := msgItem["content"].([]any)[0].(map[string]any)
	if text["type"] != "output_text" || text["text"] != "部分" {
		t.Fatalf("output_text 不符: %v", text)
	}
	call := output[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["arguments"] != `{"a":1}` {
		t.Fatalf("function_call item 不符: %v", call)
	}
	usage := got["usage"].(map[string]any)
	if usage["input_tokens"] != float64(6) || usage["total_tokens"] != float64(9) {
		t.Fatalf("usage 不符: %v", usage)
	}
}

func TestResponsesStreamEvents(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m1","model":"GLM-5.3","usage":{"input_tokens":4}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"你好"}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"c1","name":"f"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var events []string
	if err := reencodeResponsesSSE(strings.NewReader(upstream), func(ev string) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(events))
	for _, ev := range events {
		name := strings.SplitN(ev, "\n", 2)[0]
		names = append(names, strings.TrimPrefix(name, "event: "))
	}
	want := []string{
		"response.created", "response.output_text.delta",
		"response.output_item.added", "response.function_call_arguments.delta",
		"response.completed",
	}
	if len(names) != len(want) {
		t.Fatalf("事件序列不符: %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("第 %d 个事件应为 %s，得到 %s", i, want[i], names[i])
		}
	}

	// completed 事件携带完整 output 与 usage
	var payload map[string]any
	for _, ev := range events {
		if strings.Contains(ev, "response.completed") {
			dataLine := strings.SplitN(ev, "\n", 2)[1]
			if err := json.Unmarshal([]byte(strings.TrimPrefix(dataLine, "data: ")), &payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	resp, _ := payload["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Fatalf("completed 状态不符: %v", resp)
	}
	output, _ := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("completed 应含 message + function_call: %v", output)
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(2) {
		t.Fatalf("completed usage 不符: %v", usage)
	}

	// output_item.added 与 function_call_arguments.delta 必须用同一个 item_id，
	// 否则客户端无法把参数增量关联到对应工具调用。
	var addedID, deltaID string
	for _, ev := range events {
		data := strings.TrimPrefix(strings.SplitN(ev, "\n", 2)[1], "data: ")
		var obj map[string]any
		if json.Unmarshal([]byte(data), &obj) != nil {
			continue
		}
		switch {
		case strings.Contains(ev, "response.output_item.added"):
			if item, ok := obj["item"].(map[string]any); ok {
				addedID = stringOf(item["id"])
			}
		case strings.Contains(ev, "response.function_call_arguments.delta"):
			deltaID = stringOf(obj["item_id"])
		}
	}
	if addedID == "" || deltaID == "" {
		t.Fatalf("未捕获到 item id: added=%q delta=%q", addedID, deltaID)
	}
	if addedID != deltaID {
		t.Fatalf("item_id 必须一致: added=%q delta=%q", addedID, deltaID)
	}
}

func TestResponsesE2ENonStream(t *testing.T) {
	f := newFixture(t)
	f.respond = func(int) (int, string, string) {
		return http.StatusOK, "application/json",
			`{"id":"msg_a","type":"message","role":"assistant","model":"GLM-5.3",
			  "stop_reason":"end_turn","usage":{"input_tokens":6,"output_tokens":3},
			  "content":[{"type":"text","text":"回答"}]}`
	}
	code, body := post(f, t, "sk-test", `{"model":"glm-5.3-flash","input":"你好"}`)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %s", code, body)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["object"] != "response" {
		t.Fatalf("应 response 对象: %s", body)
	}
	up := f.lastUpstream()
	if up.Body["max_tokens"] != float64(8192) {
		t.Fatalf("缺省 max_tokens 应 8192: %v", up.Body["max_tokens"])
	}
}

func TestResponsesE2EStream(t *testing.T) {
	f := newFixture(t)
	f.respond = func(int) (int, string, string) {
		return http.StatusOK, "text/event-stream",
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"GLM-5.3\",\"usage\":{\"input_tokens\":4}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"回复\"}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	code, body := post(f, t, "sk-test", `{"model":"glm-5.3-flash","input":"hi","stream":true}`)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %s", code, body)
	}
	if !strings.Contains(body, "event: response.created") ||
		!strings.Contains(body, "event: response.output_text.delta") ||
		!strings.Contains(body, "event: response.completed") {
		t.Fatalf("response.* 事件缺失: %s", body)
	}
}

// post /v1/responses 请求助手（与 postChat 同源 fixture）。
func post(f *fixture, t *testing.T, key, payload string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/responses", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := make([]byte, 0)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, string(raw)
}
