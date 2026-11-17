package upstream

import (
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestChatHeadersAPIKey API Key 账号的出站形态：Bearer 与 X-API-Key 都是 key，
// 且不带 X-Domain（key 会话没有登录域）。
func TestChatHeadersAPIKey(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{
		AccessToken: "ck_abcdefgh12345678",
		APIKey:      "ck_abcdefgh12345678",
		UID:         "anonymous_12345678",
		Domain:      "copilot.tencent.com",
	}
	req := httptest.NewRequest("POST", "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-API-Key"); got != "ck_abcdefgh12345678" {
		t.Errorf("X-API-Key=%q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer ck_abcdefgh12345678" {
		t.Errorf("Authorization=%q", got)
	}
	if got := req.Header.Get("X-Domain"); got != "" {
		t.Errorf("key 账号不该带 X-Domain，got %q", got)
	}
	if got := req.Header.Get("X-User-Id"); got != "anonymous_12345678" {
		t.Errorf("X-User-Id=%q", got)
	}
}

// TestChatHeadersOAuthUnchanged 普通账号不带 X-API-Key，X-Domain 照旧。
func TestChatHeadersOAuthUnchanged(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "copilot.tencent.com"}
	req := httptest.NewRequest("POST", "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})
	if got := req.Header.Get("X-API-Key"); got != "" {
		t.Errorf("OAuth 账号不该带 X-API-Key，got %q", got)
	}
	if got := req.Header.Get("X-Domain"); got != "copilot.tencent.com" {
		t.Errorf("X-Domain=%q", got)
	}
}
