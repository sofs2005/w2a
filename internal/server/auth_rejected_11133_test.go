package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// TestChatAuthRejectedCountsNotDisableImmediately 纯 401（无 12153 业务码）：单次不
// 禁用（可能是网关抖动 / token 轮换竞态），但必须喂连续计数——否则坏号落 ErrClient
// 只换号不罚、10 分钟后又回池，表现为「面板显示健康、每次命中都 503」。
func TestChatAuthRejectedCountsNotDisableImmediately(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 401, `<html><body>401 Unauthorized</body></html>`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("单次纯 401 不该禁用账号（可能只是网关抖动）: %+v", st)
	}
	// 计数确实被喂入：再来一次仍不禁用，第 3 次达阈值（见下一个用例）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("第 2 次纯 401 仍不该禁用（阈值 %d）", pool.SessionDeadThreshold())
	}
}

// TestChatAuthRejectedDisablesAfterThreshold 连续 3 次纯 401（sessionDeadThreshold）
// → 禁用。计数器与 12153 共用。
func TestChatAuthRejectedDisablesAfterThreshold(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 401, `{"msg":"unauthorized"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	for i := 0; i < pool.SessionDeadThreshold(); i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	}

	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("连续 %d 次纯 401 应禁用账号: %+v", pool.SessionDeadThreshold(), st)
	}
	if !strings.Contains(st.Reason, "401 auth rejected") {
		t.Errorf("禁用原因应标明是纯 401 形态，got %q", st.Reason)
	}
}

// TestChat11133FailsFastNoRotation 11133（模型侧参数拒绝，如 Kimi 要求工具 schema 的
// enum 带 type）：同一 body 换任何账号都是同一个 400，必须 fail-fast 透传，
// 不轮转、不喂连败计数（实测一次连续探测把 healthy 从 19 打到 7）。
func TestChat11133FailsFastNoRotation(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, `{"code":11133,"msg":"Invalid request parameters","extError":{"code":"400001","message":"the request parameters were rejected by the model provider"}}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"kimi-k3.1-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 400 {
		t.Errorf("status=%d want 400（上游原文透传）", rec.Code)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1（请求级错误不该轮转）", calls)
	}
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if st.ConsecutiveFails != 0 {
			t.Errorf("%s ConsecutiveFails=%d want 0（请求级错误不该喂连败）", uid, st.ConsecutiveFails)
		}
	}
}
