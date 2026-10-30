// gateway_network_qa_test.go QA 独立验证（B 段）：工程师的 gateway_network_test.go
// 覆盖了「重试次数 / 零惩罚 / 粘性保留 / 502+hint / header 超时不被吃掉」，但**没有**
// 覆盖以下四点，而它们恰好是这条修复最容易写错的地方：
//
//  1. 在途名额不泄漏：重试是「同一个请求、同一个账号」的一次逻辑调用，若循环里再
//     调一次 Pool.Acquire，名额会被重复占用（SetMaxInFlight(1) 时直接自锁死）；
//  2. 真的是**同号**重试：单账号用例无法区分「同号重试」与「换号重试」（池里只有
//     一个号，换号也只能换到它自己）；
//  3. ctx 中断是**立即**的（不只是「次数对」）：退避设 200ms 时，若实现没有感知
//     ctx，用例仍会通过但客户端要多等一次退避；
//  4. TLS 阶段（第三类连接期失败）端到端走通，且 message 成因不串味；
//  5. context.DeadlineExceeded 的归类在端到端层面**仍然**是 upstream_timeout
//     （header 阶段契约不变），并钉住 isUpstreamTimeout 对连接阶段的收窄。
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// tlsHandshakeErr TLS 握手失败（RecordHeaderError 包在 url.Error 里，标准库形态）。
func tlsHandshakeErr() error {
	return &url.Error{
		Op:  "Post",
		URL: "https://www.workbuddy.ai/v2/chat/completions",
		Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"},
	}
}

// TestQAGatewayNetworkRetryKeepsInFlightSlot 在途名额：重试期间**不得**重复 Acquire。
//
// 判据两条（刻意**不**设 max_in_flight 上限：上限会把重复占名额静默吞掉，反而掩盖
// 缺陷——Acquire 超额时返回 false 且不计数，InFlight 仍是 1）：
//   - 每次打上游的瞬间，该号的 InFlight 恰好是 1（不是 2）；
//   - 请求结束后归零（重复 Acquire + 单次 Release 会残留 1，账号永久半满额）。
func TestQAGatewayNetworkRetryKeepsInFlightSlot(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
	var calls int
	var during []int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if st, ok := p.Status("solo"); ok {
			during = append(during, st.InFlight)
		}
		return nil, dnsTimeoutErr()
	})}
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: 3,
		ConnectRetry: 2, ConnectRetryBackoff: time.Millisecond,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-inflight"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
	if calls != 3 {
		t.Fatalf("应完成 1+2 次尝试, got %d", calls)
	}
	for i, n := range during {
		if n != 1 {
			t.Errorf("第 %d 次尝试时 InFlight=%d want 1（重试不得重复占名额）", i+1, n)
		}
	}
	if st, ok := p.Status("solo"); !ok || st.InFlight != 0 {
		t.Errorf("请求结束后 InFlight 必须归零（名额泄漏会让账号永久满额）, got %+v", st)
	}
}

// TestQAGatewayNetworkRetriesSameAccountNotRotation 同号重试的**唯一**可判别形态：
// 池里两个号，只有被选中的那个会失败；重试必须还打在它身上，另一个号一次都不许被调用。
func TestQAGatewayNetworkRetriesSameAccountNotRotation(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
	)
	var mu sync.Mutex
	var seen []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		authz := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, authz)
		mu.Unlock()
		return nil, dnsTimeoutErr() // 两个号都解析不了同一个域名（网关侧故障与账号无关）
	})}
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: 3,
		ConnectRetry: 2, ConnectRetryBackoff: time.Millisecond,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-same-acct"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("应 1+2 次尝试, got %d (%v)", len(got), got)
	}
	for i, authz := range got {
		if authz != got[0] {
			t.Fatalf("重试必须是**同一个号**（DNS 换号无收益）：第 %d 次换成了 %q（首跳 %q）", i+1, authz, got[0])
		}
	}
	// 另一个号完全没被碰过：不罚号、不轮转。
	hit, other := "a", "b"
	if strings.Contains(got[0], "at-b") {
		hit, other = "b", "a"
	}
	if st, _ := p.Status(hit); st.ConsecutiveFails != 0 || st.ConsecutiveTimeouts != 0 || st.Cooling {
		t.Errorf("被重试的号必须零惩罚: fails=%d timeouts=%d cooling=%v",
			st.ConsecutiveFails, st.ConsecutiveTimeouts, st.Cooling)
	}
	if st, _ := p.Status(other); st.ErrTotal != 0 || st.ConsecutiveFails != 0 {
		t.Errorf("未参与重试的号不该有任何记账: err_total=%d fails=%d", st.ErrTotal, st.ConsecutiveFails)
	}
}

