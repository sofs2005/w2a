package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// extractCacheKey 从改写后的 body 里取出 prompt_cache_key 字段值。
func extractCacheKey(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	v, _ := obj["prompt_cache_key"].(string)
	return v
}

// TestInjectPromptCacheKey_PreservesExisting 覆盖任务用例 1：
// 入站 body 已带 prompt_cache_key → 原值保留，不被网关覆盖。
func TestInjectPromptCacheKey_PreservesExisting(t *testing.T) {
	// Arrange
	uid := "user-abc-123"
	conv := "conv-xyz"
	in := `{"model":"glm-5.2","messages":[],"prompt_cache_key":"client-set-key"}`

	// Act
	out := InjectPromptCacheKey([]byte(in), uid, conv)

	// Assert
	if got := extractCacheKey(t, out); got != "client-set-key" {
		t.Fatalf("existing prompt_cache_key overwritten: got=%q want client-set-key", got)
	}
}

// TestInjectPromptCacheKey_UsesConversationID 覆盖任务用例 2：
// body 无 prompt_cache_key 但有 conversation_id → 用它做会话哈希源。
// 格式 wb2a-<uid8>-<conversationHash>：conversation_id 不逐字出现，而是驱动哈希段
// （同 id 同 key、不同 id 不同 key）。同时仍带 uid8 隔离前缀。
func TestInjectPromptCacheKey_UsesConversationID(t *testing.T) {
	uid := "user-abc-123"
	in := `{"model":"glm-5.2","messages":[],"conversation_id":"conv-42"}`

	out := InjectPromptCacheKey([]byte(in), uid, "")
	got := extractCacheKey(t, out)
	if !strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("expected wb2a- prefix, got=%q", got)
	}
	if !strings.Contains(got, "user-abc") {
		t.Fatalf("expected uid8 isolation prefix, got=%q", got)
	}
	// 同 conversation_id 两次出站 → 同 key（稳定）。
	out2 := InjectPromptCacheKey([]byte(in), uid, "")
	if got2 := extractCacheKey(t, out2); got2 != got {
		t.Fatalf("non-deterministic key for same conversation_id: %q vs %q", got, got2)
	}
	// 不同 conversation_id → 不同 key（哈希段区分）。
	inB := `{"model":"glm-5.2","messages":[],"conversation_id":"conv-99"}`
	gotB := extractCacheKey(t, InjectPromptCacheKey([]byte(inB), uid, ""))
	if gotB == got {
		t.Fatalf("different conversation_id produced same key: %q", got)
	}
}

// TestInjectPromptCacheKey_GeneratesStableKey 覆盖任务用例 3：
// body 两者都无 → 网关生成稳定键，两次同输入同账号得到同 key。
func TestInjectPromptCacheKey_GeneratesStableKey(t *testing.T) {
	uid := "user-abc-123"
	conv := "conv-stable"
	in := `{"model":"glm-5.2","messages":[]}`

	a := InjectPromptCacheKey([]byte(in), uid, conv)
	b := InjectPromptCacheKey([]byte(in), uid, conv)
	ka, kb := extractCacheKey(t, a), extractCacheKey(t, b)
	if ka != kb {
		t.Fatalf("non-deterministic key for same input+uid: a=%q b=%q", ka, kb)
	}
}

// TestInjectPromptCacheKey_AccountIsolation 覆盖任务用例 4：
// 不同账号 → key 不同（隔离验证）。
func TestInjectPromptCacheKey_AccountIsolation(t *testing.T) {
	conv := "conv-shared"
	in := `{"model":"glm-5.2","messages":[]}`

	ka := extractCacheKey(t, InjectPromptCacheKey([]byte(in), "user-aaa-111", conv))
	kb := extractCacheKey(t, InjectPromptCacheKey([]byte(in), "user-bbb-222", conv))
	if ka == kb {
		t.Fatalf("cross-account key collision: both=%q", ka)
	}
	if !strings.HasPrefix(ka, "wb2a-") || !strings.HasPrefix(kb, "wb2a-") {
		t.Fatalf("keys must carry wb2a- prefix: a=%q b=%q", ka, kb)
	}
}

