package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// 本文件覆盖任务 B 的两条「只改报文/只救请求，不改罚号」承诺：
//
//  1. /v2 判渠道校验 → 回退 /console 报 11102：对外必须是 **/v2 的 11-128 原文**
//     （不是 /console 的「模型不存在」），且分类仍是请求级（ErrFallbackModelMissing）
//     —— handler 对它零动作：不冷却、不禁用、不写模型负缓存。
//  2. 11-128 的「system 搬进 user」请求级抢救：抢救失败（搬迁后仍被拦）必须回到
//     既有渠道校验软冷却策略，不能因为多了抢救一步就把罚号弄丢或弄反。

// consoleMissingBody /console 对不提供的模型的 11102 答复（回退路径的典型形态）。
const consoleMissingBody = `{"code":11102,"msg":"model [glm-5.2] service info not found"}`

// TestChatFallbackModelMissingReportsPrimaryChannelText 回退路径的 11102 不该替上游改口。
//
// 现场：/v2 因渠道校验被拒（11-128）→ 换 /console → /console 没有这个模型（11102）。
// 照 11102 对外报，运维与客户端都会以为「模型下线了」，而真实原因是被渠道校验挡住。
// 分类必须保持请求级（ErrFallbackModelMissing → applyErrorPolicy 零动作），
// 否则「换条路没换到模型」会被记成账号故障（issue #245 修掉的正是这个坑）。
func TestChatFallbackModelMissingReportsPrimaryChannelText(t *testing.T) {
	v2Body := channelCheckBody(testAntiProbeCode())
	var paths []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.Path)
			if strings.Contains(r.URL.Path, "/v2/chat/completions") {
				return sseResponse(http.StatusBadRequest, v2Body), nil
			}
			return sseResponse(http.StatusNotFound, consoleMissingBody), nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: 600 * time.Second})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code == http.StatusOK {
		t.Fatalf("两条路径都失败时不得报成功: %s", rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "channel verification") {
		t.Errorf("应透传主路径（/v2）的渠道校验原文，实际: %s", body)
	}
	if strings.Contains(body, "service info not found") {
		t.Errorf("不得把回退路径的 11102 报文透给客户端（会把排障带向「模型下线」）: %s", body)
	}
	// 分类维持请求级 ⇒ 罚号零动作。这三条是「只改报文不改罚号」的核心断言。
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号 u1 不在池里")
	}
	if st.Cooling {
		t.Errorf("请求级分类不该软冷却账号: cooling=%v reason=%q", st.Cooling, st.Reason)
	}
	if st.Disabled {
		t.Errorf("请求级分类绝不禁用账号: reason=%q", st.DisabledReason)
	}
	if len(st.RateLimitedModels) != 0 {
		t.Errorf("不得写模型负缓存（回退路径的 11102 不是「该账号缺这个模型」）: %+v", st.RateLimitedModels)
	}
	// 路径确实是「先 /v2 再 /console」。
	if len(paths) < 2 || !strings.Contains(paths[0], "/v2/") || !strings.Contains(paths[1], "/console/") {
		t.Fatalf("应先 /v2 被拒再换 /console，实际路径=%v", paths)
	}
}

// TestChatChannelCheckMoveStillBlockedFallsBackToSoftCooldown 搬迁后仍被拦 → 既有策略。
//
// 两条路径都持续判渠道校验（system 搬进 user 也没能通过）：抢救只做一次
// （systemMoved），随后必须落到既有的「渠道校验 → 软冷却、绝不禁用」，
// 并且总调用次数有界（不得因为多了一次抢救而变成循环重试）。
func TestChatChannelCheckMoveStillBlockedFallsBackToSoftCooldown(t *testing.T) {
	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			// 无论 body 长什么样都拒：模拟「搬迁也救不了」的形态（触发词在 user 侧）。
			return sseResponse(http.StatusBadRequest, channelCheckBody(testAntiProbeCode())), nil
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

	if rec.Code == http.StatusOK {
		t.Fatalf("搬迁后仍被拦时不得报成功: %s", rec.Body.String())
	}
	// 第一轮 /v2 + /console，抢救后再一轮 /v2 + /console：2 条路径 × 2 轮 = 4 次。
	// 抢救只做一次，不会继续循环（>4 说明抢救没有 `systemMoved` 上界）。
	if calls != 4 {
		t.Errorf("上游调用=%d want 4（两条路径 × 两轮，抢救只做一次）", calls)
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("渠道校验绝不能禁用账号（凭证是好的）: reason=%q", st.DisabledReason)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("抢救失败后应回到既有软冷却策略，got %+v", st)
	}
	if !strings.Contains(st.Reason, "channel check") {
		t.Errorf("冷却原因应写明渠道校验: %q", st.Reason)
	}
}

// TestChatChannelCheckMoveSuccessLeavesAccountClean 搬迁成功 → 账号保持干净。
//
// 与上一条互补：请求级抢救的收益必须真的兑现（成功的号不该留下任何惩罚痕迹），
// 否则「只救请求不罚号」的承诺就成了「救了请求还顺带罚了号」。
func TestChatChannelCheckMoveSuccessLeavesAccountClean(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			if systemHasPersona(b) {
				return sseResponse(http.StatusBadRequest, channelCheckBody(testAntiProbeCode())), nil
			}
			return sseResponse(http.StatusOK, sseOK), nil
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
	if rec.Code != http.StatusOK {
		t.Fatalf("搬迁后应成功，got code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || len(st.RateLimitedModels) != 0 {
		t.Fatalf("抢救成功的号必须保持干净（不冷却/不禁用/不写模型负缓存）: %+v", st)
	}
}
