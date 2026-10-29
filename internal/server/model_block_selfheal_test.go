package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// selfHealModel 本文件统一使用的模型名（两域都存在的裸名，便于断言与日志阅读）。
const selfHealModel = "deepseek-v4.1-flash"

// newSelfHealUpstream 构造一个记录上游调用次数的 fake 上游（行为由 status/body 决定）。
func newSelfHealUpstream(t *testing.T, calls *int32, status int, body string) *upstream.Client {
	t.Helper()
	return newFakeUpstream(t, func(authz string) (int, string, bool) {
		atomic.AddInt32(calls, 1)
		return status, body, status == http.StatusOK
	})
}

// TestModelBlockSelfHealIsRealmSetAware 11102 全池自愈的判据必须与选号**同一个域
// 集合维度**（issue #332 的形态，本仓库是集合而非单值）。
//
// 现场：CN 域两个号都被 deepseek-v4.1-flash 的 11102 负缓存挡死，池里同时还有
// global 号（它不被该模型的负缓存挡）。旧判据若看**全池**可用性
// （ModelBlocked / AvailableUIDsForModel），返回恒非空 → 自愈永不触发 →
// PickExcludingForRealms(realm={cn}) 选不出号 → 固定 503；而且换任何模型都一样，
// 因为每个 CN 模型都会被轮流打成负缓存。本用例按单 realm / 全池实现必红。
func TestModelBlockSelfHealIsRealmSetAware(t *testing.T) {
	cn1 := &auth.Auth{UID: "cn1", AccessToken: "at-cn1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	cn2 := &auth.Auth{UID: "cn2", AccessToken: "at-cn2", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	// global 号在池里但**不该**被本请求选中（钉域 cn），它的存在只用于把「全池口径」
	// 撑成非空 —— 正是旧判据失效的成因。
	gl1 := &auth.Auth{UID: "gl1", AccessToken: "at-gl1", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	p := testPoolWith(cn1, cn2, gl1)
	for _, uid := range []string{"cn1", "cn2"} {
		p.BlockModelBackoff(uid, selfHealModel, upstream.ModelBlockReason)
	}

	var calls int32
	h := NewHandler(Config{Pool: p, Upstream: newSelfHealUpstream(t, &calls, http.StatusOK, sseOK)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"cn:`+selfHealModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("cn 域被负缓存挡住时必须自愈重试，实际 503：%s", rec.Body.String())
	}
	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("自愈后应当真的把请求发出去（不得直接失败）")
	}
	// 自愈的实际效果：CN 号上的 11102 负缓存被清掉（否则下一轮又会被挡）。
	for _, uid := range []string{"cn1", "cn2"} {
		st, ok := p.Status(uid)
		if !ok {
			t.Fatalf("账号 %s 不在池里", uid)
		}
		if len(st.RateLimitedModels) != 0 {
			t.Fatalf("%s 的 11102 负缓存应已被清掉：%+v", uid, st.RateLimitedModels)
		}
	}
}

// TestModelBlockSelfHealBareModelStillWorks 裸名（全池）口径下自愈同样生效：
// 全池所有号都被 11102 负缓存挡死时，清一次负缓存重试即可恢复。
//
// 与上一条互补：上一条防「按全池判据实现」的回归，这条防「按单域判据实现」
// 把裸名（CandidateRealms()=nil=全池）判空的回归——两条一起钉住集合维度。
func TestModelBlockSelfHealBareModelStillWorks(t *testing.T) {
	cn1 := &auth.Auth{UID: "cn1", AccessToken: "at-cn1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	gl1 := &auth.Auth{UID: "gl1", AccessToken: "at-gl1", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	p := testPoolWith(cn1, gl1)
	for _, uid := range []string{"cn1", "gl1"} {
		p.BlockModelBackoff(uid, selfHealModel, upstream.ModelBlockReason)
	}

	var calls int32
	h := NewHandler(Config{Pool: p, Upstream: newSelfHealUpstream(t, &calls, http.StatusOK, sseOK)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"`+selfHealModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("全池负缓存应自愈并成功，实际 code=%d body=%s", rec.Code, rec.Body.String())
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("自愈后应只打一次上游，实际 %d 次", n)
	}
}

// TestModelBlockSelfHealIsBounded 反向约束：模型**真的**不可用（上游持续 11102）时
// 自愈不得变成重试风暴 —— 只清一次，调用次数有上界，且不得报成功。
func TestModelBlockSelfHealIsBounded(t *testing.T) {
	cn1 := &auth.Auth{UID: "cn1", AccessToken: "at-cn1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn1)
	p.BlockModelBackoff("cn1", selfHealModel, upstream.ModelBlockReason)

	var calls int32
	up := newSelfHealUpstream(t, &calls, http.StatusNotFound,
		`{"code":11102,"msg":"model [`+selfHealModel+`] service info not found"}`)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 4})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"cn:`+selfHealModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code == http.StatusOK {
		t.Fatalf("模型确实不存在时不该报成功：%s", rec.Body.String())
	}
	if n := atomic.LoadInt32(&calls); n > 8 {
		t.Fatalf("自愈不得变成重试风暴，实际上游调用 %d 次", n)
	}
	// 清一次之后负缓存应已被重新写回（下次请求不会每轮都清）。
	if st := p.ModelBlockedInRealms(selfHealModel, pool.SingleRealm("cn")); !st.Blocked {
		t.Fatalf("上游持续 11102 时负缓存应被重新写回：%+v", st)
	}
}

// TestModelBlockSelfHealLeaves6004 6004（模型级限流，「等配额重置」语义）与 11102
// （「该后端无此模型」）共用 modelCooldowns 表，但自愈**绝不能**清掉 6004：
// 清掉它等于把「等到重置」变成「立刻再打一次」，只会放大上游限流。
//
// 断言：自愈不触发（6004 不是 11102）、不打上游、6004 条目原样留着、对外仍报限流。
func TestModelBlockSelfHealLeaves6004(t *testing.T) {
	cn1 := &auth.Auth{UID: "cn1", AccessToken: "at-cn1", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"}
	p := testPoolWith(cn1)
	reset := time.Now().Add(20 * time.Minute)
	p.CooldownSoftForModel("cn1", 600*time.Second, reset, selfHealModel, "6004 model rate limit")

	var calls int32
	h := NewHandler(Config{Pool: p, Upstream: newSelfHealUpstream(t, &calls, http.StatusOK, sseOK)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"cn:`+selfHealModel+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code == http.StatusOK {
		t.Fatalf("6004 冷却中不该报成功：%s", rec.Body.String())
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("6004 冷却应在选号阶段止损、不得打上游，实际 %d 次", n)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "rate_limit_exceeded") {
		t.Errorf("6004 应报限流而非 model_unavailable: %s", rec.Body)
	}
	st, ok := p.Status("cn1")
	if !ok {
		t.Fatal("账号 cn1 不在池里")
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != selfHealModel {
		t.Fatalf("6004 条目不得被自愈清掉：%+v", st.RateLimitedModels)
	}
}
