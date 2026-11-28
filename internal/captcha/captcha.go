// Package captcha 阿里云无痕验证码的配置获取、令牌缓存与求解器编排。
// 对应 Python 版 app/captcha.py。
//
// M1 范围：配置缓存（10 分钟）、人工回填令牌缓存（45 秒）、Solver 接口与失效语义；
// 浏览器求解器（rod 驱动 cloakbrowser 的 Chromium）在 M4 提供——此前 JWT 账号
// 依赖后台人工回填（/admin/captcha）兜底。令牌只在内存，不落盘、不进日志。
package captcha

import (
	"context"
	"encoding/base64"
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
	Enabled          bool
	SkipModelRequest bool // 上游 skip_model_request=true 時模型對話免帶驗證碼（領取套餐仍需驗）
	Prefix           string
	Region           string
	SceneID          string
}

// DefaultConfig 上游配置接口不可用时的兜底值（region 与线上实测一致；SkipModelRequest 必須維持 false）。
var DefaultConfig = Config{Enabled: true, Prefix: "no8xfe", Region: "cn", SceneID: "11xygtvd"}

const (
	defaultPrewarmTTL        = 55 * time.Second
	defaultCertifyDedupTTL   = 10 * time.Minute
	defaultIdleHibernate     = 15 * time.Minute
	defaultModelSkipBlockTTL = 10 * time.Minute
	maxUsedCertifyIDs        = 4096
)

type prewarmItem struct {
	tok       Token
	createdAt time.Time
	cfgKey    string
}

type solveOutcome struct {
	tok *Token
	err error
}

type inflightPrewarm struct {
	cfgKey  string
	done    chan solveOutcome
	claimed bool
}

// idleHibernator 支援閒置深度休眠的求解器介面（由 *BrowserSolver 實作）。
type idleHibernator interface {
	HibernateIfIdle() bool
}

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

// Manager 令牌緩存 + 配置緩存 + 求解器編排（含免驗動態回退、按出口預熱、certifyId 去重與閒置休眠）。
type Manager struct {
	solver         Solver
	configProvider ConfigProvider

	mu                    sync.Mutex
	cached                *Token
	cachedAt              time.Time
	cachedTTL             time.Duration
	configCache           *Config
	configCacheAt         time.Time
	forceModelVerifyUntil time.Time

	usedCertify    map[string]time.Time
	prewarmEnabled *bool
	prewarmTTL     time.Duration
	idleHibernate  time.Duration
	prewarmed      map[string]prewarmItem
	prewarming     map[string]*inflightPrewarm
	fgActive       int
	lastDemandAt   time.Time
	idleTimer      *time.Timer
	closed         bool
	closeCh        chan struct{}

	now func() time.Time
}

// NewManager 创建管理器。
func NewManager() *Manager {
	return &Manager{
		now:            time.Now,
		configProvider: fetchConfigHTTP,
		usedCertify:    map[string]time.Time{},
		prewarmTTL:     defaultPrewarmTTL,
		idleHibernate:  defaultIdleHibernate,
		prewarmed:      map[string]prewarmItem{},
		prewarming:     map[string]*inflightPrewarm{},
		closeCh:        make(chan struct{}),
	}
}

func (m *Manager) wallNow() time.Time {
	return m.now().Round(0)
}

func normalizeAddrKey(addrKey string) string {
	addrKey = strings.TrimSpace(addrKey)
	if addrKey == "" {
		return "direct"
	}
	return addrKey
}

func captchaConfigKey(cfg Config) string {
	return strings.Join([]string{
		strings.TrimSpace(cfg.SceneID),
		strings.TrimSpace(cfg.Region),
		strings.TrimSpace(cfg.Prefix),
	}, "|")
}

// SetSolver 注入浏览器求解器（M4）；nil 表示暂无。
func (m *Manager) SetSolver(s Solver) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.solver = s
}

// SetPrewarm 顯式啟用或停用按出口背景預熱（須由常駐服務入口顯式開啟並搭配 Close 收尾）。
func (m *Manager) SetPrewarm(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prewarmEnabled = &enabled
}

// SetPrewarmTTL 設定預熱票據有效期（測試可覆寫）。
func (m *Manager) SetPrewarmTTL(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d > 0 {
		m.prewarmTTL = d
	}
}

// SetIdleHibernate 設定無需驗流量進入深度休眠的閒置閾值（測試可覆寫）。
func (m *Manager) SetIdleHibernate(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d > 0 {
		m.idleHibernate = d
	}
}

