// proxy_test.go 出站代理的解析、接线与端到端验证。
//
// 端到端用例自带最小的 HTTP CONNECT 代理与 SOCKS5 服务端（各 ~40 行），不依赖
// 外部代理软件：x/net 没有测试用 SOCKS5 server，手写握手是社区通行做法，也顺带
// 把「我们确实按协议说话」这件事钉住（而非只验「调了哪个 API」）。
package upstream

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- 解析 ----

func TestParseProxyURL(t *testing.T) {
	valid := []struct {
		name string
		in   string
		want string // 归一后的 scheme://host
	}{
		{"http 无认证", "http://proxy.corp:3128", "http://proxy.corp:3128"},
		{"http 带认证", "http://u:p@proxy.corp:3128", "http://proxy.corp:3128"},
		{"https 代理", "https://proxy.corp:3128", "https://proxy.corp:3128"},
		{"socks5", "socks5://10.0.0.1:1080", "socks5://10.0.0.1:1080"},
		{"socks5h", "socks5h://10.0.0.1:1080", "socks5h://10.0.0.1:1080"},
		{"socks5 带认证", "socks5://u:p@10.0.0.1:1080", "socks5://10.0.0.1:1080"},
		{"大写 scheme 归一", "HTTP://proxy.corp:3128", "http://proxy.corp:3128"},
		{"仅用户名", "http://u@proxy.corp:3128", "http://proxy.corp:3128"},
		{"首尾空白容忍", "  http://proxy.corp:3128  ", "http://proxy.corp:3128"},
	}
	for _, c := range valid {
		t.Run(c.name, func(t *testing.T) {
			u, err := ParseProxyURL(c.in)
			if err != nil {
				t.Fatalf("ParseProxyURL(%q) error: %v", c.in, err)
			}
			if u == nil {
				t.Fatal("u is nil, want parsed URL")
			}
			if got := ProxyDesc(u); got != c.want {
				t.Errorf("ProxyDesc = %q, want %q", got, c.want)
			}
		})
	}

	invalid := []struct {
		name string
		in   string
	}{
		{"不支持的 scheme", "ftp://proxy.corp:3128"},
		{"无 scheme", "proxy.corp:3128"},
		{"缺 host", "http://"},
		{"空用户名配非空密码", "http://:pass@proxy.corp:3128"},
		{"带路径", "http://proxy.corp:3128/pac"},
		{"带查询", "http://proxy.corp:3128?x=1"},
		{"带片段", "http://proxy.corp:3128#frag"},
	}
	for _, c := range invalid {
		t.Run(c.name, func(t *testing.T) {
			u, err := ParseProxyURL(c.in)
			if err == nil {
				t.Fatalf("ParseProxyURL(%q) = %v, want error", c.in, u)
			}
			if u != nil {
				t.Errorf("u = %v, want nil on error", u)
			}
		})
	}
}

// TestParseProxyURLEmptyMeansUnset 空串（含纯空白）是「该域未配置」的三态之一，
// 必须与「非法」区分：前者回落环境变量，后者 WARN 后回落直连。
func TestParseProxyURLEmptyMeansUnset(t *testing.T) {
	for _, in := range []string{"", "   ", "\t"} {
		u, err := ParseProxyURL(in)
		if err != nil {
			t.Errorf("ParseProxyURL(%q) error = %v, want nil (空 = 未配置)", in, err)
		}
		if u != nil {
			t.Errorf("ParseProxyURL(%q) = %v, want nil", in, u)
		}
	}
}

