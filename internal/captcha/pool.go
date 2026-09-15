// 有界浏览器 worker 槽位池：对应 Python 版 app/captcha_browser.py 的池职责。
//
// 与 Python 版的差异：Python 每个槽位是一个 cloakbrowser worker 子进程，经
// READY/GET_TOKEN/TOKEN/FAILED/SHUTDOWN 行协议通信；Go 版 worker 是进程内
// goroutine 持有的 rod 浏览器会话，没有行协议，池只保留其调度与生命周期语义：
//   - 有界并发：size 即并发求解上限，同一 worker 上请求严格串行。
//   - 启动：全部槽位在启动总超时内就绪，任一失败或超时整体失败并清场。
//   - 超时隔离：请求超时即判死该槽位——放弃在途结果并丢弃 worker 重建；
//     迟到的求解结果只会被丢弃，绝不投递给后续请求（杜绝响应错位）。
//   - 失败分类：worker 明确报错视为求解失败（worker 仍健康，复用页面）；
//     ErrWorkerDead（浏览器退出/崩溃）触发替换。
//
// 重试与缓存属于 Manager（调用方）职责，池不代劳。
package captcha

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/web"
)

// Worker 一次可求解的浏览器会话（真实实现封装 rod 浏览器与已加载 SDK 的页面）。
type Worker interface {
	// Solve 生成一次 verify_param；实现须尊重 ctx 取消（超时/关闭时中止求解）。
	// 返回错误须用 errors.Is 区分 ErrWorkerDead（致命，槽位必须替换）。
	Solve(ctx context.Context) (string, error)
	// Close 释放底层资源（浏览器进程）；可能阻塞，调用方以看门狗超时兜底。
	Close() error
}

// WorkerFactory 创建一个就绪 worker（浏览器已启动、求解页面已加载，等价 READY）。
// 返回错误等价 Python worker 未能进入 READY。
type WorkerFactory func(ctx context.Context) (Worker, error)

// 池相关错误语义（对齐 Python 版的 CaptchaBrowserError 子类）。
var (
	// ErrPoolNotStarted 池尚未启动（或已关闭后再次调用）。
	ErrPoolNotStarted = errors.New("池未启动：请先 Start()")
	// ErrPoolClosed 池已关闭，在途请求被快速失败。
	ErrPoolClosed = errors.New("池已关闭")
	// ErrStartup 启动失败：worker 未就绪或启动总超时。
	ErrStartup = errors.New("浏览器池启动失败")
	// ErrSolveTimeout 单次求解超时；对应槽位已被判死替换。
	ErrSolveTimeout = errors.New("求解超时")
	// ErrSolverFailure worker 明确回报求解失败（求解被上游拒绝等），worker 本身仍健康。
	ErrSolverFailure = errors.New("求解失败")
	// ErrWorkerDead worker 底层浏览器已退出（崩溃/被杀），槽位必须替换。
	// 真实 worker 与测试假 worker 均以 errors.Is 上报此语义。
	ErrWorkerDead = errors.New("worker 已退出")
	// ErrQueueTimeout 等待空闲槽位超时（全池忙）。
	ErrQueueTimeout = errors.New("等待空闲 worker 超时")
)

// redactPattern 匹配疑似 token/密钥的长串（≥64 字符）。
var redactPattern = regexp.MustCompile(`[A-Za-z0-9+/=_-]{64,}`)

// whitespacePattern 折叠日志文本中的空白。
var whitespacePattern = regexp.MustCompile(`\s+`)

// redact 脱敏：抹掉疑似 token 的长串并折叠空白（对齐 Python _redact），
// 避免把验证码参数写入日志。
func redact(text string) string {
	text = redactPattern.ReplaceAllString(text, "[redacted]")
	return strings.TrimSpace(whitespacePattern.ReplaceAllString(text, " "))
}

