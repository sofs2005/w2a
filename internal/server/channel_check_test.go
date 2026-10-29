package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/upstream"
)

// 渠道校验的处置：**换路径兜底 + 绝不禁用**（2026-10-17 用户报告）。
//
// 现场：上游对 /v2/chat/completions 新增渠道强校验，被校验到的号在面板上显示
// 「已禁用（需重新登录）」—— 用户以为凭证坏了去重扫，扫完还是被禁。实际是凭封套里
// 那句历史遗留的 `request illegal` 落进了「账号被封 → 硬禁用」，而账号本身是好的。
// 官方客户端走的 /console 端点并未加强校验（报告方实测：换过去就通了）。

// channelCheckBody 一次渠道校验的封套。业务码拼接构造，避免手抄整串出错
// （源码里两种码形态只在 upstream.hitAntiProbe 维护，测试也不手写）。
func channelCheckBody(code string) string {
	return `{"code":` + code + `,"msg":"request illegal","requestId":"x",` +
		`"displayTips":{"zh":"这次调用没通过渠道校验，可能不是官方客户端发出的。",` +
		`"en":"This call failed channel verification, possibly because it was not sent from an official client."}}`
}

// testAntiProbeCode 上游线上实测形态：五位裸数字。码点拼接（"11" + "128"），
// 避免渲染/编辑链路把字面量改写。
func testAntiProbeCode() string {
	return "11" + "1" + "2" + "8"
}

func sseResponse(status int, body string) *http.Response {
	ct := "application/json"
	if status == 200 {
		ct = "text/event-stream"
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{ct}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestChatFallsBackToConsoleOnChannelCheck(t *testing.T) {
	var paths []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.Path)
			if strings.Contains(r.URL.Path, "/v2/chat/completions") {
				return sseResponse(400, channelCheckBody(testAntiProbeCode())), nil
			}
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
		t.Fatalf("应经 /console 兜底成功，got code=%d body=%s", rec.Code, rec.Body.String())
	}
	var sawV2, sawConsole bool
	for _, p := range paths {
		sawV2 = sawV2 || strings.Contains(p, "/v2/chat/completions")
		sawConsole = sawConsole || strings.Contains(p, "/console/chat/completions")
	}
	if !sawV2 || !sawConsole {
		t.Fatalf("应先在 /v2 被拒再换 /console，实际路径=%v", paths)
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Error("换路径已经成功，账号不该被禁用")
	}
	if st.Cooling {
		t.Error("换路径已经成功，账号不该被冷却")
	}
}

func TestChatChannelCheckNeverDisables(t *testing.T) {
	// 两条路径都被渠道校验拒：软冷却（退出轮换、别被反复选中），但**不禁用** ——
	// 禁用会写「需重新登录」，把用户指向错误的排查方向（凭证是好的）。
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return sseResponse(400, channelCheckBody(testAntiProbeCode())), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("渠道校验**绝不能**禁用账号（用户报告的症状）：reason=%q", st.DisabledReason)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("两条路径都被拒时应软冷却让它退出轮换：%+v", st)
	}
	if !strings.Contains(st.Reason, "channel check") {
		t.Errorf("冷却原因应写明是渠道校验，便于排障：%q", st.Reason)
	}
}

// TestApplyErrorPolicyConversationIDMissingCools 「conversation id missing」
// 按**账号级软冷却**处理（issue #193 / #201 缺陷 2）。
//
// 上游对部分账号灰度启用了渠道校验：出站缺 X-Conversation-ID 头族即被判。
// 网关已在出站补会话头族（session.ConversationIDForHeaders），仍撞上说明该账号被
// 灰度到了别的口径 —— 这时**不能**归到通用 4xx「只换号不罚号」（坏号留在池里
// "显示健康、每次被选中都失败"），也不硬禁用（灰度随时可能放开）。
func TestApplyErrorPolicyConversationIDMissingCools(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	// 上游实测形态：HTTP 400 + 裸五位业务码（非法 JSON）。
	// 理由里的码必须照实取到 —— 这条用例同时钉住 CodeOf 对非法 JSON 的容忍。
	body := `{"code":` + testAntiProbeCode() + `,"msg":"conversation id missing","requestId":"x","displayMsg":{"zh":"请求被拦截，请重新发送"}}`
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "glm-5.2",
		&upstream.Error{Kind: upstream.ErrAccountFault, Status: 400, Msg: "conversation id missing"})
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("渠道校验不应硬禁用（灰度可能放开，禁用等于让用户白重登）: %+v", st)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("应 soft_rate 软冷却（挪出池子但可自愈）: %+v", st)
	}
	if !strings.Contains(st.Reason, testAntiProbeCode()) {
		t.Errorf("冷却理由应带真实码，否则排障按错码去搜: %q", st.Reason)
	}
	if strings.Contains(st.Reason, "11140") {
		t.Errorf("理由里不该出现写死的 11140（issue #201 缺陷 1）: %q", st.Reason)
	}
}

