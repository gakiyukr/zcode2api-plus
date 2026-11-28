// proxy 包单测：Transport 缓存、URL 校验、socks5 握手（对本地假代理）。
package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTransportForValidation(t *testing.T) {
	if _, err := TransportFor("ftp://x"); err == nil {
		t.Fatal("不支持的协议应报错")
	}
	if _, err := TransportFor(""); err != nil {
		t.Fatalf("空代理（直连）不应报错: %v", err)
	}
}

func TestTransportForCache(t *testing.T) {
	a, err := TransportFor("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	b, err := TransportFor("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("同 URL 应命中缓存返回同一 Transport")
	}
	c, err := TransportFor("")
	if err != nil {
		t.Fatal(err)
	}
	if c == a {
		t.Fatal("直连与代理不应共用 Transport")
	}
}

// fakeSocks5 启动一个最小 socks5 服务器：支持无鉴权 CONNECT 并回成功，
// 之后把后续字节转发给 behind（模拟真实目标）。
func fakeSocks5(t *testing.T, behind *httptest.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSocks5(conn, behind)
		}
	}()
	return ln.Addr().String()
}

func serveSocks5(conn net.Conn, behind *httptest.Server) {
	defer conn.Close()
	head := make([]byte, 2)
	if _, e := readFull(conn, head); e != nil || head[0] != 0x05 {
		return
	}
	methods := make([]byte, head[1])
	if _, e := readFull(conn, methods); e != nil {
		return
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}
	req := make([]byte, 4)
	if _, e := readFull(conn, req); e != nil || req[1] != 0x01 {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		ip := make([]byte, 4)
		if _, e := readFull(conn, ip); e != nil {
			return
		}
		host = net.IP(ip).String()
	case 0x03:
		n := make([]byte, 1)
		if _, e := readFull(conn, n); e != nil {
			return
		}
		name := make([]byte, n[0])
		if _, e := readFull(conn, name); e != nil {
			return
		}
		host = string(name)
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, e := readFull(conn, portBytes); e != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBytes)
	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	backend, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return
	}
	defer backend.Close()
	go copyBoth(backend, conn)
	copyBoth(conn, backend)
}

func copyBoth(dst net.Conn, src net.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func TestSocks5HandshakeThroughFakeProxy(t *testing.T) {
	behind := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Host", r.Host)
		w.WriteHeader(http.StatusOK)
	}))
	defer behind.Close()

	proxyAddr := fakeSocks5(t, behind)
	behindURL, _ := url.Parse(behind.URL)
	transport, err := TransportFor("socks5://" + proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + behindURL.Host + "/hello")
	if err != nil {
		t.Fatalf("经 socks5 代理请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得到 %d", resp.StatusCode)
	}
	if host := resp.Header.Get("X-Seen-Host"); !strings.Contains(host, behindURL.Host) {
		t.Fatalf("目标 Host 不符: %q", host)
	}
}

func TestSocks5RemoteResolveATYPDomain(t *testing.T) {
	// socks5h：域名由代理解析；假代理收到 ATYP=0x03 才算通过
	var seenATYP byte
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				head := make([]byte, 2)
				if _, e := readFull(c, head); e != nil {
					return
				}
				methods := make([]byte, head[1])
				if _, e := readFull(c, methods); e != nil {
					return
				}
				_, _ = c.Write([]byte{0x05, 0x00})
				req := make([]byte, 4)
				if _, e := readFull(c, req); e != nil {
					return
				}
				seenATYP = req[3]
				n := make([]byte, 1)
				if _, e := readFull(c, n); e != nil {
					return
				}
				rest := make([]byte, int(n[0])+2)
				if _, e := readFull(c, rest); e != nil {
					return
				}
				_, _ = c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
			}(conn)
		}
	}()

	u, _ := url.Parse("socks5h://" + ln.Addr().String())
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := socks5Handshake(ctx, conn, u, "example.com:80", true); err != nil {
		t.Fatalf("socks5h 握手失败: %v", err)
	}
	if seenATYP != 0x03 {
		t.Fatalf("socks5h 应以域名 ATYP=0x03 发送，得到 %d", seenATYP)
	}
}

