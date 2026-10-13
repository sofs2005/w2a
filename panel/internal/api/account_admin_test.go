package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api-gui/internal/authstore"
	"workbuddy2api-gui/internal/config"
	"workbuddy2api-gui/internal/gateway"
	"workbuddy2api-gui/internal/ops"
)

// newAdminTestServer 起一个假网关 + 真 api.Server，用真实会话 Cookie 走完整链路。
// fake 为假网关的 handler；readOnly 控制面板只读位。
func newAdminTestServer(t *testing.T, fake http.Handler, readOnly bool) *Server {
	t.Helper()
	gw := httptest.NewServer(fake)
	t.Cleanup(gw.Close)

	store, err := authstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("构建凭证 store: %v", err)
	}
	cfg := config.Default()
	cfg.UI.Username = "admin"
	cfg.UI.Password = "pw123456"
	cfg.UI.TTL = 0
	cfg.ReadOnly = readOnly

	svc := ops.New(cfg, store, gateway.New(gw.URL, func() string { return "k" }, time.Second), nil)
	return NewServer(cfg, svc, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "test")
}

// adminReq 带会话 Cookie 请求停用/启用端点。
func adminReq(t *testing.T, s *Server, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	token, ok, _ := s.sessions.Create("admin", "pw123456", s.cfg, "1.1.1.1")
	if !ok {
		t.Fatal("测试登录失败")
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w, resp
}

// TestHandleAccountDisableForwardsToGateway 面板端点必须把 reason 转发到网关
// /admin 并回显状态；这是「面板按钮真的动了池子」的最小证据。
func TestHandleAccountDisableForwardsToGateway(t *testing.T) {
	var gotPath string
	var gotReason string
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var b struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotReason = b.Reason
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"uid":"u1","manual_disabled":true,"manual_reason":"停用测试","disabled":false,"changed":true}`))
	})
	s := newAdminTestServer(t, fake, false)

	w, resp := adminReq(t, s, http.MethodPost, "/api/accounts/u1/disable", `{"reason":"停用测试"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/admin/accounts/u1/disable" {
		t.Errorf("转发路径=%q want /admin/accounts/u1/disable", gotPath)
	}
	if gotReason != "停用测试" {
		t.Errorf("转发 reason=%q want 停用测试", gotReason)
	}
	if resp["manual_disabled"] != true || resp["manual_reason"] != "停用测试" {
		t.Errorf("响应未回显停用态: %s", w.Body.String())
	}
}

// TestHandleAccountDisableGateway404 假网关回裸文本 404（= 网关未开 admin.enabled）
// 时，面板必须把配置指引透出去，而不是把裸 404 甩给用户。
func TestHandleAccountDisableGateway404(t *testing.T) {
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found\n"))
	})
	s := newAdminTestServer(t, fake, false)

	w, _ := adminReq(t, s, http.MethodPost, "/api/accounts/u1/disable", `{}`)
	if w.Code == http.StatusOK {
		t.Fatalf("期望失败，得到 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "admin.enabled") {
		t.Errorf("错误文案应含 admin.enabled 指引: %s", w.Body.String())
	}
}

// TestHandleAccountEnableForwards 启用走 /enable 并回显「仍被系统禁用」的信息
// （前端据此提示需另处理故障）。
func TestHandleAccountEnableForwards(t *testing.T) {
	var gotPath string
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"uid":"u1","manual_disabled":false,"disabled":true,"changed":true}`))
	})
	s := newAdminTestServer(t, fake, false)

	w, resp := adminReq(t, s, http.MethodPost, "/api/accounts/u1/enable", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/admin/accounts/u1/enable" {
		t.Errorf("转发路径=%q want /admin/accounts/u1/enable", gotPath)
	}
	if resp["disabled"] != true {
		t.Errorf("响应未回显 disabled 位: %s", w.Body.String())
	}
}

// TestHandleAccountDisableReadOnly 只读模式下必须先于网关调用被拦下。
func TestHandleAccountDisableReadOnly(t *testing.T) {
	called := false
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	})
	s := newAdminTestServer(t, fake, true)

	w, _ := adminReq(t, s, http.MethodPost, "/api/accounts/u1/disable", `{}`)
	if w.Code == http.StatusOK {
		t.Fatal("只读模式下不应成功")
	}
	if called {
		t.Error("只读模式下不应打网关")
	}
}
