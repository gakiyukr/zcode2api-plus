// 请求整形与上游错误分类。对应 Python 版 routes/gateway.py 的
// 常量、_normalize_body、_is_captcha_error、_upstream_business_code、
// _is_model_concurrency_limit 与模型白名单部分。
package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	// MaxCaptchaRetries 单账号内验证码失效重试上限。
	MaxCaptchaRetries = 3
	// MaxAccountAttempts 单次请求最多尝试的账号数。
	MaxAccountAttempts = 5
)

// startPlanBusyRetryDelays 原版客户端对 Start Plan 的 3010 等待 1s、2s 重试；
// 账号仍然可用，不能把模型准入限制写成账号 cooling。
var startPlanBusyRetryDelays = []time.Duration{time.Second, 2 * time.Second}

// LiveFlagshipModel 上游目前線上主力旗艦模型名（2026-11-24 起由 GLM-5.3 改名）。
const LiveFlagshipModel = "GLM-5.4"

// ModelNameMap Z.AI 上游模型名大小寫敏感；映射已知寫法（冪等，NormalizeBody
// 套用），未命中時原樣放行——模型准入交由上游裁決，本地不設白名單。
var ModelNameMap = map[string]string{
	"glm-5.4": LiveFlagshipModel,
	"glm-5.3": "GLM-5.3",
}

// retiredFlagshipAliases 退役旗艦模型別名（歸一化為小寫連字號後比對）。
// 當帳號池中無可用原模型、但存在可用的 LiveFlagshipModel（GLM-5.4）時，
// 引擎可無縫改寫至最新旗艦模型，避免舊客戶端因上游改名而被判 absent。
var retiredFlagshipAliases = map[string]bool{
	"glm-5.3":     true,
	"glm-5.2":     true,
	"glm-5-turbo": true,
	"glm-turbo":   true,
	"glm-5.1":     true,
	"glm-4.7":     true,
}

// FlagshipAlias 若模型屬於退役旗艦別名，回傳當前主力旗艦名（GLM-5.4）；否則回傳空串。
func FlagshipAlias(modelName string) string {
	key := strings.ToLower(strings.TrimSpace(modelName))
	key = strings.ReplaceAll(key, "_", "-")
	key = strings.ReplaceAll(key, " ", "-")
	if retiredFlagshipAliases[key] {
		return LiveFlagshipModel
	}
	return ""
}

// AvailableModels /v1/models 端點在無額度快照時的預設展示清單（僅供參考，不參與請求校驗）。
var AvailableModels = []string{"glm-5.3-flash", LiveFlagshipModel}

// quotaExhaustedCodes Z.AI 官方 429 业务码中的额度/用量上限族（docs.z.ai 错误码表）：
// 1113 欠费；1308 用量上限；1309 套餐过期；1310 周/月上限；1311 套餐不含此模型；
// 1313 公平使用；1316-1321 5小时/7天上限及子账户/企业消费上限。
// 与瞬时限流（1302/1305）区分：上限族按「模型耗尽」换号，限流按冷却换号。
var quotaExhaustedCodes = map[string]bool{
	"1113": true, "1308": true, "1309": true, "1310": true, "1311": true, "1313": true,
	"1316": true, "1317": true, "1318": true, "1319": true, "1320": true, "1321": true,
}

// IsQuotaExhaustedCode 判断上游业务码是否属于额度/用量上限族。
// 429 响应须用本函数区分「该模型耗尽」（换号）与「瞬时限流」（冷却换号）；
// asyncpool 与 engine 共用同一判定，避免两条路径对同一账号标出不同状态。
func IsQuotaExhaustedCode(text string) bool {
	return quotaExhaustedCodes[UpstreamBusinessCode(text)]
}

// captchaHeaders 上游用于携带验证码挑战的响应头。
var captchaHeaders = []string{
	"x-aliyun-captcha-verify-param",
	"x-aliyun-captcha-verify-region",
	"x-aliyun-captcha-challenge",
}

// IsCaptchaError 识别上游验证码挑战：响应头、body code=3007、F001 与旧文本
// （仅对 400/403 生效，避免把 5xx 的偶发字样误判；对齐 _is_captcha_error）。
func IsCaptchaError(text string, statusCode int, header http.Header) bool {
	for _, name := range captchaHeaders {
		if header.Get(name) != "" {
			return true
		}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		candidates := []any{payload["code"]}
		if errObj, ok := payload["error"].(map[string]any); ok {
			candidates = append(candidates, errObj["code"])
		}
		for _, c := range candidates {
			if fmt.Sprint(c) == "3007" {
				return true
			}
		}
	}
	low := strings.ToLower(text)
	clientError := statusCode == 0 || statusCode == 400 || statusCode == 403
	if strings.Contains(low, "f001") {
		return clientError
	}
	if strings.Contains(low, "captcha") || strings.Contains(low, "verify token") || strings.Contains(low, "verify failed") {
		return clientError
	}
	return false
}

// UpstreamBusinessCode 提取上游错误体中的业务码。
// 兼容两种形态：zcode-plan 包装格式 {"code":..,"msg":..} 与
// api.z.ai 的 Anthropic 风格 {"error":{"message":..,"type":"1000"}}。
// 无业务码时返回空串。
func UpstreamBusinessCode(text string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return ""
	}
	code := payload["code"]
	if code == nil {
		if errObj, ok := payload["error"].(map[string]any); ok {
			code = errObj["type"]
		}
	}
	if code == nil {
		return ""
	}
	return fmt.Sprint(code)
}

