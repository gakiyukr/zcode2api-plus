// 请求转换单测：§5.7 映射表逐条一测。
package openai

import (
	"encoding/json"
	"reflect"
	"testing"
)

// mustJSON 解析 JSON 字面量为 map/slice，方便构造与断言。
func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("测试夹具 JSON 非法: %v", err)
	}
	return v
}

// convertInput 以 map 形态构造 OpenAI 请求并转换。
func convertInput(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	out, err := ConvertRequest(body)
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	return out
}

func TestSystemAndDeveloperMergeInOrder(t *testing.T) {
	// system/developer 归并到顶层 system，按出现顺序拼接（引擎注入
	// zcode_system 块时自然排在归并结果之前，见 gateway/body.go）。
	out := convertInput(t, map[string]any{
		"model": "glm-5.3-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "规则A"},
			map[string]any{"role": "developer", "content": []any{
				map[string]any{"type": "text", "text": "规则B"},
			}},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	want := mustJSON(t, `[
		{"type":"text","text":"规则A"},
		{"type":"text","text":"规则B"}]`)
	if !reflect.DeepEqual(out["system"], want) {
		t.Fatalf("system 归并不符: %v", out["system"])
	}
	if got := len(out["messages"].([]any)); got != 1 {
		t.Fatalf("system 不应出现在 messages: %d", got)
	}
}

func TestUserStringContentBridgesToTextBlock(t *testing.T) {
	out := convertInput(t, map[string]any{
		"model":    "glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	msg := out["messages"].([]any)[0].(map[string]any)
	if msg["role"] != "user" {
		t.Fatalf("role 应保留: %v", msg)
	}
	want := mustJSON(t, `[{"type":"text","text":"hi"}]`)
	if !reflect.DeepEqual(msg["content"], want) {
		t.Fatalf("字符串 content 应桥接为 text block: %v", msg["content"])
	}
}

func TestImageDataURLBecomesBase64Source(t *testing.T) {
	out := convertInput(t, map[string]any{
		"model": "glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "看图"},
			map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "data:image/png;base64,iVBORw0=",
			}},
		}}},
	})
	msg := out["messages"].([]any)[0].(map[string]any)
	want := mustJSON(t, `[
		{"type":"text","text":"看图"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0="}}]`)
	if !reflect.DeepEqual(msg["content"], want) {
		t.Fatalf("data URL 应转为 base64 image block: %v", msg["content"])
	}
}

func TestImageHTTPURLBecomesURLSource(t *testing.T) {
	out := convertInput(t, map[string]any{
		"model": "glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "https://example.com/a.png",
			}},
		}}},
	})
	msg := out["messages"].([]any)[0].(map[string]any)
	want := mustJSON(t, `[
		{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]`)
	if !reflect.DeepEqual(msg["content"], want) {
		t.Fatalf("http(s) URL 应转为 url image block: %v", msg["content"])
	}
}

func TestAssistantToolCallsBecomeToolUseBlocks(t *testing.T) {
	// assistant 的 tool_calls → tool_use block（arguments JSON 字符串解析为 input），
	// 与文本 content 共存。
	out := convertInput(t, map[string]any{
		"model": "glm-5.3-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "北京天气？"},
			map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id":   "call_1",
					"type": "function",
					"function": map[string]any{
						"name":      "get_weather",
						"arguments": `{"city":"北京"}`,
					},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "晴"},
		},
	})
	msgs := out["messages"].([]any)
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("assistant 角色应保留: %v", asst)
	}
	want := mustJSON(t, `[{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"北京"}}]`)
	if !reflect.DeepEqual(asst["content"], want) {
		t.Fatalf("tool_calls 应转为 tool_use block: %v", asst["content"])
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "user" {
		t.Fatalf("tool 消息应映射为 user: %v", tool)
	}
	wantTool := mustJSON(t, `[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"晴"}]}]`)
	if !reflect.DeepEqual(tool["content"], wantTool) {
		t.Fatalf("tool 消息应转为 tool_result block: %v", tool["content"])
	}
}

func TestMaxTokensMapping(t *testing.T) {
	// max_tokens 优先；缺 max_tokens 时取 max_completion_tokens；皆缺省 8192。
	out := convertInput(t, map[string]any{
		"model": "glm-5.3-flash", "max_tokens": float64(100),
		"messages": []any{},
	})
	if out["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens 应保留: %v", out["max_tokens"])
	}

	out = convertInput(t, map[string]any{
		"model": "glm-5.3-flash", "max_completion_tokens": float64(200),
		"messages": []any{},
	})
	if out["max_tokens"] != float64(200) {
		t.Fatalf("max_completion_tokens 应映射: %v", out["max_tokens"])
	}
	if _, ok := out["max_completion_tokens"]; ok {
		t.Fatal("输出不应携带 max_completion_tokens")
	}

	out = convertInput(t, map[string]any{"model": "glm-5.3-flash", "messages": []any{}})
	if out["max_tokens"] != float64(8192) {
		t.Fatalf("皆缺省应为 8192: %v", out["max_tokens"])
	}
}