// Stats 池运行统计（字段语义对齐 Python BrowserWorkerPool.stats）。
type Stats struct {
	Started          int   // 本次启动的槽位数
	Requests         int64 // 已派发的求解请求数
	Timeouts         int64 // 请求超时次数
	SolverFailures   int64 // worker 明确回报的求解失败次数
	WorkerRestarts   int64 // worker 替换次数（致命退出或超时判死）
	DiscardedResults int64 // 被丢弃的迟到/已放弃结果（Python: discarded_lines）
}

// solveResult 一次求解的投递物。
type solveResult struct {
	param string
	err   error
}

// solveReq 一次求解请求；done 容量 1，只投递一次，永不阻塞槽位。
type solveReq struct {
	ctx  context.Context
	done chan solveResult
	// abandoned 调用方已放弃（超时判死/取消/池关闭）：结果投递改为丢弃计数。
	abandoned atomic.Bool
	// condemned 请求超时判死标记：槽位完成本次 Solve 后必须替换 worker。
	condemned atomic.Bool
}

// slot 单个槽位：一个 worker 会话与它的请求入口。
// reqCh 无缓冲：空闲槽位必然阻塞在接收上，派发即完成交接。
type slot struct {
	id    int
	reqCh chan *solveReq
}

// generation 一次 Start→Stop 生命周期的共享句柄。槽位协程只操作自己这一代，
// 防止上一代残留协程把陈旧 worker 投进新一代的空闲队列。
type generation struct {
	done chan struct{}
	// settle 关闭 = 关闭流程已结束（在途求解完成或 shutdown 超时）。
	// 等待结果的 Solve 据此决定放弃；与 done 分离是为了让 Stop 先等在途
	// 求解完成，而不是立刻打断它们（对齐 Python shutdown 等待语义）。
	settle chan struct{}
	idle   chan *slot
	wg     sync.WaitGroup
}

// Pool 有界 worker 槽位池。零值不可用，经 NewPool 构造。
type Pool struct {
	factory WorkerFactory
	size    int

	// 超时语义对齐 Python BrowserWorkerPool 的同名参数。
	startupTimeout  time.Duration
	requestTimeout  time.Duration
	queueTimeout    time.Duration
	shutdownTimeout time.Duration
	restartDelay    time.Duration
	// closeWatchdog 放弃等待单个 worker.Close 的上限（Python 直接 kill 进程，
	// Go 关闭浏览器可能阻塞，用看门狗兜底）。
	closeWatchdog time.Duration

	mu        sync.Mutex
	cur       *generation // 当前代；nil 表示未启动或已关闭
	started   bool
	startErr  error // 启动阶段首个失败
	readyNum  int
	stats     Stats
	now       func() time.Time
	closeOnce bool // 防止重复 close(done)
}

// NewPool 创建池；size < 1 或 factory 为 nil 属编程错误，直接 panic
// （对齐 Python 的 ValueError）。超时参数默认取 config 的验证码池配置。
func NewPool(factory WorkerFactory, size int) *Pool {
	if factory == nil {
		panic("captcha: factory 不能为空")
	}
	if size < 1 {
		panic("captcha: size 必须 >= 1")
	}
	return &Pool{
		factory:         factory,
		size:            size,
		startupTimeout:  time.Duration(config.CaptchaBrowserStartupTimeout) * time.Second,
		requestTimeout:  time.Duration(config.CaptchaBrowserRequestTimeout) * time.Second,
		queueTimeout:    time.Duration(config.CaptchaBrowserQueueTimeout) * time.Second,
		shutdownTimeout: time.Duration(config.CaptchaBrowserShutdownTimeout) * time.Second,
		restartDelay:    time.Second,
		closeWatchdog:   5 * time.Second,
		now:             time.Now,
	}
}

// SetNow 注入时钟（测试用）。
func (p *Pool) SetNow(fn func() time.Time) { p.now = fn }

// IsStarted 报告池是否处于可用状态。
func (p *Pool) IsStarted() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started
}

// StatsSnapshot 返回统计快照（测试与观测用）。
func (p *Pool) StatsSnapshot() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats
}

