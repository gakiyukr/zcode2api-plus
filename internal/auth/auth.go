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
)

// Service 密钥校验服务。
type Service struct {
	Store *store.Store

	mu       sync.Mutex
	failures map[string][]time.Time
}

// New 创建鉴权服务。
func New(st *store.Store) *Service {
	return &Service{Store: st, failures: map[string][]time.Time{}}
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
	host := clientHost(r)
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

// clientHost 提取来源 IP（限速键）。
func clientHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
