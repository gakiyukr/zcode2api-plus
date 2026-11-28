// 套餐领取（Z.AI billing/preview + billing/claim），对应 Python 版 app/claim.py。
// 链路：GET /billing/preview → data.plans[]；POST /billing/claim（body
// {"plan_id"}）需验证码头 X-Aliyun-Captcha-Verify-Param（+可选 Region 头）。
// 业务码沿用 zcode-switch 映射；3007 换码重试一次。请求经 clientFor 走账号代理。
package claim

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/fingerprint"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/util"
	"zcode2api/internal/web"
)

// ClaimError 業務失敗（含上游 code 語義），消息面向使用者。
type ClaimError struct {
	Code int
	msg  string
	// RetryAfter 上游 Retry-After 指定的等待时间；0 表示未提供或无法解析。
	RetryAfter time.Duration
}

func (e *ClaimError) Error() string { return e.msg }

// MarketingTouchURL ZCode 官方活動橫幅投放端點（測試可覆寫）。
var MarketingTouchURL = "https://zcode.z.ai/api/v1/marketing/touch"

// defaultGetRetryDelays 冪等 GET 請求遇瞬態傳輸錯誤的階梯重試間隔（共 5 次嘗試）。
var defaultGetRetryDelays = []time.Duration{
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	15 * time.Second,
}

// touchSeq 進程級遞增請求計數（官方 marketingTouchService.ts 的 bootRequestSeq）。
var touchSeq atomic.Int64

var claimFail = map[int]string{
	1001: "套餐不存在",
	1002: "活動已結束或套餐暫不可領取",
	1003: "該套餐已經領取過",
	1004: "不符合領取條件",
	1005: "今日領取名額已用完",
	3001: "領取參數錯誤，請重新整理後重試",
	3007: "驗證碼校驗失敗，請重試",
	401:  "請先登入後再領取",
}

const (
	captchaHeader       = "X-Aliyun-Captcha-Verify-Param"
	captchaRegionHeader = "X-Aliyun-Captcha-Verify-Region"
)

// Service 領取服務：captcha 求解 + billing/marketing HTTP（帳號代理生效）。
type Service struct {
	Captcha *captcha.Manager
	// Client 測試注入；nil 時按帳號代理構造（25s 超時，短請求語義）。
	Client HTTPClient
	// GetRetryDelays 覆寫 GET 瞬態重試延遲（nil 使用預設 2/4/8/15s；測試可設空或短延遲）。
	GetRetryDelays []time.Duration
}

// HTTPClient 计费端点客户端抽象。
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewService 创建领取服务。
func NewService(cm *captcha.Manager) *Service {
	return &Service{Captcha: cm}
}

// clientFor 账号出站客户端：有代理走代理（25s 超时），否则默认直连。
func (s *Service) clientFor(acc *model.Account) HTTPClient {
	if s.Client != nil {
		return s.Client
	}
	if acc != nil && acc.ProxyURL != nil && *acc.ProxyURL != "" {
		if c, err := proxy.ClientFor(*acc.ProxyURL, 25*time.Second); err == nil {
			return c
		}
		web.Warn("claim", fmt.Sprintf("账号 %s 代理无效，回退直连", acc.Name))
	}
	return defaultClient
}

// defaultClient 直连用的共享客户端（同 quota 包的理由：复用连接池，
// 避免每次领取都重做 TCP/TLS 握手并累积 TIME_WAIT）。
var defaultClient = &http.Client{Timeout: 25 * time.Second}

// failMessage 业务码 → 使用者文案（带上游 msg 补充）。
func failMessage(code int, body map[string]any) string {
	base, ok := claimFail[code]
	if !ok {
		base = "領取失敗"
	}
	server := ""
	for _, key := range []string{"msg", "message"} {
		if s, isStr := body[key].(string); isStr && s != "" {
			server = s
			break
		}
	}
	if server != "" {
		return fmt.Sprintf("%s（%s）", base, server)
	}
	return base
}

