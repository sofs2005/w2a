package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/prompt"
)

// 用户报告：偶发 `no_healthy_account`，message 里装着上游原文
// `{"code":11-128,"msg":"first message is not system prompt",...displayTips 渠道校验...}`。
//
// 链路：上游对部分账号灰度启用请求**形态**校验 → messages[0] 不是 system 即整单
// 拦截 → 归 ErrAccountFault（渠道校验）→ 账号软冷却 → 客户端看到
// no_healthy_account。触发条件是网关透传了「首条 user」的请求（面板试用台未填
// 系统提示词、脚本探测请求都这种形态），而官方客户端从不发该形态。
//
// 修复：出站收口（upstream.prepareBody → prompt.EnsureLeadingSystem）补一条官方
// base-agent-instructions 模板句作为开头 system。本文件钉住端到端形态。

// postChat 发一次 /v1/chat/completions 并返回 recorder。
func postChat(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

// outboundRoles 取出站 body 的 messages 角色序列。
func outboundRoles(t *testing.T, raw []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal outbound: %v (%s)", err, raw)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("outbound messages missing: %s", raw)
	}
	roles := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("non-object message: %v", m)
		}
		r, _ := mm["role"].(string)
		roles = append(roles, r)
	}
	return roles
}

// TestChatUserOnlyRequestGetsLeadingSystem passthrough（缺省）下只带 user 的请求，
// 出站首条必须是 system —— 否则上游按「first message is not system prompt」整单拦截。
func TestChatUserOnlyRequestGetsLeadingSystem(t *testing.T) {
	var bodies [][]byte
	up := newBodyCaptureUpstream(t, &bodies, func(call int, body string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up}) // PromptMode 缺省 = passthrough

	rec := postChat(h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(bodies) != 1 {
		t.Fatalf("want 1 upstream call, got %d", len(bodies))
	}
	roles := outboundRoles(t, bodies[0])
	if len(roles) != 2 || roles[0] != "system" || roles[1] != "user" {
		t.Fatalf("roles=%v want [system user]: %s", roles, bodies[0])
	}
	// 补的必须是官方模板句，不是自造文案：上游按「是不是官方客户端的请求形态」判渠道校验。
	var obj map[string]any
	json.Unmarshal(bodies[0], &obj)
	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != prompt.FallbackSystem {
		t.Errorf("兜底 system content=%v want %q", got, prompt.FallbackSystem)
	}
	// 不允许回退到曾判定为自造形态的那句（官方分发包 grep 计数 0）。
	for _, lit := range []string{"You are a helpful assistant."} {
		if bytes.Contains(bodies[0], []byte(lit)) {
			t.Errorf("出站 body 不得含官方从不发送的字面量 %q: %s", lit, bodies[0])
		}
	}
}

// TestChatExistingSystemUntouched 客户端自带 system 时逐字透传，不追加、不替换。
func TestChatExistingSystemUntouched(t *testing.T) {
	var bodies [][]byte
	up := newBodyCaptureUpstream(t, &bodies, func(call int, body string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := postChat(h, `{"model":"glm-5.2","stream":true,"messages":[
		{"role":"system","content":"项目规范"},
		{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	roles := outboundRoles(t, bodies[0])
	if len(roles) != 2 || roles[0] != "system" || roles[1] != "user" {
		t.Fatalf("roles=%v want [system user]（不追加第二条）: %s", roles, bodies[0])
	}
	var obj map[string]any
	json.Unmarshal(bodies[0], &obj)
	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != "项目规范" {
		t.Errorf("客户端 system 被改写: %v", got)
	}
}

// TestChatDeveloperFirstNotDoubleWrapped 首条 developer 下游归一为 system，
// 兜底判定必须在归一之后 —— 否则出站会出现两条 system（多一条官方从不发的形态）。
func TestChatDeveloperFirstNotDoubleWrapped(t *testing.T) {
	var bodies [][]byte
	up := newBodyCaptureUpstream(t, &bodies, func(call int, body string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := postChat(h, `{"model":"glm-5.2","stream":true,"messages":[
		{"role":"developer","content":"工具约定"},
		{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	roles := outboundRoles(t, bodies[0])
	if len(roles) != 2 || roles[0] != "system" || roles[1] != "user" {
		t.Fatalf("roles=%v want [system user]（developer 归一后不应再补）: %s", roles, bodies[0])
	}
}
