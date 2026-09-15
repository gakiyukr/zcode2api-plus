// 响应转换单测：非流式 JSON、stop_reason/usage 映射、流式事件序列。
package openai

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// responseFixture 上游 Anthropic Messages 非流式响应夹具（顶层形态）。
const responseFixture = `{
	"id": "msg_01",
	"type": "message",
	"role": "assistant",
	"model": "GLM-5.3",
	"stop_reason": "end_turn",
	"usage": {"input_tokens": 10, "output_tokens": 5,
		"cache_read_input_tokens": 3, "cache_creation_input_tokens": 2},
	"content": [{"type": "text", "text": "你好"}]
}`

func TestConvertResponseText(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(responseFixture), &payload); err != nil {
		t.Fatal(err)
	}
	out := ConvertResponse(payload)
	if out == nil {
		t.Fatal("转换不应为 nil")
	}
	if out["id"] != "chatcmpl-msg_01" {
		t.Fatalf("id 应带 chatcmpl- 前缀: %v", out["id"])
	}
	if out["object"] != "chat.completion" || out["model"] != "GLM-5.3" {
		t.Fatalf("object/model 不符: %v", out)
	}
	choices := out["choices"].([]any)
	choice := choices[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["role"] != "assistant" || msg["content"] != "你好" {
		t.Fatalf("message 不符: %v", msg)
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("end_turn 应映射为 stop: %v", choice["finish_reason"])
	}
	if _, ok := choice["tool_calls"]; ok {
		t.Fatal("纯文本不应有 tool_calls")
	}
}

func TestConvertResponseToolUse(t *testing.T) {
	payload := map[string]any{
		"id": "msg_02", "model": "GLM-5.3", "stop_reason": "tool_use",
		"role":  "assistant",
		"usage": map[string]any{"input_tokens": 8, "output_tokens": 4},
		"content": []any{
			map[string]any{"type": "tool_use", "id": "call_9", "name": "get_weather",
				"input": map[string]any{"city": "北京"}},
		},
	}
	out := ConvertResponse(payload)
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("tool_use 应映射为 tool_calls: %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	if msg["content"] != nil {
		t.Fatalf("仅工具调用时 content 应为 null: %v", msg["content"])
	}
	want := mustJSON(t, `[{"id":"call_9","type":"function",
		"function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]`)
	if !reflect.DeepEqual(msg["tool_calls"], want) {
		t.Fatalf("tool_use 应转为 tool_calls: %v", msg["tool_calls"])
	}
}

func TestStopReasonMappings(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"refusal":       "content_filter",
		"pause_turn":    "stop", // 未列出值按 stop 处理
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Fatalf("mapStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUsageMappingSumsCacheIntoPrompt(t *testing.T) {
	// prompt_tokens = input + cache_read + cache_creation；缓存细节进 details
	usage := mapUsage(map[string]any{
		"input_tokens":                float64(10),
		"output_tokens":               float64(5),
		"cache_read_input_tokens":     float64(3),
		"cache_creation_input_tokens": float64(2),
	})
	if usage["prompt_tokens"] != float64(15) {
		t.Fatalf("prompt_tokens 应含缓存两系: %v", usage["prompt_tokens"])
	}
	if usage["completion_tokens"] != float64(5) {
		t.Fatalf("completion_tokens 应为 output: %v", usage["completion_tokens"])
	}
	if usage["total_tokens"] != float64(20) {
		t.Fatalf("total_tokens 应为和: %v", usage["total_tokens"])
	}
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(3) {
		t.Fatalf("cached_tokens 应为 cache_read: %v", details)
	}
}

// streamEvents 上游 Anthropic SSE 流夹具：文本 + 一次工具调用 + usage。
const streamEvents = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"GLM-5.3","usage":{"input_tokens":10,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}}

event: ping
data: {"type":"ping"}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"，世界"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"北京\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

// runStream 重编码并解析出 chunk 序列。
func runStream(t *testing.T, includeUsage bool) []map[string]any {
	t.Helper()
	var chunks []map[string]any
	err := reencodeSSE(strings.NewReader(streamEvents), includeUsage, func(event string) error {
		if event == "data: [DONE]\n\n" {
			return nil // 终止标记单独测（TestStreamTerminatesWithDONE）
		}
		if !strings.HasPrefix(event, "data: ") {
			t.Fatalf("chunk 应为 data: 行: %q", event)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(event, "data: ")), &m); err != nil {
			t.Fatalf("chunk JSON 非法: %v (%q)", err, event)
		}
		chunks = append(chunks, m)
		return nil
	})
	if err != nil {
		t.Fatalf("重编码失败: %v", err)
	}
	return chunks
}