// TestChatNoConsoleFallbackOnNonChannelCheck 404/405 不回落（#119 取舍）：
// /console 挂 WAF 内容规则，不能因为路径写错就整批改道。
func TestChatNoConsoleFallbackOnNonChannelCheck(t *testing.T) {
	var paths []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.Path)
			return sseResponse(404, `{"code":404,"msg":"not found"}`), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	for _, p := range paths {
		if strings.Contains(p, "/console/chat/completions") {
			t.Fatalf("非渠道校验的 404 不得回落 /console，实际路径=%v", paths)
		}
	}
}

// 11-128 按 system 内容判定：合法系统提示词不该把账号拖下水。
//
// 同一请求体先被渠道校验拦下（/v2 与 /console 都拒），handler 把 system 文本
// 搬进首条 user 后重试成功；账号不冷却、不禁用。
func TestChatChannelCheckMovesSystemIntoUserAndRetries(t *testing.T) {
	var bodies []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(b))
			if systemHasPersona(b) {
				return sseResponse(400, channelCheckBody(testAntiProbeCode())), nil
			}
			return sseResponse(200, sseOK), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough", SoftCooldown: 600 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[`+
			`{"role":"system","content":"PERSONA-SAFE"},`+
			`{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("system 文本搬进 user 后应成功，got code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(bodies) < 2 {
		t.Fatalf("应至少重试一次，实际上游请求 %d 次", len(bodies))
	}
	last := bodies[len(bodies)-1]
	if systemHasPersona([]byte(last)) {
		t.Fatalf("重试请求的 system 里仍有原 persona 文本，未搬迁: %s", last)
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(last), &req); err != nil {
		t.Fatalf("重试请求不是合法 JSON: %v (%s)", err, last)
	}
	if len(req.Messages) < 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != prompt.Degraded {
		t.Fatalf("重试请求的 system 应是中性文案（%q），got %+v", prompt.Degraded, req.Messages)
	}
	// system 文本必须一字不落地搬进首条 user（设定先于用户输入）。
	if req.Messages[1].Role != "user" ||
		req.Messages[1].Content != "PERSONA-SAFE\n\nhi" {
		t.Fatalf("原 system 文本应在 user 消息里且原 user 内容在尾部，got %+v", req.Messages[1])
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled {
		t.Fatalf("内容问题不该罚账号：cooling=%v disabled=%v", st.Cooling, st.Disabled)
	}
}

// TestChatChannelCheckMoveSkippedInCustomMode custom 模式下 system 是网关自己的
// 提示词，搬迁没有意义 —— 不做请求级抢救，按既有渠道校验策略处理（软冷却、不禁用）。
func TestChatChannelCheckMoveSkippedInCustomMode(t *testing.T) {
	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return sseResponse(400, channelCheckBody(testAntiProbeCode())), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool: p, Upstream: up, PromptMode: "custom", PromptText: "GATEWAY-PROMPT",
		SoftCooldown: 600 * time.Second,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[`+
			`{"role":"system","content":"PERSONA-SAFE"},`+
			`{"role":"user","content":"hi"}]}`)))
	if rec.Code == 200 {
		t.Fatalf("custom 模式不做搬迁抢救，应失败，got 200 body=%s", rec.Body.String())
	}
	// /v2 + /console 两条路径，没有额外的搬迁重试。
	if calls != 2 {
		t.Fatalf("custom 模式应只有两条路径的上游调用，实际 %d 次", calls)
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatal("渠道校验绝不能禁用账号")
	}
	if !st.Cooling {
		t.Fatal("两条路径都被渠道校验拒时应软冷却（既有策略不变）")
	}
}

// systemHasPersona 判断请求体里是否还有「原始 system 内容」（persona 标记仍在
// system 通道）。出站前 client 会 normalize 重排消息字段，所以按 JSON 解析判断，
// 不依赖字段顺序。
func systemHasPersona(body []byte) bool {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	for _, m := range req.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "PERSONA") {
			return true
		}
	}
	return false
}
