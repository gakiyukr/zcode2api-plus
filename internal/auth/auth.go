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

	mu       sync.Mutex
	failures map[string][]time.Time

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

	s.mu.Lock()
	attempts := s.pruneLocked(host, now)
	if len(attempts) >= maxFailures {
		s.mu.Unlock()
		return &AuthError{Status: http.StatusTooManyRequests, Message: "失败次数过多，请稍后再试"}
	}
	s.mu.Unlock()

	key := s.Store.AdminKey()
	if key == "" {
		return &AuthError{Status: http.StatusUnauthorized, Message: "未配置后台密钥"}
	}
	token := bearerToken(r)
	if token == "" {
		s.recordFailure(host, now)
		return &AuthError{Status: http.StatusUnauthorized, Message: "缺少鉴权凭证"}
	}
	if !hmac.Equal([]byte(token), []byte(key)) {
		s.recordFailure(host, now)
		return &AuthError{Status: http.StatusUnauthorized, Message: "鉴权凭证无效"}
	}

	s.mu.Lock()
	delete(s.failures, host)
	s.mu.Unlock()
	return nil
}

func (s *Service) pruneLocked(host string, now time.Time) []time.Time {
	attempts := s.failures[host]
	kept := attempts[:0]
	for _, t := range attempts {
		if now.Sub(t) <= failureWindow {
			kept = append(kept, t)
		}
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

// failureSweepThreshold 触发全表清理的条目数阈值。
// 限速窗口内正常客户端数量远低于此值。
const failureSweepThreshold = 4096

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
	guestSweepThreshold = 4096
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
// 两项配置都为空时整个校验被跳过——自建 Cap 实例的地址因部署而异，无法给出
// 合理默认值，所以默认关闭而不是默认指向某个占位地址。只填了其中一项则视为
// 配置不完整，由 capverify.Config.Enabled 判定为未启用。

// CapConfig 返回当前 Cap 校验配置。
func (s *Service) CapConfig() capverify.Config {
	endpoint, _ := s.Store.GetSetting("cap_endpoint")
	secret, _ := s.Store.GetSetting("cap_secret")
	return capverify.Config{Endpoint: endpoint, Secret: secret}
}

// SetCapConfig 保存 Cap 配置；空值表示停用人机验证。
func (s *Service) SetCapConfig(endpoint, secret string) error {
	if err := s.Store.SetSetting("cap_endpoint", strings.TrimSpace(endpoint)); err != nil {
		return err
	}
	return s.Store.SetSetting("cap_secret", strings.TrimSpace(secret))
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

	s.mu.Lock()
	attempts := s.pruneGuestLocked(host, now)
	if len(attempts) >= maxGuestFailures {
		s.mu.Unlock()
		return &AuthError{Status: http.StatusTooManyRequests, Message: "尝试次数过多，请稍后再试"}
	}
	s.mu.Unlock()

	token := strings.TrimSpace(r.Header.Get("x-invite-code"))
	if token == "" {
		s.recordGuestFailure(host, now)
		return &AuthError{Status: http.StatusUnauthorized, Message: "缺少邀请码"}
	}
	if !hmac.Equal([]byte(token), []byte(code)) {
		s.recordGuestFailure(host, now)
		return &AuthError{Status: http.StatusForbidden, Message: "邀请码无效"}
	}

	s.mu.Lock()
	delete(s.guestFailures, host)
	s.mu.Unlock()
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

	if len(s.guestCounts) >= guestSweepThreshold {
		s.sweepGuestCountsLocked(day)
	}
	entry := s.guestCounts[host]
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
	attempts := s.guestFailures[host]
	kept := attempts[:0]
	for _, t := range attempts {
		if now.Sub(t) <= failureWindow {
			kept = append(kept, t)
		}
	}
	s.guestFailures[host] = kept
	return kept
}

func (s *Service) recordGuestFailure(host string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.guestFailures) >= failureSweepThreshold {
		s.sweepGuestFailuresLocked(now)
	}
	s.guestFailures[host] = append(s.pruneGuestLocked(host, now), now)
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

func (s *Service) recordFailure(host string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.failures) >= failureSweepThreshold {
		s.sweepLocked(now)
	}
	s.failures[host] = append(s.pruneLocked(host, now), now)
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
