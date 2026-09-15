// CLI 子命令：对齐 Python 版 main.py 的命令面（login 依赖 OAuth 交互）。
// serve 之外的所有命令在此实现，main() 做分发。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/claim"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/oauth"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// cliUsage 与 Python __doc__ 对齐的用法说明。
const cliUsage = `ZCode2api (Go)

用法:
  zcode2api serve [--port 3000]        启动网关 + 后台 UI
  zcode2api login zai [--no-browser]   通过 OAuth 登录 Z.AI 并自动加入账号池
  zcode2api add-account zai <name> <jwt|key>   添加轮询账号
  zcode2api accounts [zai]             查看账号列表
  zcode2api remove-account <provider> <id|name>
  zcode2api quota                      查看各账号实时额度
  zcode2api status                     查看配置概览
  zcode2api prefetch-browser           预下载验证码用的补丁 Chromium（约 200MB）
  zcode2api set-admin-key <key>        设置后台密码
  zcode2api export [file]              导出账号
  zcode2api import <file>              导入账号
`

// runCLI 命令分发；返回进程退出码。cmd 为空串表示无参数。
func runCLI(cmd string, rest []string, serveFn func()) int {
	switch cmd {
	case "", "help", "-h", "--help":
		fmt.Print(cliUsage)
		return 0
	case "serve":
		if i := indexOf(rest, "--port"); i >= 0 && i+1 < len(rest) {
			if port, err := strconv.Atoi(rest[i+1]); err == nil {
				config.Port = port
			}
		}
		serveFn()
		return 0
	case "login":
		cmdLogin(rest)
		return 0
	case "add-account":
		cmdAddAccount(rest)
		return 0
	case "accounts":
		cmdAccounts(rest)
		return 0
	case "remove-account":
		cmdRemoveAccount(rest)
		return 0
	case "set-admin-key":
		cmdSetAdminKey(rest)
		return 0
	case "status":
		cmdStatus()
		return 0
	case "prefetch-browser":
		return cmdPrefetchBrowser()
	case "quota":
		cmdQuota()
		return 0
	case "export":
		cmdExport(rest)
		return 0
	case "import":
		cmdImport(rest)
		return 0
	}
	fmt.Println(web.Red + "未知命令: " + cmd + web.Reset)
	fmt.Print(cliUsage)
	return 1
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// openStore CLI 公共入口：打开数据库，失败即退出。
func openStore() *store.Store {
	st, err := store.New()
	if err != nil {
		web.Err("cli", "存储初始化失败: "+err.Error())
		os.Exit(1)
	}
	return st
}

// ── login ───────────────────────────────────────────────────────────────────

func cmdLogin(args []string) {
	if len(args) == 0 || args[0] != "zai" {
		fmt.Println(web.Red + "目前仅支持: zcode2api login zai" + web.Reset)
		return
	}
	flow := oauth.NewFlow()
	flowID, authorizeURL, err := flow.Init()
	if err != nil {
		fmt.Println(web.Red + "❌ 登录初始化失败: " + err.Error() + web.Reset)
		return
	}
	_ = flowID
	fmt.Println(web.Green + "\n✔ OAuth 初始化成功！请在浏览器中打开下面链接完成授权：" + web.Reset)
	fmt.Println(web.Blue + authorizeURL + web.Reset)

	if indexOf(args, "--no-browser") < 0 {
		_ = exec.Command("cmd", "/c", "start", authorizeURL).Start() // Windows
	}

	fmt.Println("\n授权完成后，请复制浏览器地址栏中的完整登录完成页地址并粘贴：")
	fmt.Print("> ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	code, state, oauthErr, err := oauth.ParseCallbackURL(line)
	if err != nil {
		fmt.Println(web.Red + "❌ 授权失败: " + err.Error() + web.Reset)
		return
	}
	if oauthErr != "" {
		fmt.Println(web.Red + "❌ 授权失败: Z.AI 拒绝授权: " + oauthErr + web.Reset)
		return
	}
	if !flow.MatchesState(state) {
		fmt.Println(web.Red + "❌ 授权失败: state 校验失败" + web.Reset)
		return
	}
	result, err := flow.ExchangeCode(code, state)
	if err != nil {
		fmt.Println(web.Red + "❌ 授权失败: " + err.Error() + web.Reset)
		return
	}

	st := openStore()
	defer func() { _ = st.Close() }()
	if result.Token != "" {
		acc, err := st.AddAccount(model.ProviderZai, "oauth-login", result.Token)
		if err != nil {
			fmt.Println(web.Red + "❌ 保存 JWT 账号失败: " + err.Error() + web.Reset)
			return
		}
		if result.Email != nil && strings.TrimSpace(*result.Email) != "" {
			st.Update(acc.Provider, acc.ID, func(a *model.Account) {
				a.Email = result.Email
				a.Name = *result.Email
			})
			// Update 改的是 Store 内部对象，acc 仍是 AddAccount 时的副本；
			// 重新取快照，否则下面会打印出改名前的旧名字。
			if fresh := st.Find(acc.Provider, acc.ID); fresh != nil {
				acc = fresh
			}
		}
		fmt.Println(web.Green + fmt.Sprintf("\n✔ 已保存 Coding Plan JWT 账号: %s (%s)", acc.Name, acc.ID) + web.Reset)
		// 入池即激活上报 + 自动领取全部可领活动套餐（失败仅提示，不中断；
		// 对齐 Python cmd_login 的 auto_claim_all_plans）
		cm := captcha.NewManager()
		if config.CaptchaBrowserEnabled {
			cm.SetSolver(captcha.NewBrowserSolver())
		}
		outcomes := claim.NewService(cm).AutoClaimAllPlans(acc)
		for _, o := range outcomes {
			if ok, _ := o["ok"].(bool); ok {
				planName, _ := o["plan_name"].(string)
				fmt.Println(web.Green + "✔ 自动领取成功: " + planName + web.Reset)
			} else {
				msg, _ := o["message"].(string)
				fmt.Println(web.Yellow + "⚠️ 自动领取失败: " + msg + web.Reset)
			}
		}
		_ = cm.Close()
	}
	if result.AccessToken != "" {
		if key, err := oauth.ExchangeAPIKey(result.AccessToken); err == nil {
			if _, err := st.AddAccount(model.ProviderZai, "oauth-apikey", key); err == nil {
				fmt.Println(web.Green + "✔ 已兑换并保存 API Key: " + key[:min(8, len(key))] + "..." + web.Reset)
			}
		} else {
			fmt.Println(web.Yellow + "⚠️ 兑换 API Key 失败: " + err.Error() + web.Reset)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ── 账号管理 ────────────────────────────────────────────────────────────────

func cmdAddAccount(args []string) {
	if len(args) < 3 {
		fmt.Println(web.Red + "格式: zcode2api add-account <zai> <name> <jwt|key>" + web.Reset)
		return
	}
	st := openStore()
	defer func() { _ = st.Close() }()
	acc, err := st.AddAccount(args[0], args[1], args[2])
	if err != nil {
		fmt.Println(web.Red + "❌ 添加失败: " + err.Error() + web.Reset)
		return
	}
	fmt.Println(web.Green + fmt.Sprintf("✔ 已添加账号 %s (%s) 模式=%s", acc.Name, acc.ID, acc.Mode) + web.Reset)
}

func cmdAccounts(args []string) {
	provider := ""
	if len(args) > 0 && args[0] == model.ProviderZai {
		provider = args[0]
	}
	st := openStore()
	defer func() { _ = st.Close() }()
	accounts := st.ListAccounts(provider)
	if len(accounts) == 0 {
		fmt.Println("无账号")
		return
	}
	fmt.Println(web.Cyan + fmt.Sprintf("\n--- 账号列表 (%s) ---", or(provider, "全部")) + web.Reset)
	now := time.Now()
	for _, a := range accounts {
		tokens := fmt.Sprintf("  tok(in/out): %d/%d", a.TotalInputTokens, a.TotalOutputTokens)
		fmt.Printf("%s  %s  %s  %s  %s%s\n", a.ID, a.Provider, a.Mode, a.EffectiveStatus(now), a.Name, tokens)
	}
}

func cmdRemoveAccount(args []string) {
	if len(args) < 2 {
		fmt.Println(web.Red + "格式: zcode2api remove-account <provider> <id|name>" + web.Reset)
		return
	}
	st := openStore()
	defer func() { _ = st.Close() }()
	ok, err := st.RemoveAccount(args[0], args[1])
	if err != nil {
		fmt.Println(web.Red + "❌ 删除失败: " + err.Error() + web.Reset)
		return
	}
	if ok {
		fmt.Println(web.Green + "✔ 已删除账号 " + args[1] + web.Reset)
	} else {
		fmt.Println(web.Yellow + "⚠️ 未找到指定账号" + web.Reset)
	}
}

func cmdSetAdminKey(args []string) {
	if len(args) == 0 {
		fmt.Println(web.Red + "格式: zcode2api set-admin-key <key>" + web.Reset)
		return
	}
	st := openStore()
	defer func() { _ = st.Close() }()
	if err := st.SetSetting("admin_key", args[0]); err != nil {
		fmt.Println(web.Red + "❌ 更新失败: " + err.Error() + web.Reset)
		return
	}
	fmt.Println(web.Green + "✔ 已更新后台密码" + web.Reset)
}

// ── status / quota ──────────────────────────────────────────────────────────

// cmdPrefetchBrowser 预下载验证码求解用的补丁 Chromium。
// 浏览器池是惰性启动的（首次 JWT 请求才下载），新机部署后先执行本命令
// 可避免首个请求等待数分钟。返回进程退出码。
func cmdPrefetchBrowser() int {
	fmt.Println(web.Cyan + "\n--- 预下载补丁 Chromium ---" + web.Reset)
	if !config.CaptchaBrowserEnabled {
		fmt.Println(web.Yellow + "验证码浏览器未启用（ZCODE_CAPTCHA_BROWSER=false），无需下载" + web.Reset)
		return 0
	}
	// 已存在则直接报告，不重复下载
	if bin, err := captcha.DiscoverBrowserBinary(); err == nil {
		fmt.Printf("%s已存在: %s%s\n", web.Green, bin, web.Reset)
		return 0
	}
	fmt.Println("本地未发现，开始下载（约 200MB，可能需要数分钟）...")
	path, err := captcha.EnsureBrowserBinary(context.Background())
	if err != nil {
		fmt.Printf("%s下载失败: %v%s\n", web.Red, err, web.Reset)
		return 1
	}
	fmt.Printf("%s已就绪: %s%s\n", web.Green, path, web.Reset)
	return 0
}

func cmdStatus() {
	fmt.Println(web.Cyan + "\n--- zcode2api-plus (Go) 状态 ---" + web.Reset)
	fmt.Printf("数据库      : %s\n", web.Blue+config.DBPath+web.Reset)
	fmt.Printf("默认端口    : %s\n", web.Blue+strconv.Itoa(config.Port)+web.Reset)
	st := openStore()
	defer func() { _ = st.Close() }()
	if st.AdminKey() != "" {
		fmt.Println("后台密码    : 已设置")
	} else {
		fmt.Println(web.Yellow + "后台密码    : 未设置（将无法登录后台）" + web.Reset)
	}
	if st.GatewayKey() != "" {
		fmt.Println("网关 API Key: 已设置")
	} else {
		fmt.Println(web.Yellow + "网关 API Key: 未设置（网关将拒绝请求）" + web.Reset)
	}
	now := time.Now()
	accounts := st.ListAccounts(model.ProviderZai)
	active := 0
	for _, a := range accounts {
		if a.IsSelectable(now) {
			active++
		}
	}
	fmt.Printf("zai        : %d 个账号，%d 个可用\n", len(accounts), active)
}

func cmdQuota() {
	st := openStore()
	defer func() { _ = st.Close() }()
	accounts := st.ListAccounts(model.ProviderZai)
	now := time.Now()
	var jwtAccounts []*model.Account
	for _, a := range accounts {
		if a.Mode == "jwt" {
			jwtAccounts = append(jwtAccounts, a)
		}
	}
	if len(jwtAccounts) == 0 {
		fmt.Println(web.Yellow + "无 Coding Plan (JWT) 账号可查询额度。" + web.Reset)
		return
	}
	fmt.Println(web.Cyan + "\n正在拉取各账号实时额度..." + web.Reset)
	qs := quota.NewService(st)
	for _, a := range jwtAccounts {
		qs.FetchQuota(a)
		// FetchQuota 只把结果经 Store.Update 写回 Store 内部对象，调用方手上的
		// 副本不会被更新——必须重新取快照才能看到刚拉到的额度。
		a = st.Find(model.ProviderZai, a.ID)
		if a == nil {
			continue
		}
		fmt.Println(web.Bold + fmt.Sprintf("\n账号: %s (%s)", a.Name, a.EffectiveStatus(now)) + web.Reset)
		if len(a.Quota) == 0 {
			fmt.Println("  无额度数据")
			continue
		}
		names := make([]string, 0, len(a.Quota))
		for name := range a.Quota {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			q := a.Quota[name]
			fmt.Printf("  %s: 剩余 %s / 总额 %s\n", web.Cyan+name+web.Reset,
				anyToNum(q["remaining"]), anyToNum(q["total"]))
		}
	}
}

// anyToNum JSON 数值展示（quota 快照里是 float64）。
func anyToNum(v any) string {
	f, ok := v.(float64)
	if !ok {
		return "0"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ── export / import ─────────────────────────────────────────────────────────

func cmdExport(args []string) {
	out := "zcode-accounts.json"
	if len(args) > 0 {
		out = args[0]
	}
	st := openStore()
	defer func() { _ = st.Close() }()
	data, err := json.MarshalIndent(st.Export(), "", "  ")
	if err != nil {
		fmt.Println(web.Red + "❌ 序列化失败: " + err.Error() + web.Reset)
		return
	}
	if err := os.WriteFile(out, data, 0o600); err != nil {
		fmt.Println(web.Red + "❌ 写入失败: " + err.Error() + web.Reset)
		return
	}
	fmt.Println(web.Green + "✔ 已导出到 " + out + web.Reset)
}

func cmdImport(args []string) {
	if len(args) == 0 {
		fmt.Println(web.Red + "格式: zcode2api import <file>" + web.Reset)
		return
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Println(web.Red + "❌ 读取失败: " + err.Error() + web.Reset)
		return
	}
	var payload store.ImportPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		fmt.Println(web.Red + "❌ 文件不是合法的导出 JSON: " + err.Error() + web.Reset)
		return
	}
	st := openStore()
	defer func() { _ = st.Close() }()
	count, err := st.ImportAccounts(payload)
	if err != nil {
		fmt.Println(web.Red + "❌ 导入失败: " + err.Error() + web.Reset)
		return
	}
	fmt.Println(web.Green + fmt.Sprintf("✔ 已导入 %d 个账号", count) + web.Reset)
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
