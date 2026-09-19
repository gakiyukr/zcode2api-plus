// Package config 承载全部运行期配置：环境变量 + 默认值。
// 对应 Python 版 app/settings.py；账号与凭证不在此处，
// 而是持久化到数据目录（见 internal/store）。
package config

import (
	"zcode2api/internal/util"

	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	// 与 Python 版一致：非空且不在假值集合内的一律按真处理。
	switch v {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// ── 目录 ────────────────────────────────────────────────────────────────────
// 数据目录：相对路径按进程工作目录解析并转为绝对路径。
// （与 Python 版的差异：Python 相对仓库根解析；Go 二进制无固定根。）
var DataDir = resolveDir("ZCODE_DATA_DIR", "data")

// DBPath 账号与设置数据库；测试可直接覆写。
var DBPath = filepath.Join(DataDir, "accounts.db")

func resolveDir(key, def string) string {
	raw := env(key, def)
	if filepath.IsAbs(raw) {
		return raw
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return raw
	}
	return abs
}

// ── 服务 ────────────────────────────────────────────────────────────────────
var (
	Port = envInt("ZCODE_PORT", 3000)
	Host = env("ZCODE_HOST", "0.0.0.0")
)

// ── 鉴权 ────────────────────────────────────────────────────────────────────
// 密钥初始值：仅在 meta 表缺失（或后台密码仍为历史默认值）时生效，写入后以数据库为准。
// 未设置环境变量时由 store 首次启动随机生成（见 internal/store 的引导逻辑）。
var (
	AdminKeyEnv   = env("ZCODE_ADMIN_KEY", "")
	GatewayKeyEnv = env("ZCODE_GATEWAY_KEY", "")
)

// ── 验证码 ──────────────────────────────────────────────────────────────────
var (
	CaptchaConfigCacheTTL = int64(envInt("CAPTCHA_CONFIG_CACHE_TTL", 600_000)) // ms，上游配置
	CaptchaManualCacheTTL = int64(envInt("CAPTCHA_MANUAL_CACHE_TTL", 45_000))  // ms，人工回填

	// 每次求解超时（秒）。与同组 CaptchaBrowser* 一致做下界钳制：0 或负值会让
	// deadline 立即过期（每次求解都瞬时超时且不触发冷却），极大值会在
	// time.Duration 乘法处溢出成负值，同样立即超时。
	CaptchaSolveTimeout = min(max(1, envInt("ZCODE_CAPTCHA_TIMEOUT", 40)), 3600)

	// 真实 Chromium（rod 驱动 cloakbrowser 下载的浏览器二进制）。
	CaptchaBrowserEnabled         = envBool("ZCODE_CAPTCHA_BROWSER", false)
	CaptchaBrowserWorkers         = max(1, envInt("ZCODE_CAPTCHA_BROWSER_WORKERS", 1))
	CaptchaBrowserStartupTimeout  = max(5, envInt("ZCODE_CAPTCHA_BROWSER_STARTUP_TIMEOUT", 90))
	CaptchaBrowserRequestTimeout  = max(5, envInt("ZCODE_CAPTCHA_BROWSER_REQUEST_TIMEOUT", 45))
	CaptchaBrowserQueueTimeout    = max(1, envInt("ZCODE_CAPTCHA_BROWSER_QUEUE_TIMEOUT", 60))
	CaptchaBrowserShutdownTimeout = max(1, envInt("ZCODE_CAPTCHA_BROWSER_SHUTDOWN_TIMEOUT", 10))
	CaptchaBrowserFailureCooldown = max(1, envInt("ZCODE_CAPTCHA_BROWSER_FAILURE_COOLDOWN", 60))
	// 显式指定浏览器可执行文件；缺省时按 cloakbrowser 缓存目录自动发现
	//（CLOAKBROWSER_BINARY_PATH / CLOAKBROWSER_CACHE_DIR，见 internal/captcha/solve.go）。
	CaptchaBrowserBin = env("ZCODE_CAPTCHA_BROWSER_BIN", "")
)

// ── Async 空闲池 ────────────────────────────────────────────────────────────
var (
	AsyncEnabled       = envBool("ZCODE_ASYNC_ENABLED", true)
	AsyncTicketTimeout = max(30, envInt("ZCODE_ASYNC_TICKET_TIMEOUT", 300))
	AsyncMaxRetries    = max(0, envInt("ZCODE_ASYNC_MAX_RETRIES", 3))
)

// ── 用量监控 ────────────────────────────────────────────────────────────────
var (
	QuotaRefreshInterval = envInt("ZCODE_QUOTA_REFRESH_INTERVAL", 60) // 秒，0=关闭
	CoolingSeconds       = envInt("ZCODE_COOLING_SECONDS", 300)       // 限流冷却（秒）
)

// ── 上游端点 ────────────────────────────────────────────────────────────────
var (
	UpstreamZai         = env("ZAI_UPSTREAM_URL", "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages")
	UpstreamZaiFallback = env("ZAI_FALLBACK_URL", "https://api.z.ai/api/anthropic/v1/messages")

	// ZCode 计费 / 额度查询端点（不可配置，与官方客户端一致）。
	ZcodeBillingBase = "https://zcode.z.ai/api/v1/zcode-plan"

	ZcodeClientVersion = env("ZCODE_CLIENT_VERSION", "3.7.7")
	// 与 Python 版保持一致的客户端平台标识；旧的 win32 参数已失效。
	ZcodeClientPlatform = env("ZCODE_CLIENT_PLATFORM", "win32-x64")

	UserAgent = env("UPSTREAM_USER_AGENT", "ZCode/"+ZcodeClientVersion)

	// AppVersion 供 /meta 与后台展示；-go 后缀标识运行时版本。
	AppVersion = "2.0.9-go"
)

// ── 设备身份 ────────────────────────────────────────────────────────────────
var (
	deviceMidOnce sync.Once
	deviceMid     string
)

// DeviceMid 返回固定设备 UUID（首次调用时生成并持久化到 <DataDir>/device_mid.txt）。
// 对应 Python 版 settings._load_or_generate_device_mid：固定标识避免频繁更换被风控。
//
// 写盘失败必须留下痕迹：该值随每个上游请求送出并用于激活上报，落不了盘时
// 每次重启都会换一个（上游视为新装置）。这里不能静默——否则管理员只看到
// 「账号莫名被风控」，没有任何线索指向 DataDir 不可写。
//
// 用 stderr 而非 internal/web：web 包依赖本包，反向引用会成环。
func DeviceMid() string {
	deviceMidOnce.Do(func() {
		path := filepath.Join(DataDir, "device_mid.txt")
		if b, err := os.ReadFile(path); err == nil {
			if mid := strings.TrimSpace(string(b)); mid != "" {
				deviceMid = mid
				return
			}
		}
		mid := util.NewUUID()
		if err := os.MkdirAll(DataDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "[!] 设备指纹目录不可建（%s），本次使用临时指纹，重启后会变化: %v\n", DataDir, err)
		} else if err := os.WriteFile(path, []byte(mid), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "[!] 设备指纹写入失败（%s），本次使用临时指纹，重启后会变化: %v\n", path, err)
		}
		deviceMid = mid
	})
	return deviceMid
}

// newUUID 生成 UUIDv4（不引入第三方依赖）。

