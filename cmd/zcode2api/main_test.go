// 服务端构造与退出语义的测试：这些是生产环境的硬约束，
// 一旦被改回默认值就会造成连接堆积或流被掐断。
package main

import (
	"net/http"
	"testing"
	"time"
)

// TestNewServerTimeoutContract 约束 HTTP 服务端的超时配置。
//
// ReadHeaderTimeout 必须有：零值意味着慢速发请求头的连接可以无限占住
// 服务端资源（Slowloris），最终耗尽连接数。
// WriteTimeout 必须为零：SSE 与 async 票务会持续数分钟，设了会在流中途
// 掐断连接（部署文档用 proxy_read_timeout 3600s 与之配合）。
func TestNewServerTimeoutContract(t *testing.T) {
	srv := newServer("127.0.0.1:0", http.NewServeMux())

	if srv.ReadHeaderTimeout <= 0 {
		t.Fatal("必须设置 ReadHeaderTimeout（否则 Slowloris 可耗尽连接）")
	}
	if srv.ReadHeaderTimeout > time.Minute {
		t.Fatalf("ReadHeaderTimeout 过长，形同未设: %v", srv.ReadHeaderTimeout)
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
