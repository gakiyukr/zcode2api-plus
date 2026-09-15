package captcha

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/config"
)

// setBrowserEnabled 覆写浏览器开关全局变量并在测试结束时还原。
func setBrowserEnabled(t *testing.T, on bool) {
	t.Helper()
	old := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = on
	t.Cleanup(func() { config.CaptchaBrowserEnabled = old })
}

// solveCfg 组合一个键完整的验证码配置（region 区分配置键）。
func solveCfg(region string) Config {
	return Config{Enabled: true, SceneID: "sc", Region: region, Prefix: "px"}
}

// newStartedFakePool 建好并启动一个假 worker 池（小超时参数）。
func newStartedFakePool(t *testing.T, f WorkerFactory, size int) *Pool {
	t.Helper()
	p := newTestPool(t, f, size)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBrowserSolverReusesPoolTokensFresh(t *testing.T) {
	setBrowserEnabled(t, true)
	m := NewManager()
	m.SetConfigProvider(func(context.Context) (Config, error) { return DefaultConfig, nil })

	s := NewBrowserSolver()
	poolCount := 0
	s.SetPoolFactory(func(Config) *Pool {
		poolCount++
		f := &scriptFactory{}
		return newStartedFakePool(t, f.create, 1)
	})
	m.SetSolver(s)
	ctx := context.Background()

	// 浏览器令牌是一次性的：不缓存，每次求解返回新值
	tok1, err := m.GetVerifyParam(ctx)
	if err != nil || tok1.VerifyParam != "token-w0-0" || tok1.Region != DefaultConfig.Region {
		t.Fatalf("首次求解不符: %+v %v", tok1, err)
	}
	tok2, err := m.GetVerifyParam(ctx)
	if err != nil || tok2.VerifyParam != "token-w0-1" {
		t.Fatalf("第二次应重新求解: %+v %v", tok2, err)
	}
	if poolCount != 1 {
		t.Fatalf("同一配置键应复用池: %d", poolCount)
	}
}

func TestBrowserSolverGatedByConfigSwitch(t *testing.T) {
	setBrowserEnabled(t, false)
	m := NewManager()
	m.SetConfigProvider(func(context.Context) (Config, error) { return DefaultConfig, nil })
	s := NewBrowserSolver()
	s.SetPoolFactory(func(Config) *Pool {
		t.Fatal("开关关闭时不应创建池")
		return nil
	})
	m.SetSolver(s)
	if _, err := m.GetVerifyParam(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("开关关闭应回退人工回填路径: %v", err)
	}
}

func TestBrowserSolverMissingConfig(t *testing.T) {
	s := NewBrowserSolver()
	called := false
	s.SetPoolFactory(func(Config) *Pool {
		called = true
		return nil
	})
	if _, err := s.Solve(context.Background(), Config{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("配置缺失应返回不可用: %v", err)
	}
	if called {
		t.Fatal("配置缺失不应创建池")
	}
}

func TestBrowserSolverStartFailureCooldown(t *testing.T) {
	s := NewBrowserSolver()
	now := time.Unix(1700000000, 0)
	s.SetNow(func() time.Time { return now })

	fail := true
	poolCount := 0
	s.SetPoolFactory(func(Config) *Pool {
		poolCount++
		f := &scriptFactory{}
		if fail {
			f.make = func(int) ([]stepFunc, error) { return nil, errors.New("浏览器二进制缺失") }
		}
		return newTestPool(t, f.create, 1) // 未启动；由求解器负责 Start
	})
	ctx := context.Background()

	if _, err := s.Solve(ctx, solveCfg("cn")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("启动失败应回退人工回填: %v", err)
	}
	if poolCount != 1 {
		t.Fatalf("失败后池应留在原位: %d", poolCount)
	}
	// 冷却期内直接短路，不再触碰工厂
	if _, err := s.Solve(ctx, solveCfg("cn")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("冷却期应返回不可用: %v", err)
	}
	if poolCount != 1 {
		t.Fatalf("冷却期不应重建池: %d", poolCount)
	}

	// 冷却结束（时钟前跳）且故障恢复：重试成功
	now = now.Add(time.Duration(config.CaptchaBrowserFailureCooldown+1) * time.Second)
	fail = false
	tok, err := s.Solve(ctx, solveCfg("cn"))
	if err != nil || tok != "token-w0-0" {
		t.Fatalf("冷却后应重试成功: %q %v", tok, err)
	}
	if poolCount != 2 {
		t.Fatalf("冷却后应重建池: %d", poolCount)
	}
}

func TestBrowserSolverSolveFailurePassthrough(t *testing.T) {
	s := NewBrowserSolver()
	s.SetPoolFactory(func(Config) *Pool {
		f := &scriptFactory{make: func(int) ([]stepFunc, error) {
			return []stepFunc{errStep("captcha SDK fail: 场景不存在")}, nil
		}}
		return newStartedFakePool(t, f.create, 1)
	})
	_, err := s.Solve(context.Background(), solveCfg("cn"))
	if !errors.Is(err, ErrSolverFailure) {
		t.Fatalf("池已启动的求解失败应原样上抛: %v", err)
	}
	if !strings.Contains(err.Error(), "真实浏览器验证码求解失败") || !strings.Contains(err.Error(), "场景不存在") {
		t.Fatalf("错误应带上下文与原因: %v", err)
	}
}

func TestBrowserSolverConfigKeyRestart(t *testing.T) {
	// 配置键（scene|region|prefix）变化：旧池整体关闭，新池重建
	s := NewBrowserSolver()
	var pools []*Pool
	s.SetPoolFactory(func(cfg Config) *Pool {
		f := &scriptFactory{}
		p := newStartedFakePool(t, f.create, 1)
		pools = append(pools, p)
		return p
	})
	ctx := context.Background()

	if _, err := s.Solve(ctx, solveCfg("cn")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Solve(ctx, solveCfg("sg")); err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 {
		t.Fatalf("配置键变化应重建池: %d", len(pools))
	}
	if pools[0].IsStarted() {
		t.Fatal("旧配置的池应已关闭")
	}
	if !pools[1].IsStarted() {
		t.Fatal("新配置的池应已启动")
	}
	// 同键再求解：继续复用
	if _, err := s.Solve(ctx, solveCfg("sg")); err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 {
		t.Fatalf("同键不应重建池: %d", len(pools))
	}
}

func TestBrowserSolverCloseStopsPool(t *testing.T) {
	s := NewBrowserSolver()
	s.SetPoolFactory(func(Config) *Pool {
		f := &scriptFactory{}
		return newStartedFakePool(t, f.create, 1)
	})
	if _, err := s.Solve(context.Background(), solveCfg("cn")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if s.pool != nil || s.poolKey != "" {
		t.Fatalf("关闭后应清空池引用: %v %q", s.pool, s.poolKey)
	}
}

// Solve 不得持锁跨越池求解：多个调用者必须能并发执行（池 size 决定上限）。
// 曾因持锁导致实效并发恒为 1，且单次 45s 求解期间其他调用者全部阻塞。
func TestBrowserSolverConcurrentSolvesDoNotSerialize(t *testing.T) {
	s := NewBrowserSolver()
	var concurrent, peak int
	var mu sync.Mutex
	release := make(chan struct{})

	s.SetPoolFactory(func(Config) *Pool {
		return newStartedFakePool(t, func(ctx context.Context) (Worker, error) {
			return &blockingWorker{onSolve: func() {
				mu.Lock()
				concurrent++
				if concurrent > peak {
					peak = concurrent
				}
				mu.Unlock()
				<-release
				mu.Lock()
				concurrent--
				mu.Unlock()
			}}, nil
		}, 3)
	})

	const n = 3
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			_, _ = s.Solve(context.Background(), solveCfg("cn"))
		}()
	}

	// 等待全部进入求解状态（不持锁才能达到 n 个并发）
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		cur := concurrent
		mu.Unlock()
		if cur >= n || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := concurrent
	mu.Unlock()
	close(release)
	wg.Wait()

	if got != n {
		t.Fatalf("Solve 应允许并发（期望 %d 个同时在求解，实际 %d）", n, got)
	}
}

// 配置键变更时，在途求解不得被中止；旧池由最后一个归还者回收。
func TestBrowserSolverConfigChangeDoesNotAbortInFlight(t *testing.T) {
	s := NewBrowserSolver()
	started := make(chan struct{})
	finished := make(chan struct{})
	release := make(chan struct{})
	var onceStart, onceFinish sync.Once

	s.SetPoolFactory(func(Config) *Pool {
		return newStartedFakePool(t, func(ctx context.Context) (Worker, error) {
			return &blockingWorker{onSolve: func() {
				// 同一 worker 会被池重复使用，只关心第一次
				onceStart.Do(func() { close(started) })
				<-release
				onceFinish.Do(func() { close(finished) })
			}}, nil
		}, 1)
	})

	go func() {
		_, _ = s.Solve(context.Background(), solveCfg("cn"))
	}()
	<-started

	// 配置键变更：旧池有在途求解，应标记 retired 而非立即 Stop
	_, _ = s.Solve(context.Background(), solveCfg("sgp"))

	// 在途求解必须仍能正常完成（未被 Stop 中止）
	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("在途求解被配置变更中止（应延迟关闭旧池）")
	}

	// 最后一个归还者应回收 retired 池
	var retired *Pool
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		retired = s.retired
		s.mu.Unlock()
		if retired == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if retired != nil {
		t.Fatal("在途求解归还后应回收 retired 池")
	}
}

// blockingWorker 在 onSolve 中执行测试指定的阻塞逻辑，完成后返回固定令牌。
type blockingWorker struct {
	onSolve func()
}

func (w *blockingWorker) Solve(ctx context.Context) (string, error) {
	if w.onSolve != nil {
		w.onSolve()
	}
	return "token", nil
}

func (w *blockingWorker) Close() error { return nil }

// 部分缺字段的配置也应被拒（不能只判「全空」）。
//
// sceneId/region/prefix 都是 SDK 初始化的必需参数；只判全空会让缺一项的配置
// 照样拉起浏览器并加载 224KB SDK，失败还被归类为 ErrSolverFailure（不触发
// 启动冷却），与错误文案「缺少 sceneId、region 或 prefix」不符。
func TestBrowserSolverRejectsPartiallyMissingConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"缺 region", Config{Enabled: true, SceneID: "sc", Prefix: "px"}},
		{"缺 prefix", Config{Enabled: true, SceneID: "sc", Region: "cn"}},
		{"缺 sceneId", Config{Enabled: true, Region: "cn", Prefix: "px"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewBrowserSolver()
			called := false
			s.SetPoolFactory(func(Config) *Pool {
				called = true
				return nil
			})
			if _, err := s.Solve(context.Background(), c.cfg); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("配置不完整应回 ErrUnavailable，实际: %v", err)
			}
			if called {
				t.Fatal("配置不完整不应创建池（会白拉起浏览器）")
			}
		})
	}
}
