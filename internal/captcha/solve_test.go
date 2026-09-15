// RodWorker 构造路径的测试：覆盖「浏览器启动成功但 CDP 连接失败」的清理分支。
package captcha

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildFakeBrowser 编译一个「印出 DevTools URL 后立即退出」的假浏览器。
//
// rod 从 stdout 抓 ws:// 地址后去连接，而进程已退出，于是
// cdp.StartWithURL 失败——这正是 newRodWorker 里 Connect 失败的分支。
func buildFakeBrowser(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fakeBrowserSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fakebrowser")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("无法编译假浏览器（跳过）: %v\n%s", err, out)
	}
	return bin
}

// 假浏览器必须真的起一个 HTTP 服务：rod 的 launcher.Launch 会在进程启动后
// 对 stdout 抓到的地址发 HTTP GET /json/version（launcher.go 的 ResolveURL），
// 拿不到 webSocketDebuggerUrl 就判定启动失败——那样走的是「启动失败」分支，
// 覆盖不到 Connect 失败。这里回一个合法 JSON 但不做 WebSocket 握手，
// 于是 Launch 成功、cdp.StartWithURL 失败，正好落在目标分支上。
const fakeBrowserSrc = `package main

import (
	"fmt"
	"net"
	"net/http"
)

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"webSocketDebuggerUrl\":\"ws://127.0.0.1:%d/devtools/browser/fake\"}", port)
	})
	// 其余路径一律普通 200：WebSocket 握手必然失败
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	fmt.Printf("DevTools listening on ws://127.0.0.1:%d/devtools/browser/fake\n", port)
	_ = http.Serve(ln, mux)
}
`

// Connect 失败时不得 panic，且必须返回错误。
//
// rod 的 Browser.Connect 只在 cdp.StartWithURL 成功后给 client 赋值，失败时
// client 仍是 nil；此时调用 Browser.Close 会走 b.client.Call(...)，对 nil
// interface 调用方法直接 panic。这段代码跑在池的槽位 goroutine 上，
// 一次浏览器启动异常就会终止整个网关进程。
func TestNewRodWorkerConnectFailureDoesNotPanic(t *testing.T) {
	bin := buildFakeBrowser(t)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Connect 失败路径不得 panic: %v", r)
		}
	}()

	_, err := newRodWorker(context.Background(), bin, Config{
		Enabled: true, SceneID: "sc", Region: "cn", Prefix: "px",
	})
	if err == nil {
		t.Fatal("连不上 DevTools 时应返回错误")
	}
	if !strings.Contains(err.Error(), "连接失败") {
		t.Fatalf("错误应指明连接失败: %v", err)
	}
}