func TestStreamReencodeSequence(t *testing.T) {
	// 事件序列：首 chunk role → 文本增量 ×2 → tool_calls delta（id/name）→
	// arguments 增量 ×2 → finish_reason 终止 → [DONE]
	chunks := runStream(t, false)
	if len(chunks) != 7 {
		t.Fatalf("应有 7 个 chunk（含 [DONE] 前全部数据 chunk）: %d", len(chunks))
	}
	for _, c := range chunks {
		if c["id"] != "chatcmpl-msg_1" || c["object"] != "chat.completion.chunk" {
			t.Fatalf("chunk id/object 不符: %v", c)
		}
		if c["model"] != "GLM-5.3" {
			t.Fatalf("chunk model 不符: %v", c)
		}
	}

	first := chunks[0]["choices"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(first["delta"], map[string]any{"role": "assistant", "content": ""}) {
		t.Fatalf("首 chunk 应带 role: %v", first["delta"])
	}

	text := ""
	for i := 1; i <= 2; i++ {
		choice := chunks[i]["choices"].([]any)[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		if s, ok := delta["content"].(string); ok {
			text += s
		} else {
			t.Fatalf("chunk %d 应为 content 增量: %v", i, delta)
		}
	}
	if text != "你好，世界" {
		t.Fatalf("文本增量拼接不符: %q", text)
	}

	// 工具调用开始 chunk：id/name + 空 arguments
	toolStart := chunks[3]["choices"].([]any)[0].(map[string]any)
	delta := toolStart["delta"].(map[string]any)
	call, ok := delta["tool_calls"].([]any)[0].(map[string]any)
	if !ok {
		t.Fatalf("chunk 3 应为 tool_calls delta: %v", delta)
	}
	if call["index"] != float64(0) || call["id"] != "call_1" || call["type"] != "function" {
		t.Fatalf("tool_calls 开始 delta 不符: %v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != "" {
		t.Fatalf("function 名称/初始 arguments 不符: %v", fn)
	}

	// arguments 增量 ×2
	args := ""
	for i := 4; i <= 5; i++ {
		choice := chunks[i]["choices"].([]any)[0].(map[string]any)
		one := choice["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
		if one["index"] != float64(0) {
			t.Fatalf("arguments 增量 index 应为 0: %v", one)
		}
		if fn, ok := one["function"].(map[string]any); ok {
			if _, hasID := one["id"]; hasID {
				t.Fatalf("arguments 增量不应重复 id: %v", one)
			}
			args += fn["arguments"].(string)
		} else {
			t.Fatalf("chunk %d 应为 arguments 增量: %v", i, one)
		}
	}
	if args != `{"city":"北京"}` {
		t.Fatalf("arguments 增量拼接不符: %q", args)
	}

	// 终止 chunk：finish_reason=tool_calls、空 delta
	last := chunks[6]["choices"].([]any)[0].(map[string]any)
	if last["finish_reason"] != "tool_calls" {
		t.Fatalf("终止 chunk finish_reason 不符: %v", last["finish_reason"])
	}
	if !reflect.DeepEqual(last["delta"], map[string]any{}) {
		t.Fatalf("终止 chunk delta 应为空对象: %v", last["delta"])
	}
}

func TestStreamTerminatesWithDONE(t *testing.T) {
	var tail []string
	err := reencodeSSE(strings.NewReader(streamEvents), false, func(event string) error {
		tail = append(tail, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tail); n == 0 || tail[n-1] != "data: [DONE]\n\n" {
		t.Fatalf("流应以 data: [DONE] 收尾: %v", tail)
	}
}

func TestStreamIncludeUsageAppendsChunk(t *testing.T) {
	chunks := runStream(t, true)
	if len(chunks) != 8 {
		t.Fatalf("include_usage 应多一个 chunk: %d", len(chunks))
	}
	usageChunk := chunks[len(chunks)-1]
	if arr := usageChunk["choices"].([]any); len(arr) != 0 {
		t.Fatalf("usage chunk choices 应为空数组: %v", usageChunk)
	}
	usage := usageChunk["usage"].(map[string]any)
	// input 10 + cache_read 3 + cache_creation 2 = 15
	if usage["prompt_tokens"] != float64(15) {
		t.Fatalf("usage chunk prompt_tokens 不符: %v", usage)
	}
	// output 取 message_delta 的 7
	if usage["completion_tokens"] != float64(7) {
		t.Fatalf("usage chunk completion_tokens 不符: %v", usage)
	}
}

func TestStreamPingAndUnknownDropped(t *testing.T) {
	// ping 与 thinking_delta 丢弃，不产生 chunk
	feed := `event: message_start
data: {"type":"message_start","message":{"id":"m","model":"GLM-5.3","usage":{}}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"嗯"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}

event: message_stop
data: {"type":"message_stop"}

`
	var n int
	if err := reencodeSSE(strings.NewReader(feed), false, func(string) error {
		n++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n != 3 { // 首 chunk + finish chunk + [DONE]
		t.Fatalf("ping/thinking 应丢弃，实际 chunk 数 %d", n)
	}
}

func TestConvertResponseRejectsMissingContent(t *testing.T) {
	// 缺 id 或 content（错误包装形态）应返回 nil
	if got := ConvertResponse(map[string]any{"id": "x"}); got != nil {
		t.Fatalf("缺 content 应返回 nil: %v", got)
	}
	if got := ConvertResponse(map[string]any{
		"id": "y", "content": []any{map[string]any{"type": "text", "text": "hi"}},
	}); got == nil {
		t.Fatal("正常形态不应返回 nil")
	}
}