// TestQAGatewayNetworkRetryAbortsImmediatelyOnClientGone ctx 中断必须是**立即**的：
// 退避基数 200ms，客户端在第一次失败时断连 → 总耗时必须远小于一次退避（<100ms），
// 且只打一次上游。sleepCtx 用 time.NewTimer + defer Stop，无 goroutine 泄漏。
func TestQAGatewayNetworkRetryAbortsImmediatelyOnClientGone(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel() // 客户端断连
		return nil, dnsTimeoutErr()
	})}
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: 3,
		ConnectRetry: 2, ConnectRetryBackoff: 200 * time.Millisecond,
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if calls != 1 {
		t.Fatalf("ctx 已取消不得继续重试, calls=%d", calls)
	}
	// 200ms 基数抖动 ±25% → 若真的 sleep 了，耗时 ≥150ms；阈值取 100ms 留足余量。
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("退避未感知 ctx：耗时 %v（应 <100ms，说明睡了退避）", elapsed)
	}
	if st, _ := p.Status("solo"); st.InFlight != 0 {
		t.Errorf("中断后 InFlight 必须归零, got %d", st.InFlight)
	}
}

// TestQAGatewayNetworkTLSRetried TLS 握手失败（第三类连接期失败）：同样同号重试、
// 502 + 专用 hint，且 message 说的是 tls 而不是 dns/dial（三类成因互不串味）。
func TestQAGatewayNetworkTLSRetried(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
	var calls int
	up := connectFailUpstream(t, &calls, alwaysFail(tlsHandshakeErr()))
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: 3,
		ConnectRetry: 2, ConnectRetryBackoff: time.Millisecond,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-tls"))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
	if calls != 3 {
		t.Fatalf("TLS 握手失败也应同号重试 2 次（共 3 次）, got %d", calls)
	}
	code, msg, hint := errorEnvelope(t, rec.Body.String())
	if code != "gateway_network_error" {
		t.Errorf("code=%q want gateway_network_error", code)
	}
	if !strings.Contains(msg, "tls handshake failed") {
		t.Errorf("message 应点明 TLS 握手失败: %q", msg)
	}
	if strings.Contains(msg, "dns resolution failed") || strings.Contains(msg, "dial failed") {
		t.Errorf("TLS 失败不得说成 DNS/拨号: %q", msg)
	}
	if hint != upstream.GatewayNetworkHint() {
		t.Errorf("hint 应为网关网络专用文案, got %q", hint)
	}
	if st, _ := p.Status("solo"); st.ConsecutiveFails != 0 || st.ConsecutiveTimeouts != 0 || st.Cooling {
		t.Errorf("TLS 失败不得罚号: fails=%d timeouts=%d cooling=%v",
			st.ConsecutiveFails, st.ConsecutiveTimeouts, st.Cooling)
	}
}

// TestQADeadlineExceededStillHeaderStage context.DeadlineExceeded 归 StageHeader，
// 端到端仍是 upstream_timeout（header 阶段契约不变）：只打一次上游、计一次连续超时。
//
// 这条同时是「客户端自己的 deadline 会不会被当成上游超时」的实证：分类器只看
// net.Error.Timeout()，**不看是谁的 deadline**；本仓没有任何给 r.Context() 加
// deadline 的中间件（唯一 WithTimeout 在 main.go 的 shutdownCtx），所以线上到达
// handler 的 DeadlineExceeded 只可能来自我们自己的 header_timeout / client.Timeout
// ——语义上确实属于 header 阶段。残余风险见报告 Minor。
func TestQADeadlineExceededStillHeaderStage(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
	p.SetTimeout(2, time.Minute)
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, &url.Error{Op: "Post", URL: "https://x", Err: context.DeadlineExceeded}
	})}
	h := NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: 3,
		ConnectRetry: 2, ConnectRetryBackoff: time.Millisecond, // 重试开着也不许作用到 header 阶段
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-deadline"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("header 阶段不得重试, calls=%d", calls)
	}
	code, _, hint := errorEnvelope(t, rec.Body.String())
	if code != "upstream_timeout" {
		t.Errorf("code=%q want upstream_timeout", code)
	}
	if hint != upstream.UpstreamTimeoutHint() {
		t.Errorf("hint 应为超时专用文案, got %q", hint)
	}
	if st, _ := p.Status("solo"); st.ConsecutiveTimeouts != 1 {
		t.Errorf("DeadlineExceeded 应计 1 次连续超时（header 阶段契约）, got %d", st.ConsecutiveTimeouts)
	}
}

