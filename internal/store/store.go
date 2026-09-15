// Package store 账号与设置的持久化存储（SQLite）。
// 对应 Python 版 app/store.py；schema 与 Python 版完全一致，
// 两个版本可互读同一个 data/accounts.db（Account JSON 字段契约见 internal/model）。
//
// 运行期账号对象常驻内存（保证轮询游标与状态实时性），
// 每次变更同步落库；进程启动时从 SQLite 读取快照。
package store

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
)

const (
	legacyAdminKey = "zcode" // 2.0.1 之前发布的固定默认后台密码，升级时强制轮换

	metaTable     = "meta"
	accountsTable = "accounts"
)

// Providers 支持的提供商（与 Python 版 PROVIDERS 一致）。
var Providers = []string{model.ProviderZai}

// ErrNotFound 代理配置等条目不存在。
var ErrNotFound = errors.New("条目不存在")

// ErrProxyNotFound 指派账号时引用的代理线路不存在（对齐 Python 版 ValueError）。
var ErrProxyNotFound = errors.New("代理配置不存在")

// Store 线程安全的账号 / 设置存储，含轮询游标。
type Store struct {
	mu       sync.Mutex
	db       *sql.DB
	accounts map[string][]*model.Account
	settings map[string]string
	rotation map[string]int

	// GeneratedAdminKey / GeneratedGatewayKey：本次启动随机生成/轮换的密钥，
	// 供启动横幅提示管理者（环境变量配置时不记录）。
	GeneratedAdminKey   string
	GeneratedGatewayKey string
}

