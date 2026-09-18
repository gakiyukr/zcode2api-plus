// 出站代理拨号器：对应 Python 版 app/proxy.py 的 make_async_client 部分。
// 支持的 scheme 与 httpx 对齐：http/https 走 CONNECT 隧道（由 http.Transport
// 的 Proxy 字段承担），socks4/socks5/socks5h 走本文件手写的拨号器。
// socks5 本地解析域名后以 IP 连接代理；socks5h 将域名交给代理解析。
package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 直连客户端的拨号参数（对齐 Python 默认语义：连接超时 30s）。
const dialTimeout = 30 * time.Second

// DefaultResponseHeaderTimeout 网关与额度查询等短请求的响应头超时。
// async 池需要更宽松的上限，见 TransportForTimeout。
const DefaultResponseHeaderTimeout = 120 * time.Second

// transportCache 按"归一化代理 URL + 响应头超时"缓存 Transport，避免每请求
// 重建连接池；nil 代理 URL（直连）同样缓存，命中热路径零开销。
var (
	transportMu   sync.Mutex
	transportPrec = map[string]*http.Transport{}
)

// TransportFor 返回指定代理 URL 的出站 Transport；raw 为空字符串表示直连。
// 代理 URL 非法时返回错误（调用方应把错误落到账号 last_error 而非 panic）。
func TransportFor(raw string) (*http.Transport, error) {
	return TransportForTimeout(raw, DefaultResponseHeaderTimeout)
}

// TransportForTimeout 同 TransportFor，但可指定响应头超时。
//
// 缓存键包含超时值：不同用途（网关 120s、async 池 180s）各自持有一份
// Transport，避免共用连接池时超时语义互相覆盖。
func TransportForTimeout(raw string, responseHeaderTimeout time.Duration) (*http.Transport, error) {
	normalized, err := NormalizeProxyURL(raw)
	if err != nil {
		return nil, err
	}
	key := ""
	if normalized != nil {
		key = *normalized
	}
	key = fmt.Sprintf("%s\x00%d", key, responseHeaderTimeout)
	transportMu.Lock()
	defer transportMu.Unlock()
	if t, ok := transportPrec[key]; ok {
		return t, nil
	}
	t := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}
	if normalized != nil {
		u, err := url.Parse(*normalized)
		if err != nil {
			return nil, fmt.Errorf("代理 URL 无效: %v", err)
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			t.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			t.DialContext = socksDialer(u, false)
		case "socks4":
			t.DialContext = socksDialer(u, true)
		default:
			return nil, fmt.Errorf("不支持的代理协议: %s", u.Scheme)
		}
	}
	transportPrec[key] = t
	return t, nil
}

// ClientFor 返回带超时的客户端（额度查询等短请求用途）。
// 流式网关请直接用 TransportFor 构造的 Transport（Go 的 http.Client 在
// 使用 Transport 时不存在整体超时，SSE 长连接天然安全）。
func ClientFor(raw string, timeout time.Duration) (*http.Client, error) {
	t, err := TransportFor(raw)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: t, Timeout: timeout}, nil
}

// socksDialer 构造 SOCKS 出站拨号函数。socks4=true 走 SOCKS4 协议
// （域名本地解析后以 IPv4 连接）；socks5 本地解析，socks5h 远端解析。
func socksDialer(u *url.URL, v4 bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	forward := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	remoteResolve := strings.EqualFold(u.Scheme, "socks5h")
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		proxyAddr := hostPort(u)
		if proxyAddr == "" {
			return nil, errors.New("代理缺少主机")
		}
		conn, err := forward.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("连接代理失败: %v", err)
		}
		defer func() {
			if err != nil {
				conn.Close()
			}
		}()
		if v4 {
			err = socks4Handshake(conn, u, addr, remoteResolve)
		} else {
			err = socks5Handshake(ctx, conn, u, addr, remoteResolve)
		}
		if err != nil {
			return nil, err
		}
		return conn, nil
	}
}

