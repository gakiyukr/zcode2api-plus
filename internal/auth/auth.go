// Package auth 后台 / 网关密钥校验。对应 Python 版 app/auth_admin.py。
// 网关密钥一律必填（fail closed）；后台密钥带单 IP 失败限速
// （5 分钟窗口 10 次失败 → 一律 429，成功清零）。
package auth

import (
	"crypto/hmac"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/capverify"
	"zcode2api/internal/store"
)

// AuthError 携带 HTTP 状态码的鉴权错误。
type AuthError struct {
	Status  int
	Message string
}

func (e *AuthError) Error() string { return e.Message }

const (
	failureWindow = 300 * time.Second
	maxFailures   = 10
	// maxGuestFailures 邀请码试错上限（同一窗口内）。
	maxGuestFailures = 10
)

// Service 密钥校验服务。
type Service struct {
	Store *store.Store

	mu        sync.Mutex
	failures  map[string][]time.Time
	lastSweep time.Time

	// guestFailures 邀请码试错的滑动窗；guestCounts 每日提交配额。
	guestFailures map[string][]time.Time
	guestCounts   map[string]guestCount
}

// New 创建鉴权服务。
func New(st *store.Store) *Service {
	return &Service{
		Store:         st,
		failures:      map[string][]time.Time{},
		guestFailures: map[string][]time.Time{},
		guestCounts:   map[string]guestCount{},
	}
}

// VerifyGatewayKey 网关密钥校验：接受 Authorization: Bearer 或 x-api-key。
func (s *Service) VerifyGatewayKey(r *http.Request) *AuthError {
	key := s.Store.GatewayKey()
	if key == "" {
		// 仅在 meta 表被手动清空时触达；宁可拒绝服务也不放行未鉴权流量
		return &AuthError{Status: http.StatusServiceUnavailable, Message: "网关未配置 API Key，请在后台设置"}
	}
	token := bearerToken(r)
	if token == "" {
		token = r.Header.Get("x-api-key")
	}
	if token == "" {
		return &AuthError{Status: http.StatusUnauthorized, Message: "缺少 API Key"}
	}
	if !hmac.Equal([]byte(token), []byte(key)) {
		return &AuthError{Status: http.StatusForbidden, Message: "API Key 无效"}
	}
	return nil
}

// VerifyAdminKey 后台密钥校验：仅接受 Authorization: Bearer 头。
// 旧版 `?app_key=` 查询参数已移除（密钥会落入反向代理与访问日志）。
func (s *Service) VerifyAdminKey(r *http.Request) *AuthError {
	host := ClientHost(r)
	now := time.Now()
	key := s.Store.AdminKey()
	token := bearerToken(r)

	// 在單一臨界區內原子完成「限速檢查 → 憑證比對 → 記錄/清除失敗」，消除併發 check-then-act 窗口
	s.mu.Lock()
	defer s.mu.Unlock()

	attempts := s.pruneLocked(host, now)
	if len(attempts) >= maxFailures {
		return &AuthError{Status: http.StatusTooManyRequests, Message: "失败次数过多，请稍后再试"}
	}
	if key == "" {
		return &AuthError{Status: http.StatusUnauthorized, Message: "未配置后台密钥"}
	}
	if token == "" {
		s.recordFailureLocked(host, now)
		return &AuthError{Status: http.StatusUnauthorized, Message: "缺少鉴权凭证"}
	}
	if !hmac.Equal([]byte(token), []byte(key)) {
		s.recordFailureLocked(host, now)
		return &AuthError{Status: http.StatusUnauthorized, Message: "鉴权凭证无效"}
	}

	delete(s.failures, host)
	return nil
}

func (s *Service) pruneLocked(host string, now time.Time) []time.Time {
	attempts, ok := s.failures[host]
	if !ok {
		return nil
	}
	kept := attempts[:0]
	for _, t := range attempts {
		if now.Sub(t) <= failureWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(s.failures, host)
		return nil
	}
	s.failures[host] = kept
	return kept
}

