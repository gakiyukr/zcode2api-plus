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
	key := strings.Join([]string{
		strings.TrimSpace(cfg.SceneID),
		strings.TrimSpace(cfg.Region),
		strings.TrimSpace(cfg.Prefix),
	}, "|")
	if key == "||" {
		return "", fmt.Errorf("%w：验证码配置缺少 sceneId、region 或 prefix", ErrUnavailable)
	}

	pool, release, err := s.acquirePool(cfg, key)
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
func (s *BrowserSolver) acquirePool(cfg Config, key string) (*Pool, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.now().Before(s.failureUntil) {
		return nil, nil, fmt.Errorf("%w：浏览器池冷却中", ErrUnavailable)
	}
	pool, err := s.ensurePoolLocked(cfg, key)
	if err != nil {
		return nil, nil, err
	}
	s.inFlight++
	release := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.inFlight--
		// 最后一个归还者关闭已被淘汰的旧池
		if s.inFlight == 0 && s.retired != nil {
			s.retired.Stop()
			s.retired = nil
		}
	}
	return pool, release, nil
}

// ensurePoolLocked 取已启动的池，必要时启动；配置键变化时重建（调用方持锁）。
func (s *BrowserSolver) ensurePoolLocked(cfg Config, key string) (*Pool, error) {
	if s.pool != nil && s.poolKey == key && s.pool.IsStarted() {
		return s.pool, nil
	}
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

	pool := s.newPool(cfg)
	if err := pool.Start(); err != nil {
		cooldown := time.Duration(config.CaptchaBrowserFailureCooldown) * time.Second
		s.failureUntil = s.now().Add(cooldown)
		web.Warn("captcha", fmt.Sprintf(
			"真实浏览器池不可用，%s 内回退人工回填: %s", cooldown, redact(err.Error())))
		return nil, fmt.Errorf("%w：浏览器池启动失败", ErrUnavailable)
	}

	s.pool = pool
	s.poolKey = key
	web.Ok("captcha", fmt.Sprintf("真实浏览器验证码池已就绪（%d 个 worker）", config.CaptchaBrowserWorkers))
	return pool, nil
}

// Close 关闭浏览器池（Manager.Close 转发）。
// Stop 可能阻塞至 shutdownTimeout（默认 10s），故在锁外执行，
// 避免阻塞其他仍在使用求解器的调用者。
func (s *BrowserSolver) Close() error {
	s.mu.Lock()
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