// hostPort 归一化代理地址（缺省端口：http(s)→80，socks→1080）。
func hostPort(u *url.URL) string {
	host := u.Hostname()
	if host == "" {
		return ""
	}
	if port := u.Port(); port != "" {
		return net.JoinHostPort(host, port)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return net.JoinHostPort(host, "80")
	default:
		return net.JoinHostPort(host, "1080")
	}
}

// socks5Handshake 执行 RFC 1928 握手（可选用户名/密码鉴权）与 CONNECT。
func socks5Handshake(ctx context.Context, conn net.Conn, u *url.URL, addr string, remoteResolve bool) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	// 方法协商：无需鉴权；URL 含用户名密码时追加 user/pass 方法
	methods := []byte{0x01, 0x00}
	if u.User != nil && u.User.Username() != "" {
		methods = []byte{0x02, 0x01, 0x00}
	}
	if _, err := conn.Write(append([]byte{0x05}, methods...)); err != nil {
		return fmt.Errorf("socks5 方法协商写入失败: %v", err)
	}
	resp := make([]byte, 2)
	if _, err := readFull(conn, resp); err != nil {
		return fmt.Errorf("socks5 方法协商读取失败: %v", err)
	}
	if resp[0] != 0x05 {
		return fmt.Errorf("socks5 方法协商版本异常: %d", resp[0])
	}
	switch resp[1] {
	case 0x00:
	case 0x02:
		if err := socks5Auth(conn, u); err != nil {
			return err
		}
	default:
		return fmt.Errorf("socks5 代理拒绝了可用鉴权方法: %d", resp[1])
	}

	// CONNECT 请求：ATYP 0x01 IPv4 / 0x03 域名（socks5h 或本地解析失败时）
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("目标地址无效: %v", err)
	}
	port, err := parsePort(portText)
	if err != nil {
		return err
	}
	var atyp byte
	var hostBytes []byte
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			atyp, hostBytes = 0x01, v4
		} else {
			atyp, hostBytes = 0x04, ip.To16()
		}
	} else if !remoteResolve {
		resolved, rerr := net.DefaultResolver.LookupIPAddr(ctx, host)
		if rerr != nil || len(resolved) == 0 {
			return fmt.Errorf("本地解析失败: %v", rerr)
		}
		// 优先 IPv4；解析结果可能只有 IPv6（或 IPv6 排在首位），
		// 此时必须用 ATYP=0x04，否则会送出 addr 长度为 0 的畸形 CONNECT。
		atyp, hostBytes = 0, nil
		for _, r := range resolved {
			if v4 := r.IP.To4(); v4 != nil {
				atyp, hostBytes = 0x01, v4
				break
			}
		}
		if atyp == 0 {
			if v6 := resolved[0].IP.To16(); v6 != nil {
				atyp, hostBytes = 0x04, v6
			} else {
				return fmt.Errorf("本地解析结果不可用: %s", host)
			}
		}
	} else {
		// ATYP=0x03 后需带 1 字节域名长度（RFC 1928）
		atyp = 0x03
		hostBytes = append([]byte{byte(len(host))}, host...)
	}
	req := append([]byte{0x05, 0x01, 0x00, atyp}, hostBytes...)
	req = binary.BigEndian.AppendUint16(req, port)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5 CONNECT 写入失败: %v", err)
	}
	// 回复：VER REP RSV ATYP ADDR(变长) PORT
	head := make([]byte, 5)
	if _, err := readFull(conn, head); err != nil {
		return fmt.Errorf("socks5 CONNECT 回复读取失败: %v", err)
	}
	if head[0] != 0x05 {
		return fmt.Errorf("socks5 回复版本非法: %d", head[0])
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5 代理连接失败，回复码 %d", head[1])
	}
	// head 已读 5 字节，其中对 0x01/0x04 而言第 5 字节是地址首字节；
	// 但对 0x03 而言它是域名长度，域名本身尚未读取——故域名分支要读
	// 长度 + 域名 + 端口，写成 (len-1)+2 会少读 1 字节，该字节留在
	// socket 里污染后续应用层数据（表现为 TLS 握手失败或请求行被吃掉）。
	var extra int
	switch head[3] {
	case 0x01:
		extra = 4 - 1 + 2
	case 0x04:
		extra = 16 - 1 + 2
	case 0x03:
		extra = int(head[4]) + 2
	default:
		return fmt.Errorf("socks5 回复地址类型非法: %d", head[3])
	}
	trailer := make([]byte, extra)
	if _, err := readFull(conn, trailer); err != nil {
		return fmt.Errorf("socks5 回复尾部读取失败: %v", err)
	}
	return nil
}

