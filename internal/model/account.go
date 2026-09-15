// Package model 定义账号数据模型与运行状态机。
// 对应 Python 版 app/models.py；Account 的 JSON 字段名与 Python dataclass
// （asdict 输出的 snake_case 键）逐一对应，保证两个版本可互读同一个 accounts.db。
package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const ProviderZai = "zai"

// 账号运行状态（取值与 Python 版 Status 常量一致）。
const (
	StatusActive    = "active"
	StatusExhausted = "exhausted"
	StatusCooling   = "cooling"
	StatusInvalid   = "invalid"
	StatusDisabled  = "disabled"
)

// IsManageable 对应 Python 版 Status.MANAGEABLE：可在后台被启用/禁用的状态。
func IsManageable(status string) bool {
	switch status {
	case StatusActive, StatusCooling, StatusExhausted:
		return true
	}
	return false
}

// Usage 一次成功回應的 token 用量（键与 UsageCollector 约定一致）。
type Usage struct {
	Input         int
	Output        int
	CacheCreation int
	CacheRead     int
}

// Account 与 Python 版 dataclass 字段一一对应（json tag 即 asdict 输出的键）。
// 可空字段使用指针且**不带 omitempty**：Python 的 json.dumps 会输出 null 键，
// 两版序列化形态必须完全一致才能互读同一数据库。
type Account struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Provider string  `json:"provider"`
	Mode     string  `json:"mode"` // "jwt" | "apiKey"
	Email    *string `json:"email"`
	JWTToken *string `json:"jwt_token"`
	APIKey   *string `json:"api_key"`
	Enabled  bool    `json:"enabled"`
	Status   string  `json:"status"`

	// 额度快照：{显示名: {total,used,remaining,available,period,period_start,period_end,expires_at,model,...}}
	Quota           map[string]map[string]any `json:"quota"`
	ExhaustedModels []string                  `json:"exhausted_models"`
	DisabledModels  []string                  `json:"disabled_models"`
	Plan            map[string]any            `json:"plan"`  // 主要方案（第一个订阅）
	Plans           []map[string]any          `json:"plans"` // 账号下全部订阅方案
	Usage           map[string]any            `json:"usage"` // 近期用量原始数据

	UseCount                 int      `json:"use_count"`
	FailCount                int      `json:"fail_count"`
	TotalInputTokens         int      `json:"total_input_tokens"`
	TotalOutputTokens        int      `json:"total_output_tokens"`
	TotalCacheCreationTokens int      `json:"total_cache_creation_tokens"`
	TotalCacheReadTokens     int      `json:"total_cache_read_tokens"`
	LastUsedAt               *float64 `json:"last_used_at"`
	LastCheckedAt            *float64 `json:"last_checked_at"`
	CoolingUntil             *float64 `json:"cooling_until"`
	LastError                *string  `json:"last_error"`
	ProxyURL                 *string  `json:"proxy_url"`
	ProxyID                  *string  `json:"proxy_id"`
	CreatedAt                float64  `json:"created_at"`
	ArchivedAt               *float64 `json:"archived_at"` // 非空表示已归档：只保留记录，不参与调度/领取/刷新
}

// Create 对应 Python 版 Account.create：按凭证形态判定 jwt/apiKey 模式。
func Create(provider, name, secret string) *Account {
	secret = strings.TrimSpace(secret)
	isJWT := strings.Count(secret, ".") == 2 && provider == ProviderZai
	if name == "" {
		name = provider + "-account"
	}
	acc := &Account{
		ID:        newAccountID(name),
		Name:      name,
		Provider:  provider,
		Enabled:   true,
		Status:    StatusActive,
		Quota:     map[string]map[string]any{},
		Plan:      map[string]any{},
		Plans:     []map[string]any{},
		Usage:     map[string]any{},
		CreatedAt: float64(time.Now().UnixNano()) / 1e9,
	}
	if isJWT {
		acc.Mode = "jwt"
		acc.JWTToken = &secret
	} else {
		acc.Mode = "apiKey"
		acc.APIKey = &secret
	}
	return acc
}

