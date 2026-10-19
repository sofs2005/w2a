package upstream

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestCommonHeadersCodeBuddyRequest 所有出站请求统一注入
// X-CodeBuddy-Request: 1（官方客户端风控闸门头，D1）。
// chat/billing/refresh 三类出站均覆盖。
func TestCommonHeadersCodeBuddyRequest(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{}

	// chat 路径
	req := mustRequest(t)
	c.ChatHeaders(req, a, "", ChatMeta{})
	if got := req.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("chat X-CodeBuddy-Request = %q want %q", got, "1")
	}

	// billing 路径
	req2 := mustRequest(t)
	c.BillingHeaders(req2, a)
	if got := req2.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("billing X-CodeBuddy-Request = %q want %q", got, "1")
	}

	// refresh 路径
	req3 := mustRequest(t)
	c.RefreshHeaders(req3, a)
	if got := req3.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("refresh X-CodeBuddy-Request = %q want %q", got, "1")
	}
}

// TestRefreshHeadersAuthRefreshSource refresh 出站 X-Auth-Refresh-Source 对齐
// 官方客户端 refresh 渠道标识为 "plugin"（D3，原值 "workbuddy" 与官方不一致）。
func TestRefreshHeadersAuthRefreshSource(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}
	req := mustRequest(t)
	c := &Client{}
	c.RefreshHeaders(req, a)

	if got := req.Header.Get("X-Auth-Refresh-Source"); got != "plugin" {
		t.Errorf("X-Auth-Refresh-Source = %q want %q", got, "plugin")
	}
}

// TestCommonHeadersAcceptLanguageByRealm Accept-Language 只在 refresh（走 CommonHeaders）
// 按 realm 切（D5）：CN 账号 zh-CN，global 账号 en-US。
// chat 路径不继承：官方 Vlm 上游从不发 Accept-Language（全量分发包计数 0），
// ChatHeaders 末尾统一 Del（见 TestChatHeadersDropsUnofficialHeaders）。
func TestCommonHeadersAcceptLanguageByRealm(t *testing.T) {
	c := &Client{}

	// CN 账号 chat：不注入。
	cnA := &auth.Auth{AccessToken: "at", UID: "c1"}
	req := mustRequest(t)
	c.ChatHeaders(req, cnA, "", ChatMeta{})
	if got := req.Header.Get("Accept-Language"); got != "" {
		t.Errorf("CN chat Accept-Language = %q want empty (官方 chat 不发)", got)
	}

	// global 账号 chat：不注入。
	glA := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	req2 := mustRequest(t)
	c.ChatHeaders(req2, glA, "", ChatMeta{})
	if got := req2.Header.Get("Accept-Language"); got != "" {
		t.Errorf("global chat Accept-Language = %q want empty (官方 chat 不发)", got)
	}

	// CN 账号 refresh
	req3 := mustRequest(t)
	c.RefreshHeaders(req3, cnA)
	if got := req3.Header.Get("Accept-Language"); got != "zh-CN" {
		t.Errorf("CN refresh Accept-Language = %q want %q", got, "zh-CN")
	}

	// global 账号 refresh
	req4 := mustRequest(t)
	c.RefreshHeaders(req4, glA)
	if got := req4.Header.Get("Accept-Language"); got != "en-US" {
		t.Errorf("global refresh Accept-Language = %q want %q", got, "en-US")
	}
}

// TestChatHeadersDropsUnofficialHeaders chat 出站不得携带官方 Vlm.send 不发/明确
// 排除的头。判据全部来自官方分发包（CN 5.8.3 / global 5.8.4 的 main/server.js
// 逐字节相同）：
//   - Origin：Vlm 的排除集 qN 显式含 `origin`；
//   - Referer / Accept-Language / X-No-Enterprise-Id / X-No-Department-Info /
//     X-Device-Token：grep 计数 0 或只属于 checkin/auth 路径；
//   - X-Root-Request-ID：grep 计数 0。
// 同时断言官方强制的 `Accept-Encoding: identity`。
func TestChatHeadersDropsUnofficialHeaders(t *testing.T) {
	c := &Client{}
	// DeviceToken 三个来源都填上，确保 Del 真的发生在注入之后。
	c.DeviceToken = "cfg-token"
	a := &auth.Auth{AccessToken: "at", UID: "c1", DeviceToken: "auth-token"}
	req := mustRequest(t)
	c.ChatHeaders(req, a, "", ChatMeta{})

	for _, h := range []string{
		"Origin", "Referer", "Accept-Language",
		"X-No-Enterprise-Id", "X-No-Department-Info",
		"X-No-Authorization", "X-No-User-Id",
		"X-Device-Token", "X-Root-Request-ID",
	} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("%s = %q want empty (官方 chat 不发)", h, got)
		}
	}
	if got := req.Header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding = %q want identity", got)
	}

	// 无凭据/无 UID 的账号也不发 X-No-* 声明头（官方没有这些头，缺省即不发）。
	empty := mustRequest(t)
	c.ChatHeaders(empty, &auth.Auth{}, "", ChatMeta{})
	for _, h := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if got := empty.Header.Get(h); got != "" {
			t.Errorf("empty-acct %s = %q want empty", h, got)
		}
	}
}

// TestAcceptHeaderStreamVsNonStream Accept 头全路径统一 application/json：
// 官方 chat 上游（VLM 网关代理 applyGatewayHeaders）硬编码 `Accept: application/json`，
// 流式由 body 的 stream:true 决定；billing/refresh 同样仅 application/json。
func TestAcceptHeaderStreamVsNonStream(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}

	// chat：application/json（不追加 text/event-stream）。
	chatReq := mustRequest(t)
	c.ChatHeaders(chatReq, a, "", ChatMeta{})
	if got := chatReq.Header.Get("Accept"); got != "application/json" {
		t.Errorf("chat Accept = %q want %q", got, "application/json")
	}

	// billing 非流式：application/json
	bReq := mustRequest(t)
	c.BillingHeaders(bReq, a)
	if got := bReq.Header.Get("Accept"); got != "application/json" {
		t.Errorf("billing Accept = %q want %q", got, "application/json")
	}

	// refresh 非流式：application/json
	rReq := mustRequest(t)
	c.RefreshHeaders(rReq, a)
	if got := rReq.Header.Get("Accept"); got != "application/json" {
		t.Errorf("refresh Accept = %q want %q", got, "application/json")
	}
}