func TestTemperatureTopPAndStopPassThrough(t *testing.T) {
	out := convertInput(t, map[string]any{
		"model": "glm-5.3-flash", "temperature": 0.5, "top_p": 0.9,
		"stop": "END", "messages": []any{},
	})
	if out["temperature"] != 0.5 || out["top_p"] != 0.9 {
		t.Fatalf("temperature/top_p 应透传: %v", out)
	}
	want := mustJSON(t, `["END"]`)
	if !reflect.DeepEqual(out["stop_sequences"], want) {
		t.Fatalf("字符串 stop 应转单元素 stop_sequences: %v", out["stop_sequences"])
	}

	out = convertInput(t, map[string]any{
		"model": "glm-5.3-flash", "stop": []any{"A", "B"}, "messages": []any{},
	})
	want = mustJSON(t, `["A","B"]`)
	if !reflect.DeepEqual(out["stop_sequences"], want) {
		t.Fatalf("数组 stop 应映射: %v", out["stop_sequences"])
	}
}

func TestToolsAndToolChoiceMapping(t *testing.T) {
	out := convertInput(t, map[string]any{
		"model": "glm-5.3-flash",
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "查天气",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		}},
		"tool_choice": "auto",
		"messages":    []any{},
	})
	wantTools := mustJSON(t, `[{"name":"get_weather","description":"查天气",
		"input_schema":{"type":"object","properties":{}}}]`)
	if !reflect.DeepEqual(out["tools"], wantTools) {
		t.Fatalf("tools 映射不符: %v", out["tools"])
	}
	wantChoice := mustJSON(t, `{"type":"auto"}`)
	if !reflect.DeepEqual(out["tool_choice"], wantChoice) {
		t.Fatalf("tool_choice auto 映射不符: %v", out["tool_choice"])
	}

	// named choice → {type:"tool", name}
	out = convertInput(t, map[string]any{
		"model": "glm-5.3-flash", "messages": []any{},
		"tool_choice": map[string]any{"type": "function",
			"function": map[string]any{"name": "get_weather"}},
	})
	wantChoice = mustJSON(t, `{"type":"tool","name":"get_weather"}`)
	if !reflect.DeepEqual(out["tool_choice"], wantChoice) {
		t.Fatalf("named tool_choice 映射不符: %v", out["tool_choice"])
	}

	// none / required 语义映射
	out = convertInput(t, map[string]any{
		"model": "glm-5.3-flash", "messages": []any{}, "tool_choice": "none",
	})
	if !reflect.DeepEqual(out["tool_choice"], mustJSON(t, `{"type":"none"}`)) {
		t.Fatalf("none 应映射 type:none: %v", out["tool_choice"])
	}
	out = convertInput(t, map[string]any{
		"model": "glm-5.3-flash", "messages": []any{}, "tool_choice": "required",
	})
	if !reflect.DeepEqual(out["tool_choice"], mustJSON(t, `{"type":"any"}`)) {
		t.Fatalf("required 应映射 type:any: %v", out["tool_choice"])
	}
}

func TestNGreaterThanOneRejected(t *testing.T) {
	_, err := ConvertRequest(map[string]any{
		"model": "glm-5.3-flash", "n": float64(2), "messages": []any{},
	})
	if err == nil {
		t.Fatal("n>1 应拒绝")
	}
	// n=1 或缺失正常
	if _, err := ConvertRequest(map[string]any{
		"model": "glm-5.3-flash", "n": float64(1), "messages": []any{},
	}); err != nil {
		t.Fatalf("n=1 应通过: %v", err)
	}
}

func TestStreamOptionsIncludeUsageNotForwarded(t *testing.T) {
	// include_usage 是 OpenAI 侧参数，Messages API 无对应字段：
	// 不得出现在送上游的请求体（handler 从原始 body 读取）。
	out := convertInput(t, map[string]any{
		"model": "glm-5.3-flash", "messages": []any{}, "stream": true,
		"stream_options": map[string]any{"include_usage": true},
	})
	if out["stream"] != true {
		t.Fatalf("stream 应透传: %v", out["stream"])
	}
	if _, leaked := out["include_usage"]; leaked {
		t.Fatalf("include_usage 不应外泄到上游请求体: %v", out["include_usage"])
	}
}

func TestOpenAIDirectivesSilentlyIgnored(t *testing.T) {
	// presence_penalty / frequency_penalty / logprobs / user 等静默忽略
	out := convertInput(t, map[string]any{
		"model": "glm-5.3-flash", "messages": []any{},
		"presence_penalty": 0.1, "frequency_penalty": 0.2, "logprobs": true,
		"user": "u-1", "seed": 42,
	})
	for _, key := range []string{"presence_penalty", "frequency_penalty", "logprobs", "user", "seed"} {
		if _, ok := out[key]; ok {
			t.Fatalf("%s 应被忽略", key)
		}
	}
}
