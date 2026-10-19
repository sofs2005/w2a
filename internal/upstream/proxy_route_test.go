package upstream

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// 账号级出口线路（面板「出口线路」）。
//
// 面板早就能给账号绑线路（auths/*.json 顶层 proxy 写线路名），但上游此前完全不读：
// 出站只有 ProxyFromEnvironment（进程级），于是「每个号一条出口」只是界面上看起来
// 生效，真正的对话流量还是全池共用宿主出口 IP。多号同 IP 正是平台最容易关联的特征。

func TestTransportForRoute(t *testing.T) {
	ConfigureProxies(map[string]string{
		"route-a": "socks5h://user:pass@a.example:1080",
		"route-b": "http://b.example:8080",
	})
	t.Cleanup(func() { ConfigureProxies(nil) })

	// 空名 = 默认（进程级环境代理），不是错误
	def, err := TransportFor("")
	if err != nil || def == nil {
		t.Fatalf("空线路名应返回默认 Transport：%v", err)
	}

	a, err := TransportFor("route-a")
	if err != nil {
		t.Fatalf("route-a: %v", err)
	}
	u, perr := a.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "api.example"}})
	if perr != nil || u == nil {
		t.Fatalf("route-a 没配置代理：%v", perr)
	}
	if u.Scheme != "socks5" {
		t.Errorf("socks5h 应归一成 socks5（Go 的 socks5 拨号本身远端解析），got %q", u.Scheme)
	}
	if u.Host != "a.example:1080" {
		t.Errorf("代理地址不对：%q", u.Host)
	}

	// 同一条线路必须复用同一个 Transport（连接池分开缓存，出口才不会串）
	again, _ := TransportFor("route-a")
	if again != a {
		t.Error("同一线路名两次取到不同 Transport —— 连接池会分裂，出口可能串")
	}
	b, _ := TransportFor("route-b")
	if b == a {
		t.Error("不同线路拿到了同一个 Transport —— 连接池共用会让 A 的空闲连接给 B 用，出口串")
	}
	if u2, _ := b.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "x"}}); u2 == nil || u2.Host != "b.example:8080" {
		t.Errorf("route-b 的代理不对：%v", u2)
	}
}

// TestTransportForDirectRoute 保留名 __direct__：显式直连，忽略环境变量代理。
//
// 没有它就无法表达「这个号必须裸出口」：不写 proxy 等于跟随环境变量，而环境里
// 有 HTTP_PROXY 时那仍然是代理出口。
func TestTransportForDirectRoute(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://env-proxy.example:3128")
	t.Setenv("HTTPS_PROXY", "http://env-proxy.example:3128")
	ConfigureProxies(map[string]string{
		"route-a":       "http://a.example:8080",
		DirectRouteName: "http://should-be-ignored:1", // 保留名不参与线路表
	})
	t.Cleanup(func() { ConfigureProxies(nil) })

	tr, err := TransportFor(DirectRouteName)
	if err != nil {
		t.Fatalf("__direct__ 应可直接取到：%v", err)
	}
	if tr.Proxy != nil {
		t.Fatal("__direct__ 的 Proxy 必须为 nil（显式直连，不看环境变量）")
	}
	// 全进程一份：连接池不分裂
	if again, _ := TransportFor(DirectRouteName); again != tr {
		t.Error("__direct__ 每次取到不同 Transport —— 连接池会分裂")
	}
	// 保留名不能出现在线路表里（否则启动日志会把它当普通线路列出来）
	for _, name := range RouteNames() {
		if name == DirectRouteName {
			t.Error("__direct__ 不应出现在 RouteNames 里")
		}
	}
}

func TestTransportForUnknownRouteFails(t *testing.T) {
	ConfigureProxies(map[string]string{"route-a": "http://a.example:8080"})
	t.Cleanup(func() { ConfigureProxies(nil) })

	_, err := TransportFor("route-z")
	if err == nil {
		t.Fatal("未知线路必须报错 —— 静默直连会让用户以为已经隔离")
	}
	if !strings.Contains(err.Error(), "route-z") {
		t.Errorf("错误里要带线路名，便于对照 config.json：%v", err)
	}

	// 一条线路都没配时，错误要说清「上游没有线路表」而不是「未知线路」
	ConfigureProxies(nil)
	_, err = TransportFor("route-a")
	if err == nil || !strings.Contains(err.Error(), "proxies") {
		t.Errorf("空线路表时应提示去配 proxies 段：%v", err)
	}
}

func TestTransportForRejectsBadAddress(t *testing.T) {
	ConfigureProxies(map[string]string{
		"bad-scheme": "ftp://x.example:21",
		"no-host":    "socks5://",
	})
	t.Cleanup(func() { ConfigureProxies(nil) })
	for _, name := range []string{"bad-scheme", "no-host"} {
		if _, err := TransportFor(name); err == nil {
			t.Errorf("%s 应报错（地址非法）", name)
		}
	}
}

func TestClientForUsesAccountRoute(t *testing.T) {
	ConfigureProxies(map[string]string{"route-a": "http://a.example:8080"})
	t.Cleanup(func() { ConfigureProxies(nil) })

	c := New()
	// 没绑线路 → 复用原 client（行为一字不变）
	plain, err := c.clientFor(&auth.Auth{UID: "u1"}, false)
	if err != nil || plain != c.HTTP {
		t.Fatalf("未绑线路应沿用共享 client：%v", err)
	}
	// 绑了线路 → 换 Transport，且代理指向该线路
	bound, err := c.clientFor(&auth.Auth{UID: "u2", ProxyName: "route-a"}, false)
	if err != nil {
		t.Fatalf("绑线路取 client 失败：%v", err)
	}
	tr, ok := bound.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型不对：%T", bound.Transport)
	}
	u, _ := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "api"}})
	if u == nil || u.Host != "a.example:8080" {
		t.Errorf("绑定的线路没生效：%v", u)
	}
	// 未知线路 → 明确报错（且错误里带账号标签，排障时知道是哪个号）。
	// 必须是 *upstream.Error（ErrRouteUnavailable）：普通 error 会被 handler 当成
	// 传输层抖动 → NoteFailures + 轮转，健康号被配置错误连坐。
	_, err = c.clientFor(&auth.Auth{UID: "u3", Nickname: "坏号", ProxyName: "nope"}, true)
	if err == nil {
		t.Fatal("未知线路必须报错")
	}
	ue, ok := err.(*Error)
	if !ok || ue.Kind != ErrRouteUnavailable {
		t.Fatalf("线路错误必须是 *Error{ErrRouteUnavailable}，got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "坏号") {
		t.Errorf("错误里应带账号标签：%v", err)
	}
}

// 未绑线路的账号，chat 侧仍走域级 client（global 账号走 ChatHTTPGlobal）。
func TestClientForUnboundKeepsRealmClient(t *testing.T) {
	ConfigureProxies(nil)
	c := New()
	gAcc := &auth.Auth{UID: "g1"}
	gAcc.SetRealm("global")
	c.GlobalEnabled = true
	got, err := c.clientFor(gAcc, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != c.ChatHTTPGlobal {
		t.Error("未绑线路的 global 账号应沿用 ChatHTTPGlobal")
	}
	cn, _ := c.clientFor(&auth.Auth{UID: "cn1"}, true)
	if cn != c.ChatHTTP {
		t.Error("未绑线路的 cn 账号应沿用 ChatHTTP")
	}
}
