package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/store"
)

// openStore 在临时路径打开存储（隔离配置变量，测试结束恢复并关闭）。
func openStore(t *testing.T) *store.Store {
	t.Helper()
	oldDB, oldAdmin, oldGW := config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	config.AdminKeyEnv = ""
	config.GatewayKeyEnv = ""
	t.Cleanup(func() {
		config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv = oldDB, oldAdmin, oldGW
	})
	s, err := store.New()
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// adminReq 构造后台校验请求（token 为空时不带 Authorization 头）。
func adminReq(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/admin/api/verify", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestVerifyAdminKeyAcceptsValidKey(t *testing.T) {
	svc := New(openStore(t))
	if e := svc.VerifyAdminKey(adminReq(svc.Store.AdminKey())); e != nil {
		t.Fatalf("有效密钥应通过: %+v", e)
	}
}

func TestVerifyAdminKeyMissingBearer(t *testing.T) {
	svc := New(openStore(t))
	e := svc.VerifyAdminKey(adminReq(""))
	if e == nil || e.Status != http.StatusUnauthorized || e.Message != "缺少鉴权凭证" {
		t.Fatalf("缺凭证应 401: %+v", e)
	}
	// 非 Bearer scheme 同样视为缺失
	r := adminReq(svc.Store.AdminKey())
	r.Header.Set("Authorization", "Basic abc")
	if e := svc.VerifyAdminKey(r); e == nil || e.Status != http.StatusUnauthorized {
		t.Fatalf("非 Bearer 应 401: %+v", e)
	}
}

func TestVerifyAdminKeyLimitsFailures(t *testing.T) {
	svc := New(openStore(t))
	key := svc.Store.AdminKey()

	// 窗口内连续失败 maxFailures 次内均为 401，此后一律 429（含正确密钥）
	for i := 0; i < maxFailures; i++ {
		e := svc.VerifyAdminKey(adminReq("wrong-" + fmt.Sprint(i)))
		if e == nil || e.Status != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应 401: %+v", i+1, e)
		}
	}
	if e := svc.VerifyAdminKey(adminReq(key)); e == nil || e.Status != http.StatusTooManyRequests {
		t.Fatalf("达到上限后正确密钥也应 429: %+v", e)
	}
	if e := svc.VerifyAdminKey(adminReq("wrong")); e == nil || e.Status != http.StatusTooManyRequests {
		t.Fatalf("达到上限后失败尝试应 429: %+v", e)
	}
}

func TestVerifyAdminKeySuccessResetsFailures(t *testing.T) {
	svc := New(openStore(t))
	key := svc.Store.AdminKey()
	for i := 0; i < maxFailures-1; i++ {
		_ = svc.VerifyAdminKey(adminReq("wrong"))
	}
	if e := svc.VerifyAdminKey(adminReq(key)); e != nil {
		t.Fatalf("成功应通过: %+v", e)
	}
	// 成功清零后可再承受同样次数的失败
	for i := 0; i < maxFailures-1; i++ {
		_ = svc.VerifyAdminKey(adminReq("wrong"))
	}
	if e := svc.VerifyAdminKey(adminReq(key)); e != nil {
		t.Fatalf("清零后不应提前限速: %+v", e)
	}
}

