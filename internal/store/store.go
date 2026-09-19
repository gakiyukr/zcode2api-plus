// Package store 账号与设置的持久化存储（SQLite）。
// 对应 Python 版 app/store.py；schema 与 Python 版完全一致，
// 两个版本可互读同一个 data/accounts.db（Account JSON 字段契约见 internal/model）。
//
// 运行期账号对象常驻内存（保证轮询游标与状态实时性），
// 每次变更同步落库；进程启动时从 SQLite 读取快照。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/util"
	"zcode2api/internal/web"
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

	// settingsSnapshot 是 settings 的不可变快照，供无锁读取。
	//
	// 读取 settings 的路径包含每次 API 请求的鉴权（VerifyGatewayKey →
	// GetSetting），若与 Update 共用 s.mu，一次慢写（磁盘满、外部进程持写锁
	// 时最多 busy_timeout 5s）会让所有请求的鉴权一起排队——DB 慢即服务不可用。
	// 快照让读取完全不碰锁；写入仍是「改 map 后发布新快照」。
	settingsSnapshot atomic.Pointer[map[string]string]

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
			adminKey = util.RandomTokenURLSafe(24)
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
			gatewayKey = "sk-" + util.RandomTokenURLSafe(24)
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
	s.publishSettings()

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
	data, err := util.MarshalJSON(acc)
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


// ── 设置 ────────────────────────────────────────────────────────────────────

// GetSetting 读取设置（第二返回值表示是否存在）。
//
// 走原子快照而非 s.mu：鉴权路径（VerifyGatewayKey/VerifyAdminKey）每次都调用
// 本函数，若与写路径共用锁，一次慢写就会让所有请求的鉴权排队。
func (s *Store) GetSetting(key string) (string, bool) {
	snap := s.settingsSnapshot.Load()
	if snap == nil {
		return "", false
	}
	v, ok := (*snap)[key]
	return v, ok
}

// publishSettings 发布 settings 的不可变快照（调用方须持有 s.mu）。
//
// 复制一份而非共享原 map：快照必须不可变，否则读到一半被并发写会触发
// Go 的并发 map 读写检测。
func (s *Store) publishSettings() {
	cp := make(map[string]string, len(s.settings))
	for k, v := range s.settings {
		cp[k] = v
	}
	s.settingsSnapshot.Store(&cp)
}

// SetSetting 更新设置并落库。
// logPersistFailure 记录一次落库失败。
//
// 统计路径（网关计 token、异步池计状态、额度刷新）刻意忽略 Update 的错误——
// 不该因为统计写不进去就让用户的对话请求失败。但完全静默会让「磁盘满导致
// 统计与状态全部不落库」没有任何线索可查：后台数字与实际持久化状态脱节，
// 重启后回滚，而日志里什么都没有。这里集中记一次，涵盖所有调用方。
//
// 调用方都持有 s.mu，故本函数必须自行确保「不在锁内做 I/O」——web.Warn 是同步
// 的 stdout 写，stdout 阻塞（管道满、终端卡住）时会把整个 Store 锁住。做法是
// 只在锁内做判断，把实际输出交给独立 goroutine。
//
// 节流到每分钟一条：落库持续失败时（磁盘满）每个请求都会走到这里，
// 不节流会把日志刷爆并掩盖其他信息。
func logPersistFailure(scope, detail string, err error) {
	if err == nil {
		return
	}
	persistLogMu.Lock()
	now := time.Now()
	allow := now.Sub(persistLogLast) >= persistLogInterval
	if allow {
		persistLogLast = now
	}
	persistLogMu.Unlock()
	if !allow {
		return
	}
	msg := fmt.Sprintf("落库失败（%s，%s）: %v；内存已改而 DB 未写入，重启后会回滚", scope, detail, err)
	go web.Warn("store", msg)
}

var (
	persistLogMu       sync.Mutex
	persistLogLast     time.Time
	persistLogInterval = time.Minute
)

func (s *Store) SetSetting(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 先落库再改内存，与 AddAccount / RemoveAccount 同一原则：落库失败时内存
	// 不能留下一个未持久化的值，否则本次进程按新值运行、重启后回滚，而调用方
	// 收到错误以为没生效。
	if err := s.setMeta(key, value); err != nil {
		logPersistFailure("setting", key, err)
		return err
	}
	s.settings[key] = value
	s.publishSettings()
	return nil
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
	data, err := util.MarshalJSON(profiles)
	if err != nil {
		return err
	}
	s.settings["proxy_profiles"] = string(data)
	s.publishSettings()
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
	profile := ProxyProfile{ID: "proxy-" + util.RandomHex(4), Name: name, URL: *normalized, Enabled: enabled}
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
//
// 返回深拷贝：调用方（后台监控、管理后台、CLI）会在锁外长期遍历，
// 直接给出内部对象会让其字段读取与 Store 的写入竞争。需要改状态请用
// Update（锁内修改），不要在副本上改字段——那不会落库。
func (s *Store) ListAccounts(provider string) []*model.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	if provider != "" {
		src := s.accounts[provider]
		out := make([]*model.Account, len(src))
		for i, a := range src {
			out[i] = a.Clone()
		}
		return out
	}
	var out []*model.Account
	for _, p := range Providers {
		for _, a := range s.accounts[p] {
			out = append(out, a.Clone())
		}
	}
	return out
}

