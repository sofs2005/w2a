package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api-gui/internal/config"
)

// newTestStore 构造会话表用于测试。
func newTestStore() *SessionStore { return NewSessionStore(time.Hour) }

func testConfig() *config.Config {
	c := config.Default()
	c.UI.Username = "admin"
	c.UI.Password = "secret123"
	c.UI.TTL = time.Hour
	return c
}

// TestCreateValidatesCredentials 正确/错误口令。
func TestCreateValidatesCredentials(t *testing.T) {
	s := newTestStore()
	cfg := testConfig()

	if _, ok, _ := s.Create("admin", "wrong", cfg, "1.1.1.1"); ok {
		t.Error("错误口令不应创建会话")
	}
	if _, ok, _ := s.Create("nobody", "secret123", cfg, "1.1.1.1"); ok {
		t.Error("错误用户名不应创建会话")
	}
	token, ok, _ := s.Create("admin", "secret123", cfg, "1.1.1.1")
	if !ok || token == "" {
		t.Fatal("正确口令应创建会话")
	}
	if user, valid := s.Validate(token); !valid || user != "admin" {
		t.Errorf("会话校验失败: user=%q valid=%v", user, valid)
	}
}

// TestBruteForceLockout 连续失败达上限后锁定同一来源。
func TestBruteForceLockout(t *testing.T) {
	s := newTestStore()
	cfg := testConfig()
	src := "9.9.9.9"

	for i := 0; i < maxLoginFailures; i++ {
		s.Create("admin", "bad", cfg, src)
	}
	// 第 N+1 次即使口令正确也应被锁定拒绝。
	_, ok, msg := s.Create("admin", "secret123", cfg, src)
	if ok {
		t.Fatal("达到失败上限后应锁定该来源")
	}
	if !strings.Contains(msg, "失败次数过多") {
		t.Errorf("应提示锁定原因, 得到 %q", msg)
	}
	// 其他来源不受影响（避免一个 IP 拖垮全站登录）。
	if _, ok, _ := s.Create("admin", "secret123", cfg, "8.8.8.8"); !ok {
		t.Error("其他来源不应被连带锁定")
	}
}

// TestSuccessResetsFailures 登录成功应清空失败计数。
func TestSuccessResetsFailures(t *testing.T) {
	s := newTestStore()
	cfg := testConfig()
	src := "7.7.7.7"

	for i := 0; i < maxLoginFailures-1; i++ {
		s.Create("admin", "bad", cfg, src)
	}
	if _, ok, _ := s.Create("admin", "secret123", cfg, src); !ok {
		t.Fatal("未达上限时应允许登录")
	}
	// 计数已清空：再次连续失败 maxLoginFailures-1 次仍不应锁定。
	for i := 0; i < maxLoginFailures-1; i++ {
		s.Create("admin", "bad", cfg, src)
	}
	if _, ok, _ := s.Create("admin", "secret123", cfg, src); !ok {
		t.Error("成功登录后失败计数应已清零")
	}
}

// TestExpiredSession 过期会话应失效。
func TestExpiredSession(t *testing.T) {
	s := NewSessionStore(time.Millisecond)
	cfg := testConfig()
	token, ok, _ := s.Create("admin", "secret123", cfg, "1.1.1.1")
	if !ok {
		t.Fatal("创建会话失败")
	}
	time.Sleep(5 * time.Millisecond)
	if _, valid := s.Validate(token); valid {
		t.Error("过期会话不应通过校验")
	}
}

// TestRevoke 注销后 token 失效。
func TestRevoke(t *testing.T) {
	s := newTestStore()
	cfg := testConfig()
	token, _, _ := s.Create("admin", "secret123", cfg, "1.1.1.1")
	s.Revoke(token)
	if _, valid := s.Validate(token); valid {
		t.Error("注销后 token 应失效")
	}
}