// TestInjectPromptCacheKey_Format 覆盖任务用例 5：
// cache key 格式校验（含 uid8 前缀）。
func TestInjectPromptCacheKey_Format(t *testing.T) {
	uid := "1234567890abcdef"
	in := `{"model":"glm-5.2","messages":[]}`
	out := InjectPromptCacheKey([]byte(in), uid, "conv-fmt")
	got := extractCacheKey(t, out)
	if !strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("expected wb2a- prefix, got=%q", got)
	}
	if !strings.Contains(got, "12345678") {
		t.Fatalf("expected uid8 (12345678) in key, got=%q", got)
	}
}

// TestInjectPromptCacheKey_EmptyConversation 边界：无 conversation_id、无入参会话标识、
// 无 user_id、无首条 user 文本 → 仍生成非空键（含 uid8 隔离段）。
//
// 注意本用例的 body 是 `{"messages":[]}`——**没有 user 消息**，故第 4 条兜底
// （StickyFallbackKey 取首条 user 文本）也派生不出会话段，convHex 落定值。
// 这是刻意的下限：判不出会话就不猜（不编造会话标识）。
func TestInjectPromptCacheKey_EmptyConversation(t *testing.T) {
	uid := "user-abc-123"
	in := `{"model":"glm-5.2","messages":[]}`
	out := InjectPromptCacheKey([]byte(in), uid, "")
	got := extractCacheKey(t, out)
	if got == "" {
		t.Fatalf("expected non-empty key when no conversation, got empty")
	}
	if !strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("expected wb2a- prefix, got=%q", got)
	}
	if !strings.Contains(got, "user-abc") {
		t.Fatalf("expected uid8 in key, got=%q", got)
	}
}

// TestInjectPromptCacheKey_NoConversationFallsBackToSession 无 conversation_id 与
// 无入站会话标识时，用**首条 user 消息**派生的会话级键兜底（fork linguo2625469
// 20254d8 报告的 bug 的我们侧修法）。
//
// 为什么必须有：Anthropic / Claude Code 流量既无 conversation_id 也无
// X-Conversation-ID，不兜底则 convHex 退化成「只由 uid 决定的定值」= 每账号一个
// 恒定 cache key，同账号下所有会话共用一个上游前缀缓存槽互相覆盖 → 命中不稳定、
// 常整段 miss。修法是按会话区分，而不是按账号恒定。
//
// 四条断言各挡一类回归：
//   - 同会话（同首条 user 文本）两次出站 → 同键（否则每次请求都 miss，优化归零）；
//   - 不同会话（首条 user 文本不同）→ 不同键（这正是本修的核心：会话间不再互相覆盖）；
//   - 跨账号同会话 → 不同键（uid8 硬隔离不得被兜底路径绕过）；
//   - 会话内追加历史（首条不变、末尾新增）→ 仍同键（兜底取的是首条而非末条）。
func TestInjectPromptCacheKey_NoConversationFallsBackToSession(t *testing.T) {
	uid := "user-abc-123"
	convA := `{"model":"glm-5.2","messages":[{"role":"user","content":"first question"}]}`
	convB := `{"model":"glm-5.2","messages":[{"role":"user","content":"another question"}]}`

	ka := extractCacheKey(t, InjectPromptCacheKey([]byte(convA), uid, ""))
	ka2 := extractCacheKey(t, InjectPromptCacheKey([]byte(convA), uid, ""))
	if ka != ka2 {
		t.Fatalf("same session produced different keys: %q vs %q", ka, ka2)
	}
	kb := extractCacheKey(t, InjectPromptCacheKey([]byte(convB), uid, ""))
	if ka == kb {
		t.Fatalf("different sessions produced same key %q（会话间仍互相覆盖）", ka)
	}
	// 跨账号：同一会话文本换账号必须换键（uid 是硬隔离因子）。
	kc := extractCacheKey(t, InjectPromptCacheKey([]byte(convA), "other-user-999", ""))
	if ka == kc {
		t.Fatalf("cross-account key collision: both=%q", ka)
	}
	// 会话内追加历史：首条 user 不变 → 键不变。
	grown := `{"model":"glm-5.2","messages":[{"role":"user","content":"first question"},` +
		`{"role":"assistant","content":"answer"},{"role":"user","content":"follow up"}]}`
	kd := extractCacheKey(t, InjectPromptCacheKey([]byte(grown), uid, ""))
	if ka != kd {
		t.Fatalf("key changed as history grew: %q vs %q（兜底取首条 user，不该随追加漂移）", ka, kd)
	}
}

