package captcha

import (
	"context"
	"net/http"
	"net/http/httptest"
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
