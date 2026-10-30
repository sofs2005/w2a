// gateway_network_test.go 端到端钉住「网关侧网络错误（DNS/Dial/TLS）」这条新分支。
//
// 线上实证（本次修复的起点）：上一轮把「连续超时 → 出池 + 解绑粘性」上线后，用户仍
// 收到 code=upstream_timeout，而日志里 4/4 次都不是上游慢，而是**容器内 DNS 解析
// 超时**（`lookup www.workbuddy.ai on 127.0.0.11:53: i/o timeout`）。三个问题：
//
//  1. 判据太粗（net.Error.Timeout() 一刀切）→ 报文说 "another account would hit the
//     same slow upstream"，把运维往上游方向带（真实原因是网关侧解析）；
//  2. 这类失败被喂进 NoteTimeout → **账号被误冷却**（与它毫无关系）；
//  3. 没有任何重试 → DNS 抖动本是毫秒级（事故当时宿主对三个上游 DNS 各 100/100
//     成功、p95=1ms），同号重试一次几乎必成，却白等 5~10s 再报 503。
//
// 本文件的用例一一对应这三点：重试次数、零惩罚、粘性保留、502 + 专用 hint、
// 以及「header 阶段超时的行为一字不变」（不能被新分类吃掉）。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// dnsTimeoutErr 线上实证形态的 DNS 解析超时：*net.DNSError 包在
// url.Error → net.OpError{dial} 里（与真实日志逐字同构）。
func dnsTimeoutErr() error {
	return &url.Error{
		Op:  "Post",
		URL: "https://www.workbuddy.ai/v2/chat/completions",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{
			Err:         "i/o timeout",
			Name:        "www.workbuddy.ai",
			Server:      "127.0.0.11:53",
			IsTimeout:   true,
			IsTemporary: true,
		}},
	}
}

// dialRefusedErr 拨号被拒（*net.OpError{Op:"dial"} + ECONNREFUSED）。
func dialRefusedErr() error {
	return &url.Error{
		Op:  "Post",
		URL: "https://www.workbuddy.ai/v2/chat/completions",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
	}
}

// connectFailUpstream 返回一个 ChatHTTP 走自定义 RoundTripper 的 fake 上游：
// 失败形态由 fail 决定（nil = 直接成功），calls 记录真实打了几次上游。
func connectFailUpstream(t *testing.T, calls *int, fail func(call int) error) *upstream.Client {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*calls++
		if err := fail(*calls); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})}
	return up
}

// alwaysFail 恒返回同一个错误（模拟「解析器彻底坏了」）。
func alwaysFail(err error) func(int) error {
	return func(int) error { return err }
}

// errorEnvelope 解出错误响应的 code / message / gateway_hint。
func errorEnvelope(t *testing.T, body string) (code, msg, hint string) {
	t.Helper()
	var e hintEnvelope
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("not openai error json: %v body=%s", err, body)
	}
	h := ""
	if e.Error.GatewayHint != nil {
		h = *e.Error.GatewayHint
	}
	return e.Error.Code, e.Error.Message, h
}

