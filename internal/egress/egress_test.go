package egress

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	old := config.DBPath
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	t.Cleanup(func() { config.DBPath = old })
	st, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAccountKey(t *testing.T) {
	if got := AccountKey(nil); got != "direct" {
		t.Fatalf("nil 帳號應為 direct: %s", got)
	}
}

func TestProbeAnonymousAndHealthyOn401(t *testing.T) {
	HoldFor("direct", -time.Second, time.Now())
	t.Cleanup(func() { HoldFor("direct", -time.Second, time.Now()) })

	var sawCredentials bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alicdn" {
			_, _ = w.Write([]byte("captcha-sdk"))
			return
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" ||
			r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Device-Mid") != "" {
			sawCredentials = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"no auth"}`))
	}))
	defer srv.Close()

	mon := &Monitor{
		Store:       testStore(t),
		Workers:     1,
		MessagesURL: srv.URL + "/messages",
		AlicdnURL:   srv.URL + "/alicdn",
		ClientFor:   func(string, time.Duration) (*http.Client, error) { return srv.Client(), nil },
		Now:         time.Now,
	}
	mon.RunOnce()
	if sawCredentials {
		t.Fatal("匿名探針不得攜帶任何帳號憑證")
	}
	if IsHeld("direct", time.Now()) {
		t.Fatal("401 代表出口可達，不應熔斷")
	}
}

func TestProbeHoldsEdgeOnWAFPage(t *testing.T) {
	HoldFor("direct", -time.Second, time.Now())
	t.Cleanup(func() { HoldFor("direct", -time.Second, time.Now()) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`<html>errors.aliyun.com request has been blocked</html>`))
	}))
	defer srv.Close()

	mon := &Monitor{
		Store:       testStore(t),
		Workers:     1,
		MessagesURL: srv.URL + "/messages",
		AlicdnURL:   srv.URL + "/alicdn",
		ClientFor:   func(string, time.Duration) (*http.Client, error) { return srv.Client(), nil },
		Now:         time.Now,
	}
	mon.RunOnce()
	if !IsHeld("direct", time.Now()) {
		t.Fatal("405 + WAF 特徵頁應提前熔斷出口")
	}
}
