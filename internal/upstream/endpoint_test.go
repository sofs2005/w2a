package upstream

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// TestCloudHostedEndpoint 云托管企业域 → 该域主机；iOA / 普通域 / global 不路由。
func TestCloudHostedEndpoint(t *testing.T) {
	cases := []struct{ domain, want string }{
		{"acme.sso.copilot.tencent.com", "https://acme.sso.copilot.tencent.com"},
		{"acme.copilot.qq.com", "https://acme.copilot.qq.com"},
		{"acme.sso.codebuddy.cn", "https://acme.sso.codebuddy.cn"},
		{"acme.sso.copilot-staging.tencent.com", "https://acme.sso.copilot-staging.tencent.com"},
		{"acme.staging-sso.codebuddy.cn", "https://acme.staging-sso.codebuddy.cn"},
		// iOA 域仍是 copilot.tencent.com
		{"tencent.sso.copilot.tencent.com", ""},
		{"tencent.sso.codebuddy.cn", ""},
		// 普通域
		{"copilot.tencent.com", ""},
		{"", ""},
	}
	for _, c := range cases {
		a := &auth.Auth{Domain: c.domain}
		if got := cloudHostedEndpoint(a); got != c.want {
			t.Errorf("domain=%q got %q want %q", c.domain, got, c.want)
		}
	}
	// global 账号不参与（其域是 www.workbuddy.ai，也不在表里）。
	g := &auth.Auth{Domain: "www.workbuddy.ai"}
	if got := cloudHostedEndpoint(g); got != "" {
		t.Errorf("global 账号不该路由: %q", got)
	}
}

// TestChatBaseCloudHosted chatBase/billingBase 都切到企业域；普通账号保持不变。
func TestChatBaseCloudHosted(t *testing.T) {
	c := &Client{ChatBaseCN: "https://copilot.tencent.com", BillingBaseCN: "https://www.codebuddy.cn"}
	a := &auth.Auth{Domain: "acme.sso.copilot.tencent.com"}
	if got := c.chatBase(a); got != "https://acme.sso.copilot.tencent.com" {
		t.Errorf("chatBase=%q", got)
	}
	if got := c.billingBase(a); got != "https://acme.sso.copilot.tencent.com" {
		t.Errorf("billingBase=%q", got)
	}
	normal := &auth.Auth{Domain: "copilot.tencent.com"}
	if got := c.chatBase(normal); got != "https://copilot.tencent.com" {
		t.Errorf("普通账号 chatBase=%q", got)
	}
}