// socks5Auth 执行 RFC 1929 用户名/密码子协商。
func socks5Auth(conn net.Conn, u *url.URL) error {
	pass, _ := u.User.Password()
	auth := append([]byte{0x01, byte(len(u.User.Username()))}, []byte(u.User.Username())...)
	auth = append(auth, byte(len(pass)))
	auth = append(auth, []byte(pass)...)
	if _, err := conn.Write(auth); err != nil {
		return fmt.Errorf("socks5 鉴权写入失败: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := readFull(conn, reply); err != nil {
		return fmt.Errorf("socks5 鉴权回复读取失败: %v", err)
	}
	if reply[1] != 0x00 {
		return errors.New("socks5 用户名或密码被拒绝")
	}
	return nil
}

// socks4Handshake 执行 SOCKS4/4a 握手（userid 取自 URL 用户名）。
// 域名总是以 4a 的形式发给代理解析（httpx 的 socks4 本地解析语义在
// IPv4-only 场景等价，4a 兼容面更广且不会暴露解析失败差异）。
func socks4Handshake(conn net.Conn, u *url.URL, addr string, _ bool) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("目标地址无效: %v", err)
	}
	port, err := parsePort(portText)
	if err != nil {
		return err
	}
	var ip net.IP
	if parsed := net.ParseIP(host); parsed != nil && parsed.To4() != nil {
		ip = parsed.To4()
	}
	var req []byte
	userID := ""
	if u.User != nil {
		userID = u.User.Username()
	}
	if ip != nil {
		req = append([]byte{0x04, 0x01}, byte(port>>8), byte(port&0xff))
		req = append(req, ip...)
		req = append(req, []byte(userID)...)
		req = append(req, 0x00)
	} else {
		// SOCKS4a：目标为域名，端口后接 userid + NUL + 域名 + NUL
		req = append([]byte{0x04, 0x01}, byte(port>>8), byte(port&0xff), 0x00, 0x00, 0x00, 0x01)
		req = append(req, []byte(userID)...)
		req = append(req, 0x00)
		req = append(req, []byte(host)...)
		req = append(req, 0x00)
	}
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks4 CONNECT 写入失败: %v", err)
	}
	resp := make([]byte, 8)
	if _, err := readFull(conn, resp); err != nil {
		return fmt.Errorf("socks4 回复读取失败: %v", err)
	}
	if resp[1] != 0x5a {
		return fmt.Errorf("socks4 代理连接失败，回复码 %d", resp[1])
	}
	return nil
}

// readFull 保证读满 n 字节（conn.Read 可能短读）。
func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// parsePort 解析端口文本（1-65535）。
func parsePort(text string) (uint16, error) {
	var port int
	for _, ch := range text {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("端口无效: %q", text)
		}
		port = port*10 + int(ch-'0')
		if port > 65535 {
			return 0, fmt.Errorf("端口超出范围: %q", text)
		}
	}
	if port == 0 {
		return 0, fmt.Errorf("端口无效: %q", text)
	}
	return uint16(port), nil
}