// TestValidateEmptyAndUnknown 空/伪造 token 一律拒绝。
func TestValidateEmptyAndUnknown(t *testing.T) {
	s := newTestStore()
	if _, ok := s.Validate(""); ok {
		t.Error("空 token 不应通过")
	}
	if _, ok := s.Validate("deadbeefdeadbeef"); ok {
		t.Error("伪造 token 不应通过")
	}
}

// TestSameOrigin 同源校验。
//
// 覆盖两类场景：
//  1. 直连（Origin 与 r.Host 一致）——原有行为不能回退；
//  2. 反向代理（issue #7）——nginx 丢端口、透传内网地址、后端 http 前端 https
//     等情形都必须判定为同源，否则用户保存配置/添加账号会被 403 拦住。
func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		hosts  []string
		want   bool
	}{
		// ── 直连（原有行为）──
		{"直连-http", "http://127.0.0.1:8787", []string{"127.0.0.1:8787"}, true},
		{"直连-域名", "https://gui.example.com", []string{"gui.example.com"}, true},
		{"直连-尾斜杠", "http://127.0.0.1:8787/", []string{"127.0.0.1:8787"}, true},

		// ── 反向代理（issue #7 回归）──
		{"反代-代理丢端口", "https://panel.example.com", []string{"panel.example.com:8787"}, true},
		{"反代-协议不同", "https://panel.example.com", []string{"http://panel.example.com"}, true},
		{"反代-Host为内网地址+XFH", "https://panel.example.com",
			[]string{"127.0.0.1:8787", "panel.example.com"}, true},
		{"反代-XFH带端口", "https://panel.example.com",
			[]string{"127.0.0.1:8787", "panel.example.com:8787"}, true},
		{"反代-XFH链取任一(已拆分的候选)", "https://panel.example.com",
			[]string{"127.0.0.1", "a.example.com", "panel.example.com"}, true},
		{"反代-Forwarded头", "https://panel.example.com",
			[]string{"127.0.0.1:8787", "panel.example.com:8787"}, true},
		{"反代-端口由XFP补", "http://panel.example.com",
			[]string{"panel.example.com:8080"}, true},

		// ── 应拒绝 ──
		{"跨站-不同域名", "https://evil.example.com", []string{"127.0.0.1:8787"}, false},
		{"跨站-域名后缀陷阱", "https://evil-panel.example.com", []string{"panel.example.com"}, false},
		{"跨站-同IP不同端口仍是同主机名→放行", "http://127.0.0.1:9999",
			[]string{"127.0.0.1:8787"}, true},
		{"空 Origin 视为无效", "", []string{"panel.example.com"}, false},
		{"null Origin 拒绝", "null", []string{"panel.example.com"}, false},
		{"候选全空", "https://panel.example.com", []string{"", "  "}, false},
		{"只有协议无主机", "https://", []string{"panel.example.com"}, false},
	}
	for _, c := range cases {
		if got := sameOrigin(c.origin, c.hosts, nil); got != c.want {
			t.Errorf("%s: sameOrigin(%q, %v) = %v, want %v", c.name, c.origin, c.hosts, got, c.want)
		}
	}
}

