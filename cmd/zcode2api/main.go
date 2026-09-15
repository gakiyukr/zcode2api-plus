// zcode2api 服务入口：API 网关 + 后台管理 API + SPA 托管。
// 对应 Python 版 app/main.py 的启动流程与横幅。
package main

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"

	"zcode2api" // 嵌入的前端构建产物（仓库根包，受 go:embed 目录约束）

	"zcode2api/internal/adminapi"
	"zcode2api/internal/asyncpool"
	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/openai"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

func main() {
	if len(os.Args) > 1 {
		os.Exit(runCLI(os.Args[1], os.Args[2:], serve))
	}
	serve()
}

// serve 启动网关 + 后台管理 + SPA（对应 Python 版 main.py serve）。
func serve() {
	st, err := store.New()
	if err != nil {
		web.Err("main", "存储初始化失败: "+err.Error())
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	mux := http.NewServeMux()
	authSvc := auth.New(st)
	cm := captcha.NewManager()
	// 浏览器池求解器（M5）：启用时注入 rod 求解，失败冷却后回退人工回填
	if config.CaptchaBrowserEnabled {
		cm.SetSolver(captcha.NewBrowserSolver())
	}
	defer func() { _ = cm.Close() }()

	// 额度查询：网关成功/耗尽路径触发刷新，后台管理端点与周期监控共用
	qs := quota.NewService(st)

	// 网关 + 后台管理 API（各自端点内建鉴权）
	engine := gateway.NewEngine(st, cm, nil)
	engine.OnQuotaRefresh = func(acc *model.Account) { _ = qs.FetchQuota(acc) }
	gw := gateway.Handler{Engine: engine, Auth: authSvc}
	gw.Register(mux)
	adminapi.New(st, authSvc, cm, qs).Register(mux)

	// OpenAI 兼容层：/v1/chat/completions 复用同一引擎（M4）
	openai.New(engine, authSvc).Register(mux)

	// Async 空闲池：与 Python 版一致按设置条件挂载
	if config.AsyncEnabled {
		asyncpool.NewPool(st, authSvc, cm).Register(mux)
	}

	// SPA 托管（/ → /admin、/assets 静态、/admin/{path...} 回落 index.html、/meta）
	web.NewSPA(distSub()).Register(mux)

	// 后台额度监控：随服务启动、退出时等待循环收尾（对齐 lifespan）
	mon := qs.NewMonitor()
	mon.Start()
	defer mon.Stop()

	printBanner(st)

	addr := fmt.Sprintf("%s:%d", config.Host, config.Port)
	web.Ok("main", "服务运行中 "+addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		web.Err("main", "服务退出: "+err.Error())
		os.Exit(1)
	}
}

// distSub 从嵌入根提取 frontend/dist 子树；缺失时返回 nil（页面路由 404 提示）。
func distSub() fs.FS {
	sub, err := fs.Sub(zcode2api.DistFS, "frontend/dist")
	if err != nil {
		return nil
	}
	return sub
}

// printBanner 打印启动横幅；密钥由引导逻辑生成时必须在此交付给管理者，
// 否则无法登录／调用（对齐 Python main.py，原文措辞保留）。
func printBanner(st *store.Store) {
	base := fmt.Sprintf("http://%s:%d", displayHost(), config.Port)
	lines := []string{
		fmt.Sprintf("%szcode2api-plus%s %sv%s · Go%s",
			web.Bold+web.Magenta, web.Reset, web.Dim, config.AppVersion, web.Reset),
		fmt.Sprintf("%s后台管理%s  %s%s/admin/login%s", web.Dim, web.Reset, web.Cyan, base, web.Reset),
		fmt.Sprintf("%s对话端点%s  %s%s/v1/messages%s", web.Dim, web.Reset, web.Cyan, base, web.Reset),
	}
	if st.GeneratedAdminKey != "" {
		lines = append(lines, fmt.Sprintf(
			"%s初始后台密码%s  %s%s%s %s（请登录后尽快在「设置」页修改）%s",
			web.Dim, web.Reset, web.Yellow, st.GeneratedAdminKey, web.Reset, web.Dim, web.Reset))
	}
	if st.GeneratedGatewayKey != "" {
		lines = append(lines, fmt.Sprintf(
			"%s网关 API Key%s  %s%s%s %s（调用 /v1/messages 需携带，可在「设置」页修改）%s",
			web.Dim, web.Reset, web.Yellow, st.GeneratedGatewayKey, web.Reset, web.Dim, web.Reset))
	}
	web.Banner(lines...)
}

// displayHost 横幅展示用主机：通配地址在浏览器中不可直接访问，显示 127.0.0.1
// （对齐 Python _display_host）。
func displayHost() string {
	switch config.Host {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return config.Host
}