// FromJSON 反序列化一段 Python 版 dataclass 导出的 JSON（未知字段忽略），
// 并把缺失的容器字段补齐为空值，保证与 Python 端默认值语义一致。
func FromJSON(data []byte) (*Account, error) {
	acc := &Account{}
	if err := json.Unmarshal(data, acc); err != nil {
		return nil, err
	}
	acc.normalize()
	return acc, nil
}

// normalize 把反序列化后的 nil 容器补齐为空值（对齐 Python dataclass 默认值）。
func (a *Account) normalize() {
	if a.Quota == nil {
		a.Quota = map[string]map[string]any{}
	}
	if a.Plan == nil {
		a.Plan = map[string]any{}
	}
	if a.Plans == nil {
		a.Plans = []map[string]any{}
	}
	if a.Usage == nil {
		a.Usage = map[string]any{}
	}
	if a.ExhaustedModels == nil {
		a.ExhaustedModels = []string{}
	}
	if a.DisabledModels == nil {
		a.DisabledModels = []string{}
	}
}

// Secret 返回当前模式的凭证（jwt 模式取 jwt_token，其余取 api_key）。
func (a *Account) Secret() string {
	if a.Mode == "jwt" {
		return derefString(a.JWTToken)
	}
	return derefString(a.APIKey)
}

// IsSelectable 是否可被轮询选中（对齐 Account.is_selectable）。
// 已归档账号一律不可选（归档即停止调用）。
func (a *Account) IsSelectable(now time.Time) bool {
	if a.ArchivedAt != nil {
		return false
	}
	if !a.Enabled || a.Status == StatusDisabled || a.Status == StatusInvalid {
		return false
	}
	if a.Status == StatusExhausted {
		return false
	}
	if a.Status == StatusCooling {
		return a.CoolingUntil != nil && !now.Before(unixTime(*a.CoolingUntil))
	}
	return true
}

// QuotaEntriesForModel 取请求模型在所有订阅下的额度列；
// 相容旧版（键即模型名）快照（对齐 quota_entries_for_model）。
func (a *Account) QuotaEntriesForModel(model any) []map[string]any {
	target := NormalizeModelName(model)
	if target == "" {
		return nil
	}
	var entries []map[string]any
	for name, quota := range a.Quota {
		entryModel, _ := quota["model"].(string)
		if entryModel == "" {
			entryModel = name
		}
		if NormalizeModelName(entryModel) == target {
			entries = append(entries, quota)
		}
	}
	return entries
}

// ModelAvailability 返回模型可用状态：available / exhausted / disabled / absent / unknown。
// absent = 快照已取得但未提供此模型（调度应跳过）；
// unknown = 尚无快照或额度列缺数值，无法判断（作后备）。
func (a *Account) ModelAvailability(model any) string {
	target := NormalizeModelName(model)
	if target == "" {
		return "unknown"
	}
	for _, m := range a.DisabledModels {
		if NormalizeModelName(m) == target {
			return "disabled"
		}
	}
	for _, m := range a.ExhaustedModels {
		if NormalizeModelName(m) == target {
			return "exhausted"
		}
	}
	entries := a.QuotaEntriesForModel(target)
	var remainings []float64
	for _, quota := range entries {
		if raw, ok := quota["remaining"]; ok && raw != nil {
			if v, err := toFloat(raw); err == nil {
				remainings = append(remainings, v)
			}
		}
	}
	if len(remainings) > 0 {
		for _, v := range remainings {
			if v > 0 {
				return "available"
			}
		}
		return "exhausted"
	}
	if len(entries) > 0 {
		return "unknown"
	}
	if len(a.Quota) > 0 {
		return "absent"
	}
	return "unknown"
}

// IsModelSelectable 账号全局可用且指定模型未耗尽时才允许调度。
func (a *Account) IsModelSelectable(model any, now time.Time) bool {
	if !a.IsSelectable(now) {
		return false
	}
	switch a.ModelAvailability(model) {
	case "available", "unknown":
		return true
	}
	return false
}

