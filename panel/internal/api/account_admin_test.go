package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api-gui/internal/authstore"
	"workbuddy2api-gui/internal/config"
	"workbuddy2api-gui/internal/ops"
)

// fakeAdmin 假的网关侧账号管理能力：记下收到的调用参数，用内存状态回显。
//
// 取代了旧版「起一个假 HTTP 网关」的写法——停用/启用/复活已改为进程内直通池，
// 面板与网关之间不再有 HTTP 层可观测，能钉住的就是「桥被怎么调用」。
type fakeAdmin struct {
	calls []string // 形如 "set u1 true 停用测试" / "revive u1"

	manual map[string]string // uid → 手动停用原因（空串 = 未停用）
	auto   map[string]bool   // uid → 系统自动禁用位
}

func newFakeAdmin() *fakeAdmin {
	return &fakeAdmin{manual: map[string]string{}, auto: map[string]bool{}}
}

// seed 预置账号：存在性由 auto/manual 两张表的键决定。
func (f *fakeAdmin) seed(uid string, autoDisabled bool) *fakeAdmin {
	if _, ok := f.manual[uid]; !ok {
		f.manual[uid] = ""
	}
	f.auto[uid] = autoDisabled
	return f
}

func (f *fakeAdmin) known(uid string) bool {
	_, ok := f.manual[uid]
	return ok
}

func (f *fakeAdmin) SetManualDisabled(uid string, disabled bool, reason string) (bool, bool) {
	f.calls = append(f.calls, "set "+uid+" "+map[bool]string{true: "true", false: "false"}[disabled]+" "+reason)
	if !f.known(uid) {
		return false, false
	}
	prev, had := f.manual[uid], f.manual[uid] != ""
	if disabled {
		f.manual[uid] = reason
	} else {
		f.manual[uid] = ""
	}
	return true, prev != f.manual[uid] || had != disabled
}

func (f *fakeAdmin) ManualDisabledState(uid string) (bool, string, bool) {
	r, ok := f.manual[uid]
	return r != "", r, ok
}

func (f *fakeAdmin) ReviveDisabled(uid string) bool {
	f.calls = append(f.calls, "revive "+uid)
	if !f.known(uid) {
		return false
	}
	changed := f.auto[uid]
	f.auto[uid] = false
	return changed
}

func (f *fakeAdmin) AccountStatus(uid string) (bool, bool) {
	v, ok := f.auto[uid]
	return v, ok
}

// newAdminTestServer 起真 api.Server + 注入式假桥，用真实会话 Cookie 走完整链路。
// admin 为 nil 时模拟独立面板模式（宿主未注入）。
func newAdminTestServer(t *testing.T, admin ops.AccountAdmin, readOnly bool) *Server {
	t.Helper()
	store, err := authstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("构建凭证 store: %v", err)
	}
	cfg := config.Default()
	cfg.UI.Username = "admin"
	cfg.UI.Password = "pw123456"
	cfg.UI.TTL = 0
	cfg.ReadOnly = readOnly

	svc := ops.New(cfg, store, nil, nil)
	if admin != nil {
		svc.SetAccountAdmin(admin)
	}
	return NewServer(cfg, svc, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), "test")
}

// adminReq 带会话 Cookie 请求停用/启用/复活端点。
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