// TestInjectPromptCacheKey_UserIDSuppressesFallback body 带 user_id 时**不**走
// 首条 user 兜底——StickyFallbackKey 的抑制契约（user 维度粒度过粗，会把一个
// user 的全部并行对话钉到同一缓存槽，比按账号恒定好不了多少）。
//
// 抑制后回落定值键：可接受的下限（判不出来就不猜），关键是**不得**因为带了
// user_id 就借首条 prompt 重新获得会话粒度。
func TestInjectPromptCacheKey_UserIDSuppressesFallback(t *testing.T) {
	uid := "user-abc-123"
	a := `{"model":"glm-5.2","user_id":"u-1","messages":[{"role":"user","content":"q1"}]}`
	b := `{"model":"glm-5.2","user_id":"u-1","messages":[{"role":"user","content":"q2"}]}`

	ka := extractCacheKey(t, InjectPromptCacheKey([]byte(a), uid, ""))
	kb := extractCacheKey(t, InjectPromptCacheKey([]byte(b), uid, ""))
	if ka != kb {
		t.Fatalf("user_id body must fall back to a constant key, got %q vs %q（兜底绕过了抑制契约）", ka, kb)
	}
	if ka == "" {
		t.Fatal("key must still be injected (non-empty)")
	}
}

// TestInjectPromptCacheKey_ExplicitConversationBeatsFallback 显式 conversation_id
// 优先于首条 user 兜底：两者都在场时按 conversation_id 分键（不因 prompt 文本相同
// 而合并两个不同会话）。
func TestInjectPromptCacheKey_ExplicitConversationBeatsFallback(t *testing.T) {
	uid := "user-abc-123"
	// 同一段 prompt 文本，但 conversation_id 不同 → 必须分键。
	a := `{"model":"glm-5.2","conversation_id":"c-1","messages":[{"role":"user","content":"same"}]}`
	b := `{"model":"glm-5.2","conversation_id":"c-2","messages":[{"role":"user","content":"same"}]}`

	ka := extractCacheKey(t, InjectPromptCacheKey([]byte(a), uid, ""))
	kb := extractCacheKey(t, InjectPromptCacheKey([]byte(b), uid, ""))
	if ka == kb {
		t.Fatalf("explicit conversation_id ignored in favor of prompt fallback: both=%q", ka)
	}
}

// TestPrepareBodyOptNoCacheKeyInjection 覆盖任务用例 6：
// PrepareBodyOpt（sanitize=false 的旧入口）行为不变——不注入 cache key。
// 向后兼容：仅传 body 不给 cacheKey 上下文时，不得引入新字段。
func TestPrepareBodyOptNoCacheKeyInjection(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[]}`
	out := PrepareBodyOpt([]byte(in), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := obj["prompt_cache_key"]; present {
		t.Fatalf("PrepareBodyOpt must NOT inject prompt_cache_key, but got: %v", obj["prompt_cache_key"])
	}
}
