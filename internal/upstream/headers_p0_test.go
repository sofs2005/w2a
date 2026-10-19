package upstream

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestCommonHeadersCodeBuddyRequest X-CodeBuddy-Request 的归属边界：
//   - **chat 路径不发**——该头属于另一个客户端（CodeBuddy VS Code fork）。WorkBuddy AI
//     全量包里它只出现在本地会话回放（buildSessionReplayRequestHeaders）与 host 运行时
//     探测（probeHostRuntimeEndpoint），不在上游 /v2/chat/completions 路径。发出去就是
//     「我们发了、官方 chat 没发」的多余客户端指纹。
//   - billing / refresh **保留** "1"：账号类接口的既有闸门头，不属 chat 渠道校验面。
func TestCommonHeadersCodeBuddyRequest(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{}

	// chat 路径：不发（ChatHeaders 末尾 Del 兜底）
	req := mustRequest(t)
	c.ChatHeaders(req, a, "", ChatMeta{})
	if got := req.Header.Get("X-CodeBuddy-Request"); got != "" {
		t.Errorf("chat X-CodeBuddy-Request = %q want empty (官方 chat 不发该头)", got)
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
//   - X-Root-Request-ID：grep 计数 0；
//   - X-CodeBuddy-Request / X-Requested-With：属另一客户端或浏览器侧，chat 上游不带。
// 同时断言官方 axios 层强制的 Accept-Encoding 取值。
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
		// 另一客户端（CodeBuddy）与浏览器侧的闸门头：官方 chat 出站不带。
		"X-CodeBuddy-Request", "X-Requested-With",
	} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("%s = %q want empty (官方 chat 不发)", h, got)
		}
	}
	if got := req.Header.Get("Accept-Encoding"); got != chatAcceptEncoding {
		t.Errorf("Accept-Encoding = %q want %q", got, chatAcceptEncoding)
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

// TestChatHeadersStainlessFingerprint 官方 chat 走 OpenAI Node SDK，SDK 的 buildHeaders
// 无条件拼入 X-Stainless-* 族（仅 CODEBUDDY_SKIP_INTERNAL_HEADERS 为真时才被
// axiosToFetchAdapter 按 `x-stainless-` 前缀剥除，该 env 默认不设）。取证：
// resources/app.asar.unpacked/cli/dist/lazy-lite-wb/3043.50ee4dca.mjs。
//
// 反例同样重要：OpenAI-Organization / OpenAI-Project 构造时是 null，SDK 在
// buildHeaders 里 delete，官方实际上线不发 → 我们发出去就是多出项。
func TestChatHeadersStainlessFingerprint(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := mustRequest(t)
	c.ChatHeaders(req, a, "", ChatMeta{})

	want := map[string]string{
		"X-Stainless-Retry-Count":     "0",      // maxRetries: 0
		"X-Stainless-Timeout":         "1200",   // timeout 1.2e6 ms ÷ 1000
		"X-Stainless-Lang":            "js",     // 常量
		"X-Stainless-Package-Version": "6.25.0", // openai@6.x 的 SDK 版本串
		"X-Stainless-OS":              "Windows",
		"X-Stainless-Arch":            "x64",
		"X-Stainless-Runtime":         "node",
	}
	for h, v := range want {
		if got := req.Header.Get(h); got != v {
			t.Errorf("%s = %q want %q", h, got, v)
		}
	}
	if got := req.Header.Get("X-Stainless-Runtime-Version"); got != defaultStainlessRuntimeVersion {
		t.Errorf("X-Stainless-Runtime-Version = %q want %q", got, defaultStainlessRuntimeVersion)
	}

	// 官方不发这两个（构造参数 null → SDK delete）。
	for _, h := range []string{"OpenAI-Organization", "OpenAI-Project"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("%s = %q want empty（官方 buildHeaders 对 null 值 delete）", h, got)
		}
	}
}

// TestStainlessOverrides Client 配置覆盖两个 stainless 字段（线上校准口，
// 不改码重编即可把 X-Stainless-Runtime-Version 调到真实桌面端 process.version）。
func TestStainlessOverrides(t *testing.T) {
	c := &Client{StainlessRuntimeVersion: "v20.18.1", StainlessTimeoutSeconds: "600"}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := mustRequest(t)
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-Stainless-Runtime-Version"); got != "v20.18.1" {
		t.Errorf("X-Stainless-Runtime-Version = %q want %q", got, "v20.18.1")
	}
	if got := req.Header.Get("X-Stainless-Timeout"); got != "600" {
		t.Errorf("X-Stainless-Timeout = %q want %q", got, "600")
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