// TestChatGatewayNetworkDNSRetriesThen502 DNS 超时（请求字节没发出去）：
//   - 同号重试 connect_retry 次（默认 2 → 共 3 次尝试），**不换号**；
//   - 账号零惩罚：consecutive_timeouts=0、consecutive_fails=0、不冷却、粘性保留；
//   - 末端 502 gateway_network_error，message 区分成因并保留原文；
//   - hint 是网关网络专用文案（不含 "no healthy account"、不含 "slow upstream"）。
func TestChatGatewayNetworkDNSRetriesThen502(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"solo"} },
	})
	p := testPoolWith(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
	var calls int
	up := connectFailUpstream(t, &calls, alwaysFail(dnsTimeoutErr()))
	h := NewHandler(Config{
		Pool: p, Upstream: up, Session: sess, MaxRotate: 3,
		ConnectRetry: 2, ConnectRetryBackoff: time.Millisecond,
	})
	sess.Bind("conv-dns", "solo")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-dns"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
	if calls != 3 {
		t.Fatalf("应同号重试 2 次（共 3 次尝试），实际 %d 次", calls)
	}
	code, msg, hint := errorEnvelope(t, rec.Body.String())
	if code != "gateway_network_error" {
		t.Errorf("code=%q want gateway_network_error（不得叫 upstream_timeout）", code)
	}
	if strings.Contains(rec.Body.String(), "upstream_timeout") {
		t.Errorf("网关网络错误不得混成 upstream_timeout: %s", rec.Body)
	}
	// message 必须区分成因，且保留原始错误文本（解析器地址/超时形态在原文里）。
	if !strings.Contains(msg, "dns resolution failed") {
		t.Errorf("message 应点明 DNS 解析失败: %q", msg)
	}
	if !strings.Contains(msg, "lookup www.workbuddy.ai") {
		t.Errorf("message 必须保留原始错误文本（排障唯一线索）: %q", msg)
	}
	if !strings.Contains(msg, "after 2 retries") {
		t.Errorf("message 应说明已重试几次: %q", msg)
	}
	// hint 必须与 message 同向，且与另外两条 hint 互不串味。
	if hint != upstream.GatewayNetworkHint() {
		t.Errorf("gateway_hint 应为网关网络专用文案, got %q", hint)
	}
	if strings.Contains(hint, "no healthy account") {
		t.Errorf("hint 不得退回池子文案: %q", hint)
	}
	if strings.Contains(hint, "slow upstream") {
		t.Errorf("hint 不得说成上游慢: %q", hint)
	}
	// 账号零惩罚：这类失败与账号无关。
	solo, _ := p.Status("solo")
	if solo.ConsecutiveTimeouts != 0 {
		t.Errorf("DNS 失败不得喂连续超时计数（线上被误冷却 60s 的就是它）: %d", solo.ConsecutiveTimeouts)
	}
	if solo.ConsecutiveFails != 0 {
		t.Errorf("DNS 失败不得喂连败计数: %d", solo.ConsecutiveFails)
	}
	if solo.Cooling {
		t.Errorf("DNS 失败不得让账号出池: kind=%q reason=%q", solo.CoolKind, solo.Reason)
	}
	// 粘性必须保留（该号下一轮仍可能是对的；解绑只会让会话重新随机分配）。
	if uid, ok := st.lastUID("conv-dns"); !ok || uid != "solo" {
		t.Errorf("网关网络错误不得解绑粘性: binding=%s ok=%v", uid, ok)
	}
	if st.delCnt != 0 {
		t.Errorf("网关网络错误不得产生 Unbind 调用, delCnt=%d", st.delCnt)
	}
}

// TestChatGatewayNetworkRetrySucceeds 第一次 DNS 失败、第二次成功：
// 客户端看到 200，账号零惩罚，且**没有 WARN 级错误文案**（自愈的抖动不该刷告警）。
func TestChatGatewayNetworkRetrySucceeds(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
	var calls int
	up := connectFailUpstream(t, &calls, func(call int) error {
		if call == 1 {
			return dnsTimeoutErr()
		}
		return nil
	})
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: 3,
		ConnectRetry: 2, ConnectRetryBackoff: time.Millisecond,
	})

	var rec *httptest.ResponseRecorder
	out := captureLog(func() {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, chatReqWithConv("conv-retry-ok"))
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("重试后应成功, code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 2 {
		t.Fatalf("应「失败 1 次 + 成功 1 次」共 2 次上游调用, got %d", calls)
	}
	solo, _ := p.Status("solo")
	if solo.ConsecutiveTimeouts != 0 || solo.ConsecutiveFails != 0 || solo.Cooling {
		t.Errorf("成功重试后账号必须零惩罚: timeouts=%d fails=%d cooling=%v",
			solo.ConsecutiveTimeouts, solo.ConsecutiveFails, solo.Cooling)
	}
	// 无 WARN 级错误文案（重试自愈的瞬时抖动只记 info 级）。
	if strings.Contains(out, "WARN: [server] gateway network error") {
		t.Errorf("自愈的重试不得打 WARN 级 gateway network error: %s", out)
	}
	if strings.Contains(out, "WARN: [server] upstream timeout") {
		t.Errorf("DNS 抖动不得被记成上游超时: %s", out)
	}
}

