package upstream

import (
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
)

// globalUAString global realm 默认出站 UA。第二段「应用名」取自官方 product.json 的
// applicationName：CN=`WorkBuddy`、global=`workbuddy-ai`（两域不同值），platform 段由
// sidecar-manager.js 硬编码 `WorkBuddy`，CLI 段两域同为 2.161.0。
const globalUAString = "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0"

// TestChatHeadersGlobalRealm global 账号的 chat 请求头对齐官方国际客户端形态：
//  1. UA 为 `WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0`（应用名随 realm，版本随 realm）；
//  2. X-Domain: www.workbuddy.ai（显式声明国际版域）；
//  3. 不发 X-No-Enterprise-Id / X-No-Department-Info / Origin / Referer
//     （官方全量分发包 grep 计数 0 或明确排除）。
func TestChatHeadersGlobalRealm(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"} // global 账号
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("User-Agent"); got != globalUAString {
		t.Errorf("global UA = %q want %q", got, globalUAString)
	}
	for _, h := range []string{"X-No-Enterprise-Id", "X-No-Department-Info", "Origin", "Referer", "Accept-Language"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("global %s = %q want empty (官方 chat 不发该头)", h, got)
		}
	}
	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Errorf("global X-Domain = %q want %q", got, "www.workbuddy.ai")
	}
	// global 账号不落 CN 侧的 X-Enterprise-Id（企业头无意义）。
	if got := req.Header.Get("X-Enterprise-Id"); got != "" {
		t.Errorf("global X-Enterprise-Id = %q want empty", got)
	}
}

// TestChatHeadersCNRealm CN 账号头对齐：UA 两段都是 `WorkBuddy`、不注入 global 专属
// X-Domain: www.workbuddy.ai；EnterpriseID 非空照常透传 X-Enterprise-Id，Domain 为空
// 不再补 X-No-Department-Info（官方不发缺省头）。
func TestChatHeadersCNRealm(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "c1", EnterpriseID: "e1"} // CN 企业账号 + 无 domain
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("User-Agent"); got != defaultUAString {
		t.Errorf("CN UA = %q want %q", got, defaultUAString)
	}
	if got := req.Header.Get("X-Enterprise-Id"); got != "e1" {
		t.Errorf("CN X-Enterprise-Id = %q want e1", got)
	}
	for _, h := range []string{"X-No-Enterprise-Id", "X-No-Department-Info", "X-Domain"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("CN %s = %q want empty", h, got)
		}
	}
}

// TestChatHeadersGlobalStrongOverride global 账号（domain 标记 global）即便带
// EnterpriseID（异常数据）也发国际客户端形态头：X-Domain=www.workbuddy.ai、
// 不发 X-No-Enterprise-Id，不落 CN 侧企业头（覆盖注入顺序：global 分支先于
// 既有分支的强覆盖语义）。
func TestChatHeadersGlobalStrongOverride(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "g2", EnterpriseID: "should-be-ignored", Domain: "www.workbuddy.ai"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-No-Enterprise-Id"); got != "" {
		t.Errorf("global(virtual enterprise) X-No-Enterprise-Id = %q want empty", got)
	}
	if got := req.Header.Get("X-Enterprise-Id"); got != "" {
		t.Errorf("global(virtual enterprise) X-Enterprise-Id = %q want empty", got)
	}
	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Errorf("global(virtual enterprise) X-Domain = %q want www.workbuddy.ai", got)
	}
	if got := req.Header.Get("X-No-Department-Info"); got != "" {
		t.Errorf("global(virtual enterprise) X-No-Department-Info = %q want empty (X-Domain supersedes)", got)
	}
}

// TestChatHeadersGlobalWithConfiguredDomain global 账号即便显式配了登录 domain，
// chat 出站 X-Domain 也固定为 www.workbuddy.ai（国际客户端形态，不回退到会话原值）。
func TestChatHeadersGlobalWithConfiguredDomain(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "g3", Domain: "login.workbuddy.ai"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Errorf("global(with custom domain) X-Domain = %q want www.workbuddy.ai", got)
	}
}

// TestChatHeadersCNNoEnterpriseZeroRegression CN 无企业/无 domain 的老形态账号：
// 两个缺省声明头都不发（官方 grep 计数 0），也不注入 global 的 X-Domain。
func TestChatHeadersCNNoEnterpriseZeroRegression(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "c2"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	for _, h := range []string{"X-No-Enterprise-Id", "X-No-Department-Info", "X-Domain"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("CN(no enterprise) %s = %q want empty", h, got)
		}
	}
}

// TestDefaultWorkBuddyUAForGlobal UA 单元级断言：第二段「应用名」随 realm 切
// （官方 product.json applicationName：CN=`WorkBuddy` / global=`workbuddy-ai`），
// 版本段同样随 realm（CN 5.8.3 / global 5.8.4）。
func TestDefaultWorkBuddyUAForGlobal(t *testing.T) {
	c := &Client{}
	if got := c.defaultWorkBuddyUAFor(&auth.Auth{}); got != defaultUAString {
		t.Errorf("defaultWorkBuddyUAFor(cn) = %q want %q", got, defaultUAString)
	}
	if got := c.defaultWorkBuddyUAFor(&auth.Auth{Domain: "www.workbuddy.ai"}); got != globalUAString {
		t.Errorf("defaultWorkBuddyUAFor(global) = %q want %q", got, globalUAString)
	}
	// version 覆盖仍生效：两段跟随 clientVersion，CLI 段保持默认。
	c2 := &Client{ClientVersion: "6.0.0"}
	if got := c2.defaultWorkBuddyUAFor(&auth.Auth{Domain: "www.workbuddy.ai"}); got != "WorkBuddy/6.0.0 workbuddy-ai/6.0.0 CLI/2.161.0" {
		t.Errorf("defaultWorkBuddyUAFor(global, v6) = %q", got)
	}
	// CN 侧应用名恒 WorkBuddy。
	if got := c.defaultWorkBuddyUAFor(&auth.Auth{Domain: "copilot.tencent.com"}); got != "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0" {
		t.Errorf("defaultWorkBuddyUAFor(cn domain) = %q want 5.8.3", got)
	}
}

// mustRequest 构造一个无 body 的 POST 请求（测试 ChatHeaders 用）。
func mustRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://chat.example/console/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}