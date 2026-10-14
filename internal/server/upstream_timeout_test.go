// upstream_timeout_test.go 钉住 isUpstreamTimeout 的三态判定口径：超时与
// 「网络抖动」此前共用同一条换号路径——注定超时的请求会轮转 MaxRotate 次
// （header_timeout 600s 时最坏约半小时），期间还给一串健康号喂连败计数。
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

type timeoutErr struct{ timeout bool }

func (e timeoutErr) Error() string   { return "stub" }
func (e timeoutErr) Timeout() bool   { return e.timeout }
func (e timeoutErr) Temporary() bool { return e.timeout }

func TestIsUpstreamTimeout(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		clientGone bool
		want       bool
	}{
		{"net.Error 超时", timeoutErr{true}, false, true},
		{"net.Error 非超时（连接被拒）", timeoutErr{false}, false, false},
		{"显式 deadline", fmt.Errorf("read: %w", context.DeadlineExceeded), false, true},
		{"客户端断连（ctx 已取消）", context.Canceled, true, false},
		{"空闲看门狗掐流（客户端仍在）", context.Canceled, false, true},
		{"普通错误", errors.New("boom"), false, false},
		{"nil", nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUpstreamTimeout(c.err, c.clientGone); got != c.want {
				t.Fatalf("isUpstreamTimeout(%v, clientGone=%v) = %v, want %v", c.err, c.clientGone, got, c.want)
			}
		})
	}

	// 保证 stub 真的实现了 net.Error（否则上面的用例会静默走不到 Timeout 分支）。
	var ne net.Error = timeoutErr{true}
	_ = ne
}

// TestChatUpstreamTimeoutNoRotationNoPenalty 端到端：上游超时必须**止损**——
// 只打一次上游（不轮转）、账号不被喂连败计数、末端给出可区分的 upstream_timeout
// 文案。修法前它走抖动分支：换号重试 + NoteFailures。
func TestChatUpstreamTimeoutNoRotationNoPenalty(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	// fake upstream 的 roundTripFunc 无法直接返回 error，改用 ChatHTTP 指向一个
	// 立即返回 timeout 错误的 RoundTripper（ChatStreamContext 优先用 ChatHTTP）。
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, timeoutErr{true}
	})}

	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "upstream_timeout") {
		t.Errorf("超时必须给出可区分的 code（不能混成 no_healthy_account）: %s", rec.Body)
	}
	// 关键：超时不轮转——只打一次上游（修法前会打到 MaxRotate 次）。
	if calls != 1 {
		t.Errorf("上游超时必须止损，实际调用 %d 次（MaxRotate=3）", calls)
	}
	// 关键：超时不罚号——两个号的连败计数都必须为 0。
	for _, uid := range []string{"a1", "a2"} {
		if s, _ := p.Status(uid); s.ConsecutiveFails != 0 {
			t.Errorf("超时不得喂连败计数: %s consecutive_fails=%d", uid, s.ConsecutiveFails)
		}
	}
}

// TestChatClientGoneNotUpstreamTimeout 客户端主动断连不得被误判为上游超时：
// 此时 r.Context() 已取消，走抖动分支而非超时分支（人已走，算成上游故障会污染统计）。
func TestChatClientGoneNotUpstreamTimeout(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})}
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 客户端已断连
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// 不得报 upstream_timeout（那是上游故障的文案）。
	if strings.Contains(rec.Body.String(), "upstream_timeout") {
		t.Errorf("客户端断连不得报 upstream_timeout: %s", rec.Body)
	}
}