// 本地解析得到 IPv6（或 IPv6 在首位）时，必须以 ATYP=0x04 发送，
// 不得送出 addr 长度为 0 的畸形 CONNECT（IPv4 优先，找不到才用 IPv6）。
func TestSocks5LocalResolveFallsBackToIPv6(t *testing.T) {
	var seenATYP byte
	var seenAddr []byte
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		head := make([]byte, 2)
		if _, e := readFull(conn, head); e != nil {
			return
		}
		methods := make([]byte, head[1])
		if _, e := readFull(conn, methods); e != nil {
			return
		}
		_, _ = conn.Write([]byte{0x05, 0x00})
		req := make([]byte, 4)
		if _, e := readFull(conn, req); e != nil {
			return
		}
		seenATYP = req[3]
		switch req[3] {
		case 0x01:
			seenAddr = make([]byte, 4)
		case 0x04:
			seenAddr = make([]byte, 16)
		case 0x03:
			n := make([]byte, 1)
			_, _ = readFull(conn, n)
			seenAddr = make([]byte, int(n[0]))
		}
		if _, e := readFull(conn, seenAddr); e != nil {
			return
		}
		port := make([]byte, 2)
		if _, e := readFull(conn, port); e != nil {
			return
		}
		_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	}()

	u, _ := url.Parse("socks5://" + ln.Addr().String())
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 用只解析出 IPv6 的主机名触发回退路径
	if err := socks5Handshake(ctx, conn, u, "[::1]:80", false); err != nil {
		t.Fatalf("IPv6 目标握手失败: %v", err)
	}
	if seenATYP != 0x04 {
		t.Fatalf("IPv6 目标应以 ATYP=0x04 发送，得到 %d", seenATYP)
	}
	if len(seenAddr) != 16 {
		t.Fatalf("ATYP=0x04 应带 16 字节地址，得到 %d", len(seenAddr))
	}
}