// ParsePlan 提取可领取套餐（plan_id/name/描述/优先级 + model_usage token 授权项）。
func ParsePlan(raw map[string]any) map[string]any {
	planID := orFirst(raw, "plan_id", "planId")
	if planID == "" {
		return nil
	}
	var grants []any
	if entitlements, ok := raw["entitlements"].([]any); ok {
		for _, item := range entitlements {
			ent, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if meter, _ := ent["meter"].(string); meter != "model_usage" {
				continue
			}
			if unit, _ := ent["unit_type"].(string); unit != "token" {
				continue
			}
			name := orFirst(ent, "show_name", "showName")
			if name == "" {
				continue
			}
			grants = append(grants, map[string]any{
				"name":   name,
				"units":  asFloat(firstNonNil(ent["grant_units"], ent["grantUnits"])),
				"period": util.OrDefault(orFirst(ent, "period"), "one_time"),
			})
		}
	}
	return map[string]any{
		"plan_id":     planID,
		"name":        orFirst(raw, "name"),
		"description": orFirst(raw, "description"),
		"priority":    asFloat(firstNonNil(raw["priority"])),
		"grants":      grants,
	}
}

// JWTUserID JWT payload 的 user_id（官方客户端事件上报以 user_id 标识用户）。
// user_id 优先，sub 兜底；解析失败回空串。
func JWTUserID(jwtToken string) string {
	parts := splitJWT(jwtToken)
	if parts == "" {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal([]byte(parts), &claims); err != nil {
		return ""
	}
	for _, key := range []string{"user_id", "sub"} {
		if s, ok := claims[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// splitJWT 解 base64url payload（第一、二个点之间）；无效返回空串。
func splitJWT(token string) string {
	first := strings.Index(token, ".")
	if first < 0 {
		return ""
	}
	second := strings.Index(token[first+1:], ".")
	if second < 0 {
		return ""
	}
	seg := token[first+1 : first+1+second]
	if seg == "" {
		return ""
	}
	if padded, err := base64.URLEncoding.DecodeString(seg); err == nil {
		return string(padded)
	}
	if raw, err := base64.RawURLEncoding.DecodeString(seg); err == nil {
		return string(raw)
	}
	return ""
}

// billingRequest 統一計費／活動請求：網路錯誤／鑑權失敗統一轉 ClaimError。
// 冪等 GET/HEAD 遇傳輸層瞬態錯誤依階梯重試（預設 5 次 2/4/8/15s）；
// POST 寫操作嚴格保持單發，避免重發撞 1003 或浪費驗證碼。
func (s *Service) billingRequest(acc *model.Account, method, path string, headers map[string]string, payload []byte) (map[string]any, error) {
	return s.billingRequestCtx(context.Background(), acc, method, path, headers, payload)
}

// billingRequestCtx 帶可取消 context 的計費／活動請求；重試退避期間支援立即中斷。
func (s *Service) billingRequestCtx(ctx context.Context, acc *model.Account, method, path string, headers map[string]string, payload []byte) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	targetURL := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		targetURL = config.ZcodeBillingBase + path
	}
	var delays []time.Duration
	if strings.EqualFold(method, http.MethodGet) || strings.EqualFold(method, http.MethodHead) {
		if s.GetRetryDelays != nil {
			delays = s.GetRetryDelays
		} else {
			delays = defaultGetRetryDelays
		}
	}
	client := s.clientFor(acc)
	var lastNetErr error
	for attempt := 0; attempt <= len(delays); attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(delays[attempt-1])
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var bodyReader io.Reader
		if len(payload) > 0 {
			bodyReader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
		if err != nil {
			return nil, &ClaimError{msg: fmt.Sprintf("上游網路錯誤: %v", err)}
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastNetErr = err
			continue // 僅網路傳輸層錯誤重試（對 POST 因 len(delays)==0 立即退出）
		}
		raw, readErr := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if readErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastNetErr = readErr
			continue
		}
		if res.StatusCode == 401 || res.StatusCode == 403 {
			text := lower(raw)
			if !containsAny(text, "captcha", "verify") {
				return nil, &ClaimError{Code: res.StatusCode, msg: fmt.Sprintf("鑑權失敗 HTTP %d", res.StatusCode)}
			}
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, &ClaimError{msg: fmt.Sprintf("上游回應非 JSON HTTP %d", res.StatusCode)}
		}
		// HTTP >= 400 且未帶非零業務碼時統一視為請求失敗，避免誤判為 code=0 成功
		if res.StatusCode >= 400 && (body["code"] == nil || BusinessCode(body) == 0) {
			return nil, &ClaimError{
				Code:       res.StatusCode,
				msg:        fmt.Sprintf("計費 API 回傳 HTTP %d", res.StatusCode),
				RetryAfter: parseRetryAfter(res.Header.Get("Retry-After")),
			}
		}
		return body, nil
	}
	return nil, &ClaimError{msg: fmt.Sprintf("上游網路錯誤: %v", lastNetErr)}
}

// parseRetryAfter 解析 Retry-After（秒數或 HTTP date）；無效值回 0。
func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}

