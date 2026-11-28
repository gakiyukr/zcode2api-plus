// Claude Code ToolSearch / defer_loading 相容清洗（對齊 magpie searchless / searchNamed）。
//
// 背景：Claude Code 送出名為 "ToolSearch" 的工具與 defer_loading 欄位時，
// 上游 Anthropic 端點會隱藏除了 tool_reference 載入以外的所有工具（連 Bash/Read/Write
// 都無法呼叫）。解法為發往上遊前將 ToolSearch 改名為 ToolSearchTool、清除 defer_loading
// 並過濾 DeferredToolPlaceholder；回應端（非串流 JSON 與 SSE 串流）再將 ToolSearchTool
// 還原回 ToolSearch。
package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"strings"

	"zcode2api/internal/util"
)

const (
	toolSearchName      = "ToolSearch"
	toolSearchAlias     = "ToolSearchTool"
	deferredPlaceholder = "DeferredToolPlaceholder"
)

// RewriteToolSearchRequest 在請求體上進行 ToolSearch 清洗（安全複製受修改的
// tools/messages/tool_choice，不污染呼叫方原物件）。
// 僅在 tools 含 ToolSearch 且不含 ToolSearchTool 時生效，回傳是否進行了改名。
func RewriteToolSearchRequest(body map[string]any) bool {
	if body == nil {
		return false
	}
	rawTools, ok := body["tools"].([]any)
	if !ok || len(rawTools) == 0 {
		return false
	}
	hasSearch := false
	for _, item := range rawTools {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := t["name"].(string)
		if name == toolSearchAlias {
			// 使用者已有名為 ToolSearchTool 的工具，不可衝突改名
			return false
		}
		if name == toolSearchName {
			hasSearch = true
		}
	}
	if !hasSearch {
		return false
	}

	// 清洗 tools：移除 DeferredToolPlaceholder、刪除 defer_loading、改名 ToolSearch
	cleanTools := make([]any, 0, len(rawTools))
	for _, item := range rawTools {
		t, ok := item.(map[string]any)
		if !ok {
			cleanTools = append(cleanTools, item)
			continue
		}
		name, _ := t["name"].(string)
		if name == deferredPlaceholder {
			continue
		}
		clone := maps.Clone(t)
		delete(clone, "defer_loading")
		if name == toolSearchName {
			clone["name"] = toolSearchAlias
		}
		cleanTools = append(cleanTools, clone)
	}
	body["tools"] = cleanTools

	// 同步改寫 assistant 歷史訊息中的 tool_use(ToolSearch)
	if rawMsgs, ok := body["messages"].([]any); ok {
		newMsgs := make([]any, len(rawMsgs))
		for i, item := range rawMsgs {
			msg, ok := item.(map[string]any)
			if !ok {
				newMsgs[i] = item
				continue
			}
			role, _ := msg["role"].(string)
			blocks, isBlocks := msg["content"].([]any)
			if role != "assistant" || !isBlocks {
				newMsgs[i] = msg
				continue
			}
			changed := false
			newBlocks := make([]any, len(blocks))
			for j, b := range blocks {
				m, ok := b.(map[string]any)
				if ok && m["type"] == "tool_use" && m["name"] == toolSearchName {
					mc := maps.Clone(m)
					mc["name"] = toolSearchAlias
					newBlocks[j] = mc
					changed = true
					continue
				}
				newBlocks[j] = b
			}
			if changed {
				mc := maps.Clone(msg)
				mc["content"] = newBlocks
				newMsgs[i] = mc
			} else {
				newMsgs[i] = msg
			}
		}
		body["messages"] = newMsgs
	}

	// 同步改寫 tool_choice
	if tc, ok := body["tool_choice"].(map[string]any); ok {
		if tc["type"] == "tool" && tc["name"] == toolSearchName {
			tcClone := maps.Clone(tc)
			tcClone["name"] = toolSearchAlias
			body["tool_choice"] = tcClone
		}
	}
	return true
}

