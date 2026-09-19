// rod 驱动真实 Chromium 的求解 worker：对应 Python 版 app/captcha_browser.py 的
// worker 入口（_build_captcha_html / _solve_once / worker_main）。
//
// 浏览器二进制复用 cloakbrowser 的同一份补丁 Chromium（发现链落空时由
// browserdl.go 自动下载，无需 Python 预下载），启动参数
// 逐项对齐 cloakbrowser.launch(headless=True) 经 playwright 落到进程的最终有效集：
// playwright 默认开关（剔除 --enable-automation / --enable-unsafe-swiftshader）
// + cloakbrowser stealth 参数（随机指纹种子）+ _BROWSER_ARGS 五项。差异项在
// newLauncher 内逐条注明。HTML 与求解 JS 从 Python 版逐字抄写。
package captcha

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"

	"zcode2api/internal/config"
	"zcode2api/internal/web"
)

//go:embed AliyunCaptcha.js.txt
var aliyunCaptchaSDK string

// aliyunCaptchaSDKEscaped 内联进 HTML 前的预处理：`</script>` 转义，
// 避免提前终止脚本标签（对齐 Python _read_sdk）。
var aliyunCaptchaSDKEscaped = strings.ReplaceAll(aliyunCaptchaSDK, "</script>", `<\/script>`)

// browserArgs cloakbrowser worker 的浏览器启动参数（与 Python 版 _BROWSER_ARGS
// 逐字一致）。其中 disable-dev-shm-usage / disable-background-networking /
// disable-component-update / no-first-run 已含于 rod 与 playwright 的默认集，
// 此处仅显式补齐 disable-sync，其余由 newLauncher 的对齐注释逐项核对。
var browserArgs = []string{
	"--disable-dev-shm-usage",
	"--disable-background-networking",
	"--disable-component-update",
	"--disable-sync",
	"--no-first-run",
}

// playwrightDisabledFeatures 对齐 playwright chromiumSwitches 的 --disable-features
// 清单（1.54；assistantMode=false）。GPU/渲染相关行为影响指纹，逐项保留。
var playwrightDisabledFeatures = []string{
	"AcceptCHFrame",
	"AutoExpandDetailsElement",
	"AvoidUnnecessaryBeforeUnloadCheckSync",
	"CertificateTransparencyComponentUpdater",
	"DestroyProfileOnBrowserClose",
	"DialMediaRouteProvider",
	"ExtensionManifestV2Disabled",
	"GlobalMediaControls",
	"HttpsUpgrades",
	"ImprovedCookieControls",
	"LazyFrameLoading",
	"LensOverlay",
	"MediaRouter",
	"PaintHolding",
	"ThirdPartyStoragePartitioning",
	"Translate",
}

