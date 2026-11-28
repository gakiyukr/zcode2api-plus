// Package claim 激活遥测上报：官方 event/report 端点的单一事实源（对应
// Python 版 app/telemetry.py）。活动套餐投放疑似以「官方客户端当日活跃」
// 为资格信号，preview 前模拟 app_launch / app_daily_active 两个事件。
// 端点不校验登录态，请求不带 Authorization。
package claim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/fingerprint"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/util"
	"zcode2api/internal/web"
)

// EventReportURL 激活事件上报端点（测试可覆写指向 mock）。
var EventReportURL = "https://zcode.z.ai/api/v1/event/report"

// ActivationElements preview 前依次上报的两个激活事件。
var ActivationElements = []string{"app_launch", "app_daily_active"}

// eventClient 上報客戶端（測試可注入；預設 10s 超時直連）。
var eventClient = &http.Client{Timeout: 10 * time.Second}

// ActivationRetryDelays 激活事件上報遇到傳輸層瞬態錯誤的階梯退避延遲
// （總計 3 次嘗試：首發 + 2s + 4s，冪等安全；測試可縮短）。
var ActivationRetryDelays = []time.Duration{2 * time.Second, 4 * time.Second}

// BuildActivationEventBody 保留舊呼叫形狀，使用預設桌面 SKU；
// 帳號路徑應使用 BuildActivationEventBodyFor 與請求頭保持同源。
func BuildActivationEventBody(element, userID, deviceMid string) map[string]any {
	return BuildActivationEventBodyFor(element, userID, deviceMid, fingerprint.For(nil))
}

// BuildActivationEventBodyFor 激活事件体（官方 sendReport 字段集固定这 16 个）。
// 设备维度全部取自帳號 SKU，避免遥测与对话请求头互相矛盾。
func BuildActivationEventBodyFor(element, userID, deviceMid string, sku fingerprint.SKU) map[string]any {
	return map[string]any{
		"event_id":           util.NewUUID(),
		"client_timezone":    sku.Timezone,
		"client_language":    sku.Language,
		"element_name":       element,
		"event_region":       "app",
		"event_type":         "view",
		"event_text":         "",
		"event_extra_detail": map[string]any{},
		"user_id":            userID,
		"screen_resolution":  sku.Screen,
		"app_version":        config.ZcodeBillingVersion,
		"device_os_category": sku.OsCategory,
		"device_os_version":  sku.OsVersion,
		"device_mid":         deviceMid,
		"mac_id":             "",
		"marketing_params":   "{}",
	}
}

// eventClientFor 返回激活上報的出站客戶端：帳號有設定 proxy_url 時走同代理出口，
// 確保激活資格訊號與 billing/claim 出口一致（代理無效時回退直連並記日誌）。
func eventClientFor(acc *model.Account) *http.Client {
	if acc != nil && acc.ProxyURL != nil && strings.TrimSpace(*acc.ProxyURL) != "" {
		if c, err := proxy.ClientFor(*acc.ProxyURL, 10*time.Second); err == nil {
			return c
		}
		web.Warn("claim", fmt.Sprintf("帳號 %s 激活上報代理無效，回退直連", acc.Name))
	}
	return eventClient
}

// PostActivationEvent 單條激活事件上報（無帳號代理，直連）。
func PostActivationEvent(userID, element, deviceMid string) error {
	return PostActivationEventFor(nil, userID, element, deviceMid)
}

// PostActivationEventFor 單條激活事件上報（跟隨帳號出站代理，無 Authorization）。
// 僅對傳輸層網路錯誤做階梯重試（預設 3 次 2s/4s）；HTTP >= 400 或業務碼非 0 立即返回錯誤。
func PostActivationEventFor(acc *model.Account, userID, element, deviceMid string) error {
	return PostActivationEventContext(context.Background(), acc, userID, element, deviceMid)
}

// PostActivationEventContext 帶可取消 context 的激活事件上報，重試退避期間可立即中斷。
func PostActivationEventContext(ctx context.Context, acc *model.Account, userID, element, deviceMid string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	body, err := json.Marshal(BuildActivationEventBodyFor(element, userID, deviceMid, fingerprint.For(acc)))
	if err != nil {
		return err
	}
	client := eventClientFor(acc)
	totalAttempts := len(ActivationRetryDelays) + 1
	var lastErr error
	for attempt := 0; attempt < totalAttempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(ActivationRetryDelays[attempt-1])
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, EventReportURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			continue // 僅網路傳輸層錯誤重試
		}
		raw, readErr := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if readErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = readErr
			continue
		}
		text := string(raw)
		if res.StatusCode >= 400 {
			return fmt.Errorf("event/report %s HTTP %d: %s", element, res.StatusCode, util.Truncate(text, 120))
		}
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		if code := BusinessCode(parsed); code != 0 {
			return fmt.Errorf("event/report %s 業務碼異常(%d): %s", element, code, util.Truncate(text, 120))
		}
		return nil
	}
	return lastErr
}

// BusinessCode 上游业务码；非对象 JSON / 缺 code / 非数字 → -1（视为失败）。
func BusinessCode(body map[string]any) int {
	if body == nil {
		return -1
	}
	return toInt(body["code"])
}

// toInt 寬鬆取整數（float64/int/int64/嚴格數字字串，無效回傳 -1）。
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case string:
		if out, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return out
		}
	}
	return -1
}