func TestVerifyGatewayKey(t *testing.T) {
	svc := New(openStore(t))
	key := svc.Store.GatewayKey()

	if e := svc.VerifyGatewayKey(httptest.NewRequest(http.MethodPost, "/v1/messages", nil)); e == nil || e.Status != http.StatusUnauthorized {
		t.Fatalf("缺凭证应 401: %+v", e)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	if e := svc.VerifyGatewayKey(r); e == nil || e.Status != http.StatusForbidden {
		t.Fatalf("错误密钥应 403: %+v", e)
	}
	r = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("x-api-key", key)
	if e := svc.VerifyGatewayKey(r); e != nil {
		t.Fatalf("x-api-key 应通过: %+v", e)
	}
}

// TestFailureTableSweepsExpiredEntries 失败计数表必须能回收过期条目。
//
// pruneLocked 只在同一 host 再次请求时触发；未鉴权的请求可以用大量不同
// 来源地址（IPv6 /64 逐请求换地址）把表撑到无界。超过阈值时应全表清理，
// 把已过窗口的条目删掉。
func TestFailureTableSweepsExpiredEntries(t *testing.T) {
	st := openStore(t)
	svc := New(st)

	old := time.Now().Add(-2 * failureWindow)
	svc.mu.Lock()
	for i := range failureSweepThreshold {
		host := fmt.Sprintf("10.0.%d.%d", i/256, i%256)
		svc.failures[host] = []time.Time{old} // 全部已过期
	}
	svc.mu.Unlock()

	svc.recordFailure("192.0.2.1", time.Now())

	svc.mu.Lock()
	remaining := len(svc.failures)
	svc.mu.Unlock()
	if remaining > 2 {
		t.Fatalf("过期条目应被清理，实际剩 %d 条", remaining)
	}
}

// ── 访客邀请码与配额 ────────────────────────────────────────────────────────

func guestReq(invite, remoteAddr string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/guest/api/start", nil)
	if invite != "" {
		r.Header.Set("x-invite-code", invite)
	}
	r.RemoteAddr = remoteAddr
	return r
}

// 未配置邀请码时必须拒绝——这个入口会写入账号池并触发真实上游调用，
// 宁可默认关闭也不能默认开放。
func TestVerifyInviteClosedWhenUnset(t *testing.T) {
	svc := New(openStore(t))
	e := svc.VerifyInvite(guestReq("anything", "203.0.113.7:1234"))
	if e == nil || e.Status != http.StatusServiceUnavailable {
		t.Fatalf("未配置邀请码应拒绝: %+v", e)
	}
}

func TestVerifyInviteAcceptsCorrectCode(t *testing.T) {
	st := openStore(t)
	svc := New(st)
	if err := svc.SetInviteCode("let-me-in"); err != nil {
		t.Fatal(err)
	}
	if e := svc.VerifyInvite(guestReq("let-me-in", "203.0.113.7:1234")); e != nil {
		t.Fatalf("正确邀请码应通过: %+v", e)
	}
	// 首尾空白被容忍：邀请码常从聊天工具复制，夹带空白是常态；
	// 拒绝它只会制造无谓的失败，不增加任何安全性。
	if e := svc.VerifyInvite(guestReq(" let-me-in ", "203.0.113.7:1234")); e != nil {
		t.Fatalf("首尾空白应被容忍: %+v", e)
	}
	// 但中间空白不宽容：那是不同的字符串
	if e := svc.VerifyInvite(guestReq("let me in", "203.0.113.7:1234")); e == nil {
		t.Fatal("中间含空白的邀请码不应通过")
	}
}

func TestVerifyInviteRejectsWrongCodeAndRateLimits(t *testing.T) {
	st := openStore(t)
	svc := New(st)
	if err := svc.SetInviteCode("right"); err != nil {
		t.Fatal(err)
	}
	const host = "198.51.100.9:5555"

	for i := range maxGuestFailures {
		e := svc.VerifyInvite(guestReq("wrong", host))
		if e == nil || e.Status != http.StatusForbidden {
			t.Fatalf("第 %d 次错误邀请码应 403: %+v", i, e)
		}
	}
	// 超过阈值后进入 429，且正确邀请码也一并拒绝（防在线爆破）
	e := svc.VerifyInvite(guestReq("right", host))
	if e == nil || e.Status != http.StatusTooManyRequests {
		t.Fatalf("超限后应 429: %+v", e)
	}
	// 其他来源不受影响
	if e := svc.VerifyInvite(guestReq("right", "198.51.100.10:5555")); e != nil {
		t.Fatalf("其他来源不应被牵连: %+v", e)
	}
}

// 成功校验应清空该来源的失败计数（与后台密钥同语义）。
func TestVerifyInviteSuccessClearsFailures(t *testing.T) {
	st := openStore(t)
	svc := New(st)
	if err := svc.SetInviteCode("right"); err != nil {
		t.Fatal(err)
	}
	const host = "198.51.100.11:5555"

	for range maxGuestFailures - 1 {
		_ = svc.VerifyInvite(guestReq("wrong", host))
	}
	if e := svc.VerifyInvite(guestReq("right", host)); e != nil {
		t.Fatalf("阈值内正确码应通过: %+v", e)
	}
	// 计数已清零：再来 maxGuestFailures-1 次错误仍不该触发 429
	for i := range maxGuestFailures - 1 {
		e := svc.VerifyInvite(guestReq("wrong", host))
		if e == nil || e.Status == http.StatusTooManyRequests {
			t.Fatalf("计数未清零，第 %d 次即超限: %+v", i, e)
		}
	}
}

// 每日配额：同一 IP 达到上限后拒绝，换 IP 恢复，跨日重置。
func TestAllowGuestSubmissionDailyQuota(t *testing.T) {
	svc := New(openStore(t))
	const host = "203.0.113.20:1234"

	for i := range guestDailyLimit {
		if !svc.AllowGuestSubmission(guestReq("", host)) {
			t.Fatalf("第 %d 次应在配额内", i+1)
		}
	}
	if svc.AllowGuestSubmission(guestReq("", host)) {
		t.Fatalf("超过每日上限（%d）应拒绝", guestDailyLimit)
	}
	if !svc.AllowGuestSubmission(guestReq("", "203.0.113.21:1234")) {
		t.Fatal("其他 IP 不应受同一配额限制")
	}

	// 跨日重置：把计数条目的日期改成昨天，再提交应放行
	svc.mu.Lock()
	entry := svc.guestCounts[ClientHost(guestReq("", host))]
	entry.day = "2000-01-01"
	svc.guestCounts[ClientHost(guestReq("", host))] = entry
	svc.mu.Unlock()

	if !svc.AllowGuestSubmission(guestReq("", host)) {
		t.Fatal("跨日后配额应重置")
	}
}

// 配额表按条目数阈值清理过期日期，避免长期运行后无界增长。
func TestGuestCountsSweepDropsStaleDays(t *testing.T) {
	svc := New(openStore(t))

	svc.mu.Lock()
	for i := range guestSweepThreshold {
		svc.guestCounts[fmt.Sprintf("10.%d.%d.1", i/256, i%256)] = guestCount{day: "2000-01-01", used: 1}
	}
	svc.mu.Unlock()

	// 触发一次写入（阈值命中时先清扫）
	if !svc.AllowGuestSubmission(guestReq("", "203.0.113.30:1234")) {
		t.Fatal("应放行")
	}

	svc.mu.Lock()
	remaining := len(svc.guestCounts)
	svc.mu.Unlock()
	if remaining > 2 {
		t.Fatalf("过期条目应被清理，实际剩 %d 条", remaining)
	}
}