// TestProxyDescOmitsCredentials 脱敏断言：/status 透出与日志只出现 scheme+host，
// 密码绝不泄漏。
func TestProxyDescOmitsCredentials(t *testing.T) {
	u, err := ParseProxyURL("socks5://alice:s3cret@10.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	desc := ProxyDesc(u)
	if strings.Contains(desc, "s3cret") || strings.Contains(desc, "alice") {
		t.Errorf("ProxyDesc(%q) leaked credentials: %q", u.String(), desc)
	}
	if desc != "socks5://10.0.0.1:1080" {
		t.Errorf("ProxyDesc = %q, want socks5://10.0.0.1:1080", desc)
	}
}

// ---- 接线 ----

// TestNewTransportGlobalUnsetMatchesCN 未配置代理时 global Transport 与 CN 侧同构：
// Proxy 仍是环境变量解析器（零回归的核心断言）。
func TestNewTransportGlobalUnsetMatchesCN(t *testing.T) {
	for _, in := range []string{"", "  "} {
		tr := newTransportGlobal(in)
		if tr.Proxy == nil {
			t.Errorf("newTransportGlobal(%q).Proxy is nil, want ProxyFromEnvironment", in)
		}
		// 加固参数一个不丢。
		if tr.TLSNextProto == nil {
			t.Errorf("newTransportGlobal(%q): TLSNextProto must be non-nil (禁 h2)", in)
		}
		if tr.TLSHandshakeTimeout != tlsHandshakeTimeout {
			t.Errorf("TLSHandshakeTimeout=%v want %v", tr.TLSHandshakeTimeout, tlsHandshakeTimeout)
		}
		if tr.ResponseHeaderTimeout != responseHeaderTimeout {
			t.Errorf("ResponseHeaderTimeout=%v want %v", tr.ResponseHeaderTimeout, responseHeaderTimeout)
		}
		if tr.MaxIdleConnsPerHost != maxIdleConnsPerHost {
			t.Errorf("MaxIdleConnsPerHost=%d want %d", tr.MaxIdleConnsPerHost, maxIdleConnsPerHost)
		}
	}
}

// TestNewTransportGlobalInvalidFallsBack 非法 URL 静默回落（normalize 已 WARN 过），
// 不 panic、不返回 nil。
func TestNewTransportGlobalInvalidFallsBack(t *testing.T) {
	tr := newTransportGlobal("ftp://nope:1")
	if tr == nil {
		t.Fatal("nil transport on invalid proxy")
	}
	if tr.Proxy == nil {
		t.Error("invalid proxy should fall back to ProxyFromEnvironment")
	}
}

// TestNewTransportGlobalHTTPProxySetsProxyFunc HTTP/HTTPS 代理走标准库 Proxy 回调。
func TestNewTransportGlobalHTTPProxySetsProxyFunc(t *testing.T) {
	tr := newTransportGlobal("http://proxy.corp:3128")
	if tr.Proxy == nil {
		t.Fatal("Proxy func is nil for http proxy")
	}
	req, _ := http.NewRequest(http.MethodGet, "https://www.workbuddy.ai/v3/config", nil)
	u, err := tr.Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil || u.Host != "proxy.corp:3128" {
		t.Errorf("Proxy(req) = %v, want host proxy.corp:3128", u)
	}
}

// TestNewTransportGlobalSOCKS5SwapsDialer SOCKS5 必须换 DialContext 并把 Proxy 置 nil
// ——否则标准库会对 socks5:// 报 unsupported protocol scheme。
func TestNewTransportGlobalSOCKS5SwapsDialer(t *testing.T) {
	// 代理地址用本机一个必然关闭的端口：失败快（connection refused），
	// 而错误仍会经 socks 层包装（x/net 把 dial 错误裹进 net.OpError）。
	tr := newTransportGlobal("socks5://127.0.0.1:1")
	if tr.Proxy != nil {
		t.Error("Proxy must be nil for socks5 (stdlib cannot handle socks5 URLs)")
	}
	if tr.DialContext == nil {
		t.Fatal("DialContext is nil for socks5")
	}
	// 加固参数仍在（applyProxy 是在 newTransport 的产物上改字段）。
	if tr.TLSHandshakeTimeout != tlsHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout=%v want %v (加固参数被丢)", tr.TLSHandshakeTimeout, tlsHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout=%v want %v (加固参数被丢)", tr.ResponseHeaderTimeout, responseHeaderTimeout)
	}
	// DialContext 确实被换成了 socks dialer（函数值不可比较，用行为验证）：
	// 拨一个必然连不上的目标，错误信息应带 "socks"（来自 x/net 的 socks 层），
	// 而直连 dialer 只会给 "connection refused" / "i/o timeout"。
	_, dialErr := tr.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if dialErr == nil {
		t.Fatal("dial to closed port should fail")
	}
	if !strings.Contains(dialErr.Error(), "socks") {
		t.Errorf("dial error = %v, want a socks-layer error (DialContext was not swapped)", dialErr)
	}
}

// ---- 端到端：经真实代理到达目标 ----