// authHeaders 計費／活動端點鑑權頭（使用帳號專屬 device_mid 與計費鏈版本號）。
func authHeaders(acc *model.Account) map[string]string {
	deviceMid := acc.DeviceMid()
	if deviceMid == "" {
		deviceMid = config.DeviceMid()
	}
	sku := fingerprint.For(acc)
	headers := map[string]string{
		"Content-Type":        "application/json",
		"User-Agent":          config.UserAgent,
		"X-ZCode-App-Version": config.ZcodeBillingVersion,
		"X-Platform":          sku.Platform,
		"X-Os-Category":       sku.OsCategory,
		"X-Os-Version":        sku.OsVersion,
		"X-Client-Language":   sku.Language,
		"X-Client-Timezone":   sku.Timezone,
		"X-Device-Mid":        deviceMid,
		"HTTP-Referer":        "https://zcode.z.ai/",
	}
	if acc.Mode == "jwt" && acc.JWTToken != nil {
		headers["Authorization"] = "Bearer " + *acc.JWTToken
	} else if acc.APIKey != nil {
		headers["x-api-key"] = *acc.APIKey
	}
	return headers
}

// ReportActivationEvents 上報激活事件（app_launch + app_daily_active），
// 跟隨帳號出站代理並攜帶帳號專屬 device_mid；返回錯誤文案或空串。
func ReportActivationEvents(acc *model.Account) string {
	return ReportActivationEventsContext(context.Background(), acc)
}

// ReportActivationEventsContext 帶可取消 context 的激活事件上報。
func ReportActivationEventsContext(ctx context.Context, acc *model.Account) string {
	userID := JWTUserID(acc.Secret())
	if userID == "" {
		return "JWT 無 user_id，跳過激活上報"
	}
	deviceMid := acc.DeviceMid()
	if deviceMid == "" {
		deviceMid = config.DeviceMid()
	}
	for _, element := range ActivationElements {
		if err := PostActivationEventContext(ctx, acc, userID, element, deviceMid); err != nil {
			return fmt.Sprintf("激活事件 %s 上報失敗: %v", element, err)
		}
	}
	return ""
}