// newLauncher 构造参数对齐的 rod 启动器（不实际拉起进程）。
//
// rod 默认集裁剪：
//   - enable-automation：暴露 navigator.webdriver=true（playwright 侧被
//     IGNORE_DEFAULT_ARGS 剔除，这里是风控识别的头号信号）；
//   - disable-site-isolation-trials / enable-features=NetworkService,…：rod 自有
//     默认，playwright 不传，其中 disable-features 改为 playwright 的完整清单。
//
// rod 默认集与 playwright 重合项（disable-dev-shm-usage、no-first-run、
// force-color-profile=srgb、metrics-recording-only、use-mock-keychain、
// no-startup-window、user-data-dir、remote-debugging-port、headless）不再重复设置。
// remote-debugging-port 与 playwright 的 -pipe 是连接方式差异，无指纹影响。
func newLauncher(bin string) *launcher.Launcher {
	l := launcher.New().Bin(bin).Headless(true)
	l = l.Delete("enable-automation").Delete("disable-site-isolation-trials").Delete("enable-features")

	// playwright chromiumSwitches 有效集（rod 默认缺失的部分）
	l.Set("disable-field-trial-config")
	l.Set("disable-back-forward-cache")
	l.Set("no-default-browser-check")
	l.Set("disable-extensions")
	l.Set("disable-features", strings.Join(playwrightDisabledFeatures, ","))
	l.Set("allow-pre-commit-input")
	l.Set("password-store", "basic")
	l.Set("no-service-autorun")
	l.Set("export-tagged-pdf")
	l.Set("disable-search-engine-choice-screen")
	l.Set("unsafely-disable-devtools-self-xss-warnings")
	l.Set("edge-skip-compat-layer-relaunch")

	// playwright headless 附加项
	l.Set("hide-scrollbars")
	l.Set("mute-audio")
	l.Set("blink-settings", "primaryHoverType=2,availableHoverTypes=2,primaryPointerType=4,availablePointerTypes=4")

	// cloakbrowser stealth：--no-sandbox + 每次启动随机的指纹种子 + 平台伪装
	//（macOS 宿主伪装原生 Mac，其余伪装 Windows，与 Python get_default_stealth_args 一致）
	l.Set("no-sandbox")
	l.Set("fingerprint", strconv.Itoa(10000+rand.IntN(90000)))
	if runtime.GOOS == "darwin" {
		l.Set("fingerprint-platform", "macos")
	} else {
		l.Set("fingerprint-platform", "windows")
	}

	// cloakbrowser _BROWSER_ARGS（均为无值开关；disable-dev-shm-usage 等四项
	// 已含于默认集，重复 Set 幂等）
	for _, arg := range browserArgs {
		l.Set(flags.Flag(strings.TrimPrefix(arg, "--")))
	}

	// --ignore-gpu-blocklist 仅在有头模式或 Windows 宿主注入（对齐 build_args）；
	// 生产形态为 Linux 无头，不注入。
	if runtime.GOOS == "windows" {
		l.Set("ignore-gpu-blocklist")
	}
	return l
}

// jsQuote 把字符串序列化为 JS 字面量（json.Marshal 与 Python json.dumps 的
// 字符串转义兼容，值仅来自上游配置的 scene/region/prefix）。
func jsQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// buildCaptchaHTML 构造加载阿里云 SDK 的最小页面；AliyunCaptchaConfig 必须先于
// SDK 执行（对齐 Python _build_captcha_html，逐字抄写）。
func buildCaptchaHTML(sdk, configJSON string) string {
	return "<!DOCTYPE html><html><head><meta charset=\"utf-8\">" +
		"<style>html,body{margin:0;padding:0}button{display:none}</style>" +
		"</head><body><div id=\"cap\"></div><button id=\"btn\"></button>" +
		"<script>window.AliyunCaptchaConfig = " + configJSON + ";</script>" +
		"<script>" + sdk + "</script></body></html>"
}

// captchaConfigJSON 验证码页面配置（对齐 Python json.dumps({"region", "prefix"})）。
func captchaConfigJSON(cfg Config) string {
	b, err := json.Marshal(struct {
		Region string `json:"region"`
		Prefix string `json:"prefix"`
	}{Region: cfg.Region, Prefix: cfg.Prefix})
	if err != nil {
		return "{}"
	}
	return string(b)
}

// solveInitJS 一次求解的初始化脚本（对齐 Python _solve_once 的 initAliyunCaptcha
// 调用，逐字抄写；SceneId/region/prefix 由调用方注入 JSON 字面量）。
const solveInitJS = `() => {
  window.__zcodeOutcome = null;
  window.__zcodeError = null;
  window.initAliyunCaptcha({
    SceneId: %s,
    mode: 'popup',
    region: %s,
    prefix: %s,
    language: 'en',
    element: '#cap',
    button: '#btn',
    captchaLogoImg: '',
    showErrorTip: false,
    getInstance(instance) {
      const start = instance && (instance.startTracelessVerification || instance.show);
      if (typeof start !== 'function') { window.__zcodeError = 'SDK instance unavailable'; return; }
      try { start.call(instance); } catch (e) { window.__zcodeError = describe(e); }
    },
    success(param) { window.__zcodeOutcome = param; },
    fail(e) { window.__zcodeError = 'SDK fail: ' + describe(e); },
    onError(e) { window.__zcodeError = 'SDK error: ' + describe(e); }
  });
  function describe(e) {
    if (e && e.message) return String(e.message);
    try { return JSON.stringify(e); } catch (_) { return String(e); }
  }
}`