// sweepLocked 清理全表中已过窗口的条目。
//
// pruneLocked 只在「同一 host 再次请求」时触发，未鉴权的请求可以用大量不同
// 来源地址（IPv6 /64 逐请求换地址）把这张表撑到无界。按 host 数阈值触发一次
// 全表扫描，把内存收回。
func (s *Service) sweepLocked(now time.Time) {
	for host, attempts := range s.failures {
		kept := attempts[:0]
		for _, t := range attempts {
			if now.Sub(t) <= failureWindow {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(s.failures, host)
			continue
		}
		s.failures[host] = kept
	}
}

// failureSweepThreshold 觸發全表清理的條目數閾值；maxTrackedHosts 為防 OOM 的來源數硬上限；
// minSweepInterval 限制全表掃描最低間隔，防止高併發下每請求重複 O(N) 掃描。
const (
	failureSweepThreshold = 1024
	sweepInterval         = 10 * time.Minute
	maxTrackedHosts       = 4096
	minSweepInterval      = 10 * time.Second
)

// ── 访客提交（/guest/*）─────────────────────────────────────────────────────
//
// 访客入口只做两件事：校验邀请码、按来源 IP 限流。它与后台密钥是两套独立
// 凭据——邀请码泄露不会危及后台，轮换邀请码也不影响管理员登录。
//
// 限流按「来源 IP + 自然日」计数，不用 VerifyAdminKey 的失败窗口：访客入口
// 的正常请求也要计数（防的是刷量而非猜密码）。注意 IPv6 可按 /64 逐请求换
// 地址，所以限流只能抬高成本，真正的防线是 OAuth 授权与入池前的实测。

const (
	// guestDailyLimit 每个来源 IP 每日可提交的账号数。
	guestDailyLimit = 3
	// guestSweepThreshold 触发全表清理的条目数阈值。
	guestSweepThreshold = 1024
)

// InviteCode 当前邀请码（空表示访客入口关闭）。
func (s *Service) InviteCode() string {
	v, _ := s.Store.GetSetting("guest_invite_code")
	return v
}

// SetInviteCode 设置邀请码并落库；空值即关闭访客入口。
func (s *Service) SetInviteCode(code string) error {
	return s.Store.SetSetting("guest_invite_code", code)
}

// ── 人机验证（Cap）────────────────────────────────────────────────────────
//
// 三个值分开存放，对应 Cap 后台给出的三项（实例地址 / site key / secret key）。
// 全部为空时整个校验被跳过——自建实例地址因部署而异，无法给出合理默认值，
// 所以默认关闭而不是指向某个占位地址。缺任意一项视为配置不完整，
// 由 capverify.Config.Enabled 判定为未启用。

// CapConfig 返回当前 Cap 校验配置。
func (s *Service) CapConfig() capverify.Config {
	instance, _ := s.Store.GetSetting("cap_instance")
	siteKey, _ := s.Store.GetSetting("cap_site_key")
	secret, _ := s.Store.GetSetting("cap_secret")
	return capverify.Config{Instance: instance, SiteKey: siteKey, Secret: secret}
}

// SetCapConfig 保存 Cap 配置；三项皆空表示停用人机验证。
func (s *Service) SetCapConfig(instance, siteKey, secret string) error {
	for _, kv := range []struct{ key, value string }{
		{"cap_instance", instance},
		{"cap_site_key", siteKey},
		{"cap_secret", secret},
	} {
		if err := s.Store.SetSetting(kv.key, strings.TrimSpace(kv.value)); err != nil {
			return err
		}
	}
	return nil
}

// VerifyInvite 校验访客提交的邀请码。
//
// 邀请码未配置时一律拒绝（fail closed）：与其默认开放，不如让管理员显式
// 开启——这个入口会写入账号池并触发真实上游调用。
func (s *Service) VerifyInvite(r *http.Request) *AuthError {
	code := s.InviteCode()
	if code == "" {
		return &AuthError{Status: http.StatusServiceUnavailable, Message: "访客提交未开放"}
	}
	host := ClientHost(r)
	now := time.Now()
	token := strings.TrimSpace(r.Header.Get("x-invite-code"))

	// 在單一臨界區內原子完成「限速檢查 → 邀請碼比對 → 記錄/清除失敗」，消除併發 check-then-act 窗口
	s.mu.Lock()
	defer s.mu.Unlock()

	attempts := s.pruneGuestLocked(host, now)
	if len(attempts) >= maxGuestFailures {
		return &AuthError{Status: http.StatusTooManyRequests, Message: "尝试次数过多，请稍后再试"}
	}
	if token == "" {
		s.recordGuestFailureLocked(host, now)
		return &AuthError{Status: http.StatusUnauthorized, Message: "缺少邀请码"}
	}
	if !hmac.Equal([]byte(token), []byte(code)) {
		s.recordGuestFailureLocked(host, now)
		return &AuthError{Status: http.StatusForbidden, Message: "邀请码无效"}
	}

	delete(s.guestFailures, host)
	return nil
}

// AllowGuestSubmission 按来源 IP 的每日配额放行一次提交。
// 返回 false 表示当日额度已用完。
func (s *Service) AllowGuestSubmission(r *http.Request) bool {
	host := ClientHost(r)
	now := time.Now()
	day := now.Format("2006-01-02")

	s.mu.Lock()
	defer s.mu.Unlock()

	needSweep := len(s.guestCounts) >= guestSweepThreshold || now.Sub(s.lastSweep) > sweepInterval
	if needSweep && now.Sub(s.lastSweep) >= minSweepInterval {
		s.sweepAllLocked(now, day)
	}
	entry, exists := s.guestCounts[host]
	if !exists && len(s.guestCounts) >= maxTrackedHosts {
		// 達硬上限且無過期可清時，先淘汰一筆舊鍵封頂容量，避免單 IP 請求被誤拒或表無界成長
		for k := range s.guestCounts {
			delete(s.guestCounts, k)
			break
		}
	}
	if entry.day != day {
		entry = guestCount{day: day}
	}
	if entry.used >= guestDailyLimit {
		s.guestCounts[host] = entry
		return false
	}
	entry.used++
	s.guestCounts[host] = entry
	return true
}

// guestCount 某来源 IP 在某日的提交计数。
type guestCount struct {
	day  string
	used int
}

func (s *Service) pruneGuestLocked(host string, now time.Time) []time.Time {
	attempts, ok := s.guestFailures[host]
	if !ok {
		return nil
	}
	kept := attempts[:0]
	for _, t := range attempts {
		if now.Sub(t) <= failureWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(s.guestFailures, host)
		return nil
	}
	s.guestFailures[host] = kept
	return kept
}

func (s *Service) recordGuestFailureLocked(host string, now time.Time) {
	needSweep := len(s.guestFailures) >= guestSweepThreshold || now.Sub(s.lastSweep) > sweepInterval
	if needSweep && now.Sub(s.lastSweep) >= minSweepInterval {
		s.sweepAllLocked(now, now.Format("2006-01-02"))
	}
	kept := s.pruneGuestLocked(host, now)
	if len(kept) == 0 && len(s.guestFailures) >= maxTrackedHosts {
		// 表滿時優先淘汰一筆過期鍵，無過期鍵則淘汰一筆既有鍵，既封頂容量亦不放行新攻擊 IP
		evicted := false
		for k, ts := range s.guestFailures {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > failureWindow {
				delete(s.guestFailures, k)
				evicted = true
				break
			}
		}
		if !evicted {
			for k := range s.guestFailures {
				delete(s.guestFailures, k)
				break
			}
		}
	}
	s.guestFailures[host] = append(kept, now)
}

func (s *Service) sweepGuestFailuresLocked(now time.Time) {
	for host, attempts := range s.guestFailures {
		kept := attempts[:0]
		for _, t := range attempts {
			if now.Sub(t) <= failureWindow {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(s.guestFailures, host)
			continue
		}
		s.guestFailures[host] = kept
	}
}

// sweepGuestCountsLocked 丢弃非今日的计数条目（跨日后自然清零）。
func (s *Service) sweepGuestCountsLocked(today string) {
	for host, entry := range s.guestCounts {
		if entry.day != today {
			delete(s.guestCounts, host)
		}
	}
}

// sweepAllLocked 一次清理全部三張限速／配額表的過期條目，防止低流量下長期累積。
func (s *Service) sweepAllLocked(now time.Time, today string) {
	s.sweepLocked(now)
	s.sweepGuestFailuresLocked(now)
	s.sweepGuestCountsLocked(today)
	s.lastSweep = now
}

func (s *Service) recordFailure(host string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordFailureLocked(host, now)
}

func (s *Service) recordFailureLocked(host string, now time.Time) {
	needSweep := len(s.failures) >= failureSweepThreshold || now.Sub(s.lastSweep) > sweepInterval
	if needSweep && now.Sub(s.lastSweep) >= minSweepInterval {
		s.sweepAllLocked(now, now.Format("2006-01-02"))
	}
	kept := s.pruneLocked(host, now)
	if len(kept) == 0 && len(s.failures) >= maxTrackedHosts {
		// 表滿時優先淘汰一筆過期鍵，無過期鍵則淘汰一筆既有鍵，既封頂容量亦不放行新攻擊 IP
		evicted := false
		for k, ts := range s.failures {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > failureWindow {
				delete(s.failures, k)
				evicted = true
				break
			}
		}
		if !evicted {
			for k := range s.failures {
				delete(s.failures, k)
				break
			}
		}
	}
	s.failures[host] = append(kept, now)
}

// bearerToken 提取 Authorization: Bearer <token>；非 Bearer 或缺失返回空。
func bearerToken(r *http.Request) string {
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	return token
}

// ClientHost 提取来源 IP（限速键）。
// 导出供访客入口复用：限速与配额必须用同一套 IP 判定，否则同一个人
// 可能在两条路径上被算作不同来源。
func ClientHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
