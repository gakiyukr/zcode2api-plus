// Package claim 激活遥测上报：官方 event/report 端点的单一事实源（对应
// Python 版 app/telemetry.py）。活动套餐投放疑似以「官方客户端当日活跃」
// 为资格信号，preview 前模拟 app_launch / app_daily_active 两个事件。
// 端点不校验登录态，请求不带 Authorization。
package claim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/util"
)

// EventReportURL 激活事件上报端点（测试可覆写指向 mock）。
var EventReportURL = "https://zcode.z.ai/api/v1/event/report"

// ActivationElements preview 前依次上报的两个激活事件。
var ActivationElements = []string{"app_launch", "app_daily_active"}

// activationScreen 桌面端常见分辨率；上游仅做形态校验，固定值即可。
const activationScreen = "2560x1440"

// eventClient 上报客户端（测试可注入；默认 10s 超时直连）。
var eventClient = &http.Client{Timeout: 10 * time.Second}

// osVersion 对齐 platform.release() 语义（Windows 下 platform.version() 同源
// 为内核版本串；Go 无直接等价，返回空由上游按形态校验兜底也可，这里取
// GOOS 通用做法：unix 读 uname，windows 取空串——上游不校验具体值）。
func osVersion() string {
	return ""
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
		"app_version":        config.ZcodeClientVersion,
		"device_os_category": osCategory,
		"device_os_version":  osVersion(),
		"device_mid":         deviceMid,
		"mac_id":             "",
		"marketing_params":   "{}",
	}
}

// PostActivationEvent 单条激活事件上报（无 Authorization）。
// HTTP >= 400 或业务码非 0 返回错误；调用方决定容错策略。
func PostActivationEvent(userID, element, deviceMid string) error {
	body, err := json.Marshal(BuildActivationEventBody(element, userID, deviceMid))
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, EventReportURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := eventClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	text := string(raw)
	if res.StatusCode >= 400 {
		return fmt.Errorf("event/report %s HTTP %d: %s", element, res.StatusCode, util.Truncate(text, 120))
	}
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	if code := BusinessCode(parsed); code != 0 {
		return fmt.Errorf("event/report %s 业务码异常(%d): %s", element, code, util.Truncate(text, 120))
	}
	return nil
}

// BusinessCode 上游业务码；非对象 JSON / 缺 code / 非数字 → -1（视为失败）。
func BusinessCode(body map[string]any) int {
	if body == nil {
		return -1
	}
	return toInt(body["code"])
}

// toInt 宽松取整数（float64/int/string 数字形态）。
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		var out int
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &out); err == nil {
			return out
		}
	}
	return -1
}


