// 會話親和性路由（Sticky Session）：三級會話鍵提取與有界 LRU 快取。
// 透過將同一多輪對話持續黏附到同一帳號，大幅提升 ZCode 上游 Prompt Cache 命中率；
// 同時結合單號滿載溢出與錯誤即時解綁，兼顧可用性與高併發分散。
package gateway

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/upstream"
)

const (
	// DefaultAffinityMaxEntries LRU 親和表預設容量上限。
	DefaultAffinityMaxEntries = 8192
	// DefaultAffinityTTL 單一會話綁定預設存活時間（每次命中會滑動續期）。
	DefaultAffinityTTL = 30 * time.Minute
)

type affinityEntry struct {
	key       string
	accountID string
	expiresAt time.Time
}

// SessionAffinity 執行緒安全的有界 LRU 會話親和表。
type SessionAffinity struct {
	mu         sync.Mutex
	maxEntries int
	ttl        time.Duration
	ll         *list.List
	items      map[string]*list.Element
}

// NewSessionAffinity 建立會話親和表；maxEntries/ttl 非正值時回退預設值。
func NewSessionAffinity(maxEntries int, ttl time.Duration) *SessionAffinity {
	if maxEntries <= 0 {
		maxEntries = DefaultAffinityMaxEntries
	}
	if ttl <= 0 {
		ttl = DefaultAffinityTTL
	}
	return &SessionAffinity{
		maxEntries: maxEntries,
		ttl:        ttl,
		ll:         list.New(),
		items:      make(map[string]*list.Element),
	}
}

// Lookup 查詢會話鍵綁定的帳號 ID；命中且未過期時滑動續期並移至表頭，已過期則移除並回傳空串。
func (a *SessionAffinity) Lookup(key string, now time.Time) string {
	if a == nil || key == "" {
		return ""
	}
	now = now.Round(0)
	a.mu.Lock()
	defer a.mu.Unlock()
	elem, ok := a.items[key]
	if !ok {
		return ""
	}
	ent := elem.Value.(*affinityEntry)
	if !now.Before(ent.expiresAt) {
		a.ll.Remove(elem)
		delete(a.items, key)
		return ""
	}
	ent.expiresAt = now.Add(a.ttl)
	a.ll.MoveToFront(elem)
	return ent.accountID
}

// Bind 綁定或更新會話鍵至指定帳號 ID；超過容量上限時淘汰最久未使用項目。
func (a *SessionAffinity) Bind(key, accountID string, now time.Time) {
	if a == nil || key == "" || accountID == "" {
		return
	}
	now = now.Round(0)
	a.mu.Lock()
	defer a.mu.Unlock()
	if elem, ok := a.items[key]; ok {
		ent := elem.Value.(*affinityEntry)
		ent.accountID = accountID
		ent.expiresAt = now.Add(a.ttl)
		a.ll.MoveToFront(elem)
		return
	}
	ent := &affinityEntry{
		key:       key,
		accountID: accountID,
		expiresAt: now.Add(a.ttl),
	}
	elem := a.ll.PushFront(ent)
	a.items[key] = elem
	for a.ll.Len() > a.maxEntries {
		back := a.ll.Back()
		if back == nil {
			break
		}
		a.ll.Remove(back)
		delete(a.items, back.Value.(*affinityEntry).key)
	}
}

// EvictIfAccount 當該會話鍵當前綁定的正是 accountID 時移除之（供故障換號即時解綁）。
func (a *SessionAffinity) EvictIfAccount(key, accountID string) {
	if a == nil || key == "" || accountID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	elem, ok := a.items[key]
	if !ok {
		return
	}
	if elem.Value.(*affinityEntry).accountID == accountID {
		a.ll.Remove(elem)
		delete(a.items, key)
	}
}

// Len 回傳目前表內有效條目數。
func (a *SessionAffinity) Len() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ll.Len()
}

// DeriveAffinityKeys 依三級策略提取會話親和鍵（lookupKey 用於選號查詢，bindKey 用於成功後綁定）：
//  1. 顯式會話標頭（x-session-id / x-claude-code-session-id / x-conversation-id）
//  2. body.metadata（session_id / conversation_id / user_id）
//  3. 首條非系統提醒使用者訊息指紋：單輪新對話不查黏性（lookupKey=""）但預埋 bindKey，
//     自第 2 輪起（含 assistant / tool_result / >=2 條 user 訊息）自動命中首輪帳號。
func DeriveAffinityKeys(body map[string]any, incomingHeaders map[string]string) (lookupKey string, bindKey string) {
	scope := deriveCallerScope(incomingHeaders)

	// 第一級：顯式會話標頭
	if s := upstream.ExtractClientSessionKey(incomingHeaders); s != "" {
		k := "h:" + scope + ":" + strings.ToLower(s)
		return k, k
	}

	// 第二級：body["metadata"] 會話／使用者欄位
	if meta, ok := body["metadata"].(map[string]any); ok {
		for _, field := range []string{"session_id", "conversation_id", "user_id"} {
			if v, ok := meta[field].(string); ok {
				if trimmed := strings.TrimSpace(v); trimmed != "" {
					k := "m:" + scope + ":" + trimmed
					return k, k
				}
			}
		}
	}

	// 第三級：首條非 <system-reminder> 使用者訊息指紋
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return "", ""
	}
	firstUserText := ""
	userTurns := 0
	isMultiTurn := false

	for _, item := range msgs {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "assistant":
			isMultiTurn = true
		case "user":
			text, hasToolResult := extractUserMessageSummary(msg["content"])
			if hasToolResult {
				isMultiTurn = true
			}
			trimmed := strings.TrimSpace(text)
			if trimmed == "" || strings.HasPrefix(trimmed, "<system-reminder>") {
				continue
			}
			userTurns++
			if firstUserText == "" {
				firstUserText = trimmed
			}
		}
	}
	if userTurns >= 2 {
		isMultiTurn = true
	}
	if firstUserText == "" {
		return "", ""
	}
	if len(firstUserText) > 4096 {
		firstUserText = firstUserText[:4096]
	}
	sum := sha256.Sum256([]byte(firstUserText))
	modelName, _ := body["model"].(string)
	fp := "f:" + scope + ":" + strings.ToLower(strings.TrimSpace(modelName)) + ":" + hex.EncodeToString(sum[:])
	if isMultiTurn {
		return fp, fp
	}
	return "", fp
}

func deriveCallerScope(incomingHeaders map[string]string) string {
	var authVal, keyVal string
	for k, v := range incomingHeaders {
		val := strings.TrimSpace(v)
		if val == "" {
			continue
		}
		switch strings.ToLower(k) {
		case "authorization":
			if authVal == "" {
				authVal = val
			}
		case "x-api-key":
			if keyVal == "" {
				keyVal = val
			}
		}
	}
	raw := authVal
	if raw == "" {
		raw = keyVal
	}
	if raw == "" {
		return "anon"
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:8])
}

func extractUserMessageSummary(content any) (text string, hasToolResult bool) {
	switch c := content.(type) {
	case string:
		return c, false
	case []any:
		var sb strings.Builder
		for _, part := range c {
			block, ok := part.(map[string]any)
			if !ok {
				continue
			}
			t, _ := block["type"].(string)
			if t == "tool_result" {
				hasToolResult = true
				continue
			}
			if t == "text" {
				if s, ok := block["text"].(string); ok && strings.TrimSpace(s) != "" {
					if sb.Len() > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(s)
				}
			}
		}
		return sb.String(), hasToolResult
	}
	return "", false
}
