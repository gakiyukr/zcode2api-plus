// Manager 的 Solver 实现：惰性启动浏览器池、按验证码配置键复用/重建池、
// 启动失败进入冷却。对应 Python 版 app/captcha.py 的 _solve_browser 与
// _ensure_browser_pool。冷却期内与配置缺失时返回 ErrUnavailable，
// 由网关映射为 503 captcha_required（人工回填兜底）。
package captcha

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/web"
)

// BrowserSolver 浏览器池求解器（注入 Manager.SetSolver）。
type BrowserSolver struct {
	mu sync.Mutex
	// pool 当前池与它绑定的配置键（scene|region|prefix，对齐 _browser_config_key）。
	pool    *Pool
	poolKey string
	// inFlight 当前池正在锁外执行的求解数；>0 时配置变更不立即 Stop，
	// 而是标记 retired，由最后一个归还者负责关闭（避免中止在途求解）。
	inFlight int
	retired  *Pool
	// starting 正在锁外启动的池及其配置键。
	// Start 最长等 CaptchaBrowserStartupTimeout（默认 90s），必须在锁外执行，
	// 否则所有并发 Solve 会卡在同一把锁上、各自请求的 ctx 取消完全无效。
	// 启动期间到达的调用者等待这个信号而非重复启动。
	starting    *Pool
	startingKey string
	startDone   chan struct{}
	// failureUntil 启动失败后的冷却截止（单调时钟）。
	failureUntil time.Time

	// now 可注入时钟（测试用）。
	now func() time.Time
	// newPool 池构造函数；测试注入假工厂，默认绑定真实 rod 工厂。
	newPool func(cfg Config) *Pool
}

// NewBrowserSolver 创建浏览器求解器；池在首次 Solve 时惰性启动。
func NewBrowserSolver() *BrowserSolver {
	return &BrowserSolver{
		now: time.Now,
		newPool: func(cfg Config) *Pool {
			return NewPool(NewRodWorkerFactory(cfg), config.CaptchaBrowserWorkers)
		},
	}
}

// SetPoolFactory 注入池构造函数（测试用）。
func (s *BrowserSolver) SetPoolFactory(f func(cfg Config) *Pool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.newPool = f
}

// SetNow 注入时钟（测试用）。
func (s *BrowserSolver) SetNow(fn func() time.Time) { s.now = fn }

// Solve 返回一个浏览器求解的 verify_param。
//
// 错误语义：
//   - 配置缺失 / 冷却期内 / 启动失败 → ErrUnavailable（管理器原样上抛，
//     网关 503 captcha_required；对齐 Python 浏览器路径返回 None 后无
//     Node 兜底的形态——jsdom 求解已被上游 F001 判死，不移植）；
//   - 池已启动但本次求解失败 → 带原因的错误（交给网关有限重试；
//     池自身负责替换超时或退出的 worker）。
//
// 并发语义：锁只保护池的选取与重建；实际求解在锁外执行，
// 因此多个调用者可并发使用池内不同槽位（并发上限由池的 size 决定）。
func (s *BrowserSolver) Solve(ctx context.Context, cfg Config) (string, error) {
	if ctx == nil {
		ctx = context.Background() // GetVerifyParam(nil) 允许 nil ctx
	}
	scene, region, prefix := strings.TrimSpace(cfg.SceneID), strings.TrimSpace(cfg.Region), strings.TrimSpace(cfg.Prefix)
	// 三项都是 SDK 初始化的必需参数。只判「全空」会让部分缺字段的配置照样
	// 拉起浏览器、加载 224KB SDK 后才失败，且失败被归类为 ErrSolverFailure
	// （不触发启动冷却），与错误文案宣称的语义不符。
	if scene == "" || region == "" || prefix == "" {
		return "", fmt.Errorf("%w：验证码配置缺少 sceneId、region 或 prefix", ErrUnavailable)
	}
	key := strings.Join([]string{scene, region, prefix}, "|")

	pool, release, err := s.acquirePool(ctx, cfg, key)
	if err != nil {
		return "", err
	}
	defer release()

	param, solveErr := pool.Solve(ctx)
	if solveErr != nil {
		web.Warn("captcha", "真实浏览器验证码求解失败: "+redact(solveErr.Error()))
		return "", fmt.Errorf("真实浏览器验证码求解失败: %w", solveErr)
	}
	if strings.TrimSpace(param) == "" {
		return "", errors.New("真实浏览器验证码求解器返回空结果")
	}
	return param, nil
}

