// 服务端构造与退出语义的测试：这些是生产环境的硬约束，
// 一旦被改回默认值就会造成连接堆积或流被掐断。
package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
