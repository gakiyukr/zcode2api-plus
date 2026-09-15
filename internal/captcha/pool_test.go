package captcha

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── 假 worker / 假工厂 ────────────────────────────────────────────────────────
// 失败注入测试基建：不触网、不启动真实浏览器，脚本化编排求解行为，
// 语义对齐 Python tests/test_captcha_browser.py 的假 worker 用例。

// stepFunc 一次求解的行为脚本；ctx 为池派发的求解上下文。
type stepFunc func(ctx context.Context) (string, error)

// tokenStep 固定 token 的成功步骤。
func tokenStep(tok string) stepFunc {
	return func(context.Context) (string, error) { return tok, nil }
}

// errStep 普通错误：求解失败，worker 仍健康（不触发替换）。
func errStep(msg string) stepFunc {
	return func(context.Context) (string, error) { return "", errors.New(msg) }
}

// deadStep 致命错误：浏览器已退出，槽位必须替换。
func deadStep() stepFunc {
	return func(context.Context) (string, error) {
		return "", fmt.Errorf("%w: 浏览器进程崩溃", ErrWorkerDead)
	}
}

// hangStep 忽略 ctx 睡眠 d 后返回 token：模拟不可取消的卡死求解，
// 结果必然迟到（池已超时判死）。
func hangStep(d time.Duration, tok string) stepFunc {
	return func(context.Context) (string, error) {
		time.Sleep(d)
		return tok, nil
	}
}

// slowStep 可被 ctx 取消的睡眠后返回 token：模拟正常的慢求解。
func slowStep(d time.Duration, tok string) stepFunc {
	return func(ctx context.Context) (string, error) {
		sleepCtx(ctx, d)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return tok, nil
	}
}

// scriptWorker 按调用序次执行 steps 的假求解 worker；脚本耗尽后返回默认成功
// token（格式 token-w<worker 序号>-<调用序号>，可同时区分 worker 与调用次序）。
type scriptWorker struct {
	id     int
	mu     sync.Mutex
	steps  []stepFunc
	n      int
	closed atomic.Int32
}

func (w *scriptWorker) Solve(ctx context.Context) (string, error) {
	w.mu.Lock()
	i := w.n
	w.n++
	var step stepFunc
	if i < len(w.steps) {
		step = w.steps[i]
	}
	w.mu.Unlock()
	if step != nil {
		return step(ctx)
	}
	return fmt.Sprintf("token-w%d-%d", w.id, i), nil
}

func (w *scriptWorker) Close() error {
	w.closed.Add(1)
	return nil
}

// scriptFactory 假 worker 工厂：第 n 个 worker 的脚本由 make 决定（nil = 全部
// 默认成功）；make 返回错误即创建失败，等价 Python worker 未能进入 READY。
type scriptFactory struct {
	mu      sync.Mutex
	next    int
	workers []*scriptWorker
	make    func(n int) ([]stepFunc, error)
}

func (f *scriptFactory) create(ctx context.Context) (Worker, error) {
	f.mu.Lock()
	n := f.next
	f.next++
	make := f.make
	f.mu.Unlock()

	if make != nil {
		steps, err := make(n)
		if err != nil {
			return nil, err
		}
		return f.spawn(n, steps), nil
	}
	return f.spawn(n, nil), nil
}

func (f *scriptFactory) spawn(n int, steps []stepFunc) *scriptWorker {
	w := &scriptWorker{id: n, steps: steps}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workers = append(f.workers, w)
	return w
}

// spawned 已创建的 worker 数。
func (f *scriptFactory) spawned() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.workers)
}

// worker 第 n 个 worker；尚未创建返回 nil。
func (f *scriptFactory) worker(n int) *scriptWorker {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < len(f.workers) {
		return f.workers[n]
	}
	return nil
}

// waitFor 轮询等待条件成立（异步关停等路径没有完成通知，只能轮询）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// newTestPool 小超时参数的池（真实 config 默认值是分钟级，测试必须缩小）。
func newTestPool(t *testing.T, f WorkerFactory, size int) *Pool {
	t.Helper()
	p := NewPool(f, size)
	p.startupTimeout = 2 * time.Second
	p.requestTimeout = 2 * time.Second
	p.queueTimeout = 2 * time.Second
	p.shutdownTimeout = 2 * time.Second
	p.restartDelay = 5 * time.Millisecond
	p.closeWatchdog = 2 * time.Second
	t.Cleanup(p.Stop)
	return p
}

// ── 基本往返 ─────────────────────────────────────────────────────────────────

