// RodWorker 构造路径的测试：覆盖「浏览器启动成功但 CDP 连接失败」的清理分支。
package captcha

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
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

// mockCDPClient 模擬 go-rod 的底層 CDP 通道，用於不啟動真實瀏覽器的單元測試。
type mockCDPClient struct {
	events chan *cdp.Event
	callFn func(ctx context.Context, sessionID, method string, params any) ([]byte, error)
}

func (m *mockCDPClient) Event() <-chan *cdp.Event { return m.events }

func (m *mockCDPClient) Call(ctx context.Context, sessionID, method string, params any) ([]byte, error) {
	return m.callFn(ctx, sessionID, method, params)
}

func TestReloadFailureReturnsErrWorkerDead(t *testing.T) {
	origProbe := probeTimeout
	probeTimeout = 100 * time.Millisecond
	defer func() { probeTimeout = origProbe }()

	// 模擬 Chromium 啟動連線成功後，在執行期崩潰：後續 Target.createTarget 與 Browser.getVersion 皆失敗
	dead := false
	mock := &mockCDPClient{
		events: make(chan *cdp.Event),
		callFn: func(ctx context.Context, sessionID, method string, params any) ([]byte, error) {
			if dead {
				return nil, errors.New("use of closed network connection")
			}
			return []byte(`{}`), nil
		},
	}
	b := rod.New().Client(mock).NoDefaultDevice()
	if err := b.Connect(); err != nil {
		t.Fatalf("mock browser connect 失敗: %v", err)
	}
	dead = true

	w := &RodWorker{
		browser:      b,
		pageDirty:    true,
		solveTimeout: time.Second,
		sdkLoadTTL:   time.Second,
	}

	_, err := w.Solve(context.Background())
	if !errors.Is(err, ErrWorkerDead) {
		t.Fatalf("reload 失敗且瀏覽器已死亡時應回傳 ErrWorkerDead，實際: %v", err)
	}

	// 驗證 Pool.serveLoop 收到 ErrWorkerDead 後會觸發重啟並以新 worker 完成後續請求
	created := 0
	p := newTestPool(t, func(ctx context.Context) (Worker, error) {
		created++
		if created == 1 {
			return w, nil
		}
		return &scriptWorker{id: created, steps: []stepFunc{tokenStep("recovered-token")}}, nil
	}, 1)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Solve(context.Background()); !errors.Is(err, ErrWorkerDead) {
		t.Fatalf("首次 Solve 應回傳 ErrWorkerDead，實際: %v", err)
	}
	tok, err := p.Solve(context.Background())
	if err != nil || tok != "recovered-token" {
		t.Fatalf("替換後新 worker 應成功求解，實際: tok=%q err=%v", tok, err)
	}
	if st := p.StatsSnapshot(); st.WorkerRestarts < 1 {
		t.Fatalf("Pool 應觸發 worker 重啟計數，實際: %+v", st)
	}
}

func TestReloadPageCloseTimeout(t *testing.T) {
	origProbe := probeTimeout
	probeTimeout = 80 * time.Millisecond
	defer func() { probeTimeout = origProbe }()

	// 模擬假死瀏覽器：先建立一個合法頁面，之後所有關閉/重建呼叫在未取消前永久阻塞
	pageCreated := false
	mock := &mockCDPClient{
		events: make(chan *cdp.Event),
		callFn: func(ctx context.Context, sessionID, method string, params any) ([]byte, error) {
			switch method {
			case "Target.createTarget":
				if !pageCreated {
					pageCreated = true
					return []byte(`{"targetId":"t1"}`), nil
				}
				return nil, errors.New("browser hung")
			case "Target.attachToTarget":
				return []byte(`{"sessionId":"s1"}`), nil
			case "Page.close", "Target.closeTarget":
				// 模擬 Chromium 假死不回應，直到 closeCtx 超時取消才返回
				<-ctx.Done()
				return nil, ctx.Err()
			default:
				return []byte(`{}`), nil
			}
		},
	}
	b := rod.New().Client(mock).NoDefaultDevice()
	if err := b.Connect(); err != nil {
		t.Fatalf("mock browser connect 失敗: %v", err)
	}
	page, err := b.Page(proto.TargetCreateTarget{})
	if err != nil {
		t.Fatalf("建立初始 mock page 失敗: %v", err)
	}
	w := &RodWorker{
		browser: b,
		page:    page,
	}

	start := time.Now()
	_ = w.reload(context.Background())
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("reload 關閉假死頁面應在 probeTimeout 內返回，實際耗時 %s", elapsed)
	}
	if w.page != nil {
		t.Fatal("reload 關閉舊頁後在載入失敗時 w.page 應已清空為 nil")
	}
}