// TestChatHeaderTimeoutUnchanged header 阶段超时（请求已发出、等首字节超时）的行为
// 必须与上一轮**完全一致**：只打一次上游、报 upstream_timeout、单次不罚号、粘性保留。
// 这条是防回归闸门——新分类器不得把 header 超时也吞进「网关网络错误」。
func TestChatHeaderTimeoutUnchanged(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"slow"} },
	})
	p := testPoolWith(&auth.Auth{UID: "slow", AccessToken: "at-slow", ExpiresAt: 9999999999})
	p.SetTimeout(2, time.Minute) // 阈 2：单次超时不出池
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, timeoutErr{true} // net.Error 超时（header 阶段）
	})}
	h := NewHandler(Config{
		Pool: p, Upstream: up, Session: sess, MaxRotate: 3,
		ConnectRetry: 2, ConnectRetryBackoff: time.Millisecond, // 重试开着也不许作用到 header
	})
	sess.Bind("conv-header", "slow")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-header"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("header 超时必须止损（不重试、不轮转），实际打了 %d 次", calls)
	}
	code, _, hint := errorEnvelope(t, rec.Body.String())
	if code != "upstream_timeout" {
		t.Errorf("code=%q want upstream_timeout（header 阶段超时的既有口径）", code)
	}
	if hint != upstream.UpstreamTimeoutHint() {
		t.Errorf("hint 应为超时专用文案, got %q", hint)
	}
	slow, _ := p.Status("slow")
	if slow.ConsecutiveTimeouts != 1 {
		t.Errorf("单次 header 超时应计 1 次连续超时, got %d", slow.ConsecutiveTimeouts)
	}
	if slow.Cooling {
		t.Error("单次超时不得让账号出池（阈 2）")
	}
	if uid, ok := st.lastUID("conv-header"); !ok || uid != "slow" {
		t.Errorf("单次超时不得解绑粘性: binding=%s ok=%v", uid, ok)
	}

	// 第二次仍超时（同一粘性号）→ 达阈（阈 2）出池 + 解绑粘性：这是上一轮超时止损
	// 的原设计，新分类器不得改变它（header 阶段仍然只打一次上游、不重试）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-header"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("第 2 次 code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if calls != 2 {
		t.Fatalf("header 超时每次只打一次上游（不重试），两次请求应共 2 次，got %d", calls)
	}
	if code, _, _ := errorEnvelope(t, rec.Body.String()); code != "upstream_timeout" {
		t.Errorf("第 2 次仍应报 upstream_timeout, got %q", code)
	}
	slow, _ = p.Status("slow")
	if !slow.Cooling {
		t.Errorf("连续两次 header 超时应临时出池: cooling=%v", slow.Cooling)
	}
	if uid, ok := st.lastUID("conv-header"); ok {
		t.Errorf("出池后必须解绑粘性（否则会话被钉死在慢号上）, 仍绑定 %s", uid)
	}
}

// TestChatGatewayNetworkDialRefused connection refused（*net.OpError{Op:"dial"}）：
// 归 StageDial，同样走同号重试，末端 502 gateway_network_error + dial 成因文案。
func TestChatGatewayNetworkDialRefused(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
	var calls int
	up := connectFailUpstream(t, &calls, alwaysFail(dialRefusedErr()))
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: 3,
		ConnectRetry: 2, ConnectRetryBackoff: time.Millisecond,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-dial"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
	if calls != 3 {
		t.Fatalf("拨号失败也应同号重试 2 次（共 3 次）, got %d", calls)
	}
	code, msg, hint := errorEnvelope(t, rec.Body.String())
	if code != "gateway_network_error" {
		t.Errorf("code=%q want gateway_network_error", code)
	}
	if !strings.Contains(msg, "dial failed") {
		t.Errorf("message 应点明拨号失败: %q", msg)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("message 必须保留原始错误文本: %q", msg)
	}
	if hint != upstream.GatewayNetworkHint() {
		t.Errorf("hint 应为网关网络专用文案, got %q", hint)
	}
	solo, _ := p.Status("solo")
	if solo.ConsecutiveFails != 0 || solo.Cooling {
		t.Errorf("拨号失败不得罚号: fails=%d cooling=%v", solo.ConsecutiveFails, solo.Cooling)
	}
}

// TestChatGatewayNetworkRetryDisabled connect_retry=0：不重试，第一次就报
// gateway_network_error（「DNS 有独立兜底、不想多等」的部署形态）。
func TestChatGatewayNetworkRetryDisabled(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
	var calls int
	up := connectFailUpstream(t, &calls, alwaysFail(dnsTimeoutErr()))
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: 3,
		ConnectRetry: 0, ConnectRetryBackoff: time.Millisecond,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-noretry"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("connect_retry=0 必须只打一次上游, got %d", calls)
	}
	code, msg, _ := errorEnvelope(t, rec.Body.String())
	if code != "gateway_network_error" {
		t.Errorf("code=%q want gateway_network_error", code)
	}
	if strings.Contains(msg, "after") && strings.Contains(msg, "retries") {
		t.Errorf("未重试时 message 不该写重试次数: %q", msg)
	}
}

