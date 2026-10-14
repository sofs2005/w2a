package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

// TestChatStreamErrorFrameNotMarkedSuccess 上游「200 已开流 + 一帧 error」是真实形态
// （6004 模型级限流、内容拦截、审核）。此前网关在读第一帧之前就 NoteSuccess +
// 清 11102 负缓存 + 绑粘性——被限流的号被记成健康，粘性把会话钉死在它身上，
// 后续每一轮都打同一个限流号。修法：成功判定与粘性绑定延后到流尾三态收敛，
// error 帧走 applyErrorPolicy 处置账号。
//
// 断言：账号**不得**记成功（success_count 保持 0）、必须被处置（进入模型级限流
// 台账 → cooling），且 error 帧原文仍透传（error-passthrough 不得因账号处置被改写）。
func TestChatStreamErrorFrameNotMarkedSuccess(t *testing.T) {
	const errFrame = `{"error":{"code":6004,"message":"模型限流，将在 2027-10-15 01:00:00 重置"}}`
	streamWithErr := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: " + errFrame + "\n\n" +
		"data: [DONE]\n\n"

	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad"} },
	})
	p := testPoolWith(&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, streamWithErr, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[],"metadata":{"conversation_id":"conv-1"}}`)))

	s, ok := p.Status("bad")
	if !ok {
		t.Fatal("account bad missing")
	}
	// 核心断言：error 帧流是失败，不得被记成健康（修法前此处为 1）。
	if s.SuccessCount != 0 {
		t.Errorf("error 帧流不得记成功: success_count=%d want 0", s.SuccessCount)
	}
	// 账号处置：6004 模型级限流写入 (账号, 模型) 负缓存台账（切模型豁免），
	// 观测为 rate_limited_models 非空——而非账号级 cooling（模型级冷却不牵连
	// 该账号的其他模型，故 Cooling 保持 false 才是正确口径）。
	if len(s.RateLimitedModels) == 0 {
		t.Errorf("6004 error 帧应记入模型级限流台账，status=%+v", s)
	}
	if s.Cooling {
		t.Errorf("6004 是模型级限流，不该把整个账号置为 cooling: %+v", s)
	}
	// error-passthrough：上游原文（含 code）必须原样到客户端。
	if !strings.Contains(rec.Body.String(), "6004") {
		t.Errorf("error 帧原文必须透传: %s", rec.Body)
	}
}

// TestChatStreamNormalMarkedSuccess 对照组：正常结束的流仍必须记成功（延后判定
// 不得把成功路径一起延丢——那会让每个账号永远不刷新 lastSuccess、连败计数永不
// 清零，是比原缺陷更糟的回归）。
func TestChatStreamNormalMarkedSuccess(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if s, _ := p.Status("good"); s.SuccessCount != 1 {
		t.Errorf("正常流必须记成功: success_count=%d want 1", s.SuccessCount)
	}
}

// TestChatStreamErrorFrameThenNextTurnAvoidsAccount 端到端：坏号在 error 帧后进入
// 模型级冷却，下一轮同一模型的请求不再选它（改走健康号）——这正是「延后成功判定」
// 要换来的效果（修法前坏号被记成功，下一轮仍被选中，会话被钉死）。
func TestChatStreamErrorFrameThenNextTurnAvoidsAccount(t *testing.T) {
	const errFrame = `{"error":{"code":6004,"message":"模型限流，将在 2027-10-15 01:00:00 重置"}}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 200, "data: " + errFrame + "\n\n" + "data: [DONE]\n\n", true
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// bad 积分更高 → 确定性优先选中（见 testPoolWith 注释）。error 帧后它被冷却，
	// 第二轮必须落到 good。
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("第一轮 code=%d body=%s", rec.Code, rec.Body)
	}

	// 第二轮：bad 已在 (bad, glm-5.2) 上冷却，选号必须避开它。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("第二轮 code=%d body=%s", rec2.Code, rec2.Body)
	}
	if strings.Contains(rec2.Body.String(), "6004") {
		t.Errorf("第二轮不该再打到被限流的 bad（会话被钉死）：%s", rec2.Body)
	}
}

// TestChatNonStreamSuccessMarkedAfterAggregate 非流式口径对齐：Aggregate 成功后才
// 记成功（流式与非流式两路收口必须同口径，否则非流式请求永远不刷新账号健康度）。
func TestChatNonStreamSuccessMarkedAfterAggregate(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true // 上游始终 SSE，非流式请求由网关 Aggregate 成 JSON
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if s, _ := p.Status("good"); s.SuccessCount != 1 {
		t.Errorf("非流式成功必须记成功: success_count=%d want 1", s.SuccessCount)
	}
}
