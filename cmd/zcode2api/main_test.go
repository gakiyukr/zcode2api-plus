// 服务端构造与退出语义的测试：这些是生产环境的硬约束，
// 一旦被改回默认值就会造成连接堆积或流被掐断。
package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewServerTimeoutContract 约束 HTTP 服务端的超时配置。
//
// ReadHeaderTimeout 必须有：零值意味着慢速发请求头的连接可以无限占住
// 服务端资源（Slowloris），最终耗尽连接数。
// IdleTimeout 必须有：它只管「响应写完、等待下一个请求」的空闲期，与流式
// 响应无关；为零时 net/http 会把读期限清空，keep-alive 空连接永不回收。
// WriteTimeout 必须为零：SSE 与 async 票务的响应阶段持续数分钟，设了会在
// 流中途掐断连接（部署文档用 proxy_read_timeout 3600s 与之配合）。
func TestNewServerTimeoutContract(t *testing.T) {
	srv := newServer("127.0.0.1:0", http.NewServeMux())

	if srv.ReadHeaderTimeout <= 0 {
		t.Fatal("必须设置 ReadHeaderTimeout（否则 Slowloris 可耗尽连接）")
	}
	if srv.ReadHeaderTimeout > time.Minute {
		t.Fatalf("ReadHeaderTimeout 过长，形同未设: %v", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout <= 0 {
		t.Fatal("必须设置 IdleTimeout（否则 keep-alive 空连接永不回收）")
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout 必须为零，否则 SSE 长连接会在流中途被掐断: %v", srv.WriteTimeout)
	}
	if srv.Handler == nil {
		t.Fatal("必须挂载 handler")
	}
	if srv.Addr != "127.0.0.1:0" {
		t.Fatalf("Addr 应透传: %s", srv.Addr)
	}
}

// TestBodyLimitContract 约束请求体大小上限。
//
// 8 个对外入口都把 r.Body 直接解进 map[string]any，解出来的内存远大于线上
// 字节数，网关还会为每个候选账号再序列化一次。没有上限时一个超大 JSON 就能
// 撑爆进程，连带杀掉所有在途 SSE 串流。
func TestBodyLimitContract(t *testing.T) {
	srv := newServer("127.0.0.1:0", http.NewServeMux())
	if srv.MaxHeaderBytes <= 0 {
		t.Fatal("必须设置 MaxHeaderBytes")
	}

	// 走一遍真实链路：handler 读体，超限应报错而非读入内存
	var readErr error
	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 0, 1024)
		tmp := make([]byte, 4096)
		for {
			n, err := r.Body.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				// io.EOF 是读完的正常结束，只有其他错误才算超限
				if !errors.Is(err, io.EOF) {
					readErr = err
				}
				break
			}
		}
		_, _ = w.Write([]byte("ok"))
	})

	handler := limitBody(mux)
	// 略超上限的请求体：读取必须在某处停下并报错，不能全量读入
	oversized := make([]byte, maxBodyBytes+1024)
	for i := range oversized {
		oversized[i] = 'a'
	}
	req := httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(oversized))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if readErr == nil {
		t.Fatal("超限请求体应在读取时报错")
	}
	var maxErr *http.MaxBytesError
	if !errors.As(readErr, &maxErr) {
		t.Fatalf("应为 MaxBytesError，得到 %T: %v", readErr, readErr)
	}

	// 正常大小的请求体必须完整可读——上限不能误伤合法请求
	readErr = nil
	normal := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)
	req = httptest.NewRequest(http.MethodPost, "/echo", bytes.NewReader(normal))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if readErr != nil {
		t.Fatalf("正常请求体不应报错: %v", readErr)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("正常请求应被处理: %s", rec.Body.String())
	}
}

// TestSecurityHeadersContract 约束浏览器侧的安全策略。
//
// 后台金钥存在 localStorage，而 localStorage 以 origin 为界、不分路径：公开的
// /guest 页与 /admin 共用同一份存储。因此必须收紧「能执行脚本的来源」，并禁止
// 本站被 iframe 嵌入（后台按钮单击即生效，点击劫持可用）。
func TestSecurityHeadersContract(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("page"))
	})
	handler := securityHeaders(mux)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))

	h := rec.Header()
	csp := h.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("必须设置 Content-Security-Policy")
	}
	// 点击劫持：后台操作全是单击生效，必须禁止被嵌入
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP 必须禁止被 iframe 嵌入: %s", csp)
	}
	// 前端实际加载两个外部脚本，缺任一都会让对应页面失效
	for _, origin := range []string{"https://cdn.jsdelivr.net", "https://o.alicdn.com"} {
		if !strings.Contains(csp, origin) {
			t.Fatalf("CSP 必须放行 %s（页面依赖该脚本）: %s", origin, csp)
		}
	}
	// Cap widget 把 PoW 放进 Blob URL 构造的 Web Worker；不放行 blob: 则
	// worker 创建被拒，人机验证永远出不来题。
	if !strings.Contains(csp, "blob:") {
		t.Fatalf("CSP 必须放行 blob:（Cap widget 的 Web Worker 依赖）: %s", csp)
	}
	if !strings.Contains(csp, "worker-src") {
		t.Fatalf("CSP 应显式声明 worker-src: %s", csp)
	}
	// MIME 嗅探：防止上传/返回的内容被当作脚本执行
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options 应为 nosniff，得到 %q", got)
	}
	if h.Get("X-Frame-Options") == "" {
		t.Fatal("应设置 X-Frame-Options 供旧浏览器兜底")
	}
	if h.Get("Referrer-Policy") == "" {
		t.Fatal("应设置 Referrer-Policy")
	}
}
