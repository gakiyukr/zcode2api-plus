// zcode2api 服务入口：API 网关 + 后台管理 API + SPA 托管。
// 对应 Python 版 app/main.py 的启动流程与横幅。
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"zcode2api" // 嵌入的前端构建产物（仓库根包，受 go:embed 目录约束）

	"zcode2api/internal/adminapi"
	"zcode2api/internal/asyncpool"
	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/guest"
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
	if err := serve(); err != nil {
		web.Err("main", "服务退出: "+err.Error())
		os.Exit(1)
	}
}

// serve 启动网关 + 后台管理 + SPA（对应 Python 版 main.py serve）。
//
// 返回 error 而非直接 os.Exit：os.Exit 会跳过所有 defer，浏览器池
// （Chromium 子进程）、SQLite 连接与监控循环都不会收尾。
func serve() error {
	st, err := store.New()
	if err != nil {
		return fmt.Errorf("存储初始化失败: %w", err)
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

	// 访客账号提交（/guest/*）：仅 OAuth + 实测通过才入池。
	// 默认关闭——需管理员在后台设置邀请码后才开放。
	guest.New(st, authSvc, cm, qs, engine).Register(mux)

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
	srv := newServer(addr, mux)

	// 收到 SIGINT/SIGTERM 时优雅退出：Shutdown 停止接受新连接并等待在途请求，
	// 返回后 defer 链才会执行（关闭浏览器池、DB、监控循环）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		web.Ok("main", "服务运行中 "+addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		web.Ok("main", "收到退出信号，正在收尾…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("优雅退出超时: %w", err)
		}
		return <-errCh
	}
}

// newServer 构造 HTTP 服务端。
//
// ReadHeaderTimeout 防 Slowloris（慢速发请求头占住连接）。
// IdleTimeout 回收 keep-alive 空连接——它只作用于「响应已写完、等待下一个
// 请求」的空闲期，与流式响应无关；不设则 net/http 会把读期限清空
// （server.go: idleTimeout()==0 且 ReadTimeout==0 → SetReadDeadline(zero)），
// 空闲连接永不回收，goroutine 与 fd 无界累积。
//
// WriteTimeout 保持零值：SSE 与 async 票务的响应阶段会持续数分钟，
// 设了会在流中途掐断（部署文档的 proxy_read_timeout 3600s 即为此配合）。
func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
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