// 域名形式的 CONNECT 回复（ATYP=0x03）必须被完整消费。
//
// 回复头 5 字节里第 5 字节是域名长度，域名本身尚未读取。原实现把它当作
// 「已读走 1 字节地址」而算出 len-1+2，少读 1 字节：那个字节会留在 socket
// 里成为应用层数据流的第一个字节，表现为 TLS 握手失败或请求行被吃掉，
// 而错误信息完全指不到原因。既有测试一律回 IPv4（0x01），因此长期未暴露。
func TestSocks5DomainReplyFullyConsumed(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()
	backendHost := strings.TrimPrefix(backend.URL, "http://")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// 假代理：按 RFC 1928 完成协商与 CONNECT，但回复用 ATYP=0x03（域名）。
	// 回复的 ATYP 与请求无关，客户端必须能消费任意形式。
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				head := make([]byte, 2)
				if _, e := readFull(c, head); e != nil || head[0] != 0x05 {
					return
				}
				methods := make([]byte, head[1])
				if _, e := readFull(c, methods); e != nil {
					return
				}
				if _, e := c.Write([]byte{0x05, 0x00}); e != nil {
					return
				}
				req := make([]byte, 4)
				if _, e := readFull(c, req); e != nil || req[1] != 0x01 {
					return
				}
				switch req[3] {
				case 0x01:
					b := make([]byte, 4+2)
					if _, e := readFull(c, b); e != nil {
						return
					}
				case 0x03:
					l := make([]byte, 1)
					if _, e := readFull(c, l); e != nil {
						return
					}
					b := make([]byte, int(l[0])+2)
					if _, e := readFull(c, b); e != nil {
						return
					}
				}
				domain := "bnd.example.com"
				reply := []byte{0x05, 0x00, 0x00, 0x03, byte(len(domain))}
				reply = append(reply, []byte(domain)...)
				reply = append(reply, 0x1F, 0x90)
				if _, e := c.Write(reply); e != nil {
					return
				}
				up, err := net.Dial("tcp", backendHost)
				if err != nil {
					return
				}
				defer up.Close()
				go copyBoth(up, c)
				copyBoth(c, up)
			}(c)
		}
	}()

	transport, err := TransportFor("socks5://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	resp, err := client.Get(backend.URL)
	if err != nil {
		t.Fatalf("域名回复下请求失败（隧道被污染）: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("响应体被污染: %q", body)
	}
}

func TestSocks5MethodNegotiation(t *testing.T) {
	// 1. 帶帳密時，方法協商必須送出 0x02（RFC 1929 帳密認證），不得送出 0x01（GSSAPI）
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	var seenMethods []byte
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		head := make([]byte, 2)
		if _, err := readFull(serverConn, head); err != nil {
			return
		}
		seenMethods = make([]byte, head[1])
		if _, err := readFull(serverConn, seenMethods); err != nil {
			return
		}
		// 回應選擇帳密認證 0x02
		_, _ = serverConn.Write([]byte{0x05, 0x02})
		// 讀取子協商：ver(1) + ulen(1) + user + plen(1) + pass
		authHead := make([]byte, 2)
		if _, err := readFull(serverConn, authHead); err != nil {
			return
		}
		uBuf := make([]byte, authHead[1])
		_, _ = readFull(serverConn, uBuf)
		pLen := make([]byte, 1)
		_, _ = readFull(serverConn, pLen)
		pBuf := make([]byte, pLen[0])
		_, _ = readFull(serverConn, pBuf)
		// 回應認證成功
		_, _ = serverConn.Write([]byte{0x01, 0x00})
		// 讀取 CONNECT 並回應成功
		req := make([]byte, 4+4+2)
		_, _ = readFull(serverConn, req)
		_, _ = serverConn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	}()

	u, _ := url.Parse("socks5://alice:secret@127.0.0.1:1080")
	if err := socks5Handshake(context.Background(), clientConn, u, "127.0.0.1:80", false); err != nil {
		t.Fatalf("帶帳密 socks5 握手失敗: %v", err)
	}
	<-serverDone

	hasUserPass := false
	for _, m := range seenMethods {
		if m == 0x01 {
			t.Fatalf("不應送出 GSSAPI 方法碼 0x01，實際 methods=%v", seenMethods)
		}
		if m == 0x02 {
			hasUserPass = true
		}
	}
	if !hasUserPass {
		t.Fatalf("帶帳密時必須包含帳密方法碼 0x02，實際 methods=%v", seenMethods)
	}

	// 2. 無帳密 URL 卻收到代理回 0x02 時，應回傳明確錯誤而非 nil pointer panic
	c2, s2 := net.Pipe()
	defer c2.Close()
	defer s2.Close()
	go func() {
		buf := make([]byte, 3)
		_, _ = readFull(s2, buf)
		_, _ = s2.Write([]byte{0x05, 0x02})
	}()
	uNoAuth, _ := url.Parse("socks5://127.0.0.1:1080")
	err := socks5Handshake(context.Background(), c2, uNoAuth, "127.0.0.1:80", false)
	if err == nil || !strings.Contains(err.Error(), "未提供憑證") {
		t.Fatalf("無帳密收到 0x02 應回傳明確錯誤，實際: %v", err)
	}
}

func TestSocks5LengthValidation(t *testing.T) {
	// 帳號超過 255 位元組應直接報錯而非截斷
	c1, s1 := net.Pipe()
	defer c1.Close()
	defer s1.Close()
	longUser := strings.Repeat("u", 256)
	uLongUser := &url.URL{Scheme: "socks5", Host: "127.0.0.1:1080", User: url.UserPassword(longUser, "ok")}
	if err := socks5Auth(c1, uLongUser); err == nil || !strings.Contains(err.Error(), "255") {
		t.Fatalf("超長帳號應報錯，實際: %v", err)
	}

	// 密碼超過 255 位元組應直接報錯
	longPass := strings.Repeat("p", 256)
	uLongPass := &url.URL{Scheme: "socks5", Host: "127.0.0.1:1080", User: url.UserPassword("ok", longPass)}
	if err := socks5Auth(c1, uLongPass); err == nil || !strings.Contains(err.Error(), "255") {
		t.Fatalf("超長密碼應報錯，實際: %v", err)
	}

	// socks5h 域名超過 255 位元組應報錯而非截斷
	c2, s2 := net.Pipe()
	defer c2.Close()
	defer s2.Close()
	go func() {
		buf := make([]byte, 3)
		_, _ = readFull(s2, buf)
		_, _ = s2.Write([]byte{0x05, 0x00})
	}()
	u, _ := url.Parse("socks5h://127.0.0.1:1080")
	longHost := strings.Repeat("a", 256) + ":80"
	if err := socks5Handshake(context.Background(), c2, u, longHost, true); err == nil || !strings.Contains(err.Error(), "255") {
		t.Fatalf("超長域名應報錯，實際: %v", err)
	}
}

