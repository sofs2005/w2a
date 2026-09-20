// panel_route_test.go 单端口路由分配测试（cmd/server/panel.go 的 newRootHandler）。
//
// 为什么单独立这个测试：面板的 SPA 回落把**未知路径一律返回 200 + index.html**，
// 于是「本该归网关的路径漏配」不会以 404 暴露，而是静默变成「200 且返回一坨
// HTML」。这类缺陷用状态码断言抓不到（200 看起来是成功），只能断言**响应体不是
// HTML**——本文件正是这么做的。
//
// 真实事故：/admin/ 未路由给网关时，POST /admin/accounts/<uid>/disable 返回控制台
// 首页 HTML + 200，cmd/acct 与面板的停用/恢复按钮全部静默失效。
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRootHandler 构造被测的合并 handler（面板装配需要真实可读的路径）。
func newTestRootHandler(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	cfg := Default()
	cfg.Listen = "127.0.0.1:17863" // loopbackURL 需要可解析的 host:port
	cfg.AuthDir = filepath.Join(dir, "auths")
	cfg.StateFile = filepath.Join(dir, "data", "state.json")
	cfg.Admin.Enabled = true

	cfgPath := filepath.Join(dir, "config.json")

	// 网关侧 handler 用一个可辨识的哨兵替代：本测试只关心「请求是否到达网关」，
	// 不关心网关内部行为（那些由 internal/server 自己的测试覆盖）。
	gw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Gateway-Handler", "1")
		w.WriteHeader(http.StatusTeapot) // 哨兵状态码：见到它即证明路由到了网关
	})

	h, err := newRootHandler(cfg, cfgPath, gw)
	if err != nil {
		t.Fatalf("newRootHandler: %v", err)
	}
	return h
}

// TestGatewayPathsReachGateway 本应归网关的路径必须真的到达网关，而不是被面板的
// SPA 回落静默吞掉。用哨兵状态码 + 哨兵响应头双重断言（避免「面板恰好也返回同样
// 状态码」造成假通过）。
func TestGatewayPathsReachGateway(t *testing.T) {
	h := newTestRootHandler(t)

	paths := []struct {
		method string
		path   string
		why    string
	}{
		{"GET", "/status", "账号池状态"},
		{"GET", "/healthz", "探活"},
		{"GET", "/v1/models", "模型列表"},
		{"POST", "/v1/chat/completions", "OpenAI 兼容主端点"},
		{"GET", "/v1/stats", "请求统计"},
		// 运维端点：漏配时会被面板 SPA 回落吞掉（200 + HTML），按钮静默失效。
		{"POST", "/admin/accounts/someuid/disable", "账号停用"},
		{"POST", "/admin/accounts/someuid/enable", "账号恢复"},
		{"POST", "/admin/accounts/someuid/revive", "账号复活"},
	}
	for _, tc := range paths {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Header().Get("X-Gateway-Handler") != "1" {
				body, _ := io.ReadAll(rec.Body)
				t.Errorf("%s（%s）未到达网关：status=%d content-type=%q body 前 120 字节=%q"+
					"\n（面板 SPA 回落会把未知路径返回 200 + HTML，静默吞掉本应归网关的路由）",
					tc.path, tc.why, rec.Code, rec.Header().Get("Content-Type"), truncate(body, 120))
			}
		})
	}
}

// TestPanelOwnsNamespace 面板自己的命名空间仍归面板（本次路由改动不得抢走它们）。
// 断言「没到达网关」即可——不预设面板返回什么状态码（前端产物可能缺失）。
func TestPanelOwnsNamespace(t *testing.T) {
	h := newTestRootHandler(t)

	for _, path := range []string{"/", "/api/session", "/assets/x.js", "/some-spa-route"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Header().Get("X-Gateway-Handler") == "1" {
			t.Errorf("%s 不应归网关（面板命名空间）", path)
		}
	}
}

// TestTrimSlashRoutesReachGateway 尾斜杠形态经 trimSlash 交给网关（既有行为，
// 防止本次改动把它挤到面板兜底）。
func TestTrimSlashRoutesReachGateway(t *testing.T) {
	h := newTestRootHandler(t)
	for _, path := range []string{"/status/", "/healthz/"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Header().Get("X-Gateway-Handler") != "1" {
			t.Errorf("%s 未到达网关（尾斜杠形态应由 trimSlash 转发）", path)
		}
	}
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
