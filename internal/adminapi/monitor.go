// 控制台监控端点：/admin/api/monitor（系统/请求/账号/服务状态）
// 与 /admin/api/usage（累计用量排行）。对应 Python 版 admin_api.py 尾段。
package adminapi

import (
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"zcode2api/internal/config"
)

func (h *Handler) handleMonitor(w http.ResponseWriter, r *http.Request) {
	_, stats := h.accountSnapshot()
	uptime := math.Max(0, nowFloat()-float64(h.StartedAt.UnixNano())/1e9)
	calls := stats["calls"].(int)
	fail := stats["fail"].(int)

	var successRate any
	if calls > 0 {
		// fail 与 calls 计数口径独立，失败数理论上可超过调用数，夹取防负值
		successRate = roundN(max(0, float64(calls-fail)/float64(calls)*100), 2)
	}
	averageQPS := 0.0
	if uptime > 0 {
		averageQPS = roundN(float64(calls)/uptime, 3)
	}

	cpuCount := runtime.NumCPU()
	if cpuCount < 1 {
		cpuCount = 1
	}
	browserStatus, browserDetail := "standby", "按需啟用"
	if config.CaptchaBrowserEnabled {
		browserStatus, browserDetail = "online", "Chromium"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ts":         nowFloat(),
		"uptime_sec": int64(math.Round(uptime)),
		"system": map[string]any{
			"cpu_count": cpuCount,
			"load_1m":   loadAverage1m(),
			"memory":    memorySnapshot(),
		},
		"requests": map[string]any{
			"total":        calls,
			"errors":       fail,
			"success_rate": successRate,
			"average_qps":  averageQPS,
		},
		"accounts": map[string]any{
			"total":     stats["total"],
			"active":    stats["active"],
			"cooling":   stats["cooling"],
			"exhausted": stats["exhausted"],
			"invalid":   stats["invalid"],
			"disabled":  stats["disabled"],
		},
		"services": []map[string]any{
			{"name": "API Gateway", "status": "online", "detail": "Anthropic Messages"},
			{"name": "額度監控", "status": "online", "detail": "背景輪詢"},
			{"name": "驗證瀏覽器", "status": browserStatus, "detail": browserDetail},
		},
	})
}

func (h *Handler) handleUsage(w http.ResponseWriter, r *http.Request) {
	accounts, stats := h.accountSnapshot()
	ranking := make([]map[string]any, 0, len(accounts))
	for _, view := range accounts {
		// 過濾歸檔帳號：archived_at 在 PublicView 中為 *float64（需區分 typed nil）
		if archived, ok := view["archived_at"].(*float64); ok && archived != nil {
			continue
		} else if f, ok := view["archived_at"].(float64); ok && f > 0 {
			continue
		}
		tokens := 0
		if tt, ok := view["total_tokens"].(map[string]int); ok {
			for _, v := range tt {
				tokens += v
			}
		}
		ranking = append(ranking, map[string]any{
			"id":       view["id"],
			"name":     view["name"],
			"provider": view["provider"],
			"requests": view["use_count"],
			"errors":   view["fail_count"],
			"tokens":   tokens,
		})
	}
	// (requests, tokens) 降序，取前 12（对齐 Python sorted(reverse=True)）
	sort.SliceStable(ranking, func(i, j int) bool {
		if ri, rj := ranking[i]["requests"].(int), ranking[j]["requests"].(int); ri != rj {
			return ri > rj
		}
		return ranking[i]["tokens"].(int) > ranking[j]["tokens"].(int)
	})
	if len(ranking) > 12 {
		ranking = ranking[:12]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ts":      nowFloat(),
		"window":  "累計",
		"summary": stats,
		"ranking": ranking,
	})
}

// loadAverage1m 读取 1 分钟平均负载；仅 Linux 支持（对应 os.getloadavg，
// 其余平台返回 null）。
func loadAverage1m() any {
	if runtime.GOOS != "linux" {
		return nil
	}
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return nil
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil
	}
	return roundN(v, 2)
}

// memorySnapshot 读取主机内存用量；非 Linux 返回全空以保持 API 可用。
func memorySnapshot() map[string]any {
	if runtime.GOOS != "linux" {
		return nilMemory()
	}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nilMemory()
	}
	meminfo := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		meminfo[key] = n
	}
	total := meminfo["MemTotal"]
	if total == 0 {
		return nilMemory()
	}
	available, ok := meminfo["MemAvailable"]
	if !ok {
		available = meminfo["MemFree"]
	}
	var availableMB any
	if available > 0 {
		availableMB = roundN(float64(available)/1024, 1)
	}
	return map[string]any{
		"total_mb":     roundN(float64(total)/1024, 1),
		"available_mb": availableMB,
		"used_percent": roundN(float64(total-available)/float64(total)*100, 1),
	}
}

func nilMemory() map[string]any {
	return map[string]any{"total_mb": nil, "available_mb": nil, "used_percent": nil}
}

func roundN(v float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(v*p) / p
}