// TestHandleAccountDisableCallsBridge 面板端点必须把 uid + reason 送进桥并回显状态；
// 这是「面板按钮真的动了池子」的最小证据。
func TestHandleAccountDisableCallsBridge(t *testing.T) {
	fake := newFakeAdmin().seed("u1", false)
	s := newAdminTestServer(t, fake, false)

	w, resp := adminReq(t, s, http.MethodPost, "/api/accounts/u1/disable", `{"reason":"停用测试"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d body=%s", w.Code, w.Body.String())
	}
	if len(fake.calls) != 1 || fake.calls[0] != "set u1 true 停用测试" {
		t.Errorf("桥调用=%v want [set u1 true 停用测试]", fake.calls)
	}
	if resp["manual_disabled"] != true || resp["manual_reason"] != "停用测试" || resp["changed"] != true {
		t.Errorf("响应未回显停用态: %s", w.Body.String())
	}
}

// TestHandleAccountDisableDefaultsReason 空体/空 reason 时桥收到的必须是默认文案，
// 而不是空串（池里会以空原因记一笔，面板上就看不到「为什么停的」）。
func TestHandleAccountDisableDefaultsReason(t *testing.T) {
	fake := newFakeAdmin().seed("u1", false)
	s := newAdminTestServer(t, fake, false)

	if w, _ := adminReq(t, s, http.MethodPost, "/api/accounts/u1/disable", `{}`); w.Code != http.StatusOK {
		t.Fatalf("HTTP %d body=%s", w.Code, w.Body.String())
	}
	if len(fake.calls) != 1 || fake.calls[0] != "set u1 true manual" {
		t.Errorf("桥调用=%v want [set u1 true manual]", fake.calls)
	}
}

// TestHandleAccountDisableUnknownUID 桥回 found=false（uid 不在池中）时给 404，
// 不能变成 500 —— 那是用户输入问题，不是面板故障。
func TestHandleAccountDisableUnknownUID(t *testing.T) {
	fake := newFakeAdmin()
	s := newAdminTestServer(t, fake, false)

	w, _ := adminReq(t, s, http.MethodPost, "/api/accounts/u9/disable", `{}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("HTTP %d want 404 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "网关账号池") {
		t.Errorf("错误文案应说明账号不在池中: %s", w.Body.String())
	}
}

// TestHandleAccountEnableEchoesDisabled 启用后系统自动禁用位仍为真时必须在响应里
// 透出（前端据此提示「已解除手动停用，但仍被系统禁用」）。
func TestHandleAccountEnableEchoesDisabled(t *testing.T) {
	fake := newFakeAdmin().seed("u1", true)
	fake.manual["u1"] = "观察几天"
	s := newAdminTestServer(t, fake, false)

	w, resp := adminReq(t, s, http.MethodPost, "/api/accounts/u1/enable", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d body=%s", w.Code, w.Body.String())
	}
	if len(fake.calls) != 1 || fake.calls[0] != "set u1 false " {
		t.Errorf("桥调用=%v want [set u1 false ]", fake.calls)
	}
	if resp["manual_disabled"] != false || resp["disabled"] != true {
		t.Errorf("响应未回显双位（手动已解、系统仍禁）: %s", w.Body.String())
	}
}

// TestHandleAccountRevive 复活走 /revive 且只清系统位，手动停用位保持不动。
func TestHandleAccountRevive(t *testing.T) {
	fake := newFakeAdmin().seed("u1", true)
	s := newAdminTestServer(t, fake, false)

	w, resp := adminReq(t, s, http.MethodPost, "/api/accounts/u1/revive", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d body=%s", w.Code, w.Body.String())
	}
	if len(fake.calls) != 1 || fake.calls[0] != "revive u1" {
		t.Errorf("桥调用=%v want [revive u1]", fake.calls)
	}
	if resp["disabled"] != false || resp["changed"] != true {
		t.Errorf("响应未回显复活结果: %s", w.Body.String())
	}
}

// TestHandleAccountReviveUnknownUID 与停用同构：uid 不存在给 404。
func TestHandleAccountReviveUnknownUID(t *testing.T) {
	fake := newFakeAdmin()
	s := newAdminTestServer(t, fake, false)

	w, _ := adminReq(t, s, http.MethodPost, "/api/accounts/u9/revive", `{}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("HTTP %d want 404 body=%s", w.Code, w.Body.String())
	}
	if len(fake.calls) != 0 {
		t.Errorf("不存在的 uid 不该调桥: %v", fake.calls)
	}
}

// TestHandleAccountDisableReadOnly 只读模式下必须先于桥调用被拦下。
func TestHandleAccountDisableReadOnly(t *testing.T) {
	fake := newFakeAdmin().seed("u1", false)
	s := newAdminTestServer(t, fake, true)

	w, _ := adminReq(t, s, http.MethodPost, "/api/accounts/u1/disable", `{}`)
	if w.Code == http.StatusOK {
		t.Fatal("只读模式下不应成功")
	}
	if len(fake.calls) != 0 {
		t.Errorf("只读模式下不应调桥: %v", fake.calls)
	}
}

// TestHandleAccountDisableUnavailable 独立面板模式（宿主未注入桥）下必须明确报
// 「部署形态不支持」，而不是 500 或静默成功。
func TestHandleAccountDisableUnavailable(t *testing.T) {
	s := newAdminTestServer(t, nil, false)

	w, _ := adminReq(t, s, http.MethodPost, "/api/accounts/u1/disable", `{}`)
	if w.Code == http.StatusOK {
		t.Fatalf("未注入桥时不应成功: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "同进程") {
		t.Errorf("错误文案应说明部署形态不支持: %s", w.Body.String())
	}
}