// ParseTouchPlans 解析 /api/v1/marketing/touch 的 data 物件，遞歸掃描
// deliveries[] 中的 claim_zcode_plan 動作與 zcode_plan 名稱（對齊 magpie touchPlans）。
func ParseTouchPlans(data any) []map[string]any {
	obj, ok := data.(map[string]any)
	if !ok || obj == nil {
		return nil
	}
	deliveries, ok := obj["deliveries"].([]any)
	if !ok || len(deliveries) == 0 {
		return []map[string]any{}
	}
	var out []map[string]any
	seen := map[string]bool{}
	for _, dv := range deliveries {
		var ids []string
		var names []string
		var walk func(v any, depth int)
		walk = func(v any, depth int) {
			if v == nil || depth > 12 {
				return
			}
			switch cur := v.(type) {
			case []any:
				for _, item := range cur {
					walk(item, depth+1)
				}
			case map[string]any:
				if t, _ := cur["type"].(string); t == "claim_zcode_plan" {
					if args, ok := cur["args"].(map[string]any); ok {
						if pid := strings.TrimSpace(orFirst(args, "plan_id", "planId")); pid != "" {
							ids = append(ids, pid)
						}
					}
				}
				if zp, ok := cur["zcode_plan"].(map[string]any); ok {
					if name := strings.TrimSpace(orFirst(zp, "name", "display_name")); name != "" {
						names = append(names, name)
					}
				}
				keys := make([]string, 0, len(cur))
				for k := range cur {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					walk(cur[k], depth+1)
				}
			}
		}
		walk(dv, 0)
		planName := ""
		if len(names) > 0 {
			planName = names[0]
		}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			name := planName
			if name == "" {
				name = id
			}
			out = append(out, map[string]any{
				"plan_id":     id,
				"name":        name,
				"description": "",
				"priority":    float64(0),
				"grants":      []any{},
				"source":      "marketing_touch",
			})
		}
	}
	return out
}

// marketingLanguage 歸一為 ZCode 活動端點接受的兩種 UI 語言（zh-CN / en-US）。
func marketingLanguage(acc *model.Account) string {
	lang := strings.ToLower(fingerprint.For(acc).Language)
	if strings.HasPrefix(lang, "zh") {
		return "zh-CN"
	}
	return "en-US"
}

// TouchPlans 拉取 /api/v1/marketing/touch 活動橫幅可領取套餐（v3.17+ 週末／限時活動主入口）。
func (s *Service) TouchPlans(acc *model.Account) ([]map[string]any, error) {
	return s.TouchPlansContext(context.Background(), acc)
}

// TouchPlansContext 帶可取消 context 拉取 /api/v1/marketing/touch 活動套餐。
func (s *Service) TouchPlansContext(ctx context.Context, acc *model.Account) ([]map[string]any, error) {
	headers := authHeaders(acc)
	headers["X-Client-Language"] = marketingLanguage(acc)
	seq := touchSeq.Add(1) - 1
	url := fmt.Sprintf("%s?seq=%d", MarketingTouchURL, seq)
	body, err := s.billingRequestCtx(ctx, acc, http.MethodGet, url, headers, nil)
	if err != nil {
		return nil, err
	}
	if code := BusinessCode(body); code != 0 {
		return nil, &ClaimError{Code: code, msg: failMessage(code, body)}
	}
	plans := ParseTouchPlans(body["data"])
	if plans == nil {
		return []map[string]any{}, nil
	}
	return plans, nil
}

// PreviewPlans 拉取帳號當前可領取套餐（合併 billing/preview 與 marketing/touch 兩路，
// 依 plan_id 去重後按優先級降序排序）。單路失敗時容錯保留另一路結果。
func (s *Service) PreviewPlans(acc *model.Account) ([]map[string]any, error) {
	return s.PreviewPlansContext(context.Background(), acc)
}

