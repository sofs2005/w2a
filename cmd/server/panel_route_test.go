// panel_route_test.go 单端口路由分配测试（cmd/server/panel.go 的 newRootHandler）。
//
// 为什么单独立这个测试：面板的 SPA 回落把**未知路径一律返回 200 + index.html**，
// 于是「本该归网关的路径漏配」不会以 404 暴露，而是静默变成「200 且返回一坨
// HTML」。这类缺陷用状态码断言抓不到（200 看起来是成功），只能断言**响应体不是
// HTML**——本文件正是这么做的。
//
// 真实事故：/admin/ 曾未路由给网关时，POST /admin/accounts/<uid>/disable 返回控制台
// 首页 HTML + 200，停用/恢复按钮全部静默失效。那组端点现已下线（面板改走进程内
// 直通），本文件反向钉住「/admin/ 不得再落到面板兜底」。
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

	cfgPath := filepath.Join(dir, "config.json")

	// 网关侧 handler 用一个可辨识的哨兵替代：本测试只关心「请求是否到达网关」，
	// 不关心网关内部行为（那些由 internal/server 自己的测试覆盖）。
	gw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Gateway-Handler", "1")
		w.WriteHeader(http.StatusTeapot) // 哨兵状态码：见到它即证明路由到了网关
	})

	// 第 4 参（账号池）传 nil：本测试不校验进程内桥，nil 会让面板保持「未接入」，
	// 恰好也钉住「池缺失时装配不 panic」。第 5 参（配置重载）同样传 nil。
	h, err := newRootHandler(cfg, cfgPath, gw, nil, nil)
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

// TestAdminNamespaceIsGone 反向钉桩：账号管理端点已从网关下线，/admin/ 必须拿到
// 明确的 404 —— 既不能到达网关（旧路由加回来），也不能落到面板 SPA 兜底
// （200 + HTML 会让人以为还能用）。
//
// 对应的能力迁到面板 /api/accounts/{uid}/{disable,enable,revive}（进程内直调池），
// 由 panel/internal/api 的测试覆盖。
func TestAdminNamespaceIsGone(t *testing.T) {
	h := newTestRootHandler(t)

	for _, path := range []string{
		"/admin/accounts/someuid/disable",
		"/admin/accounts/someuid/enable",
		"/admin/accounts/someuid/revive",
	} {
		req := httptest.NewRequest("POST", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Header().Get("X-Gateway-Handler") == "1" {
			t.Errorf("%s 到达了网关：管理面不应再注册（应改为面板进程内直通）", path)
		}
		if rec.Code != http.StatusNotFound {
			body, _ := io.ReadAll(rec.Body)
			t.Errorf("%s status=%d want 404（落进面板 SPA 兜底会变成 200 + HTML，看着像还能用）body 前 120 字节=%q",
				path, rec.Code, truncate(body, 120))
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
