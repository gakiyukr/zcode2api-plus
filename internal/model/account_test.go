package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// pythonAccountJSON 模拟 Python 版 json.dumps(asdict(account)) 的输出形态：
// 全部 28 个键都在（含 null），Account 的 json tag 必须与之完全对齐。
const pythonAccountJSON = `{
  "id": "glm-acc-1a2b3c4d",
  "name": "test",
  "provider": "zai",
  "mode": "jwt",
  "email": null,
  "jwt_token": "header.payload.sig",
  "api_key": null,
  "enabled": true,
  "status": "active",
  "quota": {
    "GLM-5.3 · pro": {"total": 100, "used": 10, "remaining": 90, "available": 90,
      "period": "monthly", "period_start": null, "period_end": null, "expires_at": null,
      "model": "GLM-5.3", "plan_name": "pro", "plan_is_trial": false},
    "GLM-5.3-Flash · trial": {"total": 50, "used": 50, "remaining": 0, "available": 0,
      "period": "daily", "period_start": null, "period_end": null, "expires_at": null,
      "model": "GLM-5.3-Flash", "plan_name": "trial", "plan_is_trial": true}
  },
  "exhausted_models": [],
  "disabled_models": ["glm_5.3_flash"],
  "plan": {"plan_name": "GLM Coding Pro"},
  "plans": [{"plan_name": "GLM Coding Pro", "entitlements": []}],
  "usage": {},
  "use_count": 3,
  "fail_count": 1,
  "total_input_tokens": 11,
  "total_output_tokens": 22,
  "total_cache_creation_tokens": 2,
  "total_cache_read_tokens": 5,
  "last_used_at": 1700000000.0,
  "last_checked_at": 1700000000.5,
  "cooling_until": null,
  "last_error": null,
  "proxy_url": null,
  "proxy_id": null,
  "created_at": 1700000001.5
}`

// pythonAccountKeys Python dataclass asdict 输出的全部键（序列化契约）。
// archived_at 是 Go 版新增（归档功能）：Python 侧已退休，旧版 JSON 读取时缺失即 nil，
// Python json.loads 对多出的键会原样保留在 dict 中，不影响旧数据互读。
var pythonAccountKeys = []string{
	"id", "name", "provider", "mode", "email", "jwt_token", "api_key",
	"enabled", "status", "quota", "exhausted_models", "disabled_models",
	"plan", "plans", "usage", "use_count", "fail_count",
	"total_input_tokens", "total_output_tokens", "total_cache_creation_tokens",
	"total_cache_read_tokens", "last_used_at", "last_checked_at", "cooling_until",
	"last_error", "proxy_url", "proxy_id", "created_at", "archived_at",
}