// solvePollJS 轮询求解结果（对齐 _solve_once 的 state 读取）。
const solvePollJS = `() => ({ outcome: window.__zcodeOutcome, error: window.__zcodeError })`

// sdkReadyJS SDK 就绪探测表达式（对齐 wait_for_function 的条件）。
// 必须包成箭头函数：rod.Eval 对裸表达式会在求值结果上调用 .apply，
// 布尔结果直接抛 TypeError（曾致 SDK 已就绪仍误报加载超时）。
const sdkReadyJS = `() => (typeof window.initAliyunCaptcha === 'function')`

// sdkLoadTimeout SDK 加载超时（对齐 Python worker 的 --sdk-load-timeout 默认 20s）。
const sdkLoadTimeout = 20 * time.Second

// probeTimeout 死亡探测（CDP Browser.getVersion）的超时上限。
//
// 这个探针只是区分「浏览器进程没了」与「页面级失败」，本身应当毫秒级返回。
// 给 3 秒：假死的浏览器在这里被判为死亡，槽位得以重建，而不是永久卡住。
const probeTimeout = 3 * time.Second

// RodWorker 单个 rod 浏览器会话的求解 worker。
// 页面内只保存求解结果，token 不落盘、不进日志。
type RodWorker struct {
	launcher *launcher.Launcher
	browser  *rod.Browser
	page     *rod.Page

	// browserCancel 取消绑定在 browser 上的 ctx。
	// rod 的 Browser 默认用 context.Background()，其 CDP 调用（classify 的
	// GetVersion 探针、Page 创建、Browser.Close）在浏览器假死时会永久阻塞——
	// 这里运行在池的槽位 goroutine 上，卡住即槽位永不归队（workers 默认 1
	// 时整池失效）。Close 时取消，让这些调用立即返回。
	browserCancel context.CancelFunc

	cfg          Config
	solveTimeout time.Duration // 单次求解超时（对齐 --solve-timeout 默认 40s）
	sdkLoadTTL   time.Duration // SDK 加载超时（对齐 --sdk-load-timeout 默认 20s）

	// pageDirty 上次求解失败/中止后置位：SDK 弹层状态已污染，下次 Solve 前重载页面。
	// 成功路径复用同一页面，避免每次重新加载 224KB SDK（对齐 worker_main）。
	pageDirty bool
}

// NewRodWorkerFactory 构造绑定验证码配置的真实求解工厂。
func NewRodWorkerFactory(cfg Config) WorkerFactory {
	return func(ctx context.Context) (Worker, error) {
		bin, err := DiscoverBrowserBinary(ctx)
		if err != nil {
			return nil, err
		}
		return newRodWorker(ctx, bin, cfg)
	}
}

