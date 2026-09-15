// Package captcha 阿里云无痕验证码的配置获取、令牌缓存与求解器编排。
// 对应 Python 版 app/captcha.py。
//
// M1 范围：配置缓存（10 分钟）、人工回填令牌缓存（45 秒）、Solver 接口与失效语义；
// 浏览器求解器（rod 驱动 cloakbrowser 的 Chromium）在 M4 提供——此前 JWT 账号
// 依赖后台人工回填（/admin/captcha）兜底。令牌只在内存，不落盘、不进日志。
package captcha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/config"
)

// Token 一次无痕验证的结果。
type Token struct {
	VerifyParam string // base64(JSON{certifyId, sceneId, isSign, securityToken})
	Region      string // 请求头 X-Aliyun-Captcha-Verify-Region 所需
}

// Config 上游下发的验证码配置。
type Config struct {
	Enabled bool
	Prefix  string
	Region  string
	SceneID string
}

// DefaultConfig 上游配置接口不可用时的兜底值（region 与线上实测一致）。
var DefaultConfig = Config{Enabled: true, Prefix: "no8xfe", Region: "cn", SceneID: "11xygtvd"}

// ErrUnavailable 无可用求解器（M4 前的常态；依赖人工回填兜底）。
var ErrUnavailable = errors.New("验证码求解器不可用（等待浏览器池或后台人工回填）")

// Solver 真实验证码求解器接口；M4 由 rod 浏览器池实现。
type Solver interface {
	Solve(ctx context.Context, cfg Config) (string, error)
	Close() error
}

// ConfigProvider 配置获取函数（默认请求上游接口；测试可覆写以避免触网）。
type ConfigProvider func(ctx context.Context) (Config, error)

// configURL 验证码配置端点（测试可覆写）。
var configURL = "https://zcode.z.ai/api/v1/client/configs"

// Manager 令牌缓存 + 配置缓存 + 求解器编排。
type Manager struct {
	solver         Solver
	configProvider ConfigProvider

	mu            sync.Mutex
	cached        *Token
	cachedAt      time.Time
	cachedTTL     time.Duration
	configCache   *Config
	configCacheAt time.Time

	now func() time.Time
}

// NewManager 创建管理器。
func NewManager() *Manager {
	return &Manager{now: time.Now, configProvider: fetchConfigHTTP}
}

// SetSolver 注入浏览器求解器（M4）；nil 表示暂无。
func (m *Manager) SetSolver(s Solver) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.solver = s
}

// SetConfigProvider 注入配置获取函数（测试用）。
func (m *Manager) SetConfigProvider(p ConfigProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configProvider = p
}

// SetNow 注入时钟（测试用）。
func (m *Manager) SetNow(fn func() time.Time) { m.now = fn }

// FetchConfig 拉取上游验证码配置；成功缓存 10 分钟，失败回退默认值（不缓存失败）。
func (m *Manager) FetchConfig(ctx context.Context) Config {
	m.mu.Lock()
	if m.configCache != nil &&
		m.now().Sub(m.configCacheAt) < time.Duration(config.CaptchaConfigCacheTTL)*time.Millisecond {
		cfg := *m.configCache
		m.mu.Unlock()
		return cfg
	}
	m.mu.Unlock()

	cfg, err := m.configProvider(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		return DefaultConfig
	}
	m.configCache = &cfg
	m.configCacheAt = m.now()
	return cfg
}

func fetchConfigHTTP(ctx context.Context) (Config, error) {
	query := fmt.Sprintf("app_version=%s&platform=%s",
		config.ZcodeClientVersion, config.ZcodeClientPlatform)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, configURL+"?"+query, nil)
	if err != nil {
		return Config{}, err
	}
	req.Header.Set("User-Agent", config.UserAgent)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Config{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Config{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data struct {
			Configs struct {
				Captcha *struct {
					Enabled *bool  `json:"enabled"`
					Prefix  string `json:"prefix"`
					Region  string `json:"region"`
					SceneID string `json:"sceneId"`
				} `json:"captcha"`
			} `json:"configs"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return Config{}, err
	}
	c := payload.Data.Configs.Captcha
	if c == nil {
		return Config{}, errors.New("上游配置缺少 captcha 对象")
	}
	return Config{
		Enabled: c.Enabled == nil || *c.Enabled, // 对齐 Python：仅显式 false 才禁用
		Prefix:  strings.TrimSpace(c.Prefix),
		Region:  strings.TrimSpace(c.Region),
		SceneID: strings.TrimSpace(c.SceneID),
	}, nil
}

// GetVerifyParam 返回验证码令牌。
// 语义对齐 Python 版：缓存命中直接返回；配置禁用返回 (nil, nil)（无需验证码）；
// 浏览器求解的令牌可能是一次性的，不写缓存；人工回填令牌按 TTL 复用。
func (m *Manager) GetVerifyParam(ctx context.Context) (*Token, error) {
	if ctx == nil {
		ctx = context.Background() // 允许 nil ctx（claim 等后台调用）
	}
	m.mu.Lock()
	if m.cached != nil && m.now().Sub(m.cachedAt) < m.cachedTTL {
		t := *m.cached
		m.mu.Unlock()
		return &t, nil
	}
	m.mu.Unlock()

	cfg := m.FetchConfig(ctx)
	if !cfg.Enabled {
		m.Invalidate()
		return nil, nil
	}

	// 浏览器求解路径
	m.mu.Lock()
	solver := m.solver
	m.mu.Unlock()
	if config.CaptchaBrowserEnabled && solver != nil {
		param, err := solver.Solve(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(param) == "" {
			return nil, errors.New("真实浏览器验证码求解器返回空结果")
		}
		return &Token{VerifyParam: param, Region: cfg.Region}, nil
	}

	return nil, ErrUnavailable
}

// SetManualParam 缓存人工回填的令牌（后台 /admin/captcha 提交），短期复用。
func (m *Manager) SetManualParam(param, region string) error {
	param = strings.TrimSpace(param)
	if param == "" {
		return errors.New("verify_param 不能为空")
	}
	if region == "" {
		region = m.FetchConfig(context.Background()).Region
	}
	if region == "" {
		region = DefaultConfig.Region
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cached = &Token{VerifyParam: param, Region: region}
	m.cachedAt = m.now()
	m.cachedTTL = time.Duration(config.CaptchaManualCacheTTL) * time.Millisecond
	return nil
}

// Invalidate 清空缓存的令牌（上游拒绝验证码时调用）。
func (m *Manager) Invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cached = nil
	m.cachedAt = time.Time{}
	m.cachedTTL = 0
}

// Close 释放求解器资源（浏览器池）。
// 求解器的 Close 可能阻塞（池关闭需等待 worker 收尾），故在锁外调用，
// 避免阻塞并发的 GetVerifyParam。
func (m *Manager) Close() error {
	m.mu.Lock()
	s := m.solver
	m.mu.Unlock()
	if s != nil {
		return s.Close()
	}
	return nil
}