func TestJSONContractWithPython(t *testing.T) {
	acc, err := FromJSON([]byte(pythonAccountJSON))
	if err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	if acc.ID != "glm-acc-1a2b3c4d" || acc.Name != "test" || acc.Mode != "jwt" {
		t.Fatalf("基本字段不符: %+v", acc)
	}
	if acc.Secret() != "header.payload.sig" {
		t.Fatalf("Secret 不符: %q", acc.Secret())
	}
	if acc.Email != nil || acc.CoolingUntil != nil || acc.LastError != nil {
		t.Fatalf("null 字段应为 nil 指针")
	}
	if acc.LastUsedAt == nil || *acc.LastUsedAt != 1700000000.0 {
		t.Fatalf("last_used_at 不符: %v", acc.LastUsedAt)
	}
	if acc.TotalCacheCreationTokens != 2 || acc.TotalCacheReadTokens != 5 {
		t.Fatalf("token 统计不符")
	}
	if !reflect.DeepEqual(acc.ExhaustedModels, []string{}) {
		t.Fatalf("空列表应保持 [] 而非 nil: %v", acc.ExhaustedModels)
	}

	// 序列化回 JSON 后，键集合必须与 Python asdict 完全一致（双向互通的前提）。
	out, err := json.Marshal(acc)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	want := map[string]bool{}
	for _, k := range pythonAccountKeys {
		want[k] = true
	}
	if len(got) != len(want) {
		t.Fatalf("键数量不符: got %d, want %d；差异: %v vs %v", len(got), len(want), keysOf(got), pythonAccountKeys)
	}
	for k := range got {
		if !want[k] {
			t.Fatalf("多出的键: %s", k)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestCreate(t *testing.T) {
	jwt := Create(ProviderZai, "main", "header.payload.sig")
	if jwt.Mode != "jwt" || jwt.JWTToken == nil || jwt.APIKey != nil {
		t.Fatalf("jwt 凭证判定错误: %+v", jwt)
	}
	if jwt.Status != StatusActive || !jwt.Enabled || jwt.Provider != ProviderZai {
		t.Fatalf("默认状态错误: %+v", jwt)
	}
	key := Create(ProviderZai, "", "sk-abc.def")
	if key.Name != "zai-account" {
		t.Fatalf("空名称应兜底 zai-account: %q", key.Name)
	}
	if key.Mode != "apiKey" || key.APIKey == nil {
		t.Fatalf("apiKey 凭证判定错误")
	}
}

func TestIsSelectable(t *testing.T) {
	now := time.Unix(1700000000, 0)
	acc := &Account{Enabled: true, Status: StatusActive}
	if !acc.IsSelectable(now) {
		t.Fatal("active 应可选")
	}
	acc.Status = StatusExhausted
	if acc.IsSelectable(now) {
		t.Fatal("exhausted 不可选")
	}
	acc.Status = StatusInvalid
	if acc.IsSelectable(now) {
		t.Fatal("invalid 不可选")
	}
	acc.Status = StatusDisabled
	if acc.IsSelectable(now) {
		t.Fatal("disabled 不可选")
	}

	future := 1700000300.0
	past := 1699999700.0
	acc.Status = StatusCooling
	acc.CoolingUntil = &future
	if acc.IsSelectable(now) {
		t.Fatal("冷却未到期不可选")
	}
	acc.CoolingUntil = &past
	if !acc.IsSelectable(now) {
		t.Fatal("冷却到期应可选")
	}
	acc.CoolingUntil = nil
	if acc.IsSelectable(now) {
		t.Fatal("cooling 但无到期时间不可选")
	}
}

func TestModelAvailability(t *testing.T) {
	acc, _ := FromJSON([]byte(pythonAccountJSON))

	if got := acc.ModelAvailability("GLM-5.3"); got != "available" {
		t.Fatalf("GLM-5.3 应 available: %s", got)
	}
	if got := acc.ModelAvailability("glm_5.3_flash"); got != "disabled" {
		t.Fatalf("停用模型应 disabled（停用优先于快照耗尽）: %s", got)
	}
	if got := acc.ModelAvailability("GLM-4.7"); got != "absent" {
		t.Fatalf("快照中不存在的模型应 absent: %s", got)
	}
	// 快照列存在但缺 remaining 数值 → unknown
	acc.Quota["Weird"] = map[string]any{"total": 1}
	if got := acc.ModelAvailability("Weird"); got != "unknown" {
		t.Fatalf("缺数值额度列应 unknown: %s", got)
	}
	// 无任何快照 → unknown
	empty := &Account{Quota: map[string]map[string]any{}}
	if got := empty.ModelAvailability("GLM-5.3"); got != "unknown" {
		t.Fatalf("无快照应 unknown: %s", got)
	}
}

func TestSyncExhaustedModels(t *testing.T) {
	acc := &Account{
		Quota: map[string]map[string]any{
			"A": {"model": "GLM-5.3", "remaining": float64(0)},
			"B": {"model": "GLM-5.3", "remaining": float64(0)}, // 同模型多订阅均为 0 → 耗尽
			"C": {"model": "GLM-5.3-Flash", "remaining": float64(3)},
			"D": {"model": "GLM-5-Turbo"}, // 缺 remaining → 不参与判定
		},
	}
	acc.SyncExhaustedModels()
	want := []string{"glm-5.3"}
	if !reflect.DeepEqual(acc.ExhaustedModels, want) {
		t.Fatalf("耗尽模型不符: got %v want %v", acc.ExhaustedModels, want)
	}

	// 任一订阅恢复余额 → 自动解除耗尽
	acc.Quota["B"]["remaining"] = float64(7)
	acc.SyncExhaustedModels()
	if len(acc.ExhaustedModels) != 0 {
		t.Fatalf("有订阅恢复后应清空耗尽标记: %v", acc.ExhaustedModels)
	}
}

func TestDisabledAndExhaustedMarks(t *testing.T) {
	acc := &Account{}
	acc.SetDisabledModels([]string{"GLM_5.3-Flash", "", "glm-5.3-flash", "GLM-5.3"})
	want := []string{"glm-5.3-flash", "glm-5.3"}
	if !reflect.DeepEqual(acc.DisabledModels, want) {
		t.Fatalf("停用模型应正規化去重: %v", acc.DisabledModels)
	}

	if acc.MarkModelExhausted("") {
		t.Fatal("空模型名不能建立标记")
	}
	if !acc.MarkModelExhausted("GLM-5.3") || !acc.MarkModelExhausted("glm_5.3") {
		t.Fatal("标记失败")
	}
	if len(acc.ExhaustedModels) != 1 {
		t.Fatalf("重复标记不应追加: %v", acc.ExhaustedModels)
	}
}

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		"GLM-5.3": "glm-5.3",
		// 底线变体（Python 版同语义：只替换 _ 与空格，不还原点号）
		"glm_5.3_flash": "glm-5.3-flash",
		"glm_5_3_flash": "glm-5-3-flash",
		"  GLM--5.3  ":  "glm-5.3",
		"GLM 5.3 Flash": "glm-5.3-flash",
		"":              "",
	}
	for in, want := range cases {
		if got := NormalizeModelName(in); got != want {
			t.Fatalf("NormalizeModelName(%v) = %q, want %q", in, got, want)
		}
	}
	if got := NormalizeModelName(nil); got != "" {
		t.Fatalf("NormalizeModelName(nil) = %q, want 空", got)
	}
}

func TestPlanTextAndTrial(t *testing.T) {
	if got := PlanText(map[string]any{"plan_name": "GLM Coding Pro"}); got != "GLM Coding Pro" {
		t.Fatalf("PlanText 单方案不符: %q", got)
	}
	multi := []any{
		map[string]any{"plan_name": "Pro"},
		map[string]any{"plan_name": "Pro"},
		map[string]any{"name": "Trial"},
	}
	if got := PlanText(multi); got != "Pro / Trial" {
		t.Fatalf("PlanText 多方案去重串接不符: %q", got)
	}
	if got := PlanText(map[string]any{}); got != "" {
		t.Fatalf("空方案应为空: %q", got)
	}

	if !IsTrialPlan(map[string]any{"plan_name": "GLM Coding 體驗版"}) {
		t.Fatal("名称含「體驗」应识别为体验方案")
	}
	if !IsTrialPlan(map[string]any{"is_trial": true}) {
		t.Fatal("is_trial=true 应识别为体验方案")
	}
	if IsTrialPlan(map[string]any{"plan_name": "GLM Coding Pro", "is_trial": false}) {
		t.Fatal("付费方案不应误判")
	}
}

func TestAccumulateAndPublicView(t *testing.T) {
	acc := &Account{
		Name:     "acc",
		Mode:     "jwt",
		JWTToken: strPtr("0123456789abcdef-verylongsecret"),
		Status:   StatusCooling,
		Plans:    []map[string]any{{"plan_name": "Pro"}},
		Quota:    map[string]map[string]any{},
	}
	acc.AccumulateTokens(Usage{Input: 10, Output: 20, CacheCreation: 1, CacheRead: 2})
	acc.AccumulateTokens(Usage{Input: 5})
	if acc.TotalInputTokens != 15 || acc.TotalOutputTokens != 20 || acc.TotalCacheCreationTokens != 1 || acc.TotalCacheReadTokens != 2 {
		t.Fatalf("累计不符: %+v", acc)
	}
	acc.ResetTokenStats()
	if acc.TotalInputTokens != 0 {
		t.Fatal("重置失败")
	}

	view := acc.PublicView(time.Now())
	if got := view["token_masked"]; got != "01234567…secret" {
		t.Fatalf("脱敏不符: %v", got)
	}
	if view["plan_name"] != "Pro" {
		t.Fatalf("plan_name 不符: %v", view["plan_name"])
	}
	if view["status"] != StatusCooling {
		t.Fatalf("cooling 状态应原样显示: %v", view["status"])
	}
	tokens := view["total_tokens"].(map[string]int)
	if tokens["input"] != 0 || tokens["output"] != 0 {
		t.Fatalf("重置后视图应为 0: %v", tokens)
	}
}

func strPtr(s string) *string { return &s }

// 账号 ID 截断按字符而非字节。
//
// Python 版 _account_id 用 name[:32]（32 个字符），Go 版原按 byte 截断：11 个
// 汉字 = 33 bytes 会被切断，产生非法 UTF-8。ID 是主键与寻址键，落库后重启
// 再载入时主键改变、同账号插入第二列，跨版本互读也会对同一账号给出不同 ID。
func TestAccountIDTruncatesByRune(t *testing.T) {
	for _, tc := range []struct {
		name string
		desc string
	}{
		{"一二三四五六七八九十壹", "11 个汉字（33 bytes）"},
		{"abcdefghijklmnopqrstuvwxyz0123456789", "纯 ASCII 超长"},
		{"测试账号名", "短 CJK"},
		{"emoji😀测试", "含 emoji（4 bytes）"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			id := newAccountID(tc.name)
			if !utf8.ValidString(id) {
				t.Fatalf("ID 必须是合法 UTF-8: %q", id)
			}
			// 前缀部分（去掉 -xxxxxxxx 后缀）不超过 32 字符
			if i := strings.LastIndex(id, "-"); i > 0 {
				if n := utf8.RuneCountInString(id[:i]); n > 32 {
					t.Fatalf("前缀应不超过 32 字符，实际 %d: %q", n, id)
				}
			}
		})
	}
}
