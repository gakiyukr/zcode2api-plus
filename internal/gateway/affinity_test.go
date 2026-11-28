package gateway

import (
	"fmt"
	"testing"
	"time"
)

func TestSessionAffinity_ThreeTierKeysAndLRUEviction(t *testing.T) {
	// L1：顯式會話標頭
	lk, bk := DeriveAffinityKeys(
		map[string]any{"model": "GLM-5.4", "messages": []any{map[string]any{"role": "user", "content": "hi"}}},
		map[string]string{"Authorization": "Bearer sk-a", "X-Session-Id": "Sess-1"},
	)
	if lk == "" || lk != bk {
		t.Fatalf("L1 標頭應同時產生 lookupKey 與 bindKey: %q / %q", lk, bk)
	}
	// 跨呼叫方作用域隔離
	lkOther, _ := DeriveAffinityKeys(
		map[string]any{"model": "GLM-5.4"},
		map[string]string{"Authorization": "Bearer sk-b", "X-Session-Id": "Sess-1"},
	)
	if lk == lkOther {
		t.Fatalf("不同呼叫方憑證不應共享會話親和鍵: %q", lk)
	}

	// L2：body.metadata
	lk2, bk2 := DeriveAffinityKeys(
		map[string]any{"metadata": map[string]any{"conversation_id": "conv-42"}},
		nil,
	)
	if lk2 == "" || lk2 != bk2 {
		t.Fatalf("L2 metadata 應產生相同 lookupKey/bindKey: %q / %q", lk2, bk2)
	}

	// L3：首輪新對話不查黏性（lookupKey==""）但預埋 bindKey；第 2 輪多輪精準命中同一鍵
	turn1 := map[string]any{
		"model": "GLM-5.4",
		"messages": []any{
			map[string]any{"role": "user", "content": "<system-reminder>Today is 2026-11-28</system-reminder>"},
			map[string]any{"role": "user", "content": "請幫我寫一個快排"},
		},
	}
	t1Lookup, t1Bind := DeriveAffinityKeys(turn1, nil)
	if t1Lookup != "" || t1Bind == "" {
		t.Fatalf("L3 首輪單條訊息應回傳 lookupKey=\"\" 且 bindKey 非空，實際: %q / %q", t1Lookup, t1Bind)
	}
	turn2 := map[string]any{
		"model": "GLM-5.4",
		"messages": []any{
			map[string]any{"role": "user", "content": "請幫我寫一個快排"},
			map[string]any{"role": "assistant", "content": "好的，這是程式碼"},
			map[string]any{"role": "user", "content": "改成非遞迴版"},
		},
	}
	t2Lookup, t2Bind := DeriveAffinityKeys(turn2, nil)
	if t2Lookup != t1Bind || t2Bind != t1Bind {
		t.Fatalf("L3 第 2 輪應命中首輪預埋指紋 %q，實際: %q / %q", t1Bind, t2Lookup, t2Bind)
	}

	// LRU 容量淘汰、滑動續期與 TTL 過期清理
	now := time.Unix(1700000000, 0)
	aff := NewSessionAffinity(2, 5*time.Minute)
	aff.Bind("k1", "acc-1", now)
	aff.Bind("k2", "acc-2", now)
	// 存取 k1 使其成為最近使用，插入 k3 時應淘汰最久未用的 k2
	if got := aff.Lookup("k1", now.Add(time.Minute)); got != "acc-1" {
		t.Fatalf("k1 應命中 acc-1，實際: %q", got)
	}
	aff.Bind("k3", "acc-3", now.Add(time.Minute))
	if aff.Len() != 2 {
		t.Fatalf("LRU 容量上限應為 2，實際: %d", aff.Len())
	}
	if got := aff.Lookup("k2", now.Add(time.Minute)); got != "" {
		t.Fatalf("最久未使用的 k2 應被淘汰，實際: %q", got)
	}
	if got := aff.Lookup("k1", now.Add(time.Minute)); got != "acc-1" {
		t.Fatalf("剛被存取過的 k1 應保留，實際: %q", got)
	}

	// TTL 過期清理
	if got := aff.Lookup("k1", now.Add(10*time.Minute)); got != "" {
		t.Fatalf("超過 TTL 後應失效，實際: %q", got)
	}
	// EvictIfAccount 僅解綁匹配帳號
	aff.Bind("k4", "acc-4", now)
	aff.EvictIfAccount("k4", "acc-other")
	if got := aff.Lookup("k4", now); got != "acc-4" {
		t.Fatalf("不匹配帳號不應誤解綁: %q", got)
	}
	aff.EvictIfAccount("k4", "acc-4")
	if got := aff.Lookup("k4", now); got != "" {
		t.Fatalf("匹配帳號應被解綁: %q", got)
	}
	// 大量寫入壓力驗證容量恆定不超限
	big := NewSessionAffinity(16, time.Minute)
	for i := 0; i < 64; i++ {
		big.Bind(fmt.Sprintf("key-%d", i), "acc-x", now)
	}
	if big.Len() != 16 {
		t.Fatalf("容量應嚴格受限於 16，實際: %d", big.Len())
	}
}
