package egress

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

const (
	// AlicdnCaptchaURL 驗證碼前端 SDK 的 CDN 位址；用於區分慢路由與 WAF 封禁。
	AlicdnCaptchaURL = "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"
	probeShortHold   = 60 * time.Second
)

type probeResult int

const (
	probeHealthy probeResult = iota
	probeUnhealthy
	probeEdgeBlocked
)

// Monitor 低頻匿名出口巡檢器。探針不帶任何帳號憑證，失敗只隔離出口，
// 不寫帳號狀態。
type Monitor struct {
	Store       *store.Store
	Interval    time.Duration
	Workers     int
	MessagesURL string
	AlicdnURL   string
	ClientFor   func(proxyURL string, timeout time.Duration) (*http.Client, error)
	Now         func() time.Time

	stop       chan struct{}
	done       chan struct{}
	start      sync.Once
	started    atomic.Bool
	stopCtx    context.Context
	stopCancel context.CancelFunc
}

// NewMonitor 建立出口巡檢器（預設每 10 分鐘、併發 4）。
func NewMonitor(st *store.Store) *Monitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Monitor{
		Store:       st,
		Interval:    time.Duration(config.EgressProbeInterval) * time.Second,
		Workers:     config.EgressProbeWorkers,
		MessagesURL: config.UpstreamZai,
		AlicdnURL:   AlicdnCaptchaURL,
		ClientFor:   proxy.ClientFor,
		Now:         time.Now,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		stopCtx:     ctx,
		stopCancel:  cancel,
	}
}

// Start 啟動背景巡檢（冪等）。
func (m *Monitor) Start() {
	m.start.Do(func() {
		m.started.Store(true)
		go m.loop()
	})
}

// Stop 停止巡檢並等待收尾；未啟動時直接返回。
func (m *Monitor) Stop() {
	if m.stopCancel != nil {
		m.stopCancel()
	}
	if !m.started.Load() {
		return
	}
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
}

// RunOnce 對全部已知出口跑一輪匿名巡檢。
func (m *Monitor) RunOnce() {
	defer func() {
		if r := recover(); r != nil {
			web.Warn("egress", fmt.Sprintf("出口巡檢異常（已隔離）: %v", r))
		}
	}()
	if m.Store == nil {
		return
	}
	targets := m.targets()
	if len(targets) == 0 {
		return
	}
	workers := m.Workers
	if workers <= 0 {
		workers = 1
	}
	workers = min(workers, len(targets))
	jobs := make(chan string, len(targets))
	for _, key := range targets {
		jobs <- key
	}
	close(jobs)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range jobs {
				select {
				case <-m.stop:
					return
				default:
				}
				m.probe(ctx, key)
			}
		}()
	}
	wg.Wait()
}

func (m *Monitor) targets() []string {
	seen := map[string]bool{}
	hasAccount := false
	for _, acc := range m.Store.ListAccounts(model.ProviderZai) {
		seen[AccountKey(acc)] = true
		hasAccount = true
	}
	for _, profile := range m.Store.ListProxyProfiles() {
		if profile.Enabled && strings.TrimSpace(profile.URL) != "" {
			seen[NormalizeKey(profile.URL)] = true
		}
	}
	// 僅在沒有任何帳號/代理時才探測直連：全代理部署不應讓主機 IP 出現在匿名探針中。
	if !hasAccount && len(seen) == 0 {
		seen["direct"] = true
	}
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func (m *Monitor) probe(ctx context.Context, key string) {
	clientFor := m.ClientFor
	if clientFor == nil {
		clientFor = proxy.ClientFor
	}
	client, err := clientFor(key, 15*time.Second)
	if err != nil {
		HoldFor(key, probeShortHold, m.now())
		web.Warn("egress", fmt.Sprintf("出口 %s 建立客戶端失敗，短暫隔離: %v", key, err))
		return
	}
	switch m.probeMessages(ctx, client) {
	case probeEdgeBlocked:
		HoldFor(key, time.Duration(config.EdgeHoldSeconds)*time.Second, m.now())
		web.Warn("egress", fmt.Sprintf("出口 %s 匿名巡檢命中 WAF 攔截頁，提前熔斷", key))
		return
	case probeUnhealthy:
		HoldFor(key, probeShortHold, m.now())
		web.Warn("egress", fmt.Sprintf("出口 %s 匿名巡檢不可達，短暫隔離", key))
		return
	}
	if !m.probeAlicdn(ctx, client) {
		HoldFor(key, probeShortHold, m.now())
		web.Warn("egress", fmt.Sprintf("出口 %s 的 alicdn 連通性異常，短暫隔離", key))
	}
}

// probeMessages 以零憑證空 body 探測 messages 端點：401/400/403 均代表
// 出口可達；只有 405 且 body 命中 WAF 特徵頁才熔斷。
func (m *Monitor) probeMessages(ctx context.Context, client *http.Client) probeResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.MessagesURL, strings.NewReader("{}"))
	if err != nil {
		return probeUnhealthy
	}
	req.Header.Set("Content-Type", "application/json")
	// 刻意不帶 Authorization/Cookie/x-api-key/X-Device-Mid/User-Agent：
	// 探針不得與任何帳號產生關聯。
	resp, err := client.Do(req)
	if err != nil {
		return probeUnhealthy
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusMethodNotAllowed && isEdgePage(body) {
		return probeEdgeBlocked
	}
	if resp.StatusCode >= 500 {
		return probeUnhealthy
	}
	return probeHealthy
}

func (m *Monitor) probeAlicdn(ctx context.Context, client *http.Client) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.AlicdnURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Range", "bytes=0-65535")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent) && len(body) > 0
}

func isEdgePage(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "errors.aliyun.com") || strings.Contains(lower, "<html")
}

func (m *Monitor) now() time.Time {
	if m.Now == nil {
		return time.Now()
	}
	return m.Now().Round(0)
}

func (m *Monitor) loop() {
	defer close(m.done)
	select {
	case <-m.stop:
		return
	case <-time.After(30 * time.Second):
	}
	for {
		if m.Interval > 0 {
			m.RunOnce()
		}
		wait := m.Interval
		if wait <= 0 {
			wait = 10 * time.Minute
		}
		select {
		case <-m.stop:
			return
		case <-time.After(wait):
		}
	}
}