func (m *Manager) shouldPrewarmLocked() bool {
	if m.closed || !config.CaptchaBrowserEnabled || m.solver == nil {
		return false
	}
	return m.prewarmEnabled != nil && *m.prewarmEnabled
}

// SetConfigProvider 注入配置获取函数（测试用）。
func (m *Manager) SetConfigProvider(p ConfigProvider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configProvider = p
}

// SetNow 注入时钟（测试用）。
func (m *Manager) SetNow(fn func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = fn
}

// FetchConfig 拉取上游验证码配置；成功缓存 10 分钟，失败回退默认值（不缓存失败）。
func (m *Manager) FetchConfig(ctx context.Context) Config {
	m.mu.Lock()
	if m.configCache != nil &&
		m.wallNow().Sub(m.configCacheAt) < time.Duration(config.CaptchaConfigCacheTTL)*time.Millisecond {
		cfg := *m.configCache
		m.mu.Unlock()
		return cfg
	}
	provider := m.configProvider
	m.mu.Unlock()

	cfg, err := provider(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		return DefaultConfig
	}
	m.configCache = &cfg
	m.configCacheAt = m.wallNow()
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
					Enabled           *bool  `json:"enabled"`
					SkipModelRequest  *bool  `json:"skip_model_request"`
					SkipModelRequestC *bool  `json:"skipModelRequest"`
					Prefix            string `json:"prefix"`
					Region            string `json:"region"`
					SceneID           string `json:"sceneId"`
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
	skipModel := (c.SkipModelRequest != nil && *c.SkipModelRequest) ||
		(c.SkipModelRequestC != nil && *c.SkipModelRequestC)
	return Config{
		Enabled:          c.Enabled == nil || *c.Enabled, // 對齊 Python：僅顯式 false 才禁用
		SkipModelRequest: skipModel,
		Prefix:           strings.TrimSpace(c.Prefix),
		Region:           strings.TrimSpace(c.Region),
		SceneID:          strings.TrimSpace(c.SceneID),
	}, nil
}

// GetVerifyParam 返回驗證碼令牌（保留既有介面，預設走領取套餐語義與 direct 出口）。
func (m *Manager) GetVerifyParam(ctx context.Context) (*Token, error) {
	return m.GetVerifyParamForClaim(ctx, "direct")
}

// GetVerifyParamForClaim 領取套餐專用驗證碼取得：忽略 skip_model_request，只要 enabled 即需驗證。
func (m *Manager) GetVerifyParamForClaim(ctx context.Context, addrKey string) (*Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if tok := m.manualToken(); tok != nil {
		return tok, nil
	}
	cfg := m.FetchConfig(ctx)
	if !cfg.Enabled {
		m.Invalidate()
		return nil, nil
	}
	return m.acquireVerifiedToken(ctx, normalizeAddrKey(addrKey), cfg)
}

// GetVerifyParamForModel 對話模型請求專用驗證碼取得：
// 當上游下發 skip_model_request=true 且未處於強制補驗期時直接回傳 (nil, nil) 免求解。
func (m *Manager) GetVerifyParamForModel(ctx context.Context, addrKey string, forceSolve bool) (*Token, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if tok := m.manualToken(); tok != nil {
		return tok, nil
	}
	cfg := m.FetchConfig(ctx)
	if !cfg.Enabled {
		m.Invalidate()
		return nil, nil
	}
	m.mu.Lock()
	skipBlocked := m.wallNow().Before(m.forceModelVerifyUntil)
	m.mu.Unlock()
	if !forceSolve && !skipBlocked && cfg.SkipModelRequest {
		return nil, nil
	}
	return m.acquireVerifiedToken(ctx, normalizeAddrKey(addrKey), cfg)
}

func (m *Manager) manualToken() *Token {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cached != nil && m.wallNow().Sub(m.cachedAt) < m.cachedTTL {
		t := *m.cached
		return &t
	}
	return nil
}

