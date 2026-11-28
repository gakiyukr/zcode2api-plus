package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
)

// maxPreambleBytes 限制首字前前奏緩衝；超過即視為已有實質內容強制交付，
// 避免上游用大量 message_start / ping 撐爆記憶體。
const maxPreambleBytes = 256 << 10

// streamHeartbeatInterval 交付開始後的 SSE 註解心跳間隔；客戶端與中間反代
// 常以 15–60 秒無資料判讀超時，10 秒留出餘量。測試可縮短。
var streamHeartbeatInterval = 10 * time.Second

// guardStopEvents 是 loopGuard 熔斷時補給客戶端的合法收尾事件。
// 三條對外路徑都在 Anthropic SSE 層工作，因此可在這裡統一補齊。
const guardStopEvents = "event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// readPreamble 讀取上游 SSE，直到出現第一個實質內容（文字、思考或工具調用）
// 為止，並回傳已讀取的原始位元組。首字前的空串流、error 事件或 EOF 回傳
// hasContent=false，呼叫方可丟棄前奏並換號重試。
func readPreamble(body io.Reader, maxBytes int) ([]byte, io.Reader, bool, error) {
	reader := bufio.NewReaderSize(body, 64<<10)
	var raw bytes.Buffer
	var data strings.Builder
	event := ""

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			raw.WriteString(line)
			if raw.Len() >= maxBytes {
				return raw.Bytes(), reader, true, nil
			}
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(trimmed, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			case strings.HasPrefix(trimmed, "data:"):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			case trimmed == "":
				eventType := event
				payload := decodeSSEData(data.String())
				if eventType == "" {
					eventType, _ = payload["type"].(string)
				}
				if eventType == "message_stop" || eventType == "error" {
					if !hasSubstantiveDelta(eventType, payload) {
						return raw.Bytes(), reader, false, nil
					}
				}
				if hasSubstantiveDelta(eventType, payload) {
					return raw.Bytes(), reader, true, nil
				}
				event = ""
				data.Reset()
			}
		}
		if err != nil {
			if err == io.EOF {
				return raw.Bytes(), reader, false, nil
			}
			return raw.Bytes(), reader, false, err
		}
	}
}

func decodeSSEData(data string) map[string]any {
	if strings.TrimSpace(data) == "" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return nil
	}
	return payload
}

// hasSubstantiveDelta 判斷一個 Anthropic SSE 事件是否已帶來可交付的實質內容。
// 空白 text/thinking 佔位、signature、message_start/message_delta 均不算。
func hasSubstantiveDelta(eventType string, payload map[string]any) bool {
	switch eventType {
	case "content_block_start":
		block, _ := payload["content_block"].(map[string]any)
		return block != nil && block["type"] == "tool_use"
	case "content_block_delta":
		delta, _ := payload["delta"].(map[string]any)
		if delta == nil {
			return false
		}
		switch delta["type"] {
		case "text_delta":
			text, _ := delta["text"].(string)
			return strings.TrimSpace(text) != ""
		case "thinking_delta":
			text, _ := delta["thinking"].(string)
			return strings.TrimSpace(text) != ""
		case "input_json_delta":
			text, _ := delta["partial_json"].(string)
			return strings.TrimSpace(text) != ""
		}
	}
	return false
}

// streamGuard 在交付階段偵測復讀死循環：最近 256 行內唯一非空行 <= 24 且
// 總字元達 4096 時，停止讀上游並補上合法的 message_delta/message_stop。
type streamGuard struct {
	src    io.Reader
	cancel func()

	buf     []byte
	line    []byte
	text    strings.Builder
	ring    []string
	ringPos int
	ringLen int
	chars   int
	counts  map[string]int

	pending   []byte
	err       error
	tripped   bool
	cancelOne sync.Once
}

func newStreamGuard(src io.Reader, cancel func()) *streamGuard {
	return &streamGuard{
		src:    src,
		cancel: cancel,
		buf:    make([]byte, 32<<10),
		ring:   make([]string, 256),
		counts: map[string]int{},
	}
}

