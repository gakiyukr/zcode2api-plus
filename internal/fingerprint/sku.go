// Package fingerprint 提供账号级的确定性桌面设备档案（SKU）。
//
// 同一账号在不同请求、不同进程重启后必须得到同一套平台、系统版本、语系、
// 时区与分辨率，避免「一台 Linux 机房跑 N 个 ZCode」的聚类特征；撞风控时
// 可通过 meta 表的 fingerprint_salt:<accountID> 旋转到另一套档案，且不改动
// accounts 表结构或 Account 的 JSON 字段。
package fingerprint

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"

	"zcode2api/internal/model"
)

// SKU 一套成套的桌面设备档案。
// Platform 是官方 X-Platform 形态（win32-x64 / darwin-arm64）；
// PlatformName 是 environment 段的 process.platform（win32 / darwin）。
type SKU struct {
	Key          string
	Platform     string
	PlatformName string
	OsCategory   string
	OsVersion    string
	Arch         string
	Language     string
	Timezone     string
	Screen       string
}

type profile struct {
	weight int
	sku    SKU
}

// profiles 只列真实存在的桌面组合（不做笛卡尔积），并按大致市场权重分配。
// 严禁出现 linux：网关部署在 Linux 云主机，若沿用宿主平台会与桌面客户端矛盾。
var profiles = []profile{
	{18, SKU{"win11-zh", "win32-x64", "win32", "windows", "10.0.22631", "x64", "zh-CN", "Asia/Shanghai", "2560x1440"}},
	{10, SKU{"win11-en", "win32-x64", "win32", "windows", "10.0.22631", "x64", "en-US", "America/Los_Angeles", "1920x1080"}},
	{12, SKU{"win10-zh", "win32-x64", "win32", "windows", "10.0.19045", "x64", "zh-CN", "Asia/Shanghai", "1920x1080"}},
	{8, SKU{"win10-en", "win32-x64", "win32", "windows", "10.0.19045", "x64", "en-US", "America/New_York", "2560x1440"}},
	{4, SKU{"win11-ja", "win32-x64", "win32", "windows", "10.0.22631", "x64", "ja-JP", "Asia/Tokyo", "2560x1440"}},
	{14, SKU{"mac15-zh", "darwin-arm64", "darwin", "macos", "24.1.0", "arm64", "zh-CN", "Asia/Shanghai", "3024x1964"}},
	{12, SKU{"mac15-en", "darwin-arm64", "darwin", "macos", "24.1.0", "arm64", "en-US", "America/Los_Angeles", "2560x1600"}},
	{8, SKU{"mac14-en", "darwin-arm64", "darwin", "macos", "23.6.0", "arm64", "en-US", "Europe/London", "2880x1800"}},
	{4, SKU{"mac14-ja", "darwin-arm64", "darwin", "macos", "23.6.0", "arm64", "ja-JP", "Asia/Tokyo", "2560x1600"}},
}

var totalWeight = func() int {
	sum := 0
	for _, p := range profiles {
		sum += p.weight
	}
	return sum
}()

var (
	saltMu sync.RWMutex
	saltFn func(accountID string) string

	cacheMu sync.RWMutex
	cache   = map[[32]byte]SKU{}
)

// SetSaltResolver 注入 salt 来源（生产环境读取 meta 表的 fingerprint_salt:<id>）。
// 注入时会清空缓存，使旋转立即生效。
func SetSaltResolver(fn func(accountID string) string) {
	saltMu.Lock()
	saltFn = fn
	saltMu.Unlock()
	cacheMu.Lock()
	cache = map[[32]byte]SKU{}
	cacheMu.Unlock()
}

// For 返回账号的稳定 SKU。种子为账号 ID + 凭据 + 可选 salt；同一账号跨请求
// 恒定，不同账号按权重分布到 Windows/macOS 桌面组合。
func For(acc *model.Account) SKU {
	if acc == nil {
		return profiles[0].sku
	}
	seed := acc.Secret()
	if seed == "" {
		seed = acc.ID
	}
	saltMu.RLock()
	fn := saltFn
	saltMu.RUnlock()
	salt := ""
	if fn != nil {
		salt = fn(acc.ID)
	}
	return ForSeed(acc.ID+"\x00"+seed, salt)
}

// ForSeed 以显式种子派生 SKU（测试与无账号场景使用）。
func ForSeed(seed, salt string) SKU {
	hash := sha256.Sum256([]byte("zcode-sku\x00" + seed + "\x00" + salt))
	cacheMu.RLock()
	sku, ok := cache[hash]
	cacheMu.RUnlock()
	if ok {
		return sku
	}

	sku = profiles[0].sku
	target := int(binary.BigEndian.Uint64(hash[:8]) % uint64(totalWeight))
	for _, p := range profiles {
		if target < p.weight {
			sku = p.sku
			break
		}
		target -= p.weight
	}

	cacheMu.Lock()
	cache[hash] = sku
	cacheMu.Unlock()
	return sku
}