type deadlineTrackingConn struct {
	net.Conn
	deadlines []time.Time
}

func (c *deadlineTrackingConn) SetDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return c.Conn.SetDeadline(t)
}

func TestSocks4HasDeadline(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	tracked := &deadlineTrackingConn{Conn: c}

	go func() {
		buf := make([]byte, 64)
		_, _ = s.Read(buf)
		_, _ = s.Write([]byte{0x00, 0x5a, 0, 0, 0, 0, 0, 0})
	}()

	u, _ := url.Parse("socks4://127.0.0.1:1080")
	if err := socks4Handshake(tracked, u, "127.0.0.1:80", false); err != nil {
		t.Fatalf("socks4 握手失敗: %v", err)
	}
	if len(tracked.deadlines) < 2 || tracked.deadlines[0].IsZero() || !tracked.deadlines[len(tracked.deadlines)-1].IsZero() {
		t.Fatalf("socks4Handshake 必須先設定非零 deadline 並在結束時清除，實際記錄: %v", tracked.deadlines)
	}

	// 同時驗證 EvictTransport 會淘汰快取並建立新的 Transport
	const raw = "http://127.0.0.1:19876"
	t1, err := TransportFor(raw)
	if err != nil {
		t.Fatal(err)
	}
	EvictTransport(raw)
	t2, err := TransportFor(raw)
	if err != nil {
		t.Fatal(err)
	}
	if t1 == t2 {
		t.Fatal("EvictTransport 後應建立新的 Transport 實例")
	}
	EvictTransport(raw)
}

func TestTransportForceHTTP2Enabled(t *testing.T) {
	direct, err := TransportFor("")
	if err != nil {
		t.Fatal(err)
	}
	if !direct.ForceAttemptHTTP2 {
		t.Fatal("直連 Transport 必須顯式啟用 ForceAttemptHTTP2")
	}
	proxied, err := TransportFor("http://127.0.0.1:18888")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { EvictTransport("http://127.0.0.1:18888") })
	if !proxied.ForceAttemptHTTP2 {
		t.Fatal("代理 Transport 必須顯式啟用 ForceAttemptHTTP2")
	}
}

func TestIPv6ZeroByteEOFFallbackToTCP4(t *testing.T) {
	ResetIPv4Fallback()
	t.Cleanup(ResetIPv4Fallback)

	const targetAddr = "zcode.z.ai:443"
	if PreferIPv4(targetAddr) {
		t.Fatal("初始狀態不應啟用 IPv4 降級")
	}

	c, s := net.Pipe()
	_ = s.Close() // 立即關閉對端模擬 IPv6 首包零位元組 EOF
	probe := &ipv6ProbeConn{Conn: c, addr: targetAddr}
	buf := make([]byte, 16)
	n, err := probe.Read(buf)
	_ = c.Close()
	if n != 0 || err == nil {
		t.Fatalf("預期讀到 0 位元組 EOF，實際 n=%d err=%v", n, err)
	}
	if !PreferIPv4(targetAddr) {
		t.Fatal("IPv6 首包零位元組 EOF 後應標記 PreferIPv4 為 true")
	}

	// CloseIdleFor 驅逐髒連線不應刪除 Transport 快取實例
	const raw = "http://127.0.0.1:19877"
	t1, err := TransportFor(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { EvictTransport(raw) })
	CloseIdleFor(raw)
	t2, err := TransportFor(raw)
	if err != nil {
		t.Fatal(err)
	}
	if t1 != t2 {
		t.Fatal("CloseIdleFor 僅驅逐閒置連線，不應刪除快取中的 Transport 實例")
	}
}
