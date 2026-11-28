package gateway

import "testing"

func TestNormalizeEffort(t *testing.T) {
	cases := []struct {
		model string
		in    string
		want  string
	}{
		{"GLM-5.4", "medium", "high"},
		{"GLM-5.3-flash", "medium", "high"},
		{"GLM-5.3-flash", "none", "low"},
		{"GLM-5.3-flash", "disabled", "low"},
		{"GLM-5.3", "none", "none"}, // 非 Flash 不猜測
		{"GLM-5.4", "low", "low"},
		{"GLM-5.4", "max", "max"},
		{"GLM-5.4", "unexpected", "unexpected"},
		{"gpt-5", "medium", "medium"}, // 非 GLM 不動
	}
	for _, tc := range cases {
		if got := NormalizeEffort(tc.model, tc.in); got != tc.want {
			t.Fatalf("NormalizeEffort(%q, %q)=%q, want %q", tc.model, tc.in, got, tc.want)
		}
	}
}

func TestNormalizeThinkingFlashDisabled(t *testing.T) {
	body := map[string]any{
		"model":    "glm-5.3-flash",
		"thinking": map[string]any{"type": "disabled"},
	}
	NormalizeBodyFor(body, false, "", "")
	if _, ok := body["thinking"]; ok {
		t.Fatalf("Flash 關閉思考應移除 thinking: %v", body)
	}
	output, ok := body["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("應補 output_config: %v", body)
	}
	if output["effort"] != "low" {
		t.Fatalf("Flash 關閉思考應歸一為 low: %v", output)
	}
}

func TestNormalizeThinkingMedium(t *testing.T) {
	body := map[string]any{
		"model":         "GLM-5.4",
		"output_config": map[string]any{"effort": "medium"},
	}
	NormalizeBodyFor(body, false, "", "")
	output := body["output_config"].(map[string]any)
	if output["effort"] != "high" {
		t.Fatalf("medium 應映射為 high: %v", output)
	}
}

func TestNormalizeThinkingNonFlashDisabledUnchanged(t *testing.T) {
	thinking := map[string]any{"type": "disabled"}
	body := map[string]any{"model": "GLM-5.3", "thinking": thinking}
	NormalizeBodyFor(body, false, "", "")
	if body["thinking"] == nil {
		t.Fatal("非 Flash 的 thinking 不應被移除")
	}
	if _, ok := body["output_config"]; ok {
		t.Fatal("非 Flash 不應注入 output_config")
	}
}
