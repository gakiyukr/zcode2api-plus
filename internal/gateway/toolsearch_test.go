package gateway

import (
	"io"
	"strings"
	"testing"
)

func TestRewriteToolSearchRequestAndRestore(t *testing.T) {
	origTools := []any{
		map[string]any{"name": "ToolSearch", "description": "search"},
		map[string]any{"name": "Bash", "defer_loading": true},
		map[string]any{"name": "DeferredToolPlaceholder"},
	}
	body := map[string]any{
		"tools":       origTools,
		"tool_choice": map[string]any{"type": "tool", "name": "ToolSearch"},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "tu_1", "name": "ToolSearch", "input": map[string]any{}},
				},
			},
		},
	}

	if !RewriteToolSearchRequest(body) {
		t.Fatal("含 ToolSearch 應觸發改名清洗")
	}
	tools := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("應移除 DeferredToolPlaceholder 剩 2 個工具: %v", tools)
	}
	if tools[0].(map[string]any)["name"] != "ToolSearchTool" {
		t.Fatalf("ToolSearch 應改名為 ToolSearchTool: %v", tools[0])
	}
	if _, hasDefer := tools[1].(map[string]any)["defer_loading"]; hasDefer {
		t.Fatalf("應清除 defer_loading: %v", tools[1])
	}
	if body["tool_choice"].(map[string]any)["name"] != "ToolSearchTool" {
		t.Fatalf("tool_choice 應同步改名: %v", body["tool_choice"])
	}
	hist := body["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if hist["name"] != "ToolSearchTool" {
		t.Fatalf("assistant 歷史 tool_use 應同步改名: %v", hist)
	}
	// 原物件不應被就地污染
	if origTools[0].(map[string]any)["name"] != "ToolSearch" {
		t.Fatal("不得污染原始 tools 物件")
	}

	// 非串流還原
	restored := string(RestoreToolSearchJSON([]byte(`{"id":"m1","content":[{"type":"tool_use","id":"u1","name":"ToolSearchTool","input":{}}]}`)))
	if !strings.Contains(restored, `"name":"ToolSearch"`) || strings.Contains(restored, "ToolSearchTool") {
		t.Fatalf("非串流回應應還原為 ToolSearch: %s", restored)
	}

	// SSE 串流還原（含跨 chunk 切分）
	sse := "event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"u1","name":"ToolSearchTool","input":{}}}` + "\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out, err := io.ReadAll(RestoreToolSearchStream(strings.NewReader(sse)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"name":"ToolSearch"`) || strings.Contains(string(out), "ToolSearchTool") {
		t.Fatalf("SSE 串流應還原為 ToolSearch: %s", string(out))
	}
}

func TestRewriteToolSearchSkipsWhenAliasExists(t *testing.T) {
	body := map[string]any{
		"tools": []any{
			map[string]any{"name": "ToolSearch"},
			map[string]any{"name": "ToolSearchTool"},
		},
	}
	if RewriteToolSearchRequest(body) {
		t.Fatal("已存在 ToolSearchTool 時應跳過改名")
	}
}
