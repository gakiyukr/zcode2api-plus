package gateway

import (
	"strings"
	"testing"

	"zcode2api/internal/upstream"
)

func TestNormalizeBodyModel(t *testing.T) {
	body := map[string]any{"model": "zai/glm-5.3"}
	NormalizeBody(body, false)
	if body["model"] != "GLM-5.3" {
		t.Fatalf("前缀剥离 + 映射不符: %v", body["model"])
	}

	// 不在映射表内的模型保持原样（白名单按 normalize 后比对）
	body2 := map[string]any{"model": "glm_5_3_flash"}
	NormalizeBody(body2, false)
	if body2["model"] != "glm_5_3_flash" {
		t.Fatalf("未映射模型不应改动: %v", body2["model"])
	}
}

func TestNormalizeBodyContentBridge(t *testing.T) {
	original := map[string]any{"role": "user", "content": "hi"}
	body := map[string]any{"messages": []any{original, map[string]any{
		"role":    "assistant",
		"content": []any{map[string]any{"type": "text", "text": "already-block"}},
	}}}
	NormalizeBody(body, false)

	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)
	blocks, ok := first["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("字符串 content 应桥接为 text block: %v", first)
	}
	if blocks[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("桥接内容不符: %v", blocks[0])
	}
	// 原始消息 map 不被改写（对齐 Python 版的 {**msg, ...} 拷贝）
	if original["content"] != "hi" {
		t.Fatalf("原始 map 不应被改写: %v", original)
	}
	// 已是列表的 content 原样保留
	second := msgs[1].(map[string]any)
	if _, isString := second["content"].(string); isString {
		t.Fatal("列表 content 不应被二次桥接")
	}
}

func TestNormalizeBodySystemInjection(t *testing.T) {
	p := upstream.ZcodePrompts()
	if p == nil {
		t.Fatal("zcode_system.json 解析失败")
	}

	// 键不存在 → 重建为官方三块
	body := map[string]any{"model": "GLM-5.3", "messages": []any{}}
	NormalizeBody(body, true)
	sys, ok := body["system"].([]any)
	if !ok || len(sys) != 3 {
		t.Fatalf("system 应重建为官方三块: %v", body["system"])
	}
	if sys[0].(map[string]any)["text"] != p.CliPrefix {
		t.Fatalf("第一块应为官方 cliPrefix: %v", sys[0])
	}
	if _, hasCache := sys[0].(map[string]any)["cache_control"]; !hasCache {
		t.Fatal("system 块应带 cache_control")
	}
	// 日期前缀应插在首个 turn
	msgs := body["messages"].([]any)
	prefix := msgs[0].(map[string]any)
	if prefix["role"] != "user" {
		t.Fatalf("首个 turn 应为日期前缀: %v", prefix)
	}
	// metadata.user_id 应标记设备
	meta := body["metadata"].(map[string]any)
	if !strings.Contains(meta["user_id"].(string), `"device_id"`) {
		t.Fatalf("metadata.user_id 应为官方 JSON 形态: %v", meta["user_id"])
	}

	// 字符串 system → 并入第三块文本（不再是独立块）
	body2 := map[string]any{"model": "GLM-5.3", "system": "be brief", "messages": []any{}}
	NormalizeBody(body2, true)
	sys2 := body2["system"].([]any)
	if len(sys2) != 3 {
		t.Fatalf("外部 system 应并入第三块，总量仍为三块: %d", len(sys2))
	}
	third := sys2[2].(map[string]any)["text"].(string)
	if !strings.HasSuffix(third, "be brief") {
		t.Fatalf("调用方文本应在第三块末尾: %q", third[max(0, len(third)-40):])
	}

	// 列表 system → 各块文本并入第三块
	userBlocks := []any{
		map[string]any{"type": "text", "text": "alpha"},
		map[string]any{"type": "text", "text": "beta"},
	}
	body3 := map[string]any{"model": "GLM-5.3", "system": userBlocks, "messages": []any{}}
	NormalizeBody(body3, true)
	sys3 := body3["system"].([]any)
	if len(sys3) != 3 {
		t.Fatalf("列表 system 应并入第三块: %d", len(sys3))
	}
	third3 := sys3[2].(map[string]any)["text"].(string)
	if !strings.Contains(third3, "alpha") || !strings.Contains(third3, "beta") {
		t.Fatalf("列表 system 文本应全部并入: %q", third3[max(0, len(third3)-60):])
	}

	// 消息 content 的 cache_control 被清除，仅最后消息末块保留
	msg := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "c1", "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": "c2"},
		},
	}
	body4 := map[string]any{"model": "GLM-5.3", "messages": []any{msg}}
	NormalizeBody(body4, true)
	msgs4 := body4["messages"].([]any)
	lastMsg := msgs4[len(msgs4)-1].(map[string]any)
	blocks4 := lastMsg["content"].([]any)
	if _, has := blocks4[0].(map[string]any)["cache_control"]; has {
		t.Fatal("消息中间块不应保留 cache_control")
	}
	if _, has := blocks4[1].(map[string]any)["cache_control"]; !has {
		t.Fatal("最后消息的末块应带 cache_control")
	}

	// 不需要注入时不改动
	body5 := map[string]any{}
	NormalizeBody(body5, false)
	if _, ok := body5["system"]; ok {
		t.Fatal("非 JWT 账号不应注入 system")
	}
}