func (g *streamGuard) Read(p []byte) (int, error) {
	for {
		if len(g.pending) > 0 {
			n := copy(p, g.pending)
			g.pending = g.pending[n:]
			return n, nil
		}
		if g.err != nil {
			return 0, g.err
		}
		n, err := g.src.Read(g.buf)
		if n > 0 {
			// 先保留原始位元組再分析；守衛只旁路觀察，不能改變交付內容。
			g.pending = append(g.pending, g.buf[:n]...)
			g.feed(g.buf[:n])
			if g.tripped {
				g.cancelOne.Do(func() {
					if g.cancel != nil {
						g.cancel()
					}
				})
				g.pending = append(g.pending, []byte(guardStopEvents)...)
				g.err = io.EOF
			}
			continue
		}
		if err != nil {
			g.err = err
		}
	}
}

func (g *streamGuard) feed(chunk []byte) {
	for _, b := range chunk {
		if b == '\n' {
			g.finishLine()
			continue
		}
		g.line = append(g.line, b)
	}
}

func (g *streamGuard) finishLine() {
	line := strings.TrimRight(string(g.line), "\r")
	g.line = g.line[:0]
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := decodeSSEData(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	delta, _ := payload["delta"].(map[string]any)
	if delta == nil {
		return
	}
	var text string
	switch delta["type"] {
	case "text_delta":
		text, _ = delta["text"].(string)
	case "thinking_delta":
		text, _ = delta["thinking"].(string)
	}
	if text == "" {
		return
	}
	g.text.WriteString(text)
	for {
		s := g.text.String()
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			break
		}
		g.addLine(s[:idx])
		g.text.Reset()
		g.text.WriteString(s[idx+1:])
	}
}

func (g *streamGuard) addLine(raw string) {
	line := strings.TrimSpace(raw)
	if line == "" {
		return
	}
	if g.ringLen < len(g.ring) {
		g.ring[g.ringLen] = line
		g.ringLen++
	} else {
		old := g.ring[g.ringPos]
		g.dropLine(old)
		g.ring[g.ringPos] = line
		g.ringPos = (g.ringPos + 1) % len(g.ring)
	}
	g.counts[line]++
	g.chars += len(line)
	if g.chars >= 4096 && len(g.counts) <= 24 && g.ringLen >= 64 {
		g.tripped = true
	}
}

func (g *streamGuard) dropLine(line string) {
	count := g.counts[line]
	if count <= 1 {
		delete(g.counts, line)
	} else {
		g.counts[line] = count - 1
	}
	g.chars -= len(line)
}

// heartbeatReader 在交付階段以讀取 goroutine + ticker 的方式插入 SSE 註解。
// 未寫出 HTTP 頭之前不能發任何位元組，因此心跳只在 commit 之後啟動；
// 首字前的等待仍保留換號能力，不會提前鎖死重試。
type heartbeatReader struct {
	src      io.Reader
	interval time.Duration
	ch       chan heartbeatChunk
	done     chan struct{}
	pending  []byte
	err      error
	closeOne sync.Once
}

type heartbeatChunk struct {
	data []byte
	err  error
}

func newHeartbeatReader(src io.Reader, interval time.Duration) *heartbeatReader {
	h := &heartbeatReader{
		src:      src,
		interval: interval,
		ch:       make(chan heartbeatChunk, 1),
		done:     make(chan struct{}),
	}
	go h.loop()
	return h
}

func (h *heartbeatReader) loop() {
	buf := make([]byte, 32<<10)
	for {
		n, err := h.src.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			select {
			case h.ch <- heartbeatChunk{data: chunk}:
			case <-h.done:
				return
			}
		}
		if err != nil {
			select {
			case h.ch <- heartbeatChunk{err: err}:
			case <-h.done:
			}
			return
		}
	}
}

func (h *heartbeatReader) Read(p []byte) (int, error) {
	for {
		if len(h.pending) > 0 {
			n := copy(p, h.pending)
			h.pending = h.pending[n:]
			return n, nil
		}
		if h.err != nil {
			return 0, h.err
		}
		timer := time.NewTimer(h.interval)
		select {
		case chunk := <-h.ch:
			timer.Stop()
			if len(chunk.data) > 0 {
				h.pending = chunk.data
				continue
			}
			if chunk.err != nil {
				h.err = chunk.err
				return 0, chunk.err
			}
		case <-timer.C:
			hb := []byte(": keepalive\n\n")
			n := copy(p, hb)
			return n, nil
		case <-h.done:
			timer.Stop()
			return 0, io.EOF
		}
	}
}

func (h *heartbeatReader) Close() error {
	h.closeOne.Do(func() { close(h.done) })
	return nil
}
