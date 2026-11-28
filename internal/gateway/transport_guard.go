// 出站傳輸防護：未寫完請求體／複用斷連透明重放追蹤，以及 HTTP/2 上傳流控停滯看門狗。
// 供 gateway.Engine 與 asyncpool.Pool 共同複用，維持兩條請求路徑一致的網路韌性。
package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"zcode2api/internal/proxy"
)

const (
	// DefaultReplayDelay 未寫完請求體或複用斷連透明重放前的短暫退避間隔。
	DefaultReplayDelay = 250 * time.Millisecond
	// DefaultUploadStallTimeout 請求體寫入流控停滯看門狗超時；寫完請求後即停錶不影響長串流。
	DefaultUploadStallTimeout = 90 * time.Second
)

// ErrUploadStalled 請求體上傳期間長時間無法推進寫入時觸發的取消原因。
var ErrUploadStalled = errors.New("上游請求體上傳流控停滯逾時")

// AttemptTraceState 記錄單次 HTTP 呼叫的連線複用、首包到達、遠端位址與請求寫入完成狀態。
type AttemptTraceState struct {
	reused       atomic.Bool
	wroteReq     atomic.Bool
	gotFirstByte atomic.Bool

	mu         sync.Mutex
	remoteAddr string
	onWroteReq func()
}

// Reused 回傳本次呼叫是否復用了既有的 keep-alive / HTTP2 連線。
func (s *AttemptTraceState) Reused() bool {
	if s == nil {
		return false
	}
	return s.reused.Load()
}

// WroteRequest 回傳請求頭與請求體是否已成功送交底層連線。
func (s *AttemptTraceState) WroteRequest() bool {
	if s == nil {
		return false
	}
	return s.wroteReq.Load()
}

// RemoteAddr 回傳本次呼叫實際建立或復用連線的遠端位址（無則為空串）。
func (s *AttemptTraceState) RemoteAddr() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remoteAddr
}

func (s *AttemptTraceState) setRemoteAddr(addr string) {
	s.mu.Lock()
	s.remoteAddr = addr
	s.mu.Unlock()
}

func (s *AttemptTraceState) setOnWroteRequest(fn func()) {
	if s == nil {
		return
	}
	s.mu.Lock()
	already := s.wroteReq.Load()
	if !already {
		s.onWroteReq = fn
	}
	s.mu.Unlock()
	if already && fn != nil {
		fn()
	}
}

func (s *AttemptTraceState) markWroteRequest() {
	s.wroteReq.Store(true)
	s.mu.Lock()
	fn := s.onWroteReq
	s.onWroteReq = nil
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// AttachReplayTrace 在請求上下文掛載 httptrace 鉤子以偵測複用連線、首包到達、遠端位址與未寫完斷連。
func AttachReplayTrace(ctx context.Context) (context.Context, *AttemptTraceState) {
	if ctx == nil {
		ctx = context.Background()
	}
	state := &AttemptTraceState{}
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Reused {
				state.reused.Store(true)
			}
			if info.Conn != nil && info.Conn.RemoteAddr() != nil {
				state.setRemoteAddr(info.Conn.RemoteAddr().String())
			}
		},
		GotFirstResponseByte: func() {
			state.gotFirstByte.Store(true)
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				state.markWroteRequest()
			}
		},
	}
	return httptrace.WithClientTrace(ctx, trace), state
}

// ShouldTransparentReplay 判斷該次出站錯誤是否符合安全透明重放條件
// （複用連線、未寫完請求體，或首包未到達且遠端位址剛觸發 IPv6 零位元組降級 tcp4）。
func ShouldTransparentReplay(ctx context.Context, err error, state *AttemptTraceState, stalled bool) bool {
	if err == nil || stalled {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if state == nil {
		return false
	}
	if state.Reused() || !state.WroteRequest() {
		return true
	}
	if !state.gotFirstByte.Load() {
		if remote := state.RemoteAddr(); remote != "" && proxy.PreferIPv4(remote) {
			return true
		}
	}
	return false
}

type stallGuardReader struct {
	r       *bytes.Reader
	mu      sync.Mutex
	timer   *time.Timer
	timeout time.Duration
	stopped bool
	stalled atomic.Bool
	cancel  context.CancelCauseFunc
}

func (g *stallGuardReader) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	if n > 0 {
		g.reset()
	}
	return n, err
}

func (g *stallGuardReader) Close() error {
	return nil
}

func (g *stallGuardReader) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || g.timer == nil {
		return
	}
	g.timer.Reset(g.timeout)
}

func (g *stallGuardReader) stopTimer() {
	g.mu.Lock()
	if !g.stopped {
		g.stopped = true
		if g.timer != nil {
			g.timer.Stop()
		}
	}
	g.mu.Unlock()
}

// WrapBodyWithStallGuard 為請求體建立上傳流控停滯看門狗：
// 僅在請求體寫入期間啟用計時器，每次推進 Read 皆重置；一旦 WroteRequest 完成即立刻停錶，
// 交棒給 Transport.ResponseHeaderTimeout，絕不干擾後續長推理與 SSE 串流。
func WrapBodyWithStallGuard(
	ctx context.Context,
	payload []byte,
	stallTimeout time.Duration,
	state *AttemptTraceState,
) (context.Context, io.ReadCloser, func(), func() bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(payload) == 0 || stallTimeout <= 0 {
		return ctx, io.NopCloser(bytes.NewReader(payload)), func() {}, func() bool { return false }
	}
	reqCtx, cancel := context.WithCancelCause(ctx)
	g := &stallGuardReader{
		r:       bytes.NewReader(payload),
		timeout: stallTimeout,
		cancel:  cancel,
	}
	g.timer = time.AfterFunc(stallTimeout, func() {
		g.mu.Lock()
		stopped := g.stopped
		g.stopped = true
		g.mu.Unlock()
		if stopped {
			return
		}
		g.stalled.Store(true)
		cancel(ErrUploadStalled)
	})
	if state != nil {
		state.setOnWroteRequest(g.stopTimer)
	}
	cleanup := func() {
		g.stopTimer()
		cancel(nil)
	}
	return reqCtx, g, cleanup, g.stalled.Load
}