// SetDisabledModels 保存手动停用模型：正規化、去除空值与重复项。
func (a *Account) SetDisabledModels(models []string) {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range models {
		n := NormalizeModelName(m)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	a.DisabledModels = out
}

// MarkModelExhausted 记录单一模型已耗尽；无模型名称时无法安全建立标记（返回 false）。
// 返回值表示是否建立了标记（已存在也返回 true，对齐 Python 版）。
func (a *Account) MarkModelExhausted(model any) bool {
	target := NormalizeModelName(model)
	if target == "" {
		return false
	}
	for _, m := range a.ExhaustedModels {
		if NormalizeModelName(m) == target {
			return true
		}
	}
	a.ExhaustedModels = append(a.ExhaustedModels, target)
	return true
}

// SyncExhaustedModels 以最新官方额度快照同步已耗尽模型；
// 任一订阅仍有余额即自动恢复（对齐 sync_exhausted_models）。
func (a *Account) SyncExhaustedModels() {
	remainings := map[string][]float64{}
	for name, quota := range a.Quota {
		raw, ok := quota["remaining"]
		if !ok || raw == nil {
			continue
		}
		v, err := toFloat(raw)
		if err != nil {
			continue
		}
		modelName, _ := quota["model"].(string)
		if modelName == "" {
			modelName = name
		}
		m := NormalizeModelName(modelName)
		remainings[m] = append(remainings[m], v)
	}
	exhausted := []string{}
	for model, values := range remainings {
		all := true
		for _, v := range values {
			if v > 0 {
				all = false
				break
			}
		}
		if all {
			exhausted = append(exhausted, model)
		}
	}
	a.ExhaustedModels = exhausted
}

// AccumulateTokens 累加一次成功回應的 token 用量。
func (a *Account) AccumulateTokens(u Usage) {
	a.TotalInputTokens += u.Input
	a.TotalOutputTokens += u.Output
	a.TotalCacheCreationTokens += u.CacheCreation
	a.TotalCacheReadTokens += u.CacheRead
}

// ResetTokenStats 清零累計 token 用量。
func (a *Account) ResetTokenStats() {
	a.TotalInputTokens = 0
	a.TotalOutputTokens = 0
	a.TotalCacheCreationTokens = 0
	a.TotalCacheReadTokens = 0
}

// EffectiveStatus 考虑冷却到期后的实时状态（对齐 effective_status）。
func (a *Account) EffectiveStatus(now time.Time) string {
	if a.Status == StatusCooling && a.CoolingUntil != nil && !now.Before(unixTime(*a.CoolingUntil)) {
		return StatusActive
	}
	return a.Status
}

// PublicView 返回给前端的脱敏视图（对齐 public_view，键名逐一对齐）。
func (a *Account) PublicView(now time.Time) map[string]any {
	secret := a.Secret()
	masked := secret
	if len(secret) > 16 {
		masked = secret[:8] + "…" + secret[len(secret)-6:]
	}
	planSource := any(a.Plan)
	if len(a.Plans) > 0 {
		planSource = a.Plans
	}
	return map[string]any{
		"id":               a.ID,
		"name":             a.Name,
		"email":            a.Email,
		"provider":         a.Provider,
		"mode":             a.Mode,
		"token_masked":     masked,
		"enabled":          a.Enabled,
		"status":           a.EffectiveStatus(now),
		"quota":            a.Quota,
		"exhausted_models": a.ExhaustedModels,
		"disabled_models":  a.DisabledModels,
		"plan":             a.Plan,
		"plans":            a.Plans,
		"plan_name":        PlanText(planSource),
		"plan_is_trial":    IsTrialPlan(planSource),
		"use_count":        a.UseCount,
		"fail_count":       a.FailCount,
		"total_tokens": map[string]int{
			"input":          a.TotalInputTokens,
			"output":         a.TotalOutputTokens,
			"cache_creation": a.TotalCacheCreationTokens,
			"cache_read":     a.TotalCacheReadTokens,
		},
		"last_used_at":    a.LastUsedAt,
		"last_checked_at": a.LastCheckedAt,
		"cooling_until":   a.CoolingUntil,
		"last_error":      a.LastError,
		"proxy_url":       a.ProxyURL,
		"proxy_id":        a.ProxyID,
		"created_at":      a.CreatedAt,
		"archived_at":     a.ArchivedAt,
	}
}

// ── 内部工具 ────────────────────────────────────────────────────────────────

// NormalizeModelName 統一模型名稱格式，供額度快照與請求模型穩定比對。
func NormalizeModelName(model any) string {
	var s string
	switch v := model.(type) {
	case nil:
		return ""
	case string:
		s = v
	default:
		s = fmt.Sprint(v)
	}
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return s
}

// PlanText 從官方方案資料取出可供辨識的名稱，多方案時去重後以「 / 」串接。
func PlanText(plan any) string {
	switch p := plan.(type) {
	case []map[string]any:
		// Account.Plans 的靜態型別：Python 版 plan_text/is_trial_plan 對任意 list 生效，
		// Go 需顯式分支處理此切片型別
		var names []string
		seen := map[string]bool{}
		for _, item := range p {
			if n := PlanText(item); n != "" && !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
		return strings.Join(names, " / ")
	case []any:
		var names []string
		seen := map[string]bool{}
		for _, item := range p {
			if n := PlanText(item); n != "" && !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
		return strings.Join(names, " / ")
	case map[string]any:
		for _, key := range []string{"plan_name", "display_name", "show_name", "name", "title", "plan_type", "plan_id"} {
			if truthy(p[key]) {
				if s := strings.TrimSpace(strOf(p[key])); s != "" {
					return s
				}
			}
		}
		return ""
	default:
		return ""
	}
}

// IsTrialPlan 辨識官方回傳的體驗、試用或免費方案，缺少標記時以方案名稱補判。
func IsTrialPlan(plan any) bool {
	return isTrialPlan(plan, 0)
}

func isTrialPlan(plan any, depth int) bool {
	if depth > 6 {
		return false
	}
	switch p := plan.(type) {
	case []map[string]any:
		// 同 PlanText：Account.Plans 的靜態型別需顯式分支
		for _, item := range p {
			if isTrialPlan(item, depth+1) {
				return true
			}
		}
		return false
	case []any:
		for _, item := range p {
			if isTrialPlan(item, depth+1) {
				return true
			}
		}
		return false
	case map[string]any:
		markers := []string{"trial", "free", "experience", "體驗", "试用", "試用"}
		for key, value := range p {
			switch normalizeKey(key) {
			case "istrial", "trial", "isfree", "free", "isexperience", "trialplan":
				if b, ok := value.(bool); ok && b {
					return true
				}
			}
			if isTrialPlan(value, depth+1) {
				return true
			}
			s := strings.ToLower(fmt.Sprint(value))
			for _, marker := range markers {
				if strings.Contains(s, marker) {
					return true
				}
			}
		}
		return false
	default:
		return false
	}
}

// normalizeKey 对应 Python：str(key).replace("_","").replace("-","").lower()
func normalizeKey(key string) string {
	s := strings.ReplaceAll(key, "_", "")
	s = strings.ReplaceAll(s, "-", "")
	return strings.ToLower(s)
}

// truthy 对应 Python 的真值判断（用于方案字段提取）。
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case float64:
		return x != 0
	case float32:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// strOf 值转显示字符串（方案名一般为字符串；数字按 Python str() 语义）。
func strOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// toFloat 宽松数值转换（对应 Python float() 语义：数字或数字字符串）。
func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case json.Number:
		return x.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(x), 64)
	}
	return 0, fmt.Errorf("非数值: %T", v)
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func unixTime(epoch float64) time.Time {
	sec := int64(epoch)
	nsec := int64((epoch - float64(sec)) * 1e9)
	return time.Unix(sec, nsec)
}

// newAccountID 对应 Python 版 _account_id：<safe-name>-<8 hex>；
// 字母数字（含 Unicode 字母，如中文）保留，其余替换为 -，截断 32 字符。
func newAccountID(name string) string {
	if name == "" {
		name = "account"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	safe := strings.Trim(b.String(), "-")
	if len(safe) > 32 {
		safe = strings.Trim(safe[:32], "-")
	}
	if safe == "" {
		safe = "account"
	}
	return safe + "-" + randomHex(4)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