func TestPoolSolveRoundtrip(t *testing.T) {
	f := &scriptFactory{}
	p := newTestPool(t, f.create, 2)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		tok, err := p.Solve(ctx)
		if err != nil {
			t.Fatalf("第 %d 次求解失败: %v", i, err)
		}
		// 轮询顺序 w0→w1→w0；worker 内部调用计数随求解递增
		if tok != fmt.Sprintf("token-w%d-%d", i%2, i/2) {
			t.Fatalf("token 应来自对应 worker: %q", tok)
		}
	}
	st := p.StatsSnapshot()
	if st.Started != 2 || st.Requests != 3 {
		t.Fatalf("统计不符: %+v", st)
	}
}

// ── 启动失败 ─────────────────────────────────────────────────────────────────

func TestPoolStartupFactoryError(t *testing.T) {
	f := &scriptFactory{make: func(int) ([]stepFunc, error) {
		return nil, errors.New("浏览器二进制缺失")
	}}
	p := newTestPool(t, f.create, 1)
	err := p.Start()
	if !errors.Is(err, ErrStartup) || !strings.Contains(err.Error(), "浏览器二进制缺失") {
		t.Fatalf("应返回启动失败并带原因: %v", err)
	}
	if f.spawned() != 0 {
		t.Fatalf("创建失败的 worker 不应计入: %d", f.spawned())
	}
	// 失败清场后池不可用
	if _, err := p.Solve(context.Background()); !errors.Is(err, ErrPoolNotStarted) {
		t.Fatalf("应返回池未启动: %v", err)
	}
}

func TestPoolStartupTimeout(t *testing.T) {
	// 工厂阻塞至启动超时：等价 Python worker 永远到不了 READY
	p := newTestPool(t, func(ctx context.Context) (Worker, error) {
		<-ctx.Done() // 工厂上下文带 startupTimeout
		return nil, ctx.Err()
	}, 1)
	p.startupTimeout = 120 * time.Millisecond
	err := p.Start()
	if !errors.Is(err, ErrStartup) {
		t.Fatalf("应返回启动失败: %v", err)
	}
	if _, serr := p.Solve(context.Background()); !errors.Is(serr, ErrPoolNotStarted) {
		t.Fatalf("超时清场后应不可用: %v", serr)
	}
}

// ── 运行期失败分类 ───────────────────────────────────────────────────────────

func TestPoolWorkerDeadReplaced(t *testing.T) {
	f := &scriptFactory{make: func(n int) ([]stepFunc, error) {
		if n == 0 {
			return []stepFunc{deadStep()}, nil
		}
		return nil, nil
	}}
	p := newTestPool(t, f.create, 1)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := p.Solve(ctx); !errors.Is(err, ErrWorkerDead) {
		t.Fatalf("应返回 worker 已退出: %v", err)
	}
	// 第二次求解阻塞等待新 worker 就绪后成功
	tok, err := p.Solve(ctx)
	if err != nil || tok != "token-w1-0" {
		t.Fatalf("替换后应成功: %q %v", tok, err)
	}
	st := p.StatsSnapshot()
	if st.WorkerRestarts < 1 {
		t.Fatalf("应记录 worker 替换: %+v", st)
	}
	// 关停是异步的：轮询等待死 worker 被 Close
	waitFor(t, 2*time.Second, func() bool {
		return f.worker(0) != nil && f.worker(0).closed.Load() == 1
	}, "死亡的 worker 应被 Close")
}

func TestPoolSolverFailureKeepsWorker(t *testing.T) {
	f := &scriptFactory{make: func(int) ([]stepFunc, error) {
		// 两次调用都返回求解失败（worker 健康、保持复用）
		return []stepFunc{errStep("captcha 上游拒绝"), errStep("captcha 上游拒绝")}, nil
	}}
	p := newTestPool(t, f.create, 1)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, err := p.Solve(ctx)
		if !errors.Is(err, ErrSolverFailure) || !strings.Contains(err.Error(), "上游拒绝") {
			t.Fatalf("第 %d 次应返回求解失败并保留原因: %v", i, err)
		}
	}
	st := p.StatsSnapshot()
	if st.SolverFailures != 2 || st.WorkerRestarts != 0 {
		t.Fatalf("求解失败不应替换 worker: %+v", st)
	}
	if w := f.worker(0); w == nil || w.closed.Load() != 0 {
		t.Fatal("健康的 worker 不应被关闭")
	}
	// 脚本耗尽后同一 worker 继续服务（默认成功）
	tok, err := p.Solve(ctx)
	if err != nil || tok != "token-w0-2" {
		t.Fatalf("worker 应保持复用: %q %v", tok, err)
	}
}