// acquirePool 取（必要时启动/重建）可用的池，并登记一次在途使用。
// 返回的 release 必须在求解结束后调用：它负责在池已被配置变更淘汰时关闭旧池，
// 从而保证「不中止在途求解」与「旧池最终被回收」两者兼得。
func (s *BrowserSolver) acquirePool(ctx context.Context, cfg Config, key string) (*Pool, func(), error) {
	for {
		s.mu.Lock()
		if s.now().Before(s.failureUntil) {
			s.mu.Unlock()
			return nil, nil, fmt.Errorf("%w：浏览器池冷却中", ErrUnavailable)
		}
		// 已有可用池：直接登记使用
		if s.pool != nil && s.poolKey == key && s.pool.IsStarted() {
			pool := s.pool
			s.inFlight++
			s.mu.Unlock()
			return pool, s.releaseFunc(), nil
		}
		// 他人正在启动同一配置：等它结束（可被 ctx 取消）后重试
		if s.starting != nil && s.startingKey == key {
			wait := s.startDone
			s.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		// 本调用者负责启动：先在锁内完成旧池淘汰与新池登记，再出锁 Start
		pool, err := s.preparePoolLocked(cfg, key)
		if err != nil {
			s.mu.Unlock()
			return nil, nil, err
		}
		s.starting, s.startingKey, s.startDone = pool, key, make(chan struct{})
		done := s.startDone
		s.mu.Unlock()

		// 锁外启动：最长 CaptchaBrowserStartupTimeout，期间不阻塞其他调用者
		startErr := pool.Start()

		s.mu.Lock()
		s.starting, s.startingKey, s.startDone = nil, "", nil
		close(done)
		if startErr != nil {
			cooldown := time.Duration(config.CaptchaBrowserFailureCooldown) * time.Second
			s.failureUntil = s.now().Add(cooldown)
			s.mu.Unlock()
			web.Warn("captcha", fmt.Sprintf(
				"真实浏览器池不可用，%s 内回退人工回填: %s", cooldown, redact(startErr.Error())))
			return nil, nil, fmt.Errorf("%w：浏览器池启动失败", ErrUnavailable)
		}
		s.pool, s.poolKey = pool, key
		s.inFlight++
		s.mu.Unlock()
		web.Ok("captcha", fmt.Sprintf("真实浏览器验证码池已就绪（%d 个 worker）", config.CaptchaBrowserWorkers))
		return pool, s.releaseFunc(), nil
	}
}

// releaseFunc 返回一次在途使用的归还函数。
func (s *BrowserSolver) releaseFunc() func() {
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.inFlight--
		// 最后一个归还者关闭已被淘汰的旧池
		if s.inFlight == 0 && s.retired != nil {
			s.retired.Stop()
			s.retired = nil
		}
	}
}

// preparePoolLocked 在锁内淘汰旧池并为新配置建池（不启动）；调用方持锁。
func (s *BrowserSolver) preparePoolLocked(cfg Config, key string) (*Pool, error) {
	if s.pool != nil {
		// 配置变更：有在途求解时延迟关闭（标记 retired，由最后一个归还者 Stop），
		// 否则立即关闭（对齐 _stop_browser_pool）。
		if s.inFlight > 0 {
			if s.retired != nil {
				s.retired.Stop()
			}
			s.retired = s.pool
		} else {
			s.pool.Stop()
		}
		s.pool = nil
		s.poolKey = ""
	}
	return s.newPool(cfg), nil
}

// Close 关闭浏览器池（Manager.Close 转发）。
// Stop 可能阻塞至 shutdownTimeout（默认 10s），故在锁外执行，
// 避免阻塞其他仍在使用求解器的调用者。
func (s *BrowserSolver) Close() error {
	s.mu.Lock()
	// 等待正在进行的启动结束：否则该池会在 Close 返回后才被赋给 s.pool，
	// 既漏关又让已关闭的求解器继续持有浏览器进程。
	for s.starting != nil {
		wait := s.startDone
		s.mu.Unlock()
		<-wait
		s.mu.Lock()
	}
	pool := s.pool
	retired := s.retired
	s.pool = nil
	s.poolKey = ""
	s.retired = nil
	s.mu.Unlock()

	if pool != nil {
		pool.Stop()
	}
	if retired != nil {
		retired.Stop()
	}
	return nil
}
