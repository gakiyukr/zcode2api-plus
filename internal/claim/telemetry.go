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
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/util"
	"zcode2api/internal/web"
)

// EventReportURL 激活事件上报端点（测试可覆写指向 mock）。
var EventReportURL = "https://zcode.z.ai/api/v1/event/report"

// ActivationElements preview 前依次上报的两个激活事件。
var ActivationElements = []string{"app_launch", "app_daily_active"}

// activationScreen 桌面端常见分辨率；上游仅做形态校验，固定值即可。
const activationScreen = "2560x1440"

// eventClient 上報客戶端（測試可注入；預設 10s 超時直連）。
var eventClient = &http.Client{Timeout: 10 * time.Second}

// ActivationRetryDelays 激活事件上報遇到傳輸層瞬態錯誤的階梯退避延遲
// （總計 3 次嘗試：首發 + 2s + 4s，冪等安全；測試可縮短）。
var ActivationRetryDelays = []time.Duration{2 * time.Second, 4 * time.Second}

func osVersion() string {
	return config.OsRelease()
}

// timezone 本机 IANA 时区名；取不到时回退 UTC（上报失败不阻断 preview）。
func timezone() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		return tz
	}
	if raw, err := os.ReadFile("/etc/timezone"); err == nil {
		if name := strings.TrimSpace(string(raw)); name != "" {
			return name
		}
	}
	return "UTC"
}

// language 本机 locale 语言标签（LC_ALL / LC_MESSAGES / LANG）。
func language() string {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw != "" {
			return strings.ReplaceAll(strings.SplitN(raw, ".", 2)[0], "_", "-")
		}
	}
	return "en-US"
}

// BuildActivationEventBody 激活事件体（官方 sendReport 字段集固定这 16 个）。
func BuildActivationEventBody(element, userID, deviceMid string) map[string]any {
	osCategory := map[string]string{
		"windows": "windows", "darwin": "macos",
	}[runtime.GOOS]
	if osCategory == "" {
		osCategory = "linux"
	}
	return map[string]any{
		"event_id":           util.NewUUID(),
		"client_timezone":    timezone(),
		"client_language":    language(),
		"element_name":       element,
		"event_region":       "app",
		"event_type":         "view",
		"event_text":         "",
		"event_extra_detail": map[string]any{},
		"user_id":            userID,
		"screen_resolution":  activationScreen,
		"app_version":        config.ZcodeBillingVersion,
		"device_os_category": osCategory,
		"device_os_version":  osVersion(),
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
	body, err := json.Marshal(BuildActivationEventBody(element, userID, deviceMid))
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
