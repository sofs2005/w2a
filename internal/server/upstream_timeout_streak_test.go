// upstream_timeout_streak_test.go 端到端钉住「上游超时止损」的四项修复（线上故障复盘）：
//
//  1. 单次超时保持原设计（150af36）：不轮转、不罚号、**粘性绑定保留**，末端给出
//     可区分的 upstream_timeout + 超时专用 gateway_hint（不再自相矛盾地说
//     "no healthy account available in pool"）；
//  2. 同一账号**连续**超时达阈：临时出池 + **解绑粘性**——否则会话被钉死在一个
//     已出池的号上，客户端重试永远命中同一个慢号（超时 → 503 → 重试 → 再等满一个
//     header_timeout 的死循环，线上实测形态）；
//  3. 成功后计数清零：一次成功即证明该号不慢，出池门槛重新累计；
//  4. 第 3 个请求落到另一个账号（这是「死循环被打断」的最终验收点）。
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
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// gatewayHintOf 解出 error.gateway_hint（不存在时返回空串，便于断言「不含」）。
func gatewayHintOf(t *testing.T, body string) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
			Hint string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("not openai error json: %v body=%s", err, body)
	}
	return envelope.Error.Hint
}

// newSlowUpstream 返回一个按 Authorization 分流的 fake 上游：slowToken 一律超时
// （模拟挂起账号），其余账号正常回 SSE。callsSlow/callsFast 记录各打了几次，
// 供「不轮转」「落到另一个号」两类断言使用。
func newSlowUpstream(t *testing.T, slowToken string, callsSlow, callsFast *int) *upstream.Client {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") == "Bearer "+slowToken {
			*callsSlow++
			return nil, timeoutErr{true}
		}
		*callsFast++
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})}
	return up
}

