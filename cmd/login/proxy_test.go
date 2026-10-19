// proxy_test.go login CLI 的代理接线：proxyForRealm 的取值优先级与
// newLoginClient 的代理生效性。
//
// 为什么 login 也要认代理：国内用户首次登录 global 账号时网关往往还没起、环境变量
// 也未必设，config.json 里的 proxy 段是此时唯一可达路径（鸡生蛋问题）。
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// writeConfig 把 raw 写进临时 config.json 并返回路径。
func writeConfig(t *testing.T, raw string) string {
	t.Helper()
	fp := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return fp
}

// TestProxyForRealmReadsConfig 从 config.json 的 proxy 段按 realm 取值。
func TestProxyForRealmReadsConfig(t *testing.T) {
	fp := writeConfig(t, `{"proxy":{"cn":"http://cn.corp:3128","global":"socks5://10.0.0.1:1080"}}`)
	if got := proxyForRealm(realmCN, fp); got != "http://cn.corp:3128" {
		t.Errorf("cn=%q", got)
	}
	if got := proxyForRealm(realmGlobal, fp); got != "socks5://10.0.0.1:1080" {
		t.Errorf("global=%q", got)
	}
}

// TestProxyForRealmMissingFileIsNotAnError 读不到 config.json 不是错误（登录常发生在
// 网关部署之前），回落空串 = 直连。
func TestProxyForRealmMissingFileIsNotAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	if got := proxyForRealm(realmGlobal, missing); got != "" {
		t.Errorf("missing config should yield empty proxy, got %q", got)
	}
}

// TestProxyForRealmCorruptConfigFallsBack 配置损坏静默回落直连（网关启动时才报错，
// login 是交互式工具，不该在登录环节抛配置解析错）。
func TestProxyForRealmCorruptConfigFallsBack(t *testing.T) {
	fp := writeConfig(t, `{"proxy":{`)
	if got := proxyForRealm(realmCN, fp); got != "" {
		t.Errorf("corrupt config should yield empty proxy, got %q", got)
	}
}

// TestProxyForRealmEnvWins env 优先于文件（与网关 applyEnv 同序：env 后应用）。
func TestProxyForRealmEnvWins(t *testing.T) {
	fp := writeConfig(t, `{"proxy":{"cn":"http://from-file:3128","global":"http://from-file:3128"}}`)
	t.Setenv("WB2A_PROXY_CN", "socks5://env-cn:1080")
	t.Setenv("WB2A_PROXY_GLOBAL", "socks5h://env-global:1080")
	if got := proxyForRealm(realmCN, fp); got != "socks5://env-cn:1080" {
		t.Errorf("cn=%q want env value", got)
	}
	if got := proxyForRealm(realmGlobal, fp); got != "socks5h://env-global:1080" {
		t.Errorf("global=%q want env value", got)
	}
}

// TestProxyForRealmNoEnvLeakAcrossRealms CN 的 env 不该被 global 读到（键按 realm 选）。
func TestProxyForRealmNoEnvLeakAcrossRealms(t *testing.T) {
	t.Setenv("WB2A_PROXY_CN", "socks5://only-cn:1080")
	fp := writeConfig(t, `{"proxy":{"global":"http://from-file:3128"}}`)
	if got := proxyForRealm(realmGlobal, fp); got != "http://from-file:3128" {
		t.Errorf("global=%q（WB2A_PROXY_CN 不得作用于 global）", got)
	}
}

// TestNewLoginClientNoProxyIsDirect 空代理串 = 直连：Transport 保持 nil（http.Client
// 的默认 Transport），且**不**隐式读 HTTP_PROXY 环境变量——login 是短命 CLI，用户显式
// 配了才走代理，避免宿主机碰巧设的全局代理把登录引到意外路径。
func TestNewLoginClientNoProxyIsDirect(t *testing.T) {
	// 故意设一个指向死地址的环境变量代理：若实现误接了 ProxyFromEnvironment，下面
	// 的请求会失败（而不是到达 httptest 目标）。
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	c, err := newLoginClient("")
	if err != nil {
		t.Fatalf("empty proxy must not error: %v", err)
	}
	if c.Transport != nil {
		t.Errorf("empty proxy should leave Transport unset (direct), got %T", c.Transport)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("direct-ok"))
	}))
	defer srv.Close()
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("direct request should not be routed through env proxy: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 32)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "direct-ok" {
		t.Errorf("body=%q", buf[:n])
	}
}

// TestNewLoginClientInvalidProxyErrors 非法代理 URL 直接报错而非静默直连：
// 交互式工具里静默回落会让用户对着「连不上」白等一轮，而根因（URL 写错）一眼可见。
func TestNewLoginClientInvalidProxyErrors(t *testing.T) {
	if _, err := newLoginClient("ftp://10.0.0.1:1080"); err == nil {
		t.Fatal("invalid proxy URL must error")
	}
}

