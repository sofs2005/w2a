package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestChatModelBlockedNotNoHealthyAccount 钉住「模型不可用」与「没有可用账号」的区分。
//
// 场景：两个号都在，且都对 glm-5.2 处于 11102 模型级负缓存中。选号返回 nil，
// lastErr 为空（本轮压根没打到上游）。修法前客户端拿到的是 503 no_healthy_account
// ——读起来像「服务端过载」，于是不停重试；真正该做的是换个模型。
//
// 断言三件事：状态码 400（不可重试语义）、code=model_unavailable、gateway_hint 里
// 带出被挡账号数与最早解封时间（上游不会告诉客户端的本地调度信息）。
func TestChatModelBlockedNotNoHealthyAccount(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	// 两个号都对 glm-5.2 写 11102 负缓存 → 全池该模型不可选。
	p.BlockModelBackoff("u1", "glm-5.2", upstream.ModelBlockReason)
	p.BlockModelBackoff("u2", "glm-5.2", upstream.ModelBlockReason)

	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if calls != 0 {
		t.Errorf("模型级阻塞应在选号阶段止损，不得打上游，实际 %d 次", calls)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（模型不可用是不可重试错误）body=%s", rec.Code, rec.Body)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "model_unavailable") {
		t.Errorf("不得混成 no_healthy_account（语义相反：该换模型，不是该重试）: %s", rec.Body)
	}
	// 上游不会告诉客户端的两件事：有几个号被挡、最早什么时候可能好。
	body := rec.Body.String()
	if !strings.Contains(body, "2 account(s)") {
		t.Errorf("gateway_hint 应带出被挡账号数: %s", body)
	}
	if strings.Contains(body, "rate_limit_exceeded") {
		t.Errorf("11102（无此模型）不得报成限流: %s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("不得同时出现旧 code: %s", body)
	}
}

// TestChatModelBlockedOneAccountFreeKeepsGeneric 只要还有一个号能服务该模型，
// 选号失败就另有原因（在途占满），必须保持原口径——否则会把「暂时抢不到名额」
// 误报成「模型下线了」。
func TestChatModelBlockedOneAccountFreeKeepsGeneric(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	// 只挡 u1：u2 仍可服务 → 不是模型级阻塞。
	p.BlockModelBackoff("u1", "glm-5.2", upstream.ModelBlockReason)
	// 把在途上限压到 1 并占住 u2，让选号也拿不到它（模拟「池子暂时无号」，
	// 而不是模型问题）。默认上限为 0 = 不限，必须显式收紧。
	p.SetMaxInFlight(1)
	if !p.Acquire("u2") {
		t.Fatal("fixture: 占位 u2 失败")
	}

	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "no_healthy_account") {
		t.Errorf("还有号能服务该模型时不得报 model_unavailable: %s", rec.Body)
	}
}

// TestChatModelBlockedKeepsUpstreamText 打到上游那一轮必须保留上游原文（含
// requestId，用户要拿去向上游反馈），只把「几个号被挡」这类本地调度信息补进
// gateway_hint —— 本地推断不得顶掉权威原文。
//
// 若覆盖分支不判 lastErr，这里 msg 会被本地通用文案替换，原文与 requestId 全丢。
func TestChatModelBlockedKeepsUpstreamText(t *testing.T) {
	const upstreamMsg = "model [glm-5.2] service info not found"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 404, `{"code":11102,"msg":"` + upstreamMsg + `","requestId":"req-abc123"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "model_unavailable") {
		t.Errorf("11102 应报 model_unavailable: %s", rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, upstreamMsg) {
		t.Errorf("必须保留上游原文（不得被本地文案顶掉）: %s", body)
	}
	if !strings.Contains(body, "req-abc123") {
		t.Errorf("必须保留上游 requestId: %s", body)
	}
	// 有上游原文时 hint 也留给上游的判定（本例：「switch model」），
	// 本地计数只在没原文的那条路径上才是唯一信息源（见上一个用例）。
	if !strings.Contains(body, "switch model") {
		t.Errorf("有上游原文时应保留上游判定的 hint: %s", body)
	}
}

// TestChatModelBlockedRotationKeepsUpstreamText 两个号都撞 11102（真实形态：
// 轮转把每个号都试一遍，全部写进负缓存）→ 轮转结束选号返回 nil，**此时 lastErr
// 非空**且 modelBlock.Blocked 为真。这条路径上「保留上游原文」与「覆盖本地文案」
// 的优先级才真正被考验：上游原文（含 requestId）必须留着，本地只补 gateway_hint。
//
// 与单号首发那条用例的区别：那时选号成功、modelBlock 根本没填充；这里两条信息
// 同时在场，优先级必须选对。
func TestChatModelBlockedRotationKeepsUpstreamText(t *testing.T) {
	const upstreamMsg = "model [glm-5.2] service info not found"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 404, `{"code":11102,"msg":"` + upstreamMsg + `","requestId":"req-xyz789"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 4})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, upstreamMsg) {
		t.Errorf("轮转耗尽时仍须保留上游原文（不被本地文案顶掉）: %s", body)
	}
	if !strings.Contains(body, "req-xyz789") {
		t.Errorf("轮转耗尽时仍须保留上游 requestId: %s", body)
	}
	if !assertJSONErrorCode(t, body, "model_unavailable") {
		t.Errorf("11102 应报 model_unavailable: %s", body)
	}
}

// 后续请求的 lastErr 同样为空（压根没打到上游），**不能**因此被判成
// model_unavailable：那是可等待的临时限流，客户端该等重置，不是换模型。
//
// 这条覆盖的正是「窗口内第二轮」的真实形态——本轮请求本身没打上游，全靠
// pool 里残留的 6004 条目解释「为什么没号」。
func TestChatRateWindowReportsRateLimitNotModelUnavailable(t *testing.T) {
	reset := time.Now().Add(35 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 2})

	// 第一轮：打到上游，两个号都被 6004 写进模型级限流窗口。
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec1.Code != http.StatusTooManyRequests {
		t.Fatalf("首轮 code=%d want 429 body=%s", rec1.Code, rec1.Body)
	}

	// 第二轮：选号直接返回 nil（全池在该模型的限流窗口内），lastErr 为空。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("窗口内 code=%d want 429（限流窗口该等，不是模型下线）body=%s", rec2.Code, rec2.Body)
	}
	if !assertJSONErrorCode(t, rec2.Body.String(), "rate_limit_exceeded") {
		t.Errorf("窗口内应报 rate_limit_exceeded: %s", rec2.Body)
	}
	// 上游不会告诉客户端本地调度信息：有几个号被挡、最早什么时候解封。
	if body := rec2.Body.String(); !strings.Contains(body, "2 account(s)") ||
		!strings.Contains(body, "earliest unblock at") {
		t.Errorf("gateway_hint 应带出被挡账号数与最早解封时间: %s", body)
	}
}
