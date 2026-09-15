// Async 空闲池测试：移植 Python 版 tests/test_async_pool.py 全部用例，
// 覆盖验证码刷新转发、白名单、ticket 生命周期、网络错误与流中断语义。
package asyncpool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// fakeSolver 依次返回预置令牌；tokens 耗尽时回退默认令牌（不关注
// 验证码内容的用例可直接使用），err 非空时始终报错。
type fakeSolver struct {
	mu     sync.Mutex
	tokens []string
	err    error
	calls  int
}

func (s *fakeSolver) Solve(ctx context.Context, cfg captcha.Config) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	if len(s.tokens) == 0 {
		return "tok-default", nil
	}
	tok := s.tokens[0]
	s.tokens = s.tokens[1:]
	return tok, nil
}

func (s *fakeSolver) Close() error { return nil }

func (s *fakeSolver) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// upstreamSpec 上游第 n 次调用的应答脚本。
type upstreamSpec struct {
	status      int
	contentType string
	body        string   // 非 SSE 应答体
	lines       []string // SSE 应答逐行写出（自动补 \n\n）
	abort       bool     // 写完后强制断连（模拟流中断）
	reset       bool     // 接受连线后立即断开（模拟连线层网络错误）
}

// scriptedUpstream 脚本化上游服务器。
type scriptedUpstream struct {
	mu    sync.Mutex
	specs []upstreamSpec
	calls []http.Header
}

func (s *scriptedUpstream) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := len(s.calls)
		s.calls = append(s.calls, r.Header.Clone())
		spec := upstreamSpec{status: http.StatusBadGateway}
		if n < len(s.specs) {
			spec = s.specs[n]
		}
		s.mu.Unlock()

		if spec.reset {
			// 未写任何响应头即断开：客户端 client.Do 直接返回网络错误
			panic(http.ErrAbortHandler)
		}
		if spec.lines != nil {
			w.Header().Set("Content-Type", orDefault(spec.contentType, "text/event-stream"))
			w.WriteHeader(spec.status)
			flusher := w.(http.Flusher)
			for _, line := range spec.lines {
				_, _ = fmt.Fprintf(w, "%s\n\n", line)
				flusher.Flush()
			}
			if spec.abort {
				panic(http.ErrAbortHandler) // 强制断连：客户端读流出错
			}
			return
		}
		w.Header().Set("Content-Type", orDefault(spec.contentType, "application/json"))
		w.WriteHeader(spec.status)
		_, _ = w.Write([]byte(spec.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *scriptedUpstream) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *scriptedUpstream) header(n int) http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[n]
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// newTestPool 打开隔离存储与空闲池；返回池、存储、验证码管理器与求解器桩。
func newTestPool(t *testing.T) (*Pool, *store.Store, *captcha.Manager, *fakeSolver) {
	t.Helper()
	oldDB, oldData, oldZai, oldFB, oldBrowser, oldMaxRetries := config.DBPath, config.DataDir,
		config.UpstreamZai, config.UpstreamZaiFallback, config.CaptchaBrowserEnabled, config.AsyncMaxRetries
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	config.DataDir = t.TempDir() // DeviceMid 持久化路径隔离
	config.CaptchaBrowserEnabled = true
	config.AsyncMaxRetries = 0 // 避免退避等待
	t.Cleanup(func() {
		config.DBPath, config.DataDir, config.UpstreamZai, config.UpstreamZaiFallback = oldDB, oldData, oldZai, oldFB
		config.CaptchaBrowserEnabled, config.AsyncMaxRetries = oldBrowser, oldMaxRetries
	})

	st, err := store.New()
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	solver := &fakeSolver{}
	cm := captcha.NewManager()
	cm.SetSolver(solver)
	// 固定配置源：避免 FetchConfig 触网（真实端点经代理约 2s，离线则回退
	// DefaultConfig 的 cn region，会让 region 断言随环境漂移）
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, Prefix: "no8xfe", Region: "sgp", SceneID: "11xygtvd"}, nil
	})
	p := NewPool(st, auth.New(st), cm)
	return p, st, cm, solver
}