func newRodWorker(ctx context.Context, bin string, cfg Config) (*RodWorker, error) {
	l := newLauncher(bin)
	url, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("浏览器启动失败: %w", err)
	}
	b := rod.New().ControlURL(url).NoDefaultDevice()
	// NoDefaultDevice：rod 默认设备模拟（LaptopWithMDPI）会用 CDP 改写 UA 与视口，
	// 覆盖补丁二进制的指纹输出，必须关闭（playwright 侧无设备模拟）。
	if err := b.Connect(); err != nil {
		// Connect 失败时 rod 的 client 仍是 nil（Browser.Connect 只在
		// cdp.StartWithURL 成功后赋值），此时 Browser.Close 会走
		// b.client.Call(...) 对 nil interface 调用方法 —— 直接 panic，
		// 而这里运行在池的槽位 goroutine 上，一次浏览器启动异常就会
		// 终止整个网关进程。只做进程侧清理。
		closeLauncher(l)
		return nil, fmt.Errorf("浏览器连接失败: %w", err)
	}
	page, err := loadSDKPage(ctx, b, cfg)
	if err != nil {
		_ = b.Close()
		closeLauncher(l)
		return nil, fmt.Errorf("页面初始化失败: %w", err)
	}
	// 绑定一个可取消的 ctx 到 browser：Browser.Context 返回克隆，故必须
	// 在此处替换，之后 w.browser 的所有 CDP 调用都受它约束。
	browserCtx, browserCancel := context.WithCancel(context.Background())
	b = b.Context(browserCtx)
	page = page.Context(browserCtx)

	return &RodWorker{
		launcher:      l,
		browser:       b,
		page:          page,
		browserCancel: browserCancel,
		cfg:           cfg,
		solveTimeout:  time.Duration(config.CaptchaSolveTimeout) * time.Second,
		sdkLoadTTL:    sdkLoadTimeout,
	}, nil
}

// loadSDKPage 加载一次 SDK 页面（对齐 Python _load_page：set_content 的
// DOMContentLoaded 超时可忽略——rod 的 SetDocumentContent 无生命周期等待，
// 就绪判定交给下面的 SDK 探测轮询）。
func loadSDKPage(ctx context.Context, b *rod.Browser, cfg Config) (*rod.Page, error) {
	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		return nil, err
	}
	page = page.Context(ctx)
	if err := page.SetDocumentContent(buildCaptchaHTML(aliyunCaptchaSDKEscaped, captchaConfigJSON(cfg))); err != nil {
		_ = page.Close()
		return nil, err
	}
	deadline := time.Now().Add(sdkLoadTimeout)
	for {
		res, err := page.Evaluate(rod.Eval(sdkReadyJS))
		if err == nil && res != nil && !res.Value.Nil() && res.Value.Bool() {
			return page, nil
		}
		if err != nil && ctx.Err() != nil {
			_ = page.Close()
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			_ = page.Close()
			return nil, fmt.Errorf("SDK 加载超时（>20s）")
		}
		sleepCtx(ctx, 100*time.Millisecond)
	}
}

// Solve 在已加载的真实 Chromium 页面中生成一次 verify_param（对齐 _solve_once）。
func (w *RodWorker) Solve(ctx context.Context) (string, error) {
	if w.pageDirty {
		if err := w.reload(ctx); err != nil {
			return "", err
		}
		w.pageDirty = false
	}
	page := w.page.Context(ctx)

	initJS := fmt.Sprintf(solveInitJS, jsQuote(w.cfg.SceneID), jsQuote(w.cfg.Region), jsQuote(w.cfg.Prefix))
	if _, err := page.Evaluate(rod.Eval(initJS)); err != nil {
		w.pageDirty = true
		return "", w.classify(ctx, err)
	}

	deadline := time.Now().Add(w.solveTimeout)
	for {
		res, err := page.Evaluate(rod.Eval(solvePollJS))
		if err != nil {
			w.pageDirty = true
			return "", w.classify(ctx, err)
		}
		// 轮询结果：null 与缺失按空串处理（gson 的 Str 对 null 会渲染 "<nil>"，
		// 必须经 Nil() 判空）
		var outcome, errMsg string
		if res != nil && !res.Value.Nil() {
			if v := res.Value.Get("outcome"); !v.Nil() {
				outcome = v.Str()
			}
			if v := res.Value.Get("error"); !v.Nil() {
				errMsg = v.Str()
			}
		}
		if errMsg != "" {
			w.pageDirty = true
			return "", fmt.Errorf("captcha %s", redact(errMsg))
		}
		if result := strings.TrimSpace(outcome); result != "" {
			return result, nil
		}
		if !time.Now().Before(deadline) {
			w.pageDirty = true
			return "", fmt.Errorf("captcha solve timeout after %gs", w.solveTimeout.Seconds())
		}
		sleepCtx(ctx, 250*time.Millisecond) // 轮询间隔对齐 Python 0.25s
	}
}