// Start 启动全部槽位，并在启动总超时内等待全部就绪；重复调用为无害空操作
// （对齐 Python：started 或 loops 存在时直接返回）。
func (p *Pool) Start() error {
	p.mu.Lock()
	if p.started || p.cur != nil {
		p.mu.Unlock()
		return nil
	}
	p.stoppedResetLocked()
	g := &generation{
		done:   make(chan struct{}),
		settle: make(chan struct{}),
		idle:   make(chan *slot, p.size),
	}
	p.cur = g
	p.mu.Unlock()

	for i := 0; i < p.size; i++ {
		g.wg.Add(1)
		go p.slotLoop(g, i)
	}

	deadline := p.now().Add(p.startupTimeout)
	for {
		p.mu.Lock()
		if p.cur != g {
			// 启动期间被 Stop（或上一轮失败清场）：按启动失败收场
			p.mu.Unlock()
			return fmt.Errorf("%w：池在启动期间被关闭", ErrStartup)
		}
		if p.startErr != nil {
			err := fmt.Errorf("%w: %s", ErrStartup, p.startErr)
			p.mu.Unlock()
			p.teardown()
			return err
		}
		if p.readyNum >= p.size {
			p.started = true
			p.stats.Started = p.size
			p.mu.Unlock()
			web.Ok("captcha_browser", fmt.Sprintf("池已启动：%d 个 worker 全部就绪", p.size))
			return nil
		}
		p.mu.Unlock()

		if !p.now().Before(deadline) {
			p.teardown()
			return fmt.Errorf("%w：%d 个 worker 未在 %s 内全部就绪",
				ErrStartup, p.size, p.startupTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Stop 优雅关闭：等待在途求解完成（至多 shutdownTimeout），随后强制清场。
// 幂等；关闭后可再次 Start（对齐 Python restart_after_stop）。
func (p *Pool) Stop() {
	if !p.teardown() {
		return
	}
	web.Ok("captcha_browser", "池已关闭")
}

// teardown 关闭当前代并等待槽位协程退出；返回 false 表示没有可关闭的代。
// 语义：close(done) 拒绝新请求；随后至多等 shutdownTimeout 让在途求解
// 完成，再 close(settle) 放弃仍在等待结果的调用方。
func (p *Pool) teardown() bool {
	p.mu.Lock()
	if p.cur == nil || p.closeOnce {
		p.mu.Unlock()
		return false
	}
	g := p.cur
	p.closeOnce = true
	p.started = false
	close(g.done)
	p.mu.Unlock()

	waitCh := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
	case <-time.After(p.shutdownTimeout):
		// 槽位仍阻塞在求解或 worker 关闭中：放弃等待，残留协程随 done 退出
	}
	close(g.settle)

	p.mu.Lock()
	p.cur = nil
	p.closeOnce = false
	p.mu.Unlock()
	return true
}

// stoppedResetLocked 重置运行状态（Start 与 Stop 共用的复位点；调用方持锁）。
func (p *Pool) stoppedResetLocked() {
	p.started = false
	p.startErr = nil
	p.readyNum = 0
	p.stats.Started = 0
}

// Solve 取一个验证码 token（内存，不落盘）。并发上限 = size；
// 错误为上列哨兵的包装，语义对齐 Python get_token 抛出的 CaptchaBrowserError 子类。
func (p *Pool) Solve(ctx context.Context) (string, error) {
	if ctx == nil {
		// claim 等调用方允许传 nil（GetVerifyParam(nil)）；select 对 nil 接口
		// 取 Done() 会空指针，统一兜底。
		ctx = context.Background()
	}
	p.mu.Lock()
	g := p.cur
	started := p.started
	p.mu.Unlock()
	if !started || g == nil {
		return "", ErrPoolNotStarted
	}

	// 等待空闲槽位（对齐 _acquire_worker 的 queue_timeout）
	var s *slot
	queueTimer := time.NewTimer(p.queueTimeout)
	defer queueTimer.Stop()
	select {
	case s = <-g.idle:
	case <-queueTimer.C:
		return "", fmt.Errorf("%w（%s）", ErrQueueTimeout, p.queueTimeout)
	case <-ctx.Done():
		return "", ctx.Err()
	case <-g.done:
		return "", ErrPoolClosed
	}

	p.countRequest()

	// 派发：空闲槽位必然阻塞在 reqCh 接收上，发送即完成交接；
	// 派发后该槽位归本次请求独占，直至求解完成自行归队。
	solveCtx, cancel := context.WithTimeout(ctx, p.requestTimeout)
	defer cancel()
	req := &solveReq{ctx: solveCtx, done: make(chan solveResult, 1)}
	select {
	case s.reqCh <- req:
	case <-ctx.Done():
		p.abandon(req)
		return "", ctx.Err()
	case <-g.done:
		p.abandon(req)
		return "", ErrPoolClosed
	}

	// 等待结果；请求超时即判死槽位（对齐 Python：超时杀进程，杜绝响应错位）
	reqTimer := time.NewTimer(p.requestTimeout)
	defer reqTimer.Stop()
	select {
	case res := <-req.done:
		if res.err != nil {
			if errors.Is(res.err, ErrWorkerDead) {
				return "", res.err
			}
			return "", fmt.Errorf("%w（worker %d）: %s", ErrSolverFailure, s.id, redact(res.err.Error()))
		}
		return res.param, nil
	case <-reqTimer.C:
		// 结果恰好同时到达：以结果为准，避免误杀已完成的求解
		select {
		case res := <-req.done:
			if res.err != nil {
				return "", fmt.Errorf("%w（worker %d）: %s", ErrSolverFailure, s.id, redact(res.err.Error()))
			}
			return res.param, nil
		default:
		}
		// 判死槽位：迟到的结果由槽位丢弃，worker 由槽位关闭重建
		req.condemned.Store(true)
		p.abandon(req)
		p.countTimeout()
		return "", fmt.Errorf("%w（>%s），worker %d 已替换", ErrSolveTimeout, p.requestTimeout, s.id)
	case <-ctx.Done():
		// 调用方放弃：不判死槽位（对齐 Python：调用方取消不影响 worker 健康），
		// 结果完成后自然丢弃
		p.abandon(req)
		return "", ctx.Err()
	case <-g.settle:
		// 池已关闭且在途等待被放弃；结果若恰好同时到达仍以结果为准
		select {
		case res := <-req.done:
			if res.err != nil {
				return "", fmt.Errorf("%w（worker %d）: %s", ErrSolverFailure, s.id, redact(res.err.Error()))
			}
			return res.param, nil
		default:
		}
		p.abandon(req)
		return "", ErrPoolClosed
	}
}

// abandon 标记请求已放弃并取消其求解上下文；持锁与否则调用方自行保证无竞争。
func (p *Pool) abandon(req *solveReq) {
	req.abandoned.Store(true)
}

// slotLoop 槽位协程主循环：创建 worker → 就绪 → 串行服务请求 → 致命错误重建。
func (p *Pool) slotLoop(g *generation, id int) {
	defer g.wg.Done()
	for {
		select {
		case <-g.done:
			return
		default:
		}

		// worker 创建（等价 Python 的 spawn + READY 阶段）；每次尝试限时
		fctx, fcancel := context.WithTimeout(context.Background(), p.startupTimeout)
		w, err := p.factory(fctx)
		fcancel()
		if err != nil {
			if !p.factoryFailed(g, id, err) {
				return
			}
			// 运行期创建失败：延迟后重试（对齐 Python spawn OSError 路径）
			web.Warn("captcha_browser", fmt.Sprintf("worker %d 创建失败，稍后重试: %s", id, redact(err.Error())))
			if !p.sleepRestart(g) {
				return
			}
			continue
		}

		if !p.markReady(g) {
			p.closeWorker(w)
			return
		}
		if p.serveLoop(g, w, id) {
			return
		}
		// worker 致命退出或被超时判死：延迟后重建（对齐 mark_broken → 重启延迟）
		if !p.sleepRestart(g) {
			return
		}
	}
}

// serveLoop 就绪 worker 的服务循环；返回 true 表示池正在关闭，false 表示
// worker 需要重建。
func (p *Pool) serveLoop(g *generation, w Worker, id int) bool {
	s := &slot{id: id, reqCh: make(chan *solveReq)}
	for {
		// 就绪入队：缓冲容量 = size 且每槽最多入队一次，永不阻塞
		select {
		case g.idle <- s:
		case <-g.done:
			p.closeWorker(w)
			return true
		}

		var req *solveReq
		select {
		case req = <-s.reqCh:
		case <-g.done:
			p.closeWorker(w)
			return true
		}

		param, err := w.Solve(req.ctx)

		// 投递结果；已放弃的请求直接丢弃计数（迟到结果绝不误投）
		if req.abandoned.Load() {
			p.countDiscarded()
		} else {
			req.done <- solveResult{param: param, err: err}
		}

		// 超时判死：无论本次结果如何都替换 worker（对齐 Python：超时即杀）
		if req.condemned.Load() {
			p.countRestart()
			web.Warn("captcha_browser", fmt.Sprintf("worker %d 求解超时，已替换", id))
			p.closeWorker(w)
			return false
		}
		if err != nil {
			if errors.Is(err, ErrWorkerDead) {
				p.countRestart()
				web.Warn("captcha_browser", fmt.Sprintf("worker %d 提前退出: %s", id, redact(err.Error())))
				p.closeWorker(w)
				return false
			}
			if !req.abandoned.Load() {
				// 求解失败但 worker 健康（真实 worker 已在内部重载页面）
				p.countSolveFailure()
				web.Warn("captcha_browser", fmt.Sprintf("worker %d 求解失败: %s", id, redact(err.Error())))
			}
		}
	}
}

// factoryFailed 记录 worker 创建失败；返回 true 表示运行期可重试，
// false 表示启动阶段失败（整体失败）或代已失效。
func (p *Pool) factoryFailed(g *generation, id int, err error) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur != g || p.closeOnce {
		return false
	}
	if !p.started {
		if p.startErr == nil {
			p.startErr = fmt.Errorf("worker %d 创建失败: %s", id, redact(err.Error()))
		}
		return false
	}
	return true
}

// markReady 槽位就绪计数；代失效（关闭/重启）时返回 false。
func (p *Pool) markReady(g *generation) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur != g || p.closeOnce {
		return false
	}
	p.readyNum++
	return true
}

// sleepRestart 重启延迟；期间池关闭则返回 false。
func (p *Pool) sleepRestart(g *generation) bool {
	timer := time.NewTimer(p.restartDelay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-g.done:
		return false
	}
}

// closeWorker 异步关闭 worker；Close 阻塞超过看门狗时限即放弃等待
// （残留浏览器进程由 leakless/进程退出兜底）。
func (p *Pool) closeWorker(w Worker) {
	go func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := w.Close(); err != nil {
				web.Warn("captcha_browser", "关闭 worker 失败: "+redact(err.Error()))
			}
		}()
		select {
		case <-done:
		case <-time.After(p.closeWatchdog):
			web.Warn("captcha_browser", "worker 关闭超时，放弃等待")
		}
	}()
}

// ── 统计（写路径持锁，读路径走 StatsSnapshot）────────────────────────────────

func (p *Pool) countRequest()      { p.mu.Lock(); p.stats.Requests++; p.mu.Unlock() }
func (p *Pool) countTimeout()      { p.mu.Lock(); p.stats.Timeouts++; p.mu.Unlock() }
func (p *Pool) countSolveFailure() { p.mu.Lock(); p.stats.SolverFailures++; p.mu.Unlock() }
func (p *Pool) countRestart()      { p.mu.Lock(); p.stats.WorkerRestarts++; p.mu.Unlock() }
func (p *Pool) countDiscarded()    { p.mu.Lock(); p.stats.DiscardedResults++; p.mu.Unlock() }
