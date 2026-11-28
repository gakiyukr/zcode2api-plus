// Package egress 管理出站出口（直連或帳號代理）的健康巡檢與邊緣熔斷。
//
// 與 gateway 共用同一份熔斷表：巡檢命中 WAF 攔截頁時提前隔離出口，
// 不必等真實對話請求踩雷；gateway 的 HoldEdgeFor / EdgeHeldFor 委派到這裡。
package egress

import (
	"strings"
	"sync"
	"time"

	"zcode2api/internal/model"
)

var holdMap sync.Map // key -> until unix nano

// NormalizeKey 把空出口統一為 direct，避免直連被拆成多個桶。
func NormalizeKey(key string) string {
	if strings.TrimSpace(key) == "" {
		return "direct"
	}
	return key
}

// AccountKey 取得帳號對應的出口鍵：有 proxy_url 用代理 URL，否則 direct。
func AccountKey(acc *model.Account) string {
	if acc != nil && acc.ProxyURL != nil {
		if u := strings.TrimSpace(*acc.ProxyURL); u != "" {
			return u
		}
	}
	return "direct"
}

// HoldFor 隔離指定出口，duration 內不再向其上遊發送請求。
func HoldFor(key string, duration time.Duration, now time.Time) {
	now = now.Round(0)
	holdMap.Store(NormalizeKey(key), now.Add(duration).UnixNano())
}

// IsHeld 判斷出口目前是否處於熔斷期。
func IsHeld(key string, now time.Time) bool {
	now = now.Round(0)
	v, ok := holdMap.Load(NormalizeKey(key))
	if !ok {
		return false
	}
	until, _ := v.(int64)
	return until > 0 && now.UnixNano() < until
}