// TestChatGatewayNetworkRetryNotBoundByMaxRotate 重试不消耗 max_rotate 名额：
// MaxRotate=1 + DNS 恒失败，仍然完成 2 次重试（共 3 次尝试），不会因「换号预算
// 耗尽」提前退出。
func TestChatGatewayNetworkRetryNotBoundByMaxRotate(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
	var calls int
	up := connectFailUpstream(t, &calls, alwaysFail(dnsTimeoutErr()))
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: 1, // 只给 1 次换号预算
		ConnectRetry: 2, ConnectRetryBackoff: time.Millisecond,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-budget"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
	if calls != 3 {
		t.Fatalf("MaxRotate=1 不得限制连接级重试：want 3 次（1+2 重试）, got %d", calls)
	}
	code, _, _ := errorEnvelope(t, rec.Body.String())
	if code != "gateway_network_error" {
		t.Errorf("code=%q want gateway_network_error", code)
	}
}

// TestChatGatewayNetworkRouteBound 出口线路例外（D 段）：绑了线路（Auth.ProxyName）的
// 号，DNS/Dial 失败可能是**该线路**的问题，不是网关全局问题。
//
// 处置要求：仍先做同号重试；重试耗尽后日志与报文必须带上线路名，且**不罚账号**
// （不冷却、不禁用——线路的问题不该记到账号头上）。
func TestChatGatewayNetworkRouteBound(t *testing.T) {
	// 一条必然连不上的线路：先占一个端口再关掉，拿到一个「确定无人监听」的地址。
	// 这样失败发生在拨号阶段（ECONNREFUSED → StageDial），且完全在本机、无外网依赖。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := "http://" + ln.Addr().String()
	ln.Close()

	upstream.ConfigureProxies(map[string]string{"route-dead": deadAddr})
	defer upstream.ConfigureProxies(nil) // 全局状态复位，避免污染其它用例

	p := testPoolWith(&auth.Auth{
		UID: "routed", AccessToken: "at-routed", ExpiresAt: 9999999999,
		ProxyName: "route-dead",
	})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: 3,
		ConnectRetry: 2, ConnectRetryBackoff: time.Millisecond,
	})

	var rec *httptest.ResponseRecorder
	out := captureLog(func() {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, chatReqWithConv("conv-route"))
	})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s log=%s", rec.Code, rec.Body, out)
	}
	code, msg, hint := errorEnvelope(t, rec.Body.String())
	if code != "gateway_network_error" {
		t.Errorf("code=%q want gateway_network_error", code)
	}
	// 报文必须点名线路：否则运维会去查网关全局 DNS（方向全错）。
	if !strings.Contains(msg, "route-dead") {
		t.Errorf("绑线路的号，message 必须点名该线路: %q", msg)
	}
	if !strings.Contains(msg, "dial failed") {
		t.Errorf("message 应点明拨号失败: %q", msg)
	}
	if hint != upstream.GatewayNetworkHint() {
		t.Errorf("hint 应为网关网络专用文案, got %q", hint)
	}
	// 日志必须带 route=（排障第一现场）与重试次数。
	if !strings.Contains(out, "gateway network error") {
		t.Fatalf("未捕获到 gateway network error 日志行（格式变了？）: %s", out)
	}
	for _, want := range []string{"stage=dial", `route="route-dead"`, "retries=2", "acct="} {
		if !strings.Contains(out, want) {
			t.Errorf("日志缺少 %q: %s", want, out)
		}
	}
	if strings.Contains(out, "account cooled") {
		t.Errorf("线路问题不得冷却账号: %s", out)
	}
	// 账号零惩罚：线路的问题不是账号的问题。
	routed, _ := p.Status("routed")
	if routed.Cooling || routed.ConsecutiveFails != 0 || routed.ConsecutiveTimeouts != 0 {
		t.Errorf("线路失败不得罚账号: cooling=%v fails=%d timeouts=%d",
			routed.Cooling, routed.ConsecutiveFails, routed.ConsecutiveTimeouts)
	}
	if uids := p.AvailableUIDs(); len(uids) != 1 || uids[0] != "routed" {
		t.Errorf("账号应保持可选（不得被禁用/冷却）, got %v", uids)
	}
}

// TestChatGatewayNetworkRetryAbortedByCtx 客户端断连后不再退避重试：
// ctx 已取消 → 退避立即中断，只打一次上游。
func TestChatGatewayNetworkRetryAbortedByCtx(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel() // 第一次失败的同时客户端断连
		return nil, dnsTimeoutErr()
	})}
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: 3,
		// 退避故意设长：若实现没有感知 ctx，用例会白等 50ms 且 calls 变成 2+。
		ConnectRetry: 2, ConnectRetryBackoff: 50 * time.Millisecond,
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if calls != 1 {
		t.Fatalf("ctx 取消后不得继续重试, calls=%d", calls)
	}
}