// classify 区分浏览器死亡与页面级失败：死亡语义交由池替换 worker。
func (w *RodWorker) classify(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// 轻量 CDP 探测：浏览器进程已退出时必然失败。
	//
	// 必须自带 deadline：w.browser 绑的是 browserCtx（WithCancel，无超时），
	// 浏览器假死（CDP socket 连著但不回应）时 Call 会永久阻塞，而这里卡住等于
	// 槽位永不归队——调用方即使超时也走不到「槽位 condemned → 重建」那一步。
	// workers 默认 1 时，一次假死就让整个验证码池永久失效且 IsStarted 仍为 true。
	//
	// Context 返回克隆，故 probe 不影响 w.browser 自身的 ctx。
	probeCtx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if _, perr := (proto.BrowserGetVersion{}).Call(w.browser.Context(probeCtx)); perr != nil {
		return fmt.Errorf("%w: %v", ErrWorkerDead, err)
	}
	return err
}

// reload 关闭旧页面并重新加载 SDK 页面（对齐 worker_main 失败后的页面重载）。
func (w *RodWorker) reload(ctx context.Context) error {
	if w.page != nil {
		_ = w.page.Close()
		w.page = nil
	}
	page, err := loadSDKPage(ctx, w.browser, w.cfg)
	if err != nil {
		// 重载失败：下一次 Solve 再试（worker 保持健康，等价 Python 的
		// FAILED 页面重载失败后继续服务）
		return err
	}
	w.page = page
	return nil
}

// Close 释放浏览器与会话资源。
//
// 顺序：先取消 ctx 让所有在途/后续 CDP 调用立即失败返回（浏览器假死时
// Close/Page 都会永久阻塞），再关页面与浏览器，最后有界地杀进程。
func (w *RodWorker) Close() error {
	if w.browserCancel != nil {
		w.browserCancel()
	}
	if w.page != nil {
		_ = w.page.Close()
		w.page = nil
	}
	if w.browser != nil {
		_ = w.browser.Close()
	}
	closeLauncher(w.launcher) // 有界：杀进程 + 移除临时 user-data-dir
	return nil
}

// launcherCleanupTimeout 关闭启动器时的等待上限。
//
// rod 的 Launcher.Cleanup 是 `<-l.exit`（等浏览器进程退出），没有任何上限；
// 浏览器假死或启动后既不打印 DevTools URL 也不退出时，它会永久阻塞——而这里
// 运行在池的槽位 goroutine 上，卡住即等于该槽位永不归队（workers 默认 1 时
// 整个池失效）。改为先杀进程、再有界等待。
const launcherCleanupTimeout = 5 * time.Second

// closeLauncher 有界地关闭启动器：杀掉浏览器进程，最多等 launcherCleanupTimeout
// 让 rod 完成自身的退出处理与 user-data-dir 清理，超时则直接返回。
func closeLauncher(l *launcher.Launcher) {
	if l == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Kill()
		l.Cleanup()
	}()
	select {
	case <-done:
	case <-time.After(launcherCleanupTimeout):
		web.Warn("captcha", "浏览器进程未在超时内退出，已放弃等待")
	}
}

// sleepCtx 可中断睡眠。
func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// ── 浏览器二进制发现 ─────────────────────────────────────────────────────────