func TestPoolRequestTimeoutReplacesWorker(t *testing.T) {
	f := &scriptFactory{make: func(n int) ([]stepFunc, error) {
		if n == 0 {
			return []stepFunc{hangStep(400*time.Millisecond, "late-token")}, nil
		}
		return nil, nil
	}}
	p := newTestPool(t, f.create, 1)
	p.requestTimeout = 80 * time.Millisecond
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := p.Solve(ctx); !errors.Is(err, ErrSolveTimeout) {
		t.Fatalf("应返回求解超时: %v", err)
	}
	// 迟到的结果绝不误投：第二次求解拿到的是新 worker 的新 token
	tok, err := p.Solve(ctx)
	if err != nil || tok == "late-token" {
		t.Fatalf("迟到的 token 不应被投递: %q %v", tok, err)
	}
	st := p.StatsSnapshot()
	if st.Timeouts != 1 || st.WorkerRestarts < 1 || st.DiscardedResults != 1 {
		t.Fatalf("统计不符: %+v", st)
	}
	waitFor(t, 2*time.Second, func() bool {
		w := f.worker(0)
		return w != nil && w.closed.Load() == 1
	}, "超时判死的 worker 应被 Close")
}

func TestPoolCallerCancelKeepsWorker(t *testing.T) {
	// 调用方取消不判死槽位（对齐 Python）：在途结果被丢弃，worker 继续复用
	f := &scriptFactory{make: func(n int) ([]stepFunc, error) {
		if n == 0 {
			return []stepFunc{hangStep(250*time.Millisecond, "late-token")}, nil
		}
		return nil, nil
	}}
	p := newTestPool(t, f.create, 1)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, err := p.Solve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回调用方取消: %v", err)
	}
	// 同一 worker 继续服务；迟到的结果只被丢弃计数
	tok, err := p.Solve(context.Background())
	if err != nil || tok != "token-w0-1" {
		t.Fatalf("worker 应保持健康并复用: %q %v", tok, err)
	}
	st := p.StatsSnapshot()
	if st.WorkerRestarts != 0 || st.DiscardedResults != 1 {
		t.Fatalf("统计不符: %+v", st)
	}
	if w := f.worker(0); w.closed.Load() != 0 {
		t.Fatal("调用方取消不应关闭 worker")
	}
}

// ── 并发与排队 ───────────────────────────────────────────────────────────────

func TestPoolSerialConcurrency(t *testing.T) {
	// size=1：请求在同一 worker 上严格串行（对齐 Python 单 worker 池）
	f := &scriptFactory{make: func(int) ([]stepFunc, error) {
		// 每次求解都耗时 100ms：脚本须覆盖全部 3 次调用
		return []stepFunc{
			slowStep(100*time.Millisecond, "tok-a"),
			slowStep(100*time.Millisecond, "tok-b"),
			slowStep(100*time.Millisecond, "tok-c"),
		}, nil
	}}
	p := newTestPool(t, f.create, 1)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	tokens := map[string]bool{}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := p.Solve(context.Background())
			if err != nil {
				t.Errorf("并发求解失败: %v", err)
				return
			}
			mu.Lock()
			tokens[tok] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	// 3 次 × 100ms 串行 ≈ 300ms；留调度抖动余量取 250ms 下限
	if elapsed < 250*time.Millisecond {
		t.Fatalf("请求应串行执行，实际耗时 %s", elapsed)
	}
	if len(tokens) != 3 {
		t.Fatalf("token 应互不相同: %v", tokens)
	}
}

func TestPoolQueueTimeout(t *testing.T) {
	// 全池忙时排队超过 queue_timeout 即快速失败；占用中的求解须正常完成
	//（求解时长须严格小于 request_timeout，避免与超时判死竞态）
	f := &scriptFactory{make: func(int) ([]stepFunc, error) {
		return []stepFunc{slowStep(200*time.Millisecond, "tok")}, nil
	}}
	p := newTestPool(t, f.create, 1)
	p.requestTimeout = 300 * time.Millisecond
	p.queueTimeout = 100 * time.Millisecond
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}

	occupyDone := make(chan error, 1)
	go func() {
		_, err := p.Solve(context.Background())
		occupyDone <- err
	}()
	// 等第一个请求完成派发（stats 由派发路径计数）
	waitFor(t, 2*time.Second, func() bool {
		return p.StatsSnapshot().Requests == 1
	}, "第一个请求应已派发")

	if _, err := p.Solve(context.Background()); !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("应返回排队超时: %v", err)
	}
	if err := <-occupyDone; err != nil {
		t.Fatalf("占用中的求解不应失败: %v", err)
	}
}

// ── 生命周期 ─────────────────────────────────────────────────────────────────