// chatReqWithConv 构造带会话标识的 chat 请求（粘性键走 conversation_id）。
func chatReqWithConv(convID string) *http.Request {
	return httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"`+convID+`"}}`))
}

// TestChatUpstreamTimeoutSingleKeepsStickyAndTimeoutHint 单次超时：
// 只打一次上游、不罚号、**粘性绑定保留**，且 gateway_hint 是超时专用文案
// （不得再出现 no healthy account available in pool —— 那会把运维指向池子）。
func TestChatUpstreamTimeoutSingleKeepsStickyAndTimeoutHint(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"slow", "fast"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "slow", AccessToken: "at-slow", ExpiresAt: 9999999999},
		&auth.Auth{UID: "fast", AccessToken: "at-fast", ExpiresAt: 9999999999},
	)
	p.SetTimeout(2, time.Minute) // 阈值 2：单次超时不出池
	var callsSlow, callsFast int
	up := newSlowUpstream(t, "at-slow", &callsSlow, &callsFast)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, MaxRotate: 3})

	sess.Bind("conv-1", "slow") // 模拟历史粘性：会话已钉在 slow 上
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-1"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "upstream_timeout") {
		t.Errorf("超时必须给出可区分的 code: %s", rec.Body)
	}
	// 止损：只打一次上游（MaxRotate=3 也不许轮转）。
	if callsSlow != 1 || callsFast != 0 {
		t.Fatalf("单次超时必须止损，slow=%d fast=%d（want 1/0）", callsSlow, callsFast)
	}
	// 单次超时不罚号：连败计数为 0、连续超时计数为 1、账号仍可选。
	stSlow, _ := p.Status("slow")
	if stSlow.ConsecutiveFails != 0 {
		t.Errorf("超时不得喂连败计数: %d", stSlow.ConsecutiveFails)
	}
	if stSlow.ConsecutiveTimeouts != 1 {
		t.Errorf("连续超时计数应为 1, got %d", stSlow.ConsecutiveTimeouts)
	}
	if stSlow.Cooling {
		t.Error("单次超时不得让账号出池")
	}
	if uids := p.AvailableUIDs(); len(uids) != 2 {
		t.Errorf("单次超时后两个号都应可选, got %v", uids)
	}
	// 关键：粘性绑定必须保留（单次超时不解绑——该号下一轮仍可能是对的）。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "slow" {
		t.Errorf("单次超时不得解绑粘性: binding=%s ok=%v", uid, ok)
	}
	if st.delCnt != 0 {
		t.Errorf("单次超时不得产生 Unbind 调用, delCnt=%d", st.delCnt)
	}
	// hint 必须与 message 同向：指向「上游慢/网络」，而不是「池里没号」。
	hint := gatewayHintOf(t, rec.Body.String())
	if hint == "" {
		t.Fatal("超时响应必须带 gateway_hint")
	}
	if hint != upstream.UpstreamTimeoutHint() {
		t.Errorf("gateway_hint 应为超时专用文案, got %q", hint)
	}
	if strings.Contains(hint, "no healthy account available in pool") {
		t.Errorf("超时 hint 不得退回池子文案（自相矛盾）: %q", hint)
	}
}

// TestChatUpstreamTimeoutStreakCoolsAndUnbindsSticky 连续两次超时（同一账号）：
// 该号临时出池 + 粘性解绑；第 3 个请求落到另一个账号。
func TestChatUpstreamTimeoutStreakCoolsAndUnbindsSticky(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"slow", "fast"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "slow", AccessToken: "at-slow", ExpiresAt: 9999999999},
		&auth.Auth{UID: "fast", AccessToken: "at-fast", ExpiresAt: 9999999999},
	)
	p.SetTimeout(2, time.Minute)
	var callsSlow, callsFast int
	up := newSlowUpstream(t, "at-slow", &callsSlow, &callsFast)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, MaxRotate: 3})

	sess.Bind("conv-1", "slow")

	// 第 1 次：超时但未达阈 → 粘性保留、不出池。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("第 1 次 code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "slow" {
		t.Fatalf("第 1 次超时不得解绑粘性: binding=%s ok=%v", uid, ok)
	}

	// 第 2 次：同一会话仍命中 slow（粘性）→ 连续超时达阈 → 出池 + 解绑。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("第 2 次 code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if callsSlow != 2 {
		t.Fatalf("第 2 次仍应命中粘性号 slow（不轮转）, callsSlow=%d", callsSlow)
	}
	stSlow, _ := p.Status("slow")
	if !stSlow.Cooling || stSlow.CoolKind != "degrade" {
		t.Fatalf("连续超时达阈应临时出池: cooling=%v kind=%q", stSlow.Cooling, stSlow.CoolKind)
	}
	if stSlow.Reason != "consecutive upstream timeouts (slow account), temporarily out of pool" {
		t.Errorf("出池 reason 应为超时专用文案（与连败降权区分）, got %q", stSlow.Reason)
	}
	if stSlow.ConsecutiveTimeouts != 0 {
		t.Errorf("达阈后计数应清零供下一轮累计, got %d", stSlow.ConsecutiveTimeouts)
	}
	// 关键：粘性必须已解绑（否则会话被钉死在一个已出池的号上）。
	if uid, ok := st.lastUID("conv-1"); ok {
		t.Errorf("连续超时出池后粘性必须解绑, 仍绑定 %s", uid)
	}
	if uids := p.AvailableUIDs(); len(uids) != 1 || uids[0] != "fast" {
		t.Fatalf("出池后仅 fast 可用, got %v", uids)
	}

	// 第 3 次：粘性已解绑 → 普通轮换落到 fast，请求成功（死循环被打断）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-1"))
	if rec.Code != 200 {
		t.Fatalf("第 3 次应成功落到 fast, code=%d body=%s", rec.Code, rec.Body)
	}
	if callsFast != 1 {
		t.Fatalf("第 3 次应打到 fast 一次, callsFast=%d", callsFast)
	}
	if callsSlow != 2 {
		t.Fatalf("第 3 次不得再打 slow（已出池）, callsSlow=%d", callsSlow)
	}
	// 成功会重绑会话到 fast（既有语义）：下一跳不再随机抽。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "fast" {
		t.Errorf("成功后会话应重绑到 fast, got %s ok=%v", uid, ok)
	}
}

// TestChatUpstreamTimeoutStreakResetBySuccess 成功后计数清零：
// 超时 1 次 → 成功 → 再超时 1 次，仍不出池（不得凭空凑满阈值）。
func TestChatUpstreamTimeoutStreakResetBySuccess(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
	p.SetTimeout(2, time.Minute)
	// 按调用序号决定行为：1=超时、2=成功、3=超时。
	var call int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		call++
		if call == 2 {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		}
		return nil, timeoutErr{true}
	})}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})

	// 第 1 次：超时，计数 1。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-x"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("第 1 次 code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if st, _ := p.Status("solo"); st.ConsecutiveTimeouts != 1 {
		t.Fatalf("第 1 次超时后计数应为 1, got %d", st.ConsecutiveTimeouts)
	}

	// 第 2 次：成功 → NoteSuccess 清零（成功是恢复的最强证据）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-x"))
	if rec.Code != 200 {
		t.Fatalf("第 2 次应成功, code=%d body=%s", rec.Code, rec.Body)
	}
	if st, _ := p.Status("solo"); st.ConsecutiveTimeouts != 0 {
		t.Fatalf("成功后连续超时计数应清零, got %d", st.ConsecutiveTimeouts)
	}

	// 第 3 次：再超时 1 次 —— 计数 1，仍不得出池。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-x"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("第 3 次 code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("solo")
	if st.ConsecutiveTimeouts != 1 {
		t.Errorf("第 3 次超时后计数应为 1, got %d", st.ConsecutiveTimeouts)
	}
	if st.Cooling {
		t.Error("成功清零后的单次超时不得出池（不得凭空凑满阈值）")
	}
	if uids := p.AvailableUIDs(); len(uids) != 1 {
		t.Errorf("账号应保持可选, got %v", uids)
	}
}
