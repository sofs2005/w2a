package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDisableAccountSendsReasonAndParsesState 钉住停用请求的路径、鉴权头、
// reason 体与响应解析——四者任一错位，面板上「点了没反应」或「原因不显示」。
func TestDisableAccountSendsReasonAndParsesState(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"uid":"u1","manual_disabled":true,"manual_reason":"观察几天","disabled":false,"changed":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL, func() string { return "secret-key" }, time.Second)
	st, err := c.DisableAccount(context.Background(), "u1", "观察几天")
	if err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}
	if gotPath != "/admin/accounts/u1/disable" {
		t.Errorf("路径=%q want /admin/accounts/u1/disable", gotPath)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("鉴权头=%q want Bearer secret-key", gotAuth)
	}
	if gotBody["reason"] != "观察几天" {
		t.Errorf("请求体 reason=%q want 观察几天", gotBody["reason"])
	}
	if !st.ManualDisabled || st.ManualReason != "观察几天" || !st.Changed || st.Disabled {
		t.Errorf("响应解析错误: %+v", st)
	}
}

// TestDisableAccountEmptyReasonSendsEmptyObject 空 reason 也要发合法 JSON 体
// （不是 nil body）：网关侧读体是可选语义，但 nil body 会让 Content-Type 缺失。
func TestDisableAccountEmptyReasonSendsEmptyObject(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		_, _ = w.Write([]byte(`{"uid":"u1","manual_disabled":true,"disabled":false,"changed":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL, func() string { return "" }, time.Second)
	if _, err := c.DisableAccount(context.Background(), "u1", ""); err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}
	if raw != "{}" {
		t.Errorf("空 reason 的请求体=%q want {}", raw)
	}
}

// TestEnableAccountHitsEnablePath enable 必须走 /enable 且不复活系统禁用位。
func TestEnableAccountHitsEnablePath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"uid":"u1","manual_disabled":false,"disabled":true,"changed":true}`))
	}))
	defer srv.Close()

	c := New(srv.URL, func() string { return "" }, time.Second)
	st, err := c.EnableAccount(context.Background(), "u1")
	if err != nil {
		t.Fatalf("EnableAccount: %v", err)
	}
	if gotPath != "/admin/accounts/u1/enable" {
		t.Errorf("路径=%q want /admin/accounts/u1/enable", gotPath)
	}
	// disabled 仍为真：enable 只解手动位，面板据此提示「仍被系统禁用」。
	if st.ManualDisabled || !st.Disabled {
		t.Errorf("enable 后状态错误: %+v", st)
	}
}

// TestAdminCallTranslates404 404 的两种来源必须分开翻译：admin.enabled 未开是
// 「配置问题」（裸文本 404），uid 不存在是「账号问题」（JSON 错误体）。
// 混为一谈会让运维照着错误提示改错地方。
func TestAdminCallTranslates404(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{"admin 未开启（裸文本）", "404 page not found\n", "admin.enabled"},
		{"账号不存在（JSON 体）", `{"error":{"message":"account not found: u9","type":"not_found"}}`, "不在网关账号池中"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := New(srv.URL, func() string { return "" }, time.Second)
			_, err := c.DisableAccount(context.Background(), "u9", "")
			if err == nil {
				t.Fatal("期望报错")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("错误=%q 应含 %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestAdminCallTranslates401 鉴权失败给的是 api_key 指引，不是原始状态码。
func TestAdminCallTranslates401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(srv.URL, func() string { return "bad" }, time.Second)
	_, err := c.DisableAccount(context.Background(), "u1", "")
	if err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Errorf("401 错误=%v 应含 api_key 指引", err)
	}
}