// concurrencyLimitCodes ZCode 官方客戶端（3.17.2+）視為併發准入上限的三個業務碼：
// 3008（使用者併發上限）、3009（模型併發上限）、3010（模型准入併發上限）。
// 皆屬暫時性併發忙碌，不代表帳號被限流或憑證失效。
var concurrencyLimitCodes = map[string]bool{
	"3008": true,
	"3009": true,
	"3010": true,
}

// IsModelConcurrencyLimit 判斷是否為併發准入限制（3008/3009/3010），
// 此類錯誤不代表帳號被限流或失效，應走重試／排隊收窄而非標記 cooling。
func IsModelConcurrencyLimit(statusCode int, text string) bool {
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	if code := UpstreamBusinessCode(text); concurrencyLimitCodes[code] {
		return true
	}
	low := strings.ToLower(text)
	return strings.Contains(low, "model admission concurrency limit") ||
		strings.Contains(low, "user concurrency limit") ||
		strings.Contains(low, "model concurrency limit")
}

// edgePageRe 上游边缘防火墙（阿里云 ESA/CDN）拦截页的特征：错误重定向域
// 或整页 HTML。这是本机出口地址被封，不是账号问题。
var edgePageRe = regexp.MustCompile(`(?i)errors\.aliyun\.com|<html`)

// IsZcodeEdgeBlocked 判断 405 是否为边缘防火墙的拦截页（本机出口被封）。
// 拦截页是网络级的：所有账号共用同一出口时全部会撞上，继续请求只会加重。
func IsZcodeEdgeBlocked(statusCode int, text string) bool {
	return statusCode == http.StatusMethodNotAllowed && edgePageRe.MatchString(text)
}

// IsZcodeRestricted 判断 405 是否为上游风控的账号级拒绝（code 3012）。
// 官方客户端在同账号上同样被拒（风控封的是账号）；与拦截页区分：
// 同一句 "request has been blocked" 文案两种形态都会出现。
// 405 本身即拒绝信号（magpie startRefused：405 状态码足矣，文本仅辅助识别）。
func IsZcodeRestricted(statusCode int, text string) bool {
	return statusCode == http.StatusMethodNotAllowed &&
		!edgePageRe.MatchString(text)
}

// Bare429ProbeReason 裸 429 雙號探測期寫入首號的冷卻原因標記；平反時嚴格比對此標記，
// 避免誤清期間其他併發請求寫入的 3012 風控長冷卻或 503 冷卻。
const Bare429ProbeReason = "上游限流 HTTP 429（請求級探測）"

// Bare429FallbackMessage 上游回傳空正文裸 429 時對外回傳的預設錯誤訊息。
const Bare429FallbackMessage = "上游觸發請求級限流（HTTP 429）"

// allowedRefusedEnvelopeKeys 靜默拒絕空信封僅允許出現的元資料鍵白名單；
// 只要回應包含任何非白名單業務欄位（如 answer / content / choices），立即判為非空信封。
var allowedRefusedEnvelopeKeys = map[string]bool{
	"code":       true,
	"msg":        true,
	"message":    true,
	"logid":      true,
	"log_id":     true,
	"request_id": true,
	"trace_id":   true,
	"success":    true,
	"data":       true,
	"timestamp":  true,
	"ts":         true,
}

// isEmptyEnvelopeData 判斷信封中的 data 欄位是否為實質空值（nil / 空字串 / 空物件 / 空陣列）。
func isEmptyEnvelopeData(v any) bool {
	switch d := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(d) == ""
	case map[string]any:
		return len(d) == 0
	case []any:
		return len(d) == 0
	default:
		return false
	}
}

// IsRefusedEnvelope 辨識上游對過舊客戶端版本回傳的靜默拒絕空信封
// （僅允許信封元資料鍵白名單、data 為空、code 為 0、msg/message 為空，且至少具備核心信封鍵）。
func IsRefusedEnvelope(buffered []byte) bool {
	trimmed := bytes.TrimSpace(buffered)
	if len(trimmed) == 0 || len(trimmed) > 1024 {
		return false
	}
	var payload map[string]any
	if err := json.Unmarshal(trimmed, &payload); err != nil || len(payload) == 0 {
		return false
	}
	for k := range payload {
		if !allowedRefusedEnvelopeKeys[k] {
			return false
		}
	}
	dataVal, hasData := payload["data"]
	if hasData && !isEmptyEnvelopeData(dataVal) {
		return false
	}
	codeVal, hasCode := payload["code"]
	if hasCode && fmt.Sprint(codeVal) != "0" {
		return false
	}
	for _, msgKey := range []string{"msg", "message"} {
		if msgVal, exists := payload[msgKey]; exists && msgVal != nil {
			if s, ok := msgVal.(string); !ok || strings.TrimSpace(s) != "" {
				return false
			}
		}
	}
	_, hasLogID := payload["logid"]
	_, hasLogIDSnake := payload["log_id"]
	_, hasSuccess := payload["success"]
	return hasCode || hasLogID || hasLogIDSnake || hasData || hasSuccess
}

// IsBare429 辨識無 Retry-After 且無任何 Z.AI 業務碼的裸 HTTP 429（疑似請求級封鎖／風控拒絕）。
func IsBare429(statusCode int, text string, header http.Header) bool {
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	if header != nil && strings.TrimSpace(header.Get("Retry-After")) != "" {
		return false
	}
	if IsModelConcurrencyLimit(statusCode, text) {
		return false
	}
	return UpstreamBusinessCode(text) == ""
}