// acquireVerifiedToken 在確認需要驗證碼時執行前景取票、在途預熱徵用、certifyId 去重與背景補票。
func (m *Manager) acquireVerifiedToken(ctx context.Context, addrKey string, cfg Config) (*Token, error) {
	cfgKey := captchaConfigKey(cfg)

	m.mu.Lock()
	now := m.wallNow()
	m.lastDemandAt = now
	m.resetIdleTimerLocked()

	solver := m.solver
	if !config.CaptchaBrowserEnabled || solver == nil {
		m.mu.Unlock()
		return nil, ErrUnavailable
	}

	// 1) 命中同出口未過期預熱票據：立即彈出使用並於背景補充下一張
	if item, ok := m.prewarmed[addrKey]; ok {
		delete(m.prewarmed, addrKey)
		if item.cfgKey == cfgKey && now.Sub(item.createdAt) < m.prewarmTTL {
			tok := item.tok
			if m.shouldPrewarmLocked() && m.fgActive == 0 {
				m.startPrewarmLocked(addrKey, cfg, cfgKey)
			}
			m.mu.Unlock()
			return &tok, nil
		}
	}

	// 2) 同出口存在未被徵用的在途預熱：前景直接徵用其結果，免重複排隊
	if pw, ok := m.prewarming[addrKey]; ok && pw.cfgKey == cfgKey && !pw.claimed {
		pw.claimed = true
		m.fgActive++
		m.mu.Unlock()

		var outcome solveOutcome
		select {
		case outcome = <-pw.done:
		case <-ctx.Done():
			m.mu.Lock()
			pw.claimed = false
			m.fgActive--
			m.mu.Unlock()
			return nil, ctx.Err()
		}

		m.mu.Lock()
		m.fgActive--
		if outcome.err == nil && m.shouldPrewarmLocked() && m.fgActive == 0 {
			m.startPrewarmLocked(addrKey, cfg, cfgKey)
		}
		m.mu.Unlock()
		return outcome.tok, outcome.err
	}

	// 3) 無可用預熱：前景直接求解
	m.fgActive++
	m.mu.Unlock()

	param, err := m.solveWithDedup(ctx, solver, cfg)

	m.mu.Lock()
	m.fgActive--
	if err == nil && m.shouldPrewarmLocked() && m.fgActive == 0 {
		m.startPrewarmLocked(addrKey, cfg, cfgKey)
	}
	m.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return &Token{VerifyParam: param, Region: cfg.Region}, nil
}

// solveWithDedup 呼叫求解器並以 certifyId 去重防 F008 重放；非 JSON/Base64 測試字串原樣放行。
func (m *Manager) solveWithDedup(ctx context.Context, solver Solver, cfg Config) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		param, err := solver.Solve(ctx, cfg)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(param) == "" {
			return "", errors.New("真实浏览器验证码求解器返回空结果")
		}
		cid := extractCertifyID(param)
		if cid == "" {
			return param, nil
		}
		if m.claimCertifyID(cid) {
			return param, nil
		}
	}
	return "", errors.New("驗證碼 certifyId 重複，已攔截重放")
}

// claimCertifyID 檢查並登記 certifyId；首次出現回傳 true，TTL 內重複回傳 false。
func (m *Manager) claimCertifyID(cid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.wallNow()
	if exp, ok := m.usedCertify[cid]; ok && now.Before(exp) {
		return false
	}
	if len(m.usedCertify) >= maxUsedCertifyIDs {
		for k, exp := range m.usedCertify {
			if !now.Before(exp) {
				delete(m.usedCertify, k)
			}
		}
		if len(m.usedCertify) >= maxUsedCertifyIDs {
			for k := range m.usedCertify {
				delete(m.usedCertify, k)
				break
			}
		}
	}
	m.usedCertify[cid] = now.Add(defaultCertifyDedupTTL)
	return true
}

// extractCertifyID 從 verifyParam（原始 JSON 或 Base64 JSON）提取 certifyId；無法解析回傳空串。
func extractCertifyID(verifyParam string) string {
	raw := strings.TrimSpace(verifyParam)
	if raw == "" {
		return ""
	}
	parse := func(data []byte) string {
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			return ""
		}
		if cid, ok := obj["certifyId"].(string); ok {
			return strings.TrimSpace(cid)
		}
		return ""
	}
	if strings.HasPrefix(raw, "{") {
		return parse([]byte(raw))
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := enc.DecodeString(raw); err == nil {
			if cid := parse(decoded); cid != "" {
				return cid
			}
		}
	}
	return ""
}

