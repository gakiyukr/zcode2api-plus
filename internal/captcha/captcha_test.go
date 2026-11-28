package captcha

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/config"
)

// setCaptchaServer 将配置端点指向测试服务器，避免测试触网。
func setCaptchaServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	old := configURL
	configURL = srv.URL
	t.Cleanup(func() {
		configURL = old
		srv.Close()
	})
}

func TestFetchConfigFromUpstream(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("app_version") != config.ZcodeClientVersion || r.URL.Query().Get("platform") != config.ZcodeClientPlatform {
			t.Errorf("应携带客户端版本与平台参数: %v", r.URL.Query())
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc"}}}}`))
	})
	m := NewManager()
	cfg := m.FetchConfig(context.Background())
	if !cfg.Enabled || cfg.Prefix != "px" || cfg.Region != "cn" || cfg.SceneID != "sc" {
		t.Fatalf("配置解析不符: %+v", cfg)
	}

	// 二次调用走 10 分钟缓存（服务已关闭仍应成功）
	if cfg2 := m.FetchConfig(context.Background()); cfg2 != cfg {
		t.Fatalf("应命中配置缓存: %+v", cfg2)
	}
}

func TestFetchConfigMissingCaptchaFallsBack(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"configs":{}}}`))
	})
	m := NewManager()
	if cfg := m.FetchConfig(context.Background()); cfg != DefaultConfig {
		t.Fatalf("缺少 captcha 对象应回退默认: %+v", cfg)
	}
}

func TestFetchConfigServerErrorFallsBack(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	m := NewManager()
	if cfg := m.FetchConfig(context.Background()); cfg != DefaultConfig {
		t.Fatalf("上游失败应回退默认: %+v", cfg)
	}
}

func TestGetVerifyParamDisabledConfig(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"configs":{"captcha":{"enabled":false}}}}`))
	})
	m := NewManager()
	tok, err := m.GetVerifyParam(context.Background())
	if tok != nil || err != nil {
		t.Fatalf("禁用配置应返回 (nil, nil): %v %v", tok, err)
	}
}

func TestNoSolverUnavailable(t *testing.T) {
	// 上游 500 → 默认配置（enabled），浏览器默认关闭 → 无求解器可用
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	m := NewManager()
	if _, err := m.GetVerifyParam(context.Background()); err != ErrUnavailable {
		t.Fatalf("应返回 ErrUnavailable: %v", err)
	}
}

func TestManualTokenCacheAndExpiry(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	m := NewManager()
	now := time.Unix(1700000000, 0)
	m.SetNow(func() time.Time { return now })

	if err := m.SetManualParam(" manual-token ", ""); err != nil {
		t.Fatal(err)
	}
	tok, err := m.GetVerifyParam(context.Background())
	if err != nil || tok == nil || tok.VerifyParam != "manual-token" {
		t.Fatalf("人工令牌应命中（含去空白）: %+v %v", tok, err)
	}
	if tok.Region != DefaultConfig.Region {
		t.Fatalf("region 缺省应取默认配置: %q", tok.Region)
	}

	// TTL 过期后走求解路径 → 浏览器关闭 → ErrUnavailable
	now = now.Add(time.Duration(config.CaptchaManualCacheTTL+1000) * time.Millisecond)
	if _, err := m.GetVerifyParam(context.Background()); err != ErrUnavailable {
		t.Fatalf("过期后应重新求解并返回 ErrUnavailable: %v", err)
	}
}

func TestInvalidateClearsManualToken(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	m := NewManager()
	now := time.Unix(1700000000, 0)
	m.SetNow(func() time.Time { return now })
	if err := m.SetManualParam("t", "cn"); err != nil {
		t.Fatal(err)
	}
	m.Invalidate()
	if _, err := m.GetVerifyParam(context.Background()); err != ErrUnavailable {
		t.Fatalf("失效后应重新求解: %v", err)
	}
}

