package gateway

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestReadPreambleStopsAtFirstToken(t *testing.T) {
	sse := "data: {\"type\":\"message_start\"}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	preamble, rest, ok, err := readPreamble(strings.NewReader(sse), maxPreambleBytes)
	if err != nil || !ok {
		t.Fatalf("應在思考增量處 commit: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(string(preamble), "hmm") {
		t.Fatalf("前奏應包含已讀事件: %q", preamble)
	}
	remaining, _ := io.ReadAll(rest)
	if !strings.Contains(string(remaining), `"hi"`) {
		t.Fatalf("剩餘流應保留後續增量: %q", remaining)
	}
}

func TestReadPreambleEmptyStream(t *testing.T) {
	sse := "data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	_, _, ok, err := readPreamble(strings.NewReader(sse), maxPreambleBytes)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("只有 message_start/message_stop 的空流不應 commit")
	}
}

func TestStreamGuardPassesNormalStreamUnchanged(t *testing.T) {
	sse := "data: {\"type\":\"message_start\"}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\\nworld\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	got, err := io.ReadAll(newStreamGuard(strings.NewReader(sse), nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sse {
		t.Fatalf("守衛不得改寫正常串流:\n got=%q\nwant=%q", got, sse)
	}
}

func TestStreamGuardTripsOnRepeatedLines(t *testing.T) {
	var b strings.Builder
	b.WriteString("data: {\"type\":\"message_start\"}\n\n")
	line := strings.Repeat("same line ", 10)
	for range 100 {
		b.WriteString("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" + line + "\\n\"}}\n\n")
	}
	b.WriteString("data: {\"type\":\"message_stop\"}\n\n")

	canceled := false
	guard := newStreamGuard(strings.NewReader(b.String()), func() { canceled = true })
	got, err := io.ReadAll(guard)
	if err != nil {
		t.Fatal(err)
	}
	if !canceled {
		t.Fatal("觸發 loopGuard 後應取消上游連線")
	}
	if !bytes.Contains(got, []byte("message_stop")) {
		t.Fatalf("熔斷後應補上終止事件: %q", got[len(got)-120:])
	}
}

func TestHeartbeatReaderEmitsKeepalive(t *testing.T) {
	pr, pw := io.Pipe()
	h := newHeartbeatReader(pr, 50*time.Millisecond)
	defer h.Close()
	defer pw.Close()

	go func() { _, _ = pw.Write([]byte("data: first\n\n")) }()
	buf := make([]byte, 64)
	n, err := h.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf[:n]), "data: first") {
		t.Fatalf("首個讀取應先轉發原始資料: %q", buf[:n])
	}

	n, err = h.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf[:n]), ": keepalive") {
		t.Fatalf("靜默期應回報心跳: %q", buf[:n])
	}
}