// TestNewLoginClientHTTPProxyRoutesThroughProxy 端到端：HTTP 代理真的被用上
// （请求打到代理，代理再把 absolute-URI 转给目标——Go 标准库对 http:// 目标不建
// CONNECT 隧道，故这里用一个记录请求的假代理即可验证「确实经过代理」）。
func TestNewLoginClientHTTPProxyRoutesThroughProxy(t *testing.T) {
	var sawProxyHit bool
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawProxyHit = true
		// 目标 URL 是绝对形式（http://host/path），标准库代理语义要求原样转发。
		// 这里不真转发，只回一个可识别的 body 证明请求到达了代理。
		w.Write([]byte("via-proxy"))
	}))
	defer proxySrv.Close()

	c, err := newLoginClient(proxySrv.URL)
	if err != nil {
		t.Fatalf("newLoginClient: %v", err)
	}
	resp, err := c.Get("http://upstream.invalid/v2/plugin/auth/state")
	if err != nil {
		t.Fatalf("request via proxy: %v", err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 32)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "via-proxy" {
		t.Errorf("body=%q want via-proxy", buf[:n])
	}
	if !sawProxyHit {
		t.Error("proxy was never hit")
	}
}

// TestCarryPreservedFieldsKeepsAccountProxy 重新登录（runOnce 落盘）是**整份覆盖**：
// 已存在文件里的账号级线路名（`proxy`，面板写入）与 device_token 必须被带过去，
// 否则「重新登录一次」就把它们悄悄抹掉 —— 线路被解绑意味着出口静默改变。
// 面板侧的 preserve.go 只护住面板自己的保存路径，护不到 login。
func TestCarryPreservedFieldsKeepsAccountProxy(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "workbuddy-u1.json")
	existing := `{"auth":{"accessToken":"old","refreshToken":"ort","expiresAt":1},"account":{"uid":"u1"},` +
		`"device_token":"dev-tok","proxy":"route-a"}`
	if err := os.WriteFile(fp, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	a := &auth.Auth{AccessToken: "new", RefreshToken: "nrt", ExpiresAt: 2, UID: "u1", FilePath: fp}
	if carried := carryPreservedFields(a, fp); !carried {
		t.Error("carryPreservedFields should report that an account proxy was carried")
	}
	if a.ProxyName != "route-a" {
		t.Errorf("ProxyName=%q want carried from existing file", a.ProxyName)
	}
	if a.DeviceToken != "dev-tok" {
		t.Errorf("DeviceToken=%q want carried from existing file", a.DeviceToken)
	}

	// 落盘后回读：两个键都还在（这才是用户真正关心的事）。
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	back, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("auth.Parse: %v", err)
	}
	if back.ProxyName != "route-a" || back.DeviceToken != "dev-tok" {
		t.Errorf("after re-login: proxy=%q device_token=%q（重新登录不得抹掉线路绑定）", back.ProxyName, back.DeviceToken)
	}
	if back.AccessToken != "new" {
		t.Errorf("AccessToken=%q want new (新凭证必须覆盖旧的)", back.AccessToken)
	}
}

// TestCarryPreservedFieldsNewAccountIsNoop 新账号（文件不存在）时静默返回 false，
// 不写任何字段、不报错。
func TestCarryPreservedFieldsNewAccountIsNoop(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "workbuddy-new.json")
	a := &auth.Auth{AccessToken: "at", UID: "u9", FilePath: fp}
	if carried := carryPreservedFields(a, fp); carried {
		t.Error("new account should report nothing carried")
	}
	if a.ProxyName != "" || a.DeviceToken != "" {
		t.Errorf("new account should not gain fields: proxy=%q device_token=%q", a.ProxyName, a.DeviceToken)
	}
}

// TestCarryPreservedFieldsCorruptFileIsNoop 旧文件损坏时静默跳过：重新登录本身可能
// 就是在修一个坏文件，不该被它挡住。
func TestCarryPreservedFieldsCorruptFileIsNoop(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "workbuddy-bad.json")
	if err := os.WriteFile(fp, []byte(`{"auth":{`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &auth.Auth{AccessToken: "at", UID: "u9", FilePath: fp}
	if carried := carryPreservedFields(a, fp); carried {
		t.Error("corrupt file should report nothing carried")
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic over corrupt file should still work: %v", err)
	}
}

// TestLoginStateAndProxyConfigAreIndependentKeys 守卫测试：proxy 段与 loginState
// 的 JSON 键互不干扰（proxyForRealm 用匿名 struct 局部解码，不该影响其他解析）。
func TestLoginStateAndProxyConfigAreIndependentKeys(t *testing.T) {
	fp := writeConfig(t, `{"proxy":{"global":"socks5://g:1080"},"state":"s1"}`)
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var ls loginState
	if err := json.Unmarshal(raw, &ls); err != nil {
		t.Fatal(err)
	}
	if ls.State != "s1" {
		t.Errorf("loginState.State=%q", ls.State)
	}
	if got := proxyForRealm(realmGlobal, fp); !strings.Contains(got, "socks5://g:1080") {
		t.Errorf("proxyForRealm=%q", got)
	}
}