// TestIsUpstreamTimeoutDeadlineRespectsClientGone 显式 deadline 必须与 Canceled
// 同口径：**客户端仍在**才算上游慢。
//
// 动机与 DNS 误冷却同型——一旦将来有中间件给 r.Context() 加 deadline，客户端自己
// 的 deadline 会命中 context.DeadlineExceeded；若这一支不看 clientGone，就会被判成
// 「上游慢」，进而 NoteTimeout 冷却无辜账号。同时钉住 header 阶段契约不变：
// net.Error.Timeout()（ResponseHeaderTimeout / http.Client.Timeout 都归此）不受
// clientGone 影响，仍返回 true。
func TestIsUpstreamTimeoutDeadlineRespectsClientGone(t *testing.T) {
	deadline := fmt.Errorf("read: %w", context.DeadlineExceeded)
	osDeadline := fmt.Errorf("write: %w", os.ErrDeadlineExceeded)

	cases := []struct {
		name       string
		err        error
		clientGone bool
		want       bool
	}{
		{"客户端断连 + DeadlineExceeded", deadline, true, false},
		{"客户端仍在 + DeadlineExceeded", deadline, false, true},
		{"客户端断连 + os.ErrDeadlineExceeded", osDeadline, true, false},
		{"客户端仍在 + os.ErrDeadlineExceeded", osDeadline, false, true},
		{"裸 context.DeadlineExceeded 且客户端已走", context.DeadlineExceeded, true, false},
		// 线上真实形态：transport 把请求 ctx 的到期错误包进 *url.Error。
		{"url.Error 包 DeadlineExceeded + 客户端已走",
			&url.Error{Op: "Post", URL: "https://www.workbuddy.ai/v2/chat/completions", Err: context.DeadlineExceeded},
			true, false},
		{"url.Error 包 DeadlineExceeded + 客户端仍在",
			&url.Error{Op: "Post", URL: "https://www.workbuddy.ai/v2/chat/completions", Err: context.DeadlineExceeded},
			false, true},
		// header 阶段契约不变：net.Error.Timeout() 分支不看 clientGone。
		{"net.Error 超时（header 阶段）+ 客户端已走", timeoutErr{true}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUpstreamTimeout(c.err, c.clientGone); got != c.want {
				t.Fatalf("isUpstreamTimeout(%v, clientGone=%v) = %v, want %v",
					c.err, c.clientGone, got, c.want)
			}
		})
	}
}

// TestIsUpstreamTimeoutRealHeaderTimeoutStillUpstream 用**真实** net/http 超时错误
// 复核「header 阶段契约不变」：ResponseHeaderTimeout 与 http.Client.Timeout 的产物
// 都是 *url.Error 包 net.Error（Timeout()==true），且**都满足**
// errors.Is(err, context.DeadlineExceeded)——所以判定不能靠 errors.Is，只能靠
// Unwrap 链里的哨兵值身份（hasDeadlineSentinel）。这条用例就是那个坑的闸门。
func TestIsUpstreamTimeoutRealHeaderTimeoutStillUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // 迟迟不给首字节
	}))
	defer srv.Close()

	cases := []struct {
		name   string
		client *http.Client
	}{
		{"ResponseHeaderTimeout", &http.Client{Transport: &http.Transport{
			ResponseHeaderTimeout: 10 * time.Millisecond,
		}}},
		{"http.Client.Timeout", &http.Client{Timeout: 10 * time.Millisecond}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := c.client.Get(srv.URL)
			if err == nil {
				resp.Body.Close()
				t.Fatalf("%s: 期望超时错误，却成功了", c.name)
			}
			// 前提：真实错误 errors.Is 到 DeadlineExceeded（net/http 的 Is 方法），
			// 但 Unwrap 链里**没有**哨兵值——正是不能用 errors.Is 的实证。
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s: 预期该错误自称 DeadlineExceeded（Go 版本变了？）: %v", c.name, err)
			}
			if hasDeadlineSentinel(err) {
				t.Fatalf("%s: 真实错误意外命中 deadline 哨兵（与注释不符）: %v", c.name, err)
			}
			if !isUpstreamTimeout(err, false) {
				t.Errorf("%s: header 阶段超时必须仍判为上游超时: %v", c.name, err)
			}
			if !isUpstreamTimeout(err, true) {
				t.Errorf("%s: net.Error 分支不看 clientGone（契约不变）: %v", c.name, err)
			}
		})
	}
}