// Find 按 provider + id/名称 查找账号，返回深拷贝（nil 表示不存在）。
func (s *Store) Find(provider, idOrName string) *model.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findLocked(provider, idOrName).Clone()
}

// FindAny 按 id 在全部提供商中查找账号，返回深拷贝（nil 表示不存在）。
func (s *Store) FindAny(idOrName string) *model.Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findAnyLocked(idOrName).Clone()
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
// 返回的是深拷贝：调用方拿到的对象与 Store 内部无共享，改它不会影响存储
// （要改状态请用 Update）。返回 nil, nil 表示账号已存在且无需新建。
func (s *Store) AddAccount(provider, name, secret string) (*model.Account, error) {
	if _, ok := s.providersSet()[provider]; !ok {
		return nil, fmt.Errorf("不支持的 provider: %s", provider)
	}
	acc := model.Create(provider, name, secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts[provider] {
		if a.Secret() != "" && a.Secret() == acc.Secret() {
			return a.Clone(), nil // 跳过重复 token
		}
	}
	// 先落库再改内存：落库失败时内存不能留下一个不存在的账号。反过来会让
	// 账号在本次进程里可用、重启后消失，而调用方收到 500 以为没建成。
	if err := s.persistAccountLocked(acc); err != nil {
		logPersistFailure("add", provider+"/"+acc.ID, err)
		return nil, err
	}
	s.accounts[provider] = append(s.accounts[provider], acc)
	return acc.Clone(), nil
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
	// 先落库再改内存：反过来时落库失败会让内存与 DB 分叉——本次进程里账号
	// 已消失，重启后又从 DB 载入回来。删除常被用来撤销可疑或外泄的凭证，
	// 这种「显示已删除、实际还在」是安全相关的静默失败。
	if err := s.deleteAccountLocked(target.ID); err != nil {
		logPersistFailure("delete", provider+"/"+target.ID, err)
		return false, err
	}
	remaining := items[:0:0]
	for _, a := range items {
		if a.ID != target.ID {
			remaining = append(remaining, a)
		}
	}
	s.accounts[provider] = remaining
	return true, nil
}

// Update 在锁内对指定账号执行修改并持久化。
//
// fn 收到的是 Store 内部持有的账号对象，字段读写全程在锁内完成，
// 因此与 Select/ListAccounts 的读取不会竞争。并发路径应使用本方法
// 而非「取指针 → 改字段 → UpdateAccount」。
//
// 返回 error 与其它写路径（SetEnabled/SetArchived/AddAccount 等）一致：
// 账号不存在返回 ErrNotFound，落库失败返回底层错误。
// 落库失败时内存状态已改（Store 内部对象是唯一的真相来源），
// 但调用方应把错误报给用户，否则会出现「界面显示已保存、重启后回滚」。
func (s *Store) Update(provider, id string, fn func(acc *model.Account)) error {
	var failed error

	// 内层闭包保留 defer 解锁：fn 由调用方提供，一旦 panic 必须仍释放锁，
	// 否则整个 Store 会永久死锁。
	ok := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		acc := s.findLocked(provider, id)
		if acc == nil {
			return false
		}
		fn(acc)
		failed = s.persistAccountLocked(acc)
		return true
	}()

	if !ok {
		return ErrNotFound
	}
	if failed != nil {
		logPersistFailure("update", provider+"/"+id, failed)
	}
	return failed
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
//
// 返回的是**深拷贝**：调用方（网关引擎、async 池）会在锁外长时间持有它，
// 若直接返回 Store 内部对象，其字段修改将与 Select 自身的加锁读取竞争
// （Status 为字符串、CoolingUntil 为指针，撕裂读可致误判或崩溃）。
// 状态修改须经 Store.Update，不要改这个副本。
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
	return acc.Clone()
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
			// 必须复制：直接别名会让内部切片的底层数组随 payload 逃出锁，
			// 调用方之后改它就会与 Clone/Update 的读取并发。
			disabled := model.CloneStrings(a.DisabledModels)
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
				if err := s.Update(acc.Provider, acc.ID, func(a *model.Account) {
					a.SetDisabledModels(item.DisabledModels)
				}); err != nil {
					return count, err
				}
			}
			count++
		}
	}
	return count, nil
}

// ── 内部工具 ────────────────────────────────────────────────────────────────





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