// PreviewPlansContext 帶可取消 context 拉取帳號當前可領取套餐。
func (s *Service) PreviewPlansContext(ctx context.Context, acc *model.Account) ([]map[string]any, error) {
	headers := authHeaders(acc)
	body, previewErr := s.billingRequestCtx(ctx, acc, http.MethodGet,
		"/billing/preview?app_version="+config.ZcodeBillingVersion+"&platform="+fingerprint.For(acc).Platform,
		headers, nil)
	var previewPlans []map[string]any
	if previewErr == nil {
		if code := BusinessCode(body); code != 0 {
			previewErr = &ClaimError{Code: code, msg: failMessage(code, body)}
		} else {
			var rawPlans []any
			if data, ok := body["data"].(map[string]any); ok {
				rawPlans, _ = data["plans"].([]any)
			}
			for _, raw := range rawPlans {
				if obj, ok := raw.(map[string]any); ok {
					if parsed := ParsePlan(obj); parsed != nil {
						previewPlans = append(previewPlans, parsed)
					}
				}
			}
		}
	}

	touchPlans, touchErr := s.TouchPlansContext(ctx, acc)
	if previewErr != nil && touchErr != nil {
		return nil, previewErr
	}
	if previewErr != nil {
		web.Warn("claim", fmt.Sprintf("帳號 %s billing/preview 失敗（已改用 marketing/touch）: %v", acc.Name, previewErr))
	}
	if touchErr != nil {
		web.Warn("claim", fmt.Sprintf("帳號 %s marketing/touch 失敗（已改用 billing/preview）: %v", acc.Name, touchErr))
	}

	seen := map[string]bool{}
	plans := make([]map[string]any, 0, len(previewPlans)+len(touchPlans))
	for _, p := range previewPlans {
		pid, _ := p["plan_id"].(string)
		if pid == "" || seen[pid] {
			continue
		}
		seen[pid] = true
		plans = append(plans, p)
	}
	for _, p := range touchPlans {
		pid, _ := p["plan_id"].(string)
		if pid == "" || seen[pid] {
			continue
		}
		seen[pid] = true
		plans = append(plans, p)
	}
	sort.Slice(plans, func(i, j int) bool {
		pi, pj := asFloat(plans[i]["priority"]), asFloat(plans[j]["priority"])
		if pi != pj {
			return pi > pj
		}
		return plans[i]["plan_id"].(string) < plans[j]["plan_id"].(string)
	})
	return plans, nil
}

// Claim 领取套餐。planID 为空时自动选优先级最高的可领套餐。
// 返回 {plan_id, plan_name, grants}；3007（验证码失败）自动换码重试一次。
func (s *Service) Claim(acc *model.Account, planID string) (map[string]any, error) {
	return s.ClaimContext(context.Background(), acc, planID)
}

// ClaimContext 帶可取消 context 領取套餐。
func (s *Service) ClaimContext(ctx context.Context, acc *model.Account, planID string) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if acc.Mode != "jwt" || acc.JWTToken == nil || *acc.JWTToken == "" {
		return nil, &ClaimError{msg: "僅 Coding Plan (JWT) 賬號支持領取"}
	}

	planName := ""
	grants := []any{}
	if planID == "" {
		plans, err := s.PreviewPlansContext(ctx, acc)
		if err != nil {
			return nil, err
		}
		if len(plans) == 0 {
			return nil, &ClaimError{msg: "沒有待領取的套餐"}
		}
		best := plans[0]
		planID, _ = best["plan_id"].(string)
		planName, _ = best["name"].(string)
		if planName == "" {
			planName = planID
		}
		if g, ok := best["grants"].([]any); ok {
			grants = g
		}
	}

	addrKey := "direct"
	if acc.ProxyURL != nil && strings.TrimSpace(*acc.ProxyURL) != "" {
		addrKey = strings.TrimSpace(*acc.ProxyURL)
	}

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		token, err := s.Captcha.GetVerifyParamForClaim(ctx, addrKey)
		if err != nil {
			// 本地驗證碼求解失敗不得冒充上游 3007（否則背景補領會誤判為當日終態）
			return nil, &ClaimError{Code: 0, msg: "驗證碼求解失敗，請到後台驗證碼頁面回填參數"}
		}
		var verifyParam, verifyRegion string
		if token != nil {
			verifyParam, verifyRegion = token.VerifyParam, token.Region
		}
		headers := claimHeaders(acc, verifyParam, verifyRegion)
		payload, _ := json.Marshal(map[string]string{"plan_id": planID})
		body, err := s.billingRequestCtx(ctx, acc, http.MethodPost, "/billing/claim", headers, payload)
		if err != nil {
			return nil, err
		}
		code := BusinessCode(body)
		if code == 0 {
			if planName == "" {
				if dataObj, ok := body["data"].(map[string]any); ok {
					planName = strings.TrimSpace(orFirst(dataObj, "plan_name", "planName", "name"))
				}
			}
			return map[string]any{"plan_id": planID, "plan_name": planName, "grants": grants}, nil
		}
		if code == 3007 && attempt == 1 {
			web.Warn("claim", fmt.Sprintf("账号 %s 验证码被拒，换码重试", acc.Name))
			s.Captcha.InvalidateForAddr(addrKey)
			lastErr = &ClaimError{Code: code, msg: failMessage(code, body)}
			continue
		}
		return nil, &ClaimError{Code: code, msg: failMessage(code, body)}
	}
	if lastErr == nil {
		lastErr = errors.New("領取失敗")
	}
	return nil, lastErr
}