func TestSetManualParamEmptyRejected(t *testing.T) {
	m := NewManager()
	if err := m.SetManualParam("  ", ""); err == nil {
		t.Fatal("空令牌应报错")
	}
}

type scriptedCaptchaSolver struct {
	mu      sync.Mutex
	outputs []string
	calls   atomic.Int32
}

func (s *scriptedCaptchaSolver) Solve(_ context.Context, _ Config) (string, error) {
	idx := int(s.calls.Add(1)) - 1
	s.mu.Lock()
	defer s.mu.Unlock()
	if idx < len(s.outputs) {
		return s.outputs[idx], nil
	}
	return "fallback-tok", nil
}

func (s *scriptedCaptchaSolver) Close() error { return nil }

func TestSkipModelRequest_ModelVsClaim(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"skip_model_request":true,"prefix":"px","region":"cn","sceneId":"sc"}}}}`))
	})
	oldBrowser := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = oldBrowser })

	m := NewManager()
	t.Cleanup(func() { _ = m.Close() })
	solver := &scriptedCaptchaSolver{outputs: []string{"claim-tok", "forced-model-tok"}}
	m.SetSolver(solver)

	// 對話模型請求受 skip_model_request=true 豁免：回傳 (nil, nil) 且不呼叫求解器
	tok, err := m.GetVerifyParamForModel(context.Background(), "direct", false)
	if tok != nil || err != nil {
		t.Fatalf("skip_model_request=true 時模型請求應免驗，實際: tok=%+v err=%v", tok, err)
	}
	if solver.calls.Load() != 0 {
		t.Fatalf("免驗時不應呼叫 solver，實際次數: %d", solver.calls.Load())
	}

	// 領取套餐專用介面忽略 skip_model_request，仍應真求解
	claimTok, err := m.GetVerifyParamForClaim(context.Background(), "direct")
	if err != nil || claimTok == nil || claimTok.VerifyParam != "claim-tok" {
		t.Fatalf("領取套餐仍須求解驗證碼，實際: tok=%+v err=%v", claimTok, err)
	}
	if solver.calls.Load() != 1 {
		t.Fatalf("領取套餐應呼叫 solver 1 次，實際: %d", solver.calls.Load())
	}

	// 呼叫 InvalidateModelSkip 後，模型請求必須立即改走真求解
	m.InvalidateModelSkip()
	forcedTok, err := m.GetVerifyParamForModel(context.Background(), "direct", false)
	if err != nil || forcedTok == nil || forcedTok.VerifyParam != "forced-model-tok" {
		t.Fatalf("InvalidateModelSkip 後模型請求應改走真求解，實際: tok=%+v err=%v", forcedTok, err)
	}
}

func TestCertifyIDDedupPreventsF008(t *testing.T) {
	oldBrowser := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = oldBrowser })

	dup := base64.StdEncoding.EncodeToString([]byte(`{"certifyId":"dup-1","sceneId":"sc"}`))
	ok2 := base64.StdEncoding.EncodeToString([]byte(`{"certifyId":"ok-2","sceneId":"sc"}`))

	m := NewManager()
	t.Cleanup(func() { _ = m.Close() })
	m.SetConfigProvider(func(context.Context) (Config, error) {
		return Config{Enabled: true, Prefix: "px", Region: "cn", SceneID: "sc"}, nil
	})
	solver := &scriptedCaptchaSolver{outputs: []string{dup, dup, ok2}}
	m.SetSolver(solver)

	// 第一次取得 dup-1 並登記至去重表
	t1, err := m.GetVerifyParamForModel(context.Background(), "direct", false)
	if err != nil || t1 == nil || t1.VerifyParam != dup {
		t.Fatalf("首次應取得 dup-1: tok=%+v err=%v", t1, err)
	}
	// 第二次求解器先吐重複的 dup-1，應被攔截重求並取得 ok-2
	t2, err := m.GetVerifyParamForModel(context.Background(), "direct", false)
	if err != nil || t2 == nil || t2.VerifyParam != ok2 {
		t.Fatalf("重複 certifyId 應自動重求取得 ok-2，實際: tok=%+v err=%v", t2, err)
	}
	if solver.calls.Load() != 3 {
		t.Fatalf("應呼叫 solver 3 次（含重複攔截重求），實際: %d", solver.calls.Load())
	}
}

type prewarmTestWorker struct {
	onSolve func(context.Context) (string, error)
	onClose func() error
}

func (w *prewarmTestWorker) Solve(ctx context.Context) (string, error) { return w.onSolve(ctx) }
func (w *prewarmTestWorker) Close() error                              { return w.onClose() }

func TestPrewarmPerAddressAndIdleHibernate(t *testing.T) {
	oldBrowser := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = oldBrowser })

	var stopped atomic.Int32
	bs := NewBrowserSolver()
	bs.SetPoolFactory(func(cfg Config) *Pool {
		var seq atomic.Int32
		return NewPool(func(context.Context) (Worker, error) {
			return &prewarmTestWorker{
				onSolve: func(context.Context) (string, error) {
					n := seq.Add(1)
					return "tok-" + string(rune('0'+n)), nil
				},
				onClose: func() error {
					stopped.Add(1)
					return nil
				},
			}, nil
		}, 1)
	})

	m := NewManager()
	t.Cleanup(func() { _ = m.Close() })
	m.SetConfigProvider(func(context.Context) (Config, error) {
		return Config{Enabled: true, Prefix: "px", Region: "cn", SceneID: "sc"}, nil
	})
	m.SetSolver(bs)
	m.SetPrewarm(true)
	m.SetIdleHibernate(80 * time.Millisecond)

	// 出口 p1 首次前景求解後會背景預熱 1 張屬於 p1 的票
	t1, err := m.GetVerifyParamForModel(context.Background(), "http://p1:8080", false)
	if err != nil || t1 == nil {
		t.Fatalf("p1 首次求解失敗: %v", err)
	}
	// 等待 p1 背景預熱落入 prewarmed
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		_, hasP1 := m.prewarmed["http://p1:8080"]
		_, hasP2 := m.prewarmed["http://p2:8080"]
		m.mu.Unlock()
		if hasP1 {
			if hasP2 {
				t.Fatal("p1 預熱不應寫入 p2 出口槽位")
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 出口 p2 取票不應誤拿 p1 的預熱票
	m.mu.Lock()
	p1Prewarmed, ok := m.prewarmed["http://p1:8080"]
	m.mu.Unlock()
	if !ok {
		t.Fatal("預期 p1 已完成背景預熱")
	}
	t2, err := m.GetVerifyParamForModel(context.Background(), "http://p2:8080", false)
	if err != nil || t2 == nil {
		t.Fatalf("p2 求解失敗: %v", err)
	}
	if t2.VerifyParam == p1Prewarmed.tok.VerifyParam {
		t.Fatal("p2 不得混用 p1 的預熱票據")
	}

	// 出口 p1 再次請求應直接命中其預熱票據
	t1Hit, err := m.GetVerifyParamForModel(context.Background(), "http://p1:8080", false)
	if err != nil || t1Hit == nil || t1Hit.VerifyParam != p1Prewarmed.tok.VerifyParam {
		t.Fatalf("p1 二次請求應直接命中預熱票 %q，實際: %+v err=%v", p1Prewarmed.tok.VerifyParam, t1Hit, err)
	}

	// 閒置超過 idleHibernate（80ms）後應自動觸發 BrowserSolver.HibernateIfIdle 釋放底層池
	hibernateDeadline := time.Now().Add(time.Second)
	for time.Now().Before(hibernateDeadline) {
		if stopped.Load() > 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if stopped.Load() == 0 {
		t.Fatal("閒置超過 idleHibernate 後應觸發 BrowserSolver 深度休眠並關閉底層 worker")
	}
}