// New 打开（必要时创建）数据库并加载快照。
func New() (*Store, error) {
	db, err := openDB(config.DBPath)
	if err != nil {
		return nil, err
	}
	s := &Store{
		db:       db,
		accounts: map[string][]*model.Account{model.ProviderZai: {}},
		settings: map[string]string{},
		rotation: map[string]int{},
	}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error { return s.db.Close() }

func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// _pragma 参数对每个新连接生效；配合 MaxOpenConns(1) 即单连接 + WAL 语义，
	// 对齐 Python 版「进程内复用单个连接」的形态。
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (s *Store) init() error {
	if _, err := s.db.Exec(fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS %s (
			id          TEXT PRIMARY KEY,
			provider    TEXT NOT NULL,
			name        TEXT,
			mode        TEXT,
			status      TEXT,
			enabled     INTEGER NOT NULL DEFAULT 1,
			created_at  REAL,
			data        TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_acc_provider ON %s (provider);
		CREATE INDEX IF NOT EXISTS idx_acc_status   ON %s (status);
	`, metaTable, accountsTable, accountsTable, accountsTable)); err != nil {
		return err
	}
	if err := s.bootstrapAuthKeys(); err != nil {
		return err
	}
	return s.load()
}

// bootstrapAuthKeys 初始化后台密码与网关 API Key，保证两者永不为空、永不為固定預設值。
// - 环境变量显式配置时，用于替换缺失值或历史默认值（写入后仍以数据库为准）；
// - 否则随机生成，并记录到 Generated* 字段供启动横幅展示；
// - 历史版本写死的管理密码「zcode」在升级时强制轮换。
func (s *Store) bootstrapAuthKeys() error {
	existing := map[string]string{}
	rows, err := s.db.Query(
		fmt.Sprintf("SELECT key, value FROM %s WHERE key IN ('admin_key', 'gateway_key')", metaTable))
	if err != nil {
		return err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		existing[k] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	adminKey := existing["admin_key"]
	if adminKey == "" || adminKey == legacyAdminKey {
		if config.AdminKeyEnv != "" {
			adminKey = config.AdminKeyEnv
		} else {
			adminKey = randomTokenURLSafe(24)
			s.GeneratedAdminKey = adminKey
		}
		if err := s.setMeta("admin_key", adminKey); err != nil {
			return err
		}
	}

	gatewayKey := existing["gateway_key"]
	if gatewayKey == "" {
		if config.GatewayKeyEnv != "" {
			gatewayKey = config.GatewayKeyEnv
		} else {
			gatewayKey = "sk-" + randomTokenURLSafe(24)
			s.GeneratedGatewayKey = gatewayKey
		}
		if err := s.setMeta("gateway_key", gatewayKey); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) load() error {
	metaRows := map[string]string{}
	rows, err := s.db.Query(fmt.Sprintf("SELECT key, value FROM %s", metaTable))
	if err != nil {
		return err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		metaRows[k] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	settings := metaRows
	// 密钥由 bootstrapAuthKeys 保证存在；此处缺省空值即拒绝鉴权（fail closed）
	if _, ok := settings["admin_key"]; !ok {
		settings["admin_key"] = ""
	}
	if _, ok := settings["gateway_key"]; !ok {
		settings["gateway_key"] = ""
	}
	if _, ok := settings["quota_refresh_interval"]; !ok {
		settings["quota_refresh_interval"] = strconv.Itoa(config.QuotaRefreshInterval)
	}
	s.settings = settings

	accounts := map[string][]*model.Account{model.ProviderZai: {}}
	rows, err = s.db.Query(fmt.Sprintf(
		"SELECT data FROM %s ORDER BY created_at ASC", accountsTable))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return err
		}
		acc, err := model.FromJSON([]byte(data))
		if err != nil {
			continue // 坏行跳过（对齐 Python 版）
		}
		if _, ok := accounts[acc.Provider]; ok {
			accounts[acc.Provider] = append(accounts[acc.Provider], acc)
		}
	}
	s.accounts = accounts
	return rows.Err()
}

// ── 持久化（调用方须持有 s.mu）──────────────────────────────────────────────

func (s *Store) persistAccountLocked(acc *model.Account) error {
	data, err := marshalJSON(acc)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		fmt.Sprintf(`INSERT OR REPLACE INTO %s
			(id, provider, name, mode, status, enabled, created_at, data)
			VALUES (?,?,?,?,?,?,?,?)`, accountsTable),
		acc.ID, acc.Provider, acc.Name, acc.Mode, acc.Status, boolToInt(acc.Enabled), acc.CreatedAt, string(data))
	return err
}

func (s *Store) deleteAccountLocked(id string) error {
	_, err := s.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE id = ?", accountsTable), id)
	return err
}

func (s *Store) setMeta(key, value string) error {
	_, err := s.db.Exec(
		fmt.Sprintf("INSERT OR REPLACE INTO %s (key, value) VALUES (?, ?)", metaTable), key, value)
	return err
}

// marshalJSON 与 Python json.dumps(ensure_ascii=False) 对齐：不转义 HTML 字符。
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ── 设置 ────────────────────────────────────────────────────────────────────

// GetSetting 读取设置（第二返回值表示是否存在）。
func (s *Store) GetSetting(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.settings[key]
	return v, ok
}

// SetSetting 更新设置并落库。
func (s *Store) SetSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings[key] = value
	return s.setMeta(key, value)
}

func (s *Store) AdminKey() string {
	v, _ := s.GetSetting("admin_key")
	return v
}

func (s *Store) GatewayKey() string {
	v, _ := s.GetSetting("gateway_key")
	return v
}

// QuotaRefreshInterval 额度刷新间隔（秒，非负；非法值回退默认）。
func (s *Store) QuotaRefreshInterval() int {
	v, ok := s.GetSetting("quota_refresh_interval")
	if !ok {
		return config.QuotaRefreshInterval
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return config.QuotaRefreshInterval
	}
	return max(0, n)
}

// ── 代理設定 ────────────────────────────────────────────────────────────────

// ProxyProfile 命名代理出口（設定以 JSON 儲存在 meta 表中）。
type ProxyProfile struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

// ListProxyProfiles 列出全部代理线路。
func (s *Store) ListProxyProfiles() []ProxyProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listProxyProfilesLocked()
}

func (s *Store) listProxyProfilesLocked() []ProxyProfile {
	out := []ProxyProfile{}
	raw := s.settings["proxy_profiles"]
	if raw == "" {
		return out
	}
	var profiles []ProxyProfile
	if err := json.Unmarshal([]byte(raw), &profiles); err != nil {
		return out
	}
	for _, p := range profiles {
		if p.ID != "" && p.URL != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Store) saveProxyProfilesLocked(profiles []ProxyProfile) error {
	data, err := marshalJSON(profiles)
	if err != nil {
		return err
	}
	s.settings["proxy_profiles"] = string(data)
	return s.setMeta("proxy_profiles", string(data))
}

// AddProxyProfile 新增命名代理出口。
func (s *Store) AddProxyProfile(name, url string, enabled bool) (ProxyProfile, error) {
	normalized, err := proxy.NormalizeProxyURL(url)
	if err != nil {
		return ProxyProfile{}, err
	}
	if normalized == nil {
		return ProxyProfile{}, errors.New("代理 URL 不能為空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	profiles := s.listProxyProfilesLocked()
	name = strings.TrimSpace(name)
	if name == "" {
		name = fmt.Sprintf("代理-%d", len(profiles)+1)
	}
	for _, p := range profiles {
		if p.Name == name {
			return ProxyProfile{}, errors.New("代理名稱已存在")
		}
	}
	profile := ProxyProfile{ID: "proxy-" + randomHex(4), Name: name, URL: *normalized, Enabled: enabled}
	profiles = append(profiles, profile)
	return profile, s.saveProxyProfilesLocked(profiles)
}

// UpdateProxyProfile 更新代理线路；同步指派了该线路的账号。
func (s *Store) UpdateProxyProfile(profileID, name, url string, enabled bool) (ProxyProfile, error) {
	normalized, err := proxy.NormalizeProxyURL(url)
	if err != nil {
		return ProxyProfile{}, err
	}
	if normalized == nil {
		return ProxyProfile{}, errors.New("代理 URL 不能為空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	profiles := s.listProxyProfilesLocked()
	target := -1
	for i, p := range profiles {
		if p.ID == profileID {
			target = i
			break
		}
	}
	if target < 0 {
		return ProxyProfile{}, ErrNotFound
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = profiles[target].Name
	}
	for i, p := range profiles {
		if i != target && p.Name == name {
			return ProxyProfile{}, errors.New("代理名稱已存在")
		}
	}
	profiles[target].Name = name
	profiles[target].URL = *normalized
	profiles[target].Enabled = enabled
	if err := s.saveProxyProfilesLocked(profiles); err != nil {
		return ProxyProfile{}, err
	}
	for _, acc := range s.allAccountsLocked() {
		if acc.ProxyID != nil && *acc.ProxyID == profileID {
			acc.ProxyURL = normalized
			if err := s.persistAccountLocked(acc); err != nil {
				return ProxyProfile{}, err
			}
		}
	}
	return profiles[target], nil
}

// DeleteProxyProfile 删除代理线路并解除账号指派；不存在返回 false。
func (s *Store) DeleteProxyProfile(profileID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	profiles := s.listProxyProfilesLocked()
	remaining := make([]ProxyProfile, 0, len(profiles))
	removed := false
	for _, p := range profiles {
		if p.ID == profileID {
			removed = true
			continue
		}
		remaining = append(remaining, p)
	}
	if !removed {
		return false, nil
	}
	if err := s.saveProxyProfilesLocked(remaining); err != nil {
		return false, err
	}
	for _, acc := range s.allAccountsLocked() {
		if acc.ProxyID != nil && *acc.ProxyID == profileID {
			acc.ProxyID = nil
			acc.ProxyURL = nil
			if err := s.persistAccountLocked(acc); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

// AssignProxyProfile 把账号指派到代理线路；profileID 为空表示直连。
func (s *Store) AssignProxyProfile(accountID, profileID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.findAnyLocked(accountID)
	if acc == nil {
		return false, nil
	}
	var profileURL *string
	if profileID != "" {
		found := false
		for _, p := range s.listProxyProfilesLocked() {
			if p.ID == profileID {
				u := p.URL
				profileURL = &u
				found = true
				break
			}
		}
		if !found {
			return false, ErrProxyNotFound
		}
		acc.ProxyID = &profileID
		acc.ProxyURL = profileURL
	} else {
		acc.ProxyID = nil
		acc.ProxyURL = nil
	}
	return true, s.persistAccountLocked(acc)
}

// ── 账号读取 ────────────────────────────────────────────────────────────────

// ListAccounts 列出账号；provider 为空表示全部。
func (s *Store) ListAccounts(provider string) []*model.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	if provider != "" {
		src := s.accounts[provider]
		out := make([]*model.Account, len(src))
		copy(out, src)
		return out
	}
	var out []*model.Account
	for _, p := range Providers {
		out = append(out, s.accounts[p]...)
	}
	return out
}

// Find 按 provider + id/名称 查找账号。
func (s *Store) Find(provider, idOrName string) *model.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findLocked(provider, idOrName)
}

// FindAny 按 id 在全部提供商中查找账号。
func (s *Store) FindAny(idOrName string) *model.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findAnyLocked(idOrName)
}

func (s *Store) findLocked(provider, idOrName string) *model.Account {
	for _, a := range s.accounts[provider] {
		if a.ID == idOrName || a.Name == idOrName {
			return a
		}
	}
	return nil
}

func (s *Store) findAnyLocked(idOrName string) *model.Account {
	for _, p := range Providers {
		for _, a := range s.accounts[p] {
			if a.ID == idOrName {
				return a
			}
		}
	}
	return nil
}

func (s *Store) allAccountsLocked() []*model.Account {
	var out []*model.Account
	for _, p := range Providers {
		out = append(out, s.accounts[p]...)
	}
	return out
}

// ── 账号增删改 ──────────────────────────────────────────────────────────────

// AddAccount 添加账号；重复 token 直接返回既有账号（对齐 Python 版）。
func (s *Store) AddAccount(provider, name, secret string) (*model.Account, error) {
	if _, ok := s.providersSet()[provider]; !ok {
		return nil, fmt.Errorf("不支持的 provider: %s", provider)
	}
	acc := model.Create(provider, name, secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts[provider] {
		if a.Secret() != "" && a.Secret() == acc.Secret() {
			return a, nil // 跳过重复 token
		}
	}
	s.accounts[provider] = append(s.accounts[provider], acc)
	if err := s.persistAccountLocked(acc); err != nil {
		return nil, err
	}
	return acc, nil
}

func (s *Store) providersSet() map[string]bool {
	// Providers 是固定小切片，直接现构集合即可。
	set := map[string]bool{}
	for _, p := range Providers {
		set[p] = true
	}
	return set
}

// RemoveAccount 删除账号；未找到返回 false。
func (s *Store) RemoveAccount(provider, idOrName string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.accounts[provider]
	target := s.findLocked(provider, idOrName)
	if target == nil {
		return false, nil
	}
	remaining := items[:0:0]
	for _, a := range items {
		if a.ID != target.ID {
			remaining = append(remaining, a)
		}
	}
	s.accounts[provider] = remaining
	if err := s.deleteAccountLocked(target.ID); err != nil {
		return false, err
	}
	return true, nil
}

// UpdateAccount 持久化某个账号的当前状态。
func (s *Store) UpdateAccount(acc *model.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 防御已删除账号复活：后台流/额度刷新可能长期持有旧对象，
	// 若删除后完成回写，INSERT OR REPLACE 会把账号重新插回 SQLite。
	if s.findLocked(acc.Provider, acc.ID) == nil {
		return fmt.Errorf("账号已不存在，拒绝回写: %s", acc.ID)
	}
	return s.persistAccountLocked(acc)
}

// SetEnabled 启用/禁用账号（禁用同时置 DISABLED 状态）。
func (s *Store) SetEnabled(provider, idOrName string, enabled bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.findLocked(provider, idOrName)
	if acc == nil {
		return false, nil
	}
	acc.Enabled = enabled
	if !enabled {
		acc.Status = model.StatusDisabled
	} else if acc.Status == model.StatusDisabled {
		acc.Status = model.StatusActive
	}
	if err := s.persistAccountLocked(acc); err != nil {
		return false, err
	}
	return true, nil
}

// SetArchived 归档/恢复账号：归档即强制停用（无论原状态），调度、领取、刷新全部跳过；
// 恢复后保持停用状态，需手动启用才会重新参与调度。归档时间取当前时刻。
func (s *Store) SetArchived(provider, idOrName string, archived bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.findLocked(provider, idOrName)
	if acc == nil {
		return false, nil
	}
	if archived {
		now := float64(time.Now().UnixNano()) / 1e9
		acc.ArchivedAt = &now
		acc.Enabled = false
		acc.Status = model.StatusDisabled
	} else {
		acc.ArchivedAt = nil
	}
	if err := s.persistAccountLocked(acc); err != nil {
		return false, err
	}
	return true, nil
}

// ── 轮询选择 ────────────────────────────────────────────────────────────────

// Select 按模型额度与 round-robin 选择账号。
// 有该模型余额的账号优先；尚无快照无法判断者仅作后备；
// 快照中未提供此模型（absent）的账号一律排除。
// skipIDs 保证同一次请求不会重复尝试已失败的账号。
func (s *Store) Select(provider string, skipIDs map[string]bool, modelName string) *model.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var base []*model.Account
	for _, a := range s.accounts[provider] {
		if a.IsSelectable(now) && !skipIDs[a.ID] {
			base = append(base, a)
		}
	}
	pool := base
	if modelName != "" {
		var available, unknown []*model.Account
		for _, a := range base {
			switch a.ModelAvailability(modelName) {
			case "available":
				available = append(available, a)
			case "unknown":
				unknown = append(unknown, a)
			}
		}
		if len(available) > 0 {
			pool = available
		} else {
			pool = unknown
		}
	}
	if len(pool) == 0 {
		return nil
	}
	// 持有未耗尽一次性优惠（period 含 one_time 且 remaining>0）的账号优先：
	// 优惠额度不用会过期，每日体验额度次日刷新；优惠组耗尽后自然回落全池轮询。
	var promo, regular []*model.Account
	for _, a := range pool {
		if hasPromoQuota(a) {
			promo = append(promo, a)
		} else {
			regular = append(regular, a)
		}
	}
	if len(promo) > 0 {
		pool = promo
	}
	key := provider + ":" + orStar(modelName)
	idx := s.rotation[key] % len(pool)
	acc := pool[idx]
	s.rotation[key] = (idx + 1) % len(pool)
	return acc
}

func orStar(modelName string) string {
	if modelName == "" {
		return "*"
	}
	return modelName
}

// hasPromoQuota 账号额度快照中是否存在未耗尽的一次性优惠额度。
// 快照缺 remaining 视为未知（不参与优先判定），仅明确 remaining>0 才算优惠在握。
func hasPromoQuota(a *model.Account) bool {
	for _, q := range a.Quota {
		period, _ := q["period"].(string)
		if !strings.Contains(period, "one_time") {
			continue
		}
		if v, ok := asNumber(q["remaining"]); ok && v > 0 {
			return true
		}
	}
	return false
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

// ── 导入 / 导出 ─────────────────────────────────────────────────────────────

type exportAccount struct {
	Name           string   `json:"name"`
	Mode           string   `json:"mode"`
	Secret         string   `json:"secret"`
	DisabledModels []string `json:"disabled_models"`
}

// ExportPayload 导出格式（version 1，与 Python 版一致）。
type ExportPayload struct {
	Version    int                        `json:"version"`
	ExportedAt float64                    `json:"exported_at"`
	Providers  map[string][]exportAccount `json:"providers"`
}

// Export 导出全部账号（含明文凭证，仅用于备份/迁移）。
func (s *Store) Export() ExportPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	providers := map[string][]exportAccount{}
	for _, p := range Providers {
		list := []exportAccount{}
		for _, a := range s.accounts[p] {
			disabled := a.DisabledModels
			if disabled == nil {
				disabled = []string{}
			}
			list = append(list, exportAccount{
				Name:           a.Name,
				Mode:           a.Mode,
				Secret:         a.Secret(),
				DisabledModels: disabled,
			})
		}
		providers[p] = list
	}
	return ExportPayload{
		Version:    1,
		ExportedAt: float64(time.Now().UnixNano()) / 1e9,
		Providers:  providers,
	}
}

type importItem struct {
	Name           string   `json:"name"`
	Secret         string   `json:"secret"`
	Token          string   `json:"token"`
	JWTToken       string   `json:"jwtToken"`
	APIKey         string   `json:"apiKey"`
	DisabledModels []string `json:"disabled_models"`
}

// ImportPayload 导入格式（与 Python 版兼容；secret 可用多个别名键）。
type ImportPayload struct {
	Providers map[string][]importItem `json:"providers"`
}

// ImportAccounts 导入账号，返回导入数量。
func (s *Store) ImportAccounts(payload ImportPayload) (int, error) {
	count := 0
	for provider, items := range payload.Providers {
		if !s.providersSet()[provider] {
			continue
		}
		for _, item := range items {
			secret := firstNonEmpty(item.Secret, item.Token, item.JWTToken, item.APIKey)
			if secret == "" {
				continue
			}
			acc, err := s.AddAccount(provider, item.Name, secret)
			if err != nil {
				return count, err
			}
			if item.DisabledModels != nil {
				acc.SetDisabledModels(item.DisabledModels)
				if err := s.UpdateAccount(acc); err != nil {
					return count, err
				}
			}
			count++
		}
	}
	return count, nil
}

// ── 内部工具 ────────────────────────────────────────────────────────────────

func randomTokenURLSafe(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