// addJWTAccount 添加一个可被 Select 选中的 jwt 账号。
func addJWTAccount(t *testing.T, st *store.Store, name string) *model.Account {
	t.Helper()
	acc, err := st.AddAccount(model.ProviderZai, name, "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

// insertTicket 手工建票（不启动后台任务），供同步驱动 processTicket。
func insertTicket(p *Pool, id string, body map[string]any) *ticket {
	tk := &ticket{
		status:    "pending",
		body:      body,
		queue:     make(chan ticketEvent, 256),
		createdAt: time.Now(),
	}
	p.mu.Lock()
	p.tickets[id] = tk
	p.mu.Unlock()
	return tk
}

// drainEvents 取走队列中的全部事件。
func drainEvents(tk *ticket) []ticketEvent {
	var out []ticketEvent
	for {
		select {
		case ev := <-tk.queue:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestCode3007RefreshesTokenAndForwardsSSE(t *testing.T) {
	// 3007 后刷新验证码令牌重试，成功流转发为 chunk + done。
	p, st, _, solver := newTestPool(t)
	addJWTAccount(t, st, "async-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusBadRequest, body: `{"code":3007,"msg":"captcha verify failed"}`},
		{status: http.StatusOK, lines: []string{`data: {"id":"ok"}`, `data: [DONE]`}},
	}}
	config.UpstreamZai = up.start(t).URL
	solver.tokens = []string{"first-token", "fresh-token"}

	tk := insertTicket(p, "ticket-captcha", map[string]any{
		"model": "GLM-5.3", "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	})
	p.processTicket(context.Background(), "ticket-captcha")

	events := drainEvents(tk)
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "ready,chunk,done" {
		t.Fatalf("事件序列不符: %v", events)
	}
	if chunk, ok := events[1].Data.(map[string]any); !ok || chunk["id"] != "ok" {
		t.Fatalf("chunk 内容不符: %v", events[1].Data)
	}
	if up.callCount() != 2 {
		t.Fatalf("应两次请求上游: %d", up.callCount())
	}
	if got := up.header(0).Get("X-Aliyun-Captcha-Verify-Param"); got != "first-token" {
		t.Fatalf("首次应携带 first-token: %q", got)
	}
	if got := up.header(0).Get("X-Aliyun-Captcha-Verify-Region"); got != "sgp" {
		t.Fatalf("region 头不符: %q", got)
	}
	if got := up.header(1).Get("X-Aliyun-Captcha-Verify-Param"); got != "fresh-token" {
		t.Fatalf("重试应携带 fresh-token: %q", got)
	}
	if solver.callCount() != 2 {
		t.Fatalf("验证码应求解两次: %d", solver.callCount())
	}
}

func TestMissingCaptchaTokenIsNotSentBare(t *testing.T) {
	// 求解失败时不得裸打上游；耗尽后返回 captcha_required。
	p, st, _, solver := newTestPool(t)
	addJWTAccount(t, st, "async-acc")

	up := &scriptedUpstream{}
	config.UpstreamZai = up.start(t).URL
	solver.err = errors.New("browser down")

	tk := insertTicket(p, "ticket-no-token", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-no-token")

	events := drainEvents(tk)
	if up.callCount() != 0 {
		t.Fatalf("求解失败不得请求上游: %d", up.callCount())
	}
	last := events[len(events)-1]
	if last.Type != "error" {
		t.Fatalf("末事件应为 error: %v", events)
	}
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "captcha_required" {
		t.Fatalf("错误类型应为 captcha_required: %v", last.Data)
	}
	if !strings.Contains(fmt.Sprint(errObj["message"]), "browser down") {
		t.Fatalf("错误信息应含求解失败原因: %v", errObj)
	}
	if solver.callCount() != gateway.MaxCaptchaRetries {
		t.Fatalf("应求解 %d 次: %d", gateway.MaxCaptchaRetries, solver.callCount())
	}
}

func newTestMux(t *testing.T, p *Pool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	p.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAsyncDisabledReturns503(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	srv := newTestMux(t, p)
	_ = st.SetSetting("gateway_key", "sk-test")

	oldEnabled := config.AsyncEnabled
	config.AsyncEnabled = false
	t.Cleanup(func() { config.AsyncEnabled = oldEnabled })

	// 鉴权先于功能开关校验（对齐 FastAPI Depends），需携带有效 key 才能触达 503
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/async/v1/messages",
		strings.NewReader(`{"model":"GLM-5.3","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("关闭时应 503: %d", resp.StatusCode)
	}
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	errObj := payload["error"].(map[string]any)
	if errObj["type"] != "feature_disabled" {
		t.Fatalf("错误类型应为 feature_disabled: %v", payload)
	}
}

func TestMessagesRejectsModelOutsideWhitelist(t *testing.T) {
	// 入口端點必須在建票前擋下白名單外的模型，不得轉發上游。
	p, st, _, _ := newTestPool(t)
	srv := newTestMux(t, p)
	_ = st.SetSetting("gateway_key", "sk-test")

	oldEnabled := config.AsyncEnabled
	config.AsyncEnabled = true
	t.Cleanup(func() { config.AsyncEnabled = oldEnabled })

	body := `{"model":"GLM-5-Turbo","max_tokens":8,"messages":[]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/async/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("白名单外应 400: %d", resp.StatusCode)
	}
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	errObj := payload["error"].(map[string]any)
	if errObj["type"] != "model_not_allowed" {
		t.Fatalf("错误类型应为 model_not_allowed: %v", payload)
	}
	p.mu.Lock()
	remaining := len(p.tickets)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("不应建票: %d", remaining)
	}
}

func TestMessagesAcceptsWhitelistedModel(t *testing.T) {
	// 白名单内模型正常建票并返回 SSE 流；后台任务失败时以 error 事件收尾并清票。
	p, st, _, solver := newTestPool(t)
	addJWTAccount(t, st, "async-acc") // 无账号会先报 no_account，需提供可用账号
	srv := newTestMux(t, p)
	_ = st.SetSetting("gateway_key", "sk-test")
	solver.err = errors.New("browser down") // 后台任务走 captcha_required 失败路径

	oldEnabled := config.AsyncEnabled
	config.AsyncEnabled = true
	t.Cleanup(func() { config.AsyncEnabled = oldEnabled })

	body := `{"model":"glm-5.3-flash","max_tokens":8,"messages":[]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/async/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("白名单内应 200: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("应为 SSE 响应: %s", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, "event: ticket\ndata: ") {
		t.Fatalf("首事件应为 ticket: %q", text)
	}
	if !strings.Contains(text, `"status":"pending"`) {
		t.Fatalf("ticket 事件应含 pending: %s", text)
	}
	if !strings.Contains(text, "event: error") || !strings.Contains(text, "captcha_required") {
		t.Fatalf("流应以 captcha_required 错误收尾: %s", text)
	}
	p.mu.Lock()
	remaining := len(p.tickets)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("流结束后应释放票务: %d", remaining)
	}
}

func TestSSEDoneReleasesTicket(t *testing.T) {
	// 正常 done 事件後同樣要釋放 ticket，不得殘留。
	p, _, _, _ := newTestPool(t)
	tk := insertTicket(p, "ticket-done", map[string]any{"messages": []any{}})
	tk.queue <- ticketEvent{Type: "done"}

	var out []string
	p.streamTicket(context.Background(), func(s string) error {
		out = append(out, s)
		return nil
	}, "ticket-done")

	if last := out[len(out)-1]; last != "event: done\ndata: {}\n\n" {
		t.Fatalf("末事件应为 done: %v", out)
	}
	p.mu.Lock()
	remaining := len(p.tickets)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("done 后应释放票务: %d", remaining)
	}
}

func TestClientDisconnectReleasesTicket(t *testing.T) {
	// 客戶端中途斷開（ctx 取消）時應清票並中止後台任務。
	p, _, _, _ := newTestPool(t)
	insertTicket(p, "ticket-drop", map[string]any{"messages": []any{}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.streamTicket(ctx, func(string) error { return nil }, "ticket-drop")
	}()
	cancel()
	<-done

	p.mu.Lock()
	remaining := len(p.tickets)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("断开后应释放票务: %d", remaining)
	}
}

// 票务逾时必须显式投递终止事件，否则客户端只看到连接关闭，
// 无法区分「已完成」与「被超时截断」。
func TestTicketTimeoutEmitsErrorEvent(t *testing.T) {
	p, _, _, _ := newTestPool(t)
	old := config.AsyncTicketTimeout
	config.AsyncTicketTimeout = 30 // 下限；用 createdAt 回拨触发立即逾时
	t.Cleanup(func() { config.AsyncTicketTimeout = old })

	tk := insertTicket(p, "ticket-timeout", map[string]any{"messages": []any{}})
	tk.createdAt = time.Now().Add(-time.Duration(config.AsyncTicketTimeout+1) * time.Second)

	var out []string
	p.streamTicket(context.Background(), func(s string) error {
		out = append(out, s)
		return nil
	}, "ticket-timeout")

	if len(out) == 0 {
		t.Fatal("逾时应至少投递 ticket 事件与终止事件")
	}
	last := out[len(out)-1]
	if !strings.Contains(last, "event: error") || !strings.Contains(last, "ticket_timeout") {
		t.Fatalf("逾时应投递 ticket_timeout 错误事件: %q", last)
	}
	p.mu.Lock()
	remaining := len(p.tickets)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("逾时后应释放票务: %d", remaining)
	}
}

func TestReleaseTicketIgnoresUnknownIDAndFinishedTask(t *testing.T) {
	// 未知 id 與已結束任務都應安全跳過。
	p, _, _, _ := newTestPool(t)

	p.releaseTicket("no-such-ticket") // 不應拋錯

	_, cancel := context.WithCancel(context.Background())
	cancel() // 任务已结束：cancel 无操作
	insertTicket(p, "ticket-gone", map[string]any{}).cancel = cancel
	p.releaseTicket("ticket-gone")

	p.mu.Lock()
	remaining := len(p.tickets)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("应移除票务: %d", remaining)
	}
}

func TestSweepRemovesExpiredOrphansAndKeepsFresh(t *testing.T) {
	// 超過生命周期 + 寬限的孤兒 ticket 應被清掃，新建的不受影響。
	p, _, _, _ := newTestPool(t)

	stale := &ticket{
		status: "pending",
		queue:  make(chan ticketEvent, 1),
		createdAt: time.Now().Add(-time.Duration(config.AsyncTicketTimeout)*time.Second -
			120*time.Second),
	}
	p.mu.Lock()
	p.tickets["ticket-stale"] = stale
	p.mu.Unlock()
	insertTicket(p, "ticket-fresh", map[string]any{})

	p.sweepExpiredTickets()

	p.mu.Lock()
	_, hasStale := p.tickets["ticket-stale"]
	_, hasFresh := p.tickets["ticket-fresh"]
	p.mu.Unlock()
	if hasStale {
		t.Fatal("过期孤儿应被清扫")
	}
	if !hasFresh {
		t.Fatal("新建票务应保留")
	}
}

func TestNewTicketSweepsOrphans(t *testing.T) {
	// _new_ticket 建票前先清扫（对齐 Python 版顺序）。
	p, _, _, _ := newTestPool(t)

	stale := &ticket{
		status: "pending",
		queue:  make(chan ticketEvent, 1),
		createdAt: time.Now().Add(-time.Duration(config.AsyncTicketTimeout)*time.Second -
			120*time.Second),
	}
	p.mu.Lock()
	p.tickets["ticket-stale"] = stale
	p.mu.Unlock()

	p.newTicket(map[string]any{"messages": []any{}}) // 无账号：后台任务只会投递 no_account

	p.mu.Lock()
	_, hasStale := p.tickets["ticket-stale"]
	p.mu.Unlock()
	if hasStale {
		t.Fatal("建票应先清扫孤儿")
	}
}

func TestNetworkErrorDeliversMaxRetries(t *testing.T) {
	// 上游連線異常必須走到報錯路徑，不得因日誌呼叫炸掉 ticket。
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "async-acc")

	// 上游接受连线后立即断开：client.Do 返回网络错误
	up := &scriptedUpstream{specs: []upstreamSpec{{reset: true}}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-neterr", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-neterr")

	events := drainEvents(tk)
	last := events[len(events)-1]
	if last.Type != "error" {
		t.Fatalf("末事件应为 error: %v", events)
	}
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "max_retries" {
		t.Fatalf("错误类型应为 max_retries: %v", last.Data)
	}
	if up.callCount() != 1 {
		t.Fatalf("ASYNC_MAX_RETRIES=0 应只尝试一次: %d", up.callCount())
	}
}

func TestMidStreamFailureTerminatesWithoutRetry(t *testing.T) {
	// 已發出 chunk 後流中斷：終止票務並回 error 事件，不得重發。
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "midstream")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, lines: []string{`data: {"id":"msg1"}`}, abort: true},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-midstream", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-midstream")

	events := drainEvents(tk)
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "ready,chunk,error" {
		t.Fatalf("事件序列不符: %v", events)
	}
	if chunk, ok := events[1].Data.(map[string]any); !ok || chunk["id"] != "msg1" {
		t.Fatalf("chunk 内容不符: %v", events[1].Data)
	}
	errObj, _ := events[2].Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "upstream_stream_interrupted" {
		t.Fatalf("错误类型应为 upstream_stream_interrupted: %v", events[2].Data)
	}
	if up.callCount() != 1 {
		t.Fatalf("流中断后不得换号重发: %d", up.callCount())
	}
}

func TestFailureBeforeFirstChunkStillRetries(t *testing.T) {
	// 一個 chunk 都沒發出時中斷：仍按網絡錯誤走換號重試路徑。
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "prestream")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, lines: []string{}, abort: true},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-prestream", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-prestream")

	events := drainEvents(tk)
	last := events[len(events)-1]
	if last.Type != "error" {
		t.Fatalf("末事件应为 error: %v", events)
	}
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "max_retries" {
		t.Fatalf("错误类型应为 max_retries: %v", last.Data)
	}
	if up.callCount() != 1 {
		t.Fatalf("ASYNC_MAX_RETRIES=0 应只尝试一次: %d", up.callCount())
	}
}

func TestUpstreamErrorDeliveredAsEvent(t *testing.T) {
	// 非 200 且非验证码/限流错误：错误体原样投递并终止，不得换号重试。
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "upstream-err")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusInternalServerError, body: `{"code":9999,"msg":"boom"}`},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-upstream", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-upstream")

	events := drainEvents(tk)
	last := events[len(events)-1]
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "upstream_error" {
		t.Fatalf("错误类型应为 upstream_error: %v", events)
	}
	if !strings.Contains(fmt.Sprint(errObj["message"]), "boom") {
		t.Fatalf("错误体应原样透传: %v", errObj)
	}
	if up.callCount() != 1 {
		t.Fatalf("已投递错误后不得重试: %d", up.callCount())
	}
}

func TestRateLimitMarksCoolingAndRetries(t *testing.T) {
	// 429：账号进入冷却，换号重试；无更多账号时报 max_retries。
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "cooling-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusTooManyRequests, body: `{"code":1302,"msg":"rate limited"}`},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-429", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-429")

	events := drainEvents(tk)
	last := events[len(events)-1]
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "max_retries" {
		t.Fatalf("重试耗尽应报 max_retries: %v", events)
	}
	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status != model.StatusCooling {
		t.Fatalf("429 应进入冷却: %s", acc.Status)
	}
	if acc.LastError == nil || !strings.Contains(*acc.LastError, "HTTP 429") {
		t.Fatalf("last_error 应记录 429: %v", acc.LastError)
	}
}

// TestAccountProxyIsUsed 账号配置的 proxy_url 必须作用于 async 路径。
//
// README 与 PLAN §5.9 都承诺「该账号的网关请求、额度查询与套餐领取均走对应
// 代理」。async 池曾忽略 proxy_url 直接出站：配置代理的账号在这条路径上以
// 服务器真实 IP 连上游，正是使用者配置代理要规避的（IP 绑定、地区限制、风控）。
// TestAccountProxyIsUsed 账号配置的 proxy_url 必须作用于 async 路径。
//
// README 与 PLAN §5.9 都承诺「该账号的网关请求、额度查询与套餐领取均走对应
// 代理」。async 池曾忽略 proxy_url 直接出站：配置代理的账号在这条路径上以
// 服务器真实 IP 连上游，正是使用者配置代理要规避的（IP 绑定、地区限制、风控）。
func TestAccountProxyIsUsed(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	acc := addJWTAccount(t, st, "proxied")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, contentType: "text/event-stream", lines: []string{
			`data: {"type":"message_delta","usage":{"output_tokens":1}}`,
		}},
	}}
	config.UpstreamZai = up.start(t).URL

	// 最小 CONNECT 代理：记录被请求的目标，再把连接原样转发到真实上游。
	// 用裸 TCP listener 而非 httptest，因为 CONNECT 需要接管连接（Hijack）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var mu sync.Mutex
	var connects []string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}

				// 明文 http 目标走绝对 URI（Proxy 字段的标准行为），
				// https 目标才走 CONNECT；两种都记为该代理被使用。
				target := req.Host
				if target == "" {
					target = req.URL.Host
				}
				mu.Lock()
				connects = append(connects, target)
				mu.Unlock()

				// 把请求原样转发到真实上游并回传响应
				outReq := req.Clone(context.Background())
				outReq.RequestURI = ""
				if outReq.URL.Host == "" {
					outReq.URL.Host = target
				}
				resp, err := http.DefaultTransport.RoundTrip(outReq)
				if err != nil {
					return
				}
				defer resp.Body.Close()
				_ = resp.Write(c)
			}(conn)
		}
	}()

	st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		proxyURL := "http://" + ln.Addr().String()
		a.ProxyURL = &proxyURL
	})

	tk := insertTicket(p, "ticket-proxy", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-proxy")
	_ = drainEvents(tk)

	mu.Lock()
	gotConnects := len(connects)
	mu.Unlock()
	if gotConnects == 0 {
		t.Fatalf("账号配置了代理，请求却未经代理出站（上游调用=%d）", up.callCount())
	}
	if up.callCount() == 0 {
		t.Fatal("上游应收到请求")
	}
}

func TestSSEJSONEscapesNonASCII(t *testing.T) {
	// sseJSON 对齐 Python json.dumps 默认 ensure_ascii=True。
	got := sseJSON(map[string]any{"id": "中文", "n": float64(3)})
	if !strings.Contains(got, `\u4e2d\u6587`) {
		t.Fatalf("非 ASCII 应转义为小写 \\u 序列: %s", got)
	}
	if strings.Contains(got, "中") {
		t.Fatalf("不应包含原始字符: %s", got)
	}
	// ASCII 保持原样、HTML 字符不转义（对齐 Python 行为）
	got = sseJSON(map[string]any{"s": "<a>&</a>"})
	if !strings.Contains(got, "<a>&</a>") {
		t.Fatalf("HTML 字符不应转义: %s", got)
	}
}

// async 路径必须与 engine 用同一套分类：429 的额度上限码族标「该模型耗尽」，
// 而非一律标 cooling（曾因两条路径各自实现而分歧）。
// TestJSONBusinessErrorIsNotTreatedAsStream 上游用 HTTP 200 包装业务错误时，
// 不得当成成功串流交付。
//
// ZCode 有时在 200 里回 {"code":1005,...}（每日额度用完）。引擎有专门的
// content-type 分支处理它；async 曾直接 forwardSSE，于是客户端收到
// ready→done 的「成功」串流但零 chunk，账号也不被标状态——额度耗尽的账号
// 会一直留在轮询池里被反复选中、反复白耗上游请求。
func TestJSONBusinessErrorIsNotTreatedAsStream(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "daily-exhausted")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, contentType: "application/json",
			body: `{"code":1005,"msg":"exceed quota limit"}`},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-1005", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-1005")
	events := drainEvents(tk)

	// 不得出现 chunk 或 done：这不是一次成功交付
	for _, ev := range events {
		if ev.Type == "chunk" || ev.Type == "done" {
			t.Fatalf("业务错误不应交付为成功串流: %+v", events)
		}
	}
	// 账号必须被标记该模型耗尽（与 engine 一致），否则会被反复选中
	acc := st.ListAccounts(model.ProviderZai)[0]
	if !containsStr(acc.ExhaustedModels, "glm-5.3") {
		t.Fatalf("应标记模型耗尽: status=%s exhausted=%v", acc.Status, acc.ExhaustedModels)
	}
}

// TestJSONNonZeroCodeDeliveredAsError 其余业务码应作为 error 事件投递，
// 且携带上游 msg，而不是静默变成「成功但零 chunk」。
func TestJSONNonZeroCodeDeliveredAsError(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "biz-err")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, contentType: "application/json",
			body: `{"code":1234,"msg":"something went wrong"}`},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-biz", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-biz")
	events := drainEvents(tk)

	last := events[len(events)-1]
	if last.Type != "error" {
		t.Fatalf("业务错误应投递 error 事件: %+v", events)
	}
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "upstream_error" {
		t.Fatalf("错误类型应为 upstream_error: %v", last.Data)
	}
	if msg, _ := errObj["message"].(string); msg != "something went wrong" {
		t.Fatalf("应取上游 msg 字段: %v", errObj["message"])
	}
}

func TestQuotaExhaustedCodeMarksModelNotCooling(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "quota-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusTooManyRequests, body: `{"code":1310,"msg":"weekly limit"}`},
	}}
	config.UpstreamZai = up.start(t).URL

	insertTicket(p, "ticket-quota", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-quota")

	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status == model.StatusCooling {
		t.Fatalf("额度上限码族不应标 cooling（应与 engine 一致）: %s", acc.Status)
	}
	if !containsStr(acc.ExhaustedModels, "glm-5.3") {
		t.Fatalf("应标记该模型耗尽（正規化為小寫）: %v", acc.ExhaustedModels)
	}
}

// 401 应标 invalid（账号失效），不得落入冷却分支。
func TestUnauthorizedMarksInvalid(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "bad-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusUnauthorized, body: ""},
	}}
	config.UpstreamZai = up.start(t).URL

	insertTicket(p, "ticket-401", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-401")

	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status != model.StatusInvalid {
		t.Fatalf("401 应标 invalid: %s", acc.Status)
	}
}

// 3010 并发准入限制：账号仍可用，不得标 cooling 或 invalid。
func TestConcurrencyLimitKeepsAccountState(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "busy-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusTooManyRequests, body: `{"code":3010,"msg":"model admission concurrency limit"}`},
	}}
	config.UpstreamZai = up.start(t).URL

	insertTicket(p, "ticket-3010", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-3010")

	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status != model.StatusActive {
		t.Fatalf("3010 不应改变账号状态（当前 %s）", acc.Status)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
