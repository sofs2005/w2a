package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// 账号级出口线路解析失败 = 配置错误，不是账号问题。
//
// 换多少个号结果都一样（线路名在账号文件里，或线路表本身缺失），所以必须：
// 上游被调用 0 次、账号不被 NoteFailures / 不被冷却禁用、直接 503。
// 回归点：若线路错误退化成普通 error，handler 会走「网络抖动」分支 →
// NoteFailures + fail + 轮转，一串健康号被一个配置笔误连坐罚分。
func TestRouteUnavailableStopsWithoutPenalizing(t *testing.T) {
	upstream.ConfigureProxies(map[string]string{"route-a": "http://a.example:8080"})
	t.Cleanup(func() { upstream.ConfigureProxies(nil) })

	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return sseResponse(200, sseOK), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	bound := &auth.Auth{UID: "u-route", AccessToken: "at1", ExpiresAt: 9999999999}
	bound.ProxyName = "no-such-route"
	p := testPoolWith(bound, &auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("配置错误应回 503，got %d body=%s", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Errorf("线路解析失败不该发出任何上游请求（更不该轮转到别的号）：calls=%d", calls)
	}
	for _, uid := range []string{"u-route", "u2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled {
			t.Errorf("%s 不该被罚（这是配置问题）：%+v", uid, st)
		}
		if st.ConsecutiveFails != 0 {
			t.Errorf("%s 不该累计连败（NoteFailures 会把它临时踢出池）：consecutive_fails=%d", uid, st.ConsecutiveFails)
		}
	}
}

// 线路表为空但账号绑了名字：同样 503、不罚号（这是最常见的部署失误——
// 面板绑了线路，网关 config 还没加 proxies 段）。
func TestRouteMissingTableStopsWithoutPenalizing(t *testing.T) {
	upstream.ConfigureProxies(nil)
	t.Cleanup(func() { upstream.ConfigureProxies(nil) })

	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return sseResponse(200, sseOK), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	bound := &auth.Auth{UID: "u-route", AccessToken: "at1", ExpiresAt: 9999999999}
	bound.ProxyName = "route-a"
	p := testPoolWith(bound)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusServiceUnavailable || calls != 0 {
		t.Fatalf("应 503 且不发上游请求，got code=%d calls=%d", rec.Code, calls)
	}
	st, _ := p.Status("u-route")
	if st.Cooling || st.Disabled || st.ConsecutiveFails != 0 {
		t.Errorf("账号不该被罚：%+v", st)
	}
}

// 未绑线路的账号必须走原路径（零回归）：ConfigureProxies 置空后正常成功。
func TestUnboundAccountUnaffectedByRouteTable(t *testing.T) {
	upstream.ConfigureProxies(map[string]string{"route-a": "http://a.example:8080"})
	t.Cleanup(func() { upstream.ConfigureProxies(nil) })

	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return sseResponse(200, sseOK), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("未绑线路不该受线路表影响，got %d body=%s", rec.Code, rec.Body.String())
	}
}