func TestPoolSolveNotStartedAndAfterStop(t *testing.T) {
	f := &scriptFactory{}
	p := newTestPool(t, f.create, 1)
	if _, err := p.Solve(context.Background()); !errors.Is(err, ErrPoolNotStarted) {
		t.Fatalf("启动前应返回池未启动: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	p.Stop()
	if _, err := p.Solve(context.Background()); !errors.Is(err, ErrPoolNotStarted) {
		t.Fatalf("关闭后应返回池未启动: %v", err)
	}
}

func TestPoolStopIdempotentAndRestart(t *testing.T) {
	f := &scriptFactory{}
	p := newTestPool(t, f.create, 1)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("重复 Start 应为无害空操作: %v", err)
	}
	p.Stop()
	p.Stop() // 幂等
	if _, err := p.Solve(context.Background()); !errors.Is(err, ErrPoolNotStarted) {
		t.Fatalf("关闭后应不可用: %v", err)
	}
	// 关闭后可再次启动（对齐 Python restart_after_stop）
	if err := p.Start(); err != nil {
		t.Fatalf("重启失败: %v", err)
	}
	if _, err := p.Solve(context.Background()); err != nil {
		t.Fatalf("重启后应可求解: %v", err)
	}
}

func TestPoolStopWaitsInflight(t *testing.T) {
	// 用 channel 明確等待 worker 真正開始求解：
	// StatsSnapshot().Requests 在派發時就自增，此時 worker 未必已進入求解，
	// Stop 可能搶在開始前走「快速放棄」路徑，導致間歇性失敗。
	started := make(chan struct{})
	f := &scriptFactory{make: func(int) ([]stepFunc, error) {
		return []stepFunc{func(ctx context.Context) (string, error) {
			close(started)
			sleepCtx(ctx, 200*time.Millisecond)
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "inflight-token", nil
		}}, nil
	}}
	p := newTestPool(t, f.create, 1)
	p.shutdownTimeout = 5 * time.Second
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}

	type solveResult struct {
		tok string
		err error
	}
	solveDone := make(chan solveResult, 1)
	go func() {
		tok, err := p.Solve(context.Background())
		solveDone <- solveResult{tok, err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker 未开始求解")
	}

	p.Stop() // 应等待在途求解完成
	select {
	case res := <-solveDone:
		if res.err != nil {
			t.Fatalf("在途求解不应被中断: %v", res.err)
		}
		if res.tok != "inflight-token" {
			t.Fatalf("在途结果应正常投递: %q", res.tok)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 应等待在途求解完成")
	}
}

func TestPoolStopKillsStragglers(t *testing.T) {
	// 滞留超过 shutdown_timeout：Stop 放弃等待并快速失败在途请求
	f := &scriptFactory{make: func(int) ([]stepFunc, error) {
		return []stepFunc{slowStep(8*time.Second, "never")}, nil
	}}
	p := newTestPool(t, f.create, 1)
	p.requestTimeout = 10 * time.Second
	p.shutdownTimeout = 120 * time.Millisecond
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}

	solveDone := make(chan error, 1)
	go func() {
		_, err := p.Solve(context.Background())
		solveDone <- err
	}()
	waitFor(t, 2*time.Second, func() bool {
		return p.StatsSnapshot().Requests == 1
	}, "在途请求应已派发")

	begin := time.Now()
	p.Stop()
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("Stop 应快速放弃滞留槽位，实际 %s", elapsed)
	}
	select {
	case err := <-solveDone:
		if !errors.Is(err, ErrPoolClosed) {
			t.Fatalf("滞留请求应快速失败: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("滞留请求未被快速失败")
	}
}

// ── 日志脱敏 ─────────────────────────────────────────────────────────────────

func TestRedact(t *testing.T) {
	long := strings.Repeat("A", 80)
	// verify_param= 全部落在字符集内，会与长串连成一个 ≥64 匹配段被整体抹除
	// （与 Python _redact 同正则，行为一致）。
	if got := redact("failed verify_param=" + long); got != "failed [redacted]" {
		t.Fatalf("长串应被抹除: %q", got)
	}
	if got := redact("failed key: " + long); got != "failed key: [redacted]" {
		t.Fatalf("带分隔符的长串应被抹除: %q", got)
	}
	// base64/URL 安全字符集（含 =、-、_）都视为疑似 token
	padded := strings.Repeat("e", 60) + "==_-"
	if got := redact("token " + padded); !strings.Contains(got, "[redacted]") {
		t.Fatalf("疑似 token 应被抹除: %q", got)
	}
	if got := redact("line1\n  line2\t\ttab"); got != "line1 line2 tab" {
		t.Fatalf("空白应折叠: %q", got)
	}
	if got := redact("  short string  "); got != "short string" {
		t.Fatalf("短文本应原样保留（去首尾空白）: %q", got)
	}
	if got := redact(strings.Repeat("a", 63)); got != strings.Repeat("a", 63) {
		t.Fatalf("63 字符未达阈值应保留: %q", got)
	}
}