// RestoreToolSearchJSON 將非串流回應中的 ToolSearchTool 還原為 ToolSearch。
func RestoreToolSearchJSON(raw []byte) []byte {
	if !bytes.Contains(raw, []byte(`"`+toolSearchAlias+`"`)) {
		return raw
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return raw
	}
	content, ok := payload["content"].([]any)
	if !ok {
		return raw
	}
	changed := false
	for _, item := range content {
		block, ok := item.(map[string]any)
		if ok && block["type"] == "tool_use" && block["name"] == toolSearchAlias {
			block["name"] = toolSearchName
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := util.MarshalJSON(payload)
	if err != nil {
		return raw
	}
	return out
}

// RestoreToolSearchStream 包裝 SSE 串流讀取器，逐行將 content_block 中
// name=="ToolSearchTool" 的 tool_use 還原為 "ToolSearch"，其餘位元組原樣透傳。
func RestoreToolSearchStream(r io.Reader) io.Reader {
	return &toolSearchStreamReader{r: r}
}

// maxToolSearchLineBytes 單行 SSE 緩衝上限，避免上游缺失換行符時無限耗盡記憶體。
const maxToolSearchLineBytes = 8 << 20

type toolSearchStreamReader struct {
	r          io.Reader
	raw        []byte // 尚未切行的原始緩衝
	out        []byte // 已處理待交付的位元組
	pendingErr error  // 有已解析資料待交付時暫存的讀取錯誤
	eof        bool
}

func (s *toolSearchStreamReader) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.pendingErr != nil {
			err := s.pendingErr
			s.pendingErr = nil
			return 0, err
		}
		if s.eof {
			if len(s.raw) > 0 {
				s.out = append(s.out, restoreSSELine(s.raw)...)
				s.raw = nil
				break
			}
			return 0, io.EOF
		}
		buf := make([]byte, 8192)
		n, err := s.r.Read(buf)
		if n > 0 {
			s.raw = append(s.raw, buf[:n]...)
			for {
				idx := bytes.IndexByte(s.raw, '\n')
				if idx < 0 {
					break
				}
				line := s.raw[:idx+1]
				s.raw = s.raw[idx+1:]
				s.out = append(s.out, restoreSSELine(line)...)
			}
			// 單行未遇換行符且已超上限：直接刷出避免緩衝無界成長
			if len(s.raw) >= maxToolSearchLineBytes {
				s.out = append(s.out, s.raw...)
				s.raw = nil
			}
		}
		if err == io.EOF {
			s.eof = true
			if len(s.raw) > 0 {
				s.out = append(s.out, restoreSSELine(s.raw)...)
				s.raw = nil
			}
			if len(s.out) == 0 {
				return 0, io.EOF
			}
			break
		}
		if err != nil {
			if len(s.raw) > 0 {
				s.out = append(s.out, restoreSSELine(s.raw)...)
				s.raw = nil
			}
			if len(s.out) > 0 {
				// 先交付已解析資料，下次 Read 再回報底層錯誤（data-before-error）
				s.pendingErr = err
				break
			}
			return 0, err
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

func restoreSSELine(line []byte) []byte {
	if !bytes.HasPrefix(line, []byte("data:")) || !bytes.Contains(line, []byte(`"`+toolSearchAlias+`"`)) {
		return line
	}
	hasNL := bytes.HasSuffix(line, []byte("\n"))
	hasCR := bytes.HasSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
	trimmed := strings.TrimSpace(string(line[len("data:"):]))
	var payload map[string]any
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		return line
	}
	cb, ok := payload["content_block"].(map[string]any)
	if !ok || cb["type"] != "tool_use" || cb["name"] != toolSearchAlias {
		return line
	}
	cb["name"] = toolSearchName
	encoded, err := util.MarshalJSON(payload)
	if err != nil {
		return line
	}
	out := append([]byte("data: "), encoded...)
	if hasCR {
		out = append(out, '\r')
	}
	if hasNL {
		out = append(out, '\n')
	}
	return out
}