// startPrewarmLocked 為指定出口啟動單飛背景預熱（呼叫方須持 m.mu）。
func (m *Manager) startPrewarmLocked(addrKey string, cfg Config, cfgKey string) {
	if _, busy := m.prewarming[addrKey]; busy {
		return
	}
	if item, ok := m.prewarmed[addrKey]; ok && item.cfgKey == cfgKey && m.wallNow().Sub(item.createdAt) < m.prewarmTTL {
		return
	}
	solver := m.solver
	closeCh := m.closeCh
	pw := &inflightPrewarm{
		cfgKey: cfgKey,
		done:   make(chan solveOutcome, 1),
	}
	m.prewarming[addrKey] = pw

	go func() {
		// 啟動瞬間若管理器已關閉，或尚未被前景徵用且已有前景請求在途，立即讓路放棄背景預熱；
		// 若已被前景徵用（pw.claimed），表示前景正等待本任務產出，必須繼續完成求解。
		m.mu.Lock()
		if m.closed || (!pw.claimed && m.fgActive > 0) {
			if m.prewarming[addrKey] == pw {
				delete(m.prewarming, addrKey)
			}
			claimed := pw.claimed
			m.mu.Unlock()
			if claimed {
				pw.done <- solveOutcome{err: ErrUnavailable}
			}
			close(pw.done)
			return
		}
		m.mu.Unlock()

		bgCtx, cancel := context.WithCancel(context.Background())
		stopWatch := make(chan struct{})
		go func() {
			select {
			case <-closeCh:
				cancel()
			case <-stopWatch:
			}
		}()

		param, err := m.solveWithDedup(bgCtx, solver, cfg)
		close(stopWatch)
		cancel()

		var outcome solveOutcome
		if err != nil {
			outcome = solveOutcome{err: err}
		} else {
			outcome = solveOutcome{tok: &Token{VerifyParam: param, Region: cfg.Region}}
		}

		m.mu.Lock()
		if m.prewarming[addrKey] == pw {
			delete(m.prewarming, addrKey)
		}
		claimed := pw.claimed
		if !claimed && err == nil && !m.closed {
			m.prewarmed[addrKey] = prewarmItem{
				tok:       *outcome.tok,
				createdAt: m.wallNow(),
				cfgKey:    cfgKey,
			}
		}
		m.mu.Unlock()

		if claimed {
			pw.done <- outcome
		}
		close(pw.done)
	}()
}

// resetIdleTimerLocked 重置無流量休眠計時器（呼叫方須持 m.mu）。
func (m *Manager) resetIdleTimerLocked() {
	if m.closed || m.idleHibernate <= 0 {
		return
	}
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	dur := m.idleHibernate
	m.idleTimer = time.AfterFunc(dur, m.onIdleTimeout)
}

func (m *Manager) onIdleTimeout() {
	m.mu.Lock()
	if m.closed || m.fgActive > 0 {
		m.mu.Unlock()
		return
	}
	if m.wallNow().Sub(m.lastDemandAt) < m.idleHibernate {
		m.mu.Unlock()
		return
	}
	clear(m.prewarmed)
	hib, ok := m.solver.(idleHibernator)
	m.mu.Unlock()
	if ok && hib != nil {
		hib.HibernateIfIdle()
	}
}

// InvalidateModelSkip 當免驗模型請求遭遇上游 3007 或挑戰頭時，
// 立即清空配置快取並啟動短期強制模型驗證，避免後續請求繼續免驗碰壁。
func (m *Manager) InvalidateModelSkip() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configCache = nil
	m.configCacheAt = time.Time{}
	m.forceModelVerifyUntil = m.wallNow().Add(defaultModelSkipBlockTTL)
	m.cached = nil
	m.cachedAt = time.Time{}
	m.cachedTTL = 0
}

// InvalidateForAddr 清空人工快取與指定出口的預熱票據（上游拒絕該出口驗證碼時呼叫）。
func (m *Manager) InvalidateForAddr(addrKey string) {
	addrKey = normalizeAddrKey(addrKey)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cached = nil
	m.cachedAt = time.Time{}
	m.cachedTTL = 0
	delete(m.prewarmed, addrKey)
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
	m.cachedAt = m.wallNow()
	m.cachedTTL = time.Duration(config.CaptchaManualCacheTTL) * time.Millisecond
	return nil
}

// Invalidate 清空缓存的令牌（上游拒绝验证码时调用）。
func (m *Manager) Invalidate() {
	m.InvalidateForAddr("direct")
}

// Close 释放求解器资源（浏览器池）並中止背景預熱與休眠計時器。
// 求解器的 Close 可能阻塞（池关闭需等待 worker 收尾），故在锁外调用，
// 避免阻塞并发的 GetVerifyParam。
func (m *Manager) Close() error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		close(m.closeCh)
	}
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	clear(m.prewarmed)
	s := m.solver
	m.mu.Unlock()
	if s != nil {
		return s.Close()
	}
	return nil
}