// DiscoverBrowserBinary 定位 cloakbrowser 下载的 Chromium 可执行文件：
//  1. ZCODE_CAPTCHA_BROWSER_BIN 显式指定；
//  2. CLOAKBROWSER_BINARY_PATH（cloakbrowser 自身的覆盖变量，与 Python 版共享）；
//  3. CLOAKBROWSER_CACHE_DIR（默认 ~/.cloakbrowser）下 chromium-<版本>[<suffix>]
//     目录，取版本号最高者。可执行文件名按平台：Linux chrome、Windows chrome.exe、
//     macOS Chromium.app 包（对齐 cloakbrowser get_binary_path）。
func DiscoverBrowserBinary(ctx context.Context) (string, error) {
	if bin := strings.TrimSpace(config.CaptchaBrowserBin); bin != "" {
		if info, err := os.Stat(bin); err == nil && !info.IsDir() {
			return bin, nil
		}
		return "", fmt.Errorf("ZCODE_CAPTCHA_BROWSER_BIN 指定的浏览器二进制不存在: %s", bin)
	}
	if p := strings.TrimSpace(os.Getenv("CLOAKBROWSER_BINARY_PATH")); p != "" {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
		return "", fmt.Errorf("CLOAKBROWSER_BINARY_PATH 指定的浏览器二进制不存在: %s", p)
	}

	cacheDir := strings.TrimSpace(os.Getenv("CLOAKBROWSER_CACHE_DIR"))
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法定位浏览器缓存目录: %w", err)
		}
		cacheDir = filepath.Join(home, ".cloakbrowser")
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		// 缓存目录不可读（首次部署）：走自动下载，无需 Python 预下载。
		return discoverViaDownload(ctx)
	}
	type candidate struct {
		version []int
		path    string
	}
	var candidates []candidate
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "chromium-") {
			continue
		}
		v := strings.TrimPrefix(e.Name(), "chromium-")
		v = strings.TrimSuffix(v, "-pro")
		nums, ok := parseVersion(v)
		if !ok {
			continue
		}
		path := filepath.Join(cacheDir, e.Name(), executableName())
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			candidates = append(candidates, candidate{version: nums, path: path})
		}
	}
	if len(candidates) == 0 {
		return discoverViaDownload(ctx)
	}
	// 版本降序，取最高
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].version, candidates[j].version
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] > b[k]
			}
		}
		return len(a) > len(b)
	})
	return candidates[0].path, nil
}

// discoverViaDownload 发现链落空后的兜底：自动下载补丁 Chromium 再重扫缓存目录。
// 下载失败返回原始错误（人工回填兜底不受影响）。
//
// 传入 ctx 而非自建 Background：首次下载约 200MB，可能远超调用方的
// CaptchaBrowserStartupTimeout；不自建 ctx 会让调用方放弃后下载仍在跑，
// 槽位 goroutine 与 cm.Close() 都无法取消它。
func discoverViaDownload(ctx context.Context) (string, error) {
	version, err := EnsureBrowserBinary(ctx)
	if err != nil {
		return "", fmt.Errorf("自动下载补丁 Chromium 失败（可手动执行 python -m cloakbrowser install 或设 ZCODE_CAPTCHA_BROWSER_BIN）: %w", err)
	}
	bin := filepath.Join(cloakBinaryDir(version), executableName())
	if info, statErr := os.Stat(bin); statErr != nil || info.IsDir() {
		return "", fmt.Errorf("补丁 Chromium 安装异常: %s", bin)
	}
	return bin, nil
}

// parseVersion 解析点分版本号为整数段（如 146.0.7680.177.5）。
func parseVersion(s string) ([]int, bool) {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return nil, false
	}
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, false
		}
		nums = append(nums, n)
	}
	return nums, true
}

// executableName 按平台返回 Chromium 可执行文件名（对齐 cloakbrowser）。
func executableName() string {
	switch runtime.GOOS {
	case "windows":
		return "chrome.exe"
	case "darwin":
		return filepath.Join("Chromium.app", "Contents", "MacOS", "Chromium")
	default:
		return "chrome"
	}
}
