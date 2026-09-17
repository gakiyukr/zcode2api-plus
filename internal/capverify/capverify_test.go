package capverify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyDisabledWithoutConfig(t *testing.T) {
	c := New()
	// 未配置时不应发起请求，也不应把 token 当作有效
	for _, cfg := range []Config{
		{},
		{Instance: "https://cap.example.com"},
		{SiteKey: "abc"},
		{Secret: "secret"},
		{Instance: "https://cap.example.com", Secret: "s"},
		{Instance: "https://cap.example.com", SiteKey: "abc"},
		{Instance: "   ", SiteKey: "  ", Secret: "  "},
	} {
		if cfg.Enabled() {
			t.Fatalf("配置 %+v 不应被视为已启用", cfg)
		}
		err := c.Verify(context.Background(), cfg, "any-token")
		if !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("配置 %+v: 期望 ErrNotConfigured，得到 %v", cfg, err)
		}
	}
}

func TestVerifySuccess(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	c := New()
	cfg := Config{Instance: srv.URL, SiteKey: "sitekey", Secret: "sec"}
	if err := c.Verify(context.Background(), cfg, "tok"); err != nil {
		t.Fatalf("校验应通过，得到 %v", err)
	}
	if gotPath != "/sitekey/siteverify" {
		t.Fatalf("请求路径错误: %s", gotPath)
	}
	// 尾部斜杠不应导致双斜杠
	if gotBody == "" || !contains(gotBody, `"secret":"sec"`) || !contains(gotBody, `"response":"tok"`) {
		t.Fatalf("请求体不含预期字段: %s", gotBody)
	}
}

func TestVerifyRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":false}`))
	}))
	defer srv.Close()

	c := New()
	cfg := Config{Instance: srv.URL, SiteKey: "k", Secret: "sec"}
	err := c.Verify(context.Background(), cfg, "bad")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("期望 ErrInvalidToken，得到 %v", err)
	}
}

func TestVerifyEmptyTokenRejectedWithoutRequest(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	c := New()
	cfg := Config{Instance: srv.URL, SiteKey: "k", Secret: "sec"}
	// 空 token 必须在本地就被拒，不能白跑一趟网络请求
	if err := c.Verify(context.Background(), cfg, "  "); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("期望 ErrInvalidToken，得到 %v", err)
	}
	if called {
		t.Fatal("空 token 不应发起请求")
	}
}

func TestVerifyServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New()
	cfg := Config{Instance: srv.URL, SiteKey: "k", Secret: "sec"}
	err := c.Verify(context.Background(), cfg, "tok")
	// 服务端故障必须与「校验失败」区分：前者是配置/网络问题，后者是访客的问题
	if err == nil || errors.Is(err, ErrInvalidToken) {
		t.Fatalf("服务端错误不应被当作 token 无效，得到 %v", err)
	}
}

func TestVerifyUnreachable(t *testing.T) {
	c := New()
	cfg := Config{Instance: "http://127.0.0.1:1", SiteKey: "k", Secret: "sec"}
	err := c.Verify(context.Background(), cfg, "tok")
	if err == nil || errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrNotConfigured) {
		t.Fatalf("连接失败应返回网络错误，得到 %v", err)
	}
}

func TestVerifyMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := New()
	cfg := Config{Instance: srv.URL, SiteKey: "k", Secret: "sec"}
	if err := c.Verify(context.Background(), cfg, "tok"); err == nil {
		t.Fatal("非法响应应报错")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}


// Endpoint 的拼接是配置里最容易出错的一环：管理员会照抄 Cap 后台的三个值，
// 而斜杠有无、site key 是否带路径分隔符都不该影响结果。
func TestEndpointComposition(t *testing.T) {
	cases := []struct {
		name     string
		instance string
		siteKey  string
		want     string
	}{
		{"常规", "https://cap.example.com", "abc123", "https://cap.example.com/abc123/"},
		{"实例带尾斜杠", "https://cap.example.com/", "abc123", "https://cap.example.com/abc123/"},
		{"site key 带斜杠", "https://cap.example.com", "/abc123/", "https://cap.example.com/abc123/"},
		{"两者都带斜杠", "https://cap.example.com/", "/abc123/", "https://cap.example.com/abc123/"},
		{"带端口", "http://127.0.0.1:3000", "abc", "http://127.0.0.1:3000/abc/"},
		{"带子路径", "https://example.com/cap", "abc", "https://example.com/cap/abc/"},
		{"两端空白", "  https://cap.example.com  ", "  abc  ", "https://cap.example.com/abc/"},
		{"缺 site key", "https://cap.example.com", "", ""},
		{"缺实例", "", "abc", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Config{Instance: c.instance, SiteKey: c.siteKey}.Endpoint()
			if got != c.want {
				t.Fatalf("Endpoint() = %q，期望 %q", got, c.want)
			}
		})
	}
}