// claimHeaders billing/claim 客戶端請求頭形態。
func claimHeaders(acc *model.Account, verifyParam, region string) map[string]string {
	headers := authHeaders(acc)
	if verifyParam != "" {
		headers[captchaHeader] = verifyParam
	}
	if region != "" {
		headers[captchaRegionHeader] = region
	}
	headers["X-ZCode-App-Version"] = config.ZcodeBillingVersion
	headers["X-Platform"] = fingerprint.For(acc).Platform
	return headers
}

// AutoClaimAllPlans 新账号入池自动领取：激活上报 + 逐个领取全部可领套餐。
// 入池链路的 fire-and-forget 收尾：任何失败只记日志/返回 outcome，绝不 panic。
// 重复执行安全（上游 1003 已领取过幂等）。
func (s *Service) AutoClaimAllPlans(acc *model.Account) []map[string]any {
	if acc.Mode != "jwt" || acc.JWTToken == nil || *acc.JWTToken == "" {
		return nil
	}
	outcomes := []map[string]any{}

	if err := ReportActivationEvents(acc); err != "" {
		web.Warn("claim", fmt.Sprintf("账号 %s 激活上报失败: %s", acc.Name, err))
	}

	plans, err := s.PreviewPlans(acc)
	if err != nil {
		var ce *ClaimError
		if errors.As(err, &ce) {
			web.Ok("claim", fmt.Sprintf("账号 %s 无可领套餐（%s）", acc.Name, err))
		} else {
			web.Warn("claim", fmt.Sprintf("账号 %s preview 异常: %v", acc.Name, err))
		}
		return outcomes
	}
	if len(plans) == 0 {
		web.Ok("claim", fmt.Sprintf("账号 %s 上游无投放套餐，跳过领取", acc.Name))
		return outcomes
	}

	for _, plan := range plans {
		planID, _ := plan["plan_id"].(string)
		result, err := s.Claim(acc, planID)
		if err != nil {
			outcomes = append(outcomes, map[string]any{
				"account_id": acc.ID, "account_name": acc.Name,
				"ok": false, "plan_id": planID, "message": err.Error(),
			})
			web.Warn("claim", fmt.Sprintf("账号 %s 自动领取 %s 失败: %v", acc.Name, planID, err))
			continue
		}
		outcome := map[string]any{
			"account_id": acc.ID, "account_name": acc.Name, "ok": true,
		}
		for k, v := range result {
			outcome[k] = v
		}
		planName, _ := outcome["plan_name"].(string)
		if planName == "" {
			if name, _ := plan["name"].(string); name != "" {
				planName = name
			} else {
				planName = planID
			}
			outcome["plan_name"] = planName
		}
		if g, ok := outcome["grants"].([]any); !ok || len(g) == 0 {
			if pg, ok := plan["grants"].([]any); ok && len(pg) > 0 {
				outcome["grants"] = pg
			}
		}
		outcomes = append(outcomes, outcome)
		web.Ok("claim", fmt.Sprintf("账号 %s 自动领取成功: %s", acc.Name, planName))
	}
	return outcomes
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

// orFirst 依次取字符串字段（含 camelCase 别名），全空返回空串。
func orFirst(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := m[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// firstNonNil 取首个非 nil 值。
func firstNonNil(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// asFloat 宽松取 float（float64/int；JSON 解析一律 float64）。
func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0
	}
}

// lower 小写化原始体（鉴权失败文本检测）。
func lower(raw []byte) string {
	out := make([]byte, len(raw))
	for i, b := range raw {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		out[i] = b
	}
	return string(out)
}

// containsAny 文本包含任一子串。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