// startConnectProxy 起一个最小 HTTP CONNECT 代理（测试用）。返回代理地址与命中计数。
func startConnectProxy(t *testing.T, requireAuth string) (addr string, hits *int32) {
	t.Helper()
	var n int32
	hits = &n
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				if req.Method != http.MethodConnect {
					fmt.Fprintf(c, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
					return
				}
				if requireAuth != "" && req.Header.Get("Proxy-Authorization") != requireAuth {
					fmt.Fprintf(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
					return
				}
				upstreamConn, err := net.Dial("tcp", req.Host)
				if err != nil {
					fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer upstreamConn.Close()
				// 计数放在隧道建立成功之后：只数「真的转发了流量」的请求。
				atomicAdd(hits)
				fmt.Fprintf(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
				done := make(chan struct{}, 2)
				go func() { io.Copy(upstreamConn, br); done <- struct{}{} }()
				go func() { io.Copy(c, upstreamConn); done <- struct{}{} }()
				<-done
			}(conn)
		}
	}()
	return ln.Addr().String(), hits
}

// startSOCKS5Proxy 起一个最小 SOCKS5 服务端（no-auth 或 user/pass，仅 CONNECT）。
// 手写握手：x/net 不提供测试用 server，而 30 行就能把协议钉住。
func startSOCKS5Proxy(t *testing.T, wantUser, wantPass string) (addr string, hits *int32) {
	t.Helper()
	var n int32
	hits = &n
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// 1) greeting: VER(5) NMETHODS METHODS...
				hdr := make([]byte, 2)
				if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
					return
				}
				methods := make([]byte, hdr[1])
				if _, err := io.ReadFull(c, methods); err != nil {
					return
				}
				useAuth := wantUser != ""
				// 2) method selection: 选 user/pass(2) 或 no-auth(0)。
				if useAuth {
					if _, err := c.Write([]byte{5, 2}); err != nil {
						return
					}
					// RFC1929: VER(1) ULEN USER PLEN PASS
					ah := make([]byte, 2)
					if _, err := io.ReadFull(c, ah); err != nil || ah[0] != 1 {
						return
					}
					user := make([]byte, ah[1])
					if _, err := io.ReadFull(c, user); err != nil {
						return
					}
					pl := make([]byte, 1)
					if _, err := io.ReadFull(c, pl); err != nil {
						return
					}
					pass := make([]byte, pl[0])
					if _, err := io.ReadFull(c, pass); err != nil {
						return
					}
					if string(user) != wantUser || string(pass) != wantPass {
						c.Write([]byte{1, 1}) // 认证失败
						return
					}
					if _, err := c.Write([]byte{1, 0}); err != nil { // 认证成功
						return
					}
				} else {
					if _, err := c.Write([]byte{5, 0}); err != nil {
						return
					}
				}
				// 3) request: VER CMD RSV ATYP DST.ADDR DST.PORT
				reqHdr := make([]byte, 4)
				if _, err := io.ReadFull(c, reqHdr); err != nil || reqHdr[0] != 5 || reqHdr[1] != 1 {
					return
				}
				var host string
				switch reqHdr[3] {
				case 1: // IPv4
					b := make([]byte, 4)
					if _, err := io.ReadFull(c, b); err != nil {
						return
					}
					host = net.IP(b).String()
				case 3: // FQDN
					l := make([]byte, 1)
					if _, err := io.ReadFull(c, l); err != nil {
						return
					}
					b := make([]byte, l[0])
					if _, err := io.ReadFull(c, b); err != nil {
						return
					}
					host = string(b)
				default:
					return
				}
				pb := make([]byte, 2)
				if _, err := io.ReadFull(c, pb); err != nil {
					return
				}
				target := net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(pb)))
				up, err := net.Dial("tcp", target)
				if err != nil {
					c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}) // connection refused
					return
				}
				defer up.Close()
				atomicAdd(hits)
				// reply: VER REP RSV ATYP BND.ADDR BND.PORT（全零即可）
				if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
					return
				}
				done := make(chan struct{}, 2)
				go func() { io.Copy(up, c); done <- struct{}{} }()
				go func() { io.Copy(c, up); done <- struct{}{} }()
				<-done
			}(conn)
		}
	}()
	return ln.Addr().String(), hits
}

// atomicAdd / loadInt32 计数器小工具（原子，代理 goroutine 并发自增）。
func atomicAdd(p *int32)       { atomic.AddInt32(p, 1) }
func loadInt32(p *int32) int32 { return atomic.LoadInt32(p) }

