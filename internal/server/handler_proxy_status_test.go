// handler_proxy_status_test.go /status 的出站代理透出（脱敏）。
//
// 核心断言不是「字段在不在」，而是**密码一个字都不能出现**：/status 是运维端点，
// 会被截图、被采集、被写进 issue。透出的信息只需够运维核对「是不是我设的那个代理」。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// proxyStatusBody 起一个最小 handler 并取 /status 的 JSON 体。
func proxyStatusBody(t *testing.T, up *upstream.Client) map[string]any {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	return body
}

// proxyEntry 取 /status 的 proxy.<realm> 对象。
func proxyEntry(t *testing.T, body map[string]any, realm string) map[string]any {
	t.Helper()
	proxy, ok := body["proxy"].(map[string]any)
	if !ok {
		t.Fatalf("status has no proxy object: %v", body["proxy"])
	}
	entry, ok := proxy[realm].(map[string]any)
	if !ok {
		t.Fatalf("status has no proxy.%s object: %v", realm, proxy[realm])
	}
	return entry
}

// TestStatusProxyDisabledByDefault 未配代理时两域都显式报 enabled=false
// （零值也写出——缺失会让人误以为「这个键没实现」）。
func TestStatusProxyDisabledByDefault(t *testing.T) {
	body := proxyStatusBody(t, upstream.New())
	for _, realm := range []string{"cn", "global"} {
		entry := proxyEntry(t, body, realm)
		if entry["enabled"] != false {
			t.Errorf("proxy.%s.enabled=%v want false", realm, entry["enabled"])
		}
		// 未启用时不该编造 scheme/addr。
		if _, ok := entry["scheme"]; ok {
			t.Errorf("proxy.%s should not carry scheme when disabled", realm)
		}
		if _, ok := entry["addr"]; ok {
			t.Errorf("proxy.%s should not carry addr when disabled", realm)
		}
	}
}

// TestStatusProxyNeverLeaksCredentials 配了带账号密码的代理后，/status 只给
// scheme+addr，**用户名与密码一律不出现**（连 base64 的 Proxy-Authorization 形态也不行）。
func TestStatusProxyNeverLeaksCredentials(t *testing.T) {
	const secret = "sup3r-s3cret-pw"
	up := upstream.New()
	up.SetProxies("", "socks5://alice:"+secret+"@10.0.0.1:1080")

	body := proxyStatusBody(t, up)

	entry := proxyEntry(t, body, "global")
	if entry["enabled"] != true {
		t.Fatalf("proxy.global.enabled=%v want true", entry["enabled"])
	}
	if entry["scheme"] != "socks5" {
		t.Errorf("proxy.global.scheme=%v want socks5", entry["scheme"])
	}
	if entry["addr"] != "10.0.0.1:1080" {
		t.Errorf("proxy.global.addr=%v want 10.0.0.1:1080", entry["addr"])
	}
	if proxyEntry(t, body, "cn")["enabled"] != false {
		t.Error("proxy.cn should stay disabled when only proxy.global is set (两域独立)")
	}

	// 全响应体扫一遍：密码与用户名都不能以任何形态出现。
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{secret, "alice", "alice:", "YWxpY2U"} {
		if strings.Contains(string(raw), needle) {
			t.Errorf("/status leaked %q (credential material must never leave the process): %s", needle, raw)
		}
	}
}

// TestStatusProxyReflectsAppliedNotConfigured /status 报告的是**真正接上去的**代理：
// 非法 URL 经 SetProxies 回落（不替换 Transport）后，/status 如实显示未启用，
// 而不是照抄配置里那串错的。
func TestStatusProxyReflectsAppliedNotConfigured(t *testing.T) {
	up := upstream.New()
	up.SetProxies("", "ftp://10.0.0.1:1080") // 非法 scheme
	if entry := proxyEntry(t, proxyStatusBody(t, up), "global"); entry["enabled"] != false {
		t.Errorf("invalid proxy must report enabled=false, got %v", entry)
	}
}

// TestStatusProxyCNOnly 只配 CN 时 global 保持未启用（两域独立，不是「跟随」）。
func TestStatusProxyCNOnly(t *testing.T) {
	up := upstream.New()
	up.SetProxies("http://proxy.corp:3128", "")
	body := proxyStatusBody(t, up)
	if entry := proxyEntry(t, body, "cn"); entry["enabled"] != true || entry["scheme"] != "http" || entry["addr"] != "proxy.corp:3128" {
		t.Errorf("proxy.cn=%v want enabled http://proxy.corp:3128", entry)
	}
	if entry := proxyEntry(t, body, "global"); entry["enabled"] != false {
		t.Errorf("proxy.global=%v want disabled (realms are independent)", entry)
	}
}