// TestHostnameOf 主机名归一化。
func TestHostnameOf(t *testing.T) {
	cases := map[string]string{
		"https://panel.example.com":               "panel.example.com",
		"http://panel.example.com:8787":           "panel.example.com",
		"panel.example.com:8787":                  "panel.example.com",
		"panel.example.com":                       "panel.example.com",
		"PANEL.Example.COM":                       "panel.example.com",
		"panel.example.com.":                      "panel.example.com",
		"https://user:pw@panel.example.com:443/x": "panel.example.com",
		"[::1]:8787":                              "[::1]",
		"[::1]":                                   "[::1]",
		"":                                        "",
		"   ":                                     "",
		"https://":                                "",
		"null":                                    "null",
	}
	for in, want := range cases {
		if got := hostnameOf(in); got != want {
			t.Errorf("hostnameOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRequestHostsCollectsForwarded 验证候选主机收集（含各转发头形态）。
func TestRequestHostsCollectsForwarded(t *testing.T) {
	r := httptest.NewRequest("POST", "http://127.0.0.1:8787/api/config", nil)
	r.Host = "127.0.0.1:8787"
	r.Header.Set("X-Forwarded-Host", "panel.example.com:8787")
	r.Header.Set("Forwarded", `for=1.2.3.4;host=other.example.com;proto=https`)
	r.Header.Set("X-Forwarded-Port", "443")

	hosts := requestHosts(r)
	joined := strings.Join(hosts, "|")
	for _, want := range []string{"127.0.0.1:8787", "panel.example.com:8787", "other.example.com"} {
		found := false
		for _, h := range hosts {
			if h == want {
				found = true
			}
		}
		if !found {
			t.Errorf("候选缺少 %q（得到 %s）", want, joined)
		}
	}
	// 无端口的候选应被补上 X-Forwarded-Port。
	if !strings.Contains(joined, "other.example.com:443") {
		t.Errorf("应据 X-Forwarded-Port 补端口，得到 %s", joined)
	}
}

// TestSameOriginAllowlist 显式白名单（allowed_origins）生效，
// 用于反代连 Host 都不透传、后端无从推导的兜底场景。
func TestSameOriginAllowlist(t *testing.T) {
	// 无反代头、Host 为后端地址 → 默认拒绝。
	if sameOrigin("https://panel.example.com", []string{"127.0.0.1:8787"}, nil) {
		t.Error("无从推导时应拒绝")
	}
	// 配了白名单 → 放行。
	if !sameOrigin("https://panel.example.com", []string{"127.0.0.1:8787"},
		[]string{"panel.example.com"}) {
		t.Error("白名单命中应放行")
	}
	// 白名单带端口/协议也应归一化匹配。
	if !sameOrigin("https://panel.example.com", []string{"127.0.0.1:8787"},
		[]string{"https://panel.example.com:8443"}) {
		t.Error("白名单应忽略协议与端口")
	}
	// 白名单不匹配的域名仍拒绝。
	if sameOrigin("https://evil.example.com", []string{"127.0.0.1:8787"},
		[]string{"panel.example.com"}) {
		t.Error("非白名单域名不应放行")
	}
}

// TestSameOriginRejectsForgedOrigin 伪造 Origin 必须被拒
// （浏览器不允许脚本设置 Host/X-Forwarded-*，故这些头不可被跨站攻击者利用）。
func TestSameOriginRejectsForgedOrigin(t *testing.T) {
	// 攻击者站点伪造请求，Host 头会被浏览器设成目标站点（他不控），
	// 但他能控制的 Origin 是他的站点。
	if sameOrigin("https://attacker.example", []string{"panel.example.com"}, nil) {
		t.Error("攻击者 Origin 不应被放行")
	}
	// 即便他在 Origin 里塞上路径试图绕过。
	if sameOrigin("https://attacker.example/panel.example.com", []string{"panel.example.com"}, nil) {
		t.Error("带路径的伪造 Origin 不应被放行")
	}
	// 试图用子域名混淆。
	if sameOrigin("https://panel.example.com.attacker.example", []string{"panel.example.com"}, nil) {
		t.Error("后缀混淆应被拒绝")
	}
}

// TestClientSource 来源解析（IP 提取）。
func TestClientSource(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.5:54321"
	if got := clientSource(r); got != "203.0.113.5" {
		t.Errorf("clientSource = %q, want 203.0.113.5", got)
	}
	// 无端口的 RemoteAddr：原样返回，不应 panic。
	r.RemoteAddr = "203.0.113.5"
	if got := clientSource(r); got != "203.0.113.5" {
		t.Errorf("clientSource = %q", got)
	}
}

// TestNewSessionStoreDefaultTTL 非正 TTL 应回落默认值（避免会话永不过期）。
func TestNewSessionStoreDefaultTTL(t *testing.T) {
	s := NewSessionStore(0)
	if s.ttl <= 0 {
		t.Errorf("TTL 应回落正值, 得到 %v", s.ttl)
	}
}