// TestQAIsUpstreamTimeoutExcludesConnectStage isUpstreamTimeout 的收窄是**纵深防御**
// 的那一层（不只靠调用点顺序）：DNS/Dial/TLS 一律 false，header 阶段仍 true。
func TestQAIsUpstreamTimeoutExcludesConnectStage(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"DNS 超时（线上形态）", dnsTimeoutErr(), false},
		{"拨号被拒", dialRefusedErr(), false},
		{"TLS 握手", tlsHandshakeErr(), false},
		{"header 阶段超时", timeoutErr{true}, true},
		{"DeadlineExceeded", context.DeadlineExceeded, true},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isUpstreamTimeout(c.err, false); got != c.want {
			t.Errorf("%s: isUpstreamTimeout=%v want %v", c.name, got, c.want)
		}
	}
	// 客户端已断连时，context.Canceled 不算上游超时（人已走，别污染超时统计）。
	if isUpstreamTimeout(context.Canceled, true) {
		t.Error("clientGone + context.Canceled 不得判成上游超时")
	}
}

// TestQAGatewayNetworkEnvelopeNoCrossTalk 三类错误的信封互不串味（一次跑完三种，
// 同一份断言表）：code / message / hint 三列都必须指向同一个成因。
func TestQAGatewayNetworkEnvelopeNoCrossTalk(t *testing.T) {
	envelope := func(t *testing.T, err error) (status int, code, msg, hint string) {
		t.Helper()
		p := testPoolWith(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
		var calls int
		up := connectFailUpstream(t, &calls, alwaysFail(err))
		h := NewHandler(Config{
			Pool: p, Upstream: up, MaxRotate: 3,
			ConnectRetry: 1, ConnectRetryBackoff: time.Millisecond,
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, chatReqWithConv("conv-crosstalk"))
		var e struct {
			Error struct {
				Message     string `json:"message"`
				Code        string `json:"code"`
				GatewayHint string `json:"gateway_hint"`
			} `json:"error"`
		}
		if uerr := json.Unmarshal(rec.Body.Bytes(), &e); uerr != nil {
			t.Fatalf("%v: 非 JSON 信封: %v body=%s", err, uerr, rec.Body)
		}
		return rec.Code, e.Error.Code, e.Error.Message, e.Error.GatewayHint
	}

	status, code, msg, hint := envelope(t, dnsTimeoutErr())
	if status != http.StatusBadGateway || code != "gateway_network_error" ||
		!strings.Contains(msg, "dns resolution failed") || hint != upstream.GatewayNetworkHint() {
		t.Errorf("DNS 信封串味: status=%d code=%q msg=%q hint=%q", status, code, msg, hint)
	}
	status, code, msg, hint = envelope(t, dialRefusedErr())
	if status != http.StatusBadGateway || code != "gateway_network_error" ||
		!strings.Contains(msg, "dial failed") || hint != upstream.GatewayNetworkHint() {
		t.Errorf("Dial 信封串味: status=%d code=%q msg=%q hint=%q", status, code, msg, hint)
	}
	status, code, msg, hint = envelope(t, timeoutErr{true})
	if status != http.StatusServiceUnavailable || code != "upstream_timeout" ||
		hint != upstream.UpstreamTimeoutHint() || strings.Contains(msg, "gateway could not reach upstream") {
		t.Errorf("header 超时信封串味: status=%d code=%q msg=%q hint=%q", status, code, msg, hint)
	}
}