// TestHTTPProxyEndToEnd 经真实 CONNECT 代理取回目标响应（含 Basic 认证头）。
//
// 目标必须是 **HTTPS**：标准库只对 https:// 目标用 CONNECT 建隧道，对 http:// 目标
// 是把绝对 URL 直接发给代理（不经 CONNECT）。生产上游（copilot.tencent.com /
// www.workbuddy.ai）全是 https，故 CONNECT 才是要钉住的路径。
func TestHTTPProxyEndToEnd(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello-from-target")
	}))
	defer target.Close()

	basic := "Basic " + basicAuth("alice", "s3cret")
	proxyAddr, hits := startConnectProxy(t, basic)

	tr := newTransportGlobal("http://alice:s3cret@" + proxyAddr)
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // 测试自签证书
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("GET via proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-from-target" {
		t.Errorf("body = %q, want hello-from-target", body)
	}
	if got := loadInt32(hits); got == 0 {
		t.Error("proxy was never hit: request did not go through the CONNECT tunnel")
	}
}

// TestHTTPProxyAuthRequired 认证头确实被标准库带上：错密码必须被代理拒绝。
func TestHTTPProxyAuthRequired(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer target.Close()

	proxyAddr, hits := startConnectProxy(t, "Basic "+basicAuth("alice", "s3cret"))
	// 配错密码 → 407 → 请求失败，且代理侧从未建立隧道（hits 保持 0）。
	tr := newTransportGlobal("http://alice:wrong@" + proxyAddr)
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	if _, err := client.Get(target.URL); err == nil {
		t.Fatal("request with wrong proxy password should fail")
	}
	if got := loadInt32(hits); got != 0 {
		t.Errorf("tunnel hits = %d, want 0 (auth should have been rejected)", got)
	}
}

// TestSOCKS5ProxyEndToEnd 经真实 SOCKS5 代理取回目标响应。
func TestSOCKS5ProxyEndToEnd(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello-via-socks")
	}))
	defer target.Close()

	proxyAddr, hits := startSOCKS5Proxy(t, "", "")
	tr := newTransportGlobal("socks5://" + proxyAddr)
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("GET via socks5: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello-via-socks" {
		t.Errorf("body = %q, want hello-via-socks", body)
	}
	if got := loadInt32(hits); got == 0 {
		t.Error("socks5 proxy was never hit")
	}
}

// TestSOCKS5ProxyWithAuth SOCKS5 用户名密码认证（RFC1929）走通。
func TestSOCKS5ProxyWithAuth(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "authed")
	}))
	defer target.Close()

	proxyAddr, hits := startSOCKS5Proxy(t, "bob", "hunter2")
	tr := newTransportGlobal("socks5://bob:hunter2@" + proxyAddr)
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("GET via authed socks5: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "authed" {
		t.Errorf("body = %q, want authed", body)
	}
	if got := loadInt32(hits); got == 0 {
		t.Error("socks5 proxy was never hit")
	}
}

// TestSOCKS5ProxyWrongPasswordRejected 认证失败必须拒绝（否则「配了密码」是假象）。
func TestSOCKS5ProxyWrongPasswordRejected(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer target.Close()

	proxyAddr, hits := startSOCKS5Proxy(t, "bob", "hunter2")
	tr := newTransportGlobal("socks5://bob:wrong@" + proxyAddr)
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	if _, err := client.Get(target.URL); err == nil {
		t.Fatal("request with wrong socks5 password should fail")
	}
	if got := loadInt32(hits); got != 0 {
		t.Errorf("tunnel hits = %d, want 0", got)
	}
}

// TestProxyTransportForProxyURLForLogin login CLI 复用同一份接线（ProxyTransportFor）。
func TestProxyTransportForProxyURLForLogin(t *testing.T) {
	// 空 = (nil, nil)：login 侧据此回落直连。
	tr, err := ProxyTransportFor("")
	if err != nil || tr != nil {
		t.Errorf("ProxyTransportFor(\"\") = (%v, %v), want (nil, nil)", tr, err)
	}
	// 非法 = 报错（login 是交互式工具，要立刻让用户看到）。
	if _, err := ProxyTransportFor("ftp://x:1"); err == nil {
		t.Error("ProxyTransportFor(invalid) should error")
	}
	// 合法 = 带代理的 Transport。
	tr, err = ProxyTransportFor("http://proxy.corp:3128")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://x.example", nil)
	if u, _ := tr.Proxy(req); u == nil || u.Host != "proxy.corp:3128" {
		t.Errorf("Proxy(req) = %v, want proxy.corp:3128", u)
	}
}

// basicAuth 构造 Basic 凭据。
func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}
