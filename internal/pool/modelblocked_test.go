package pool

import (
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestModelBlockedCauseDistinguishesRateLimit 6004 与 11102 共用 modelCooldowns，
// 但性质相反：限流（该等）vs 无此模型（该换）。Cause 是调用方唯一的分流依据，
// 判据是 ResetAt —— 只有 6004 带得上游重置墙钟。
func TestModelBlockedCauseDistinguishesRateLimit(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	reset := time.Now().Add(time.Hour)

	// 6004：经真实入口写入（带 ResetAt）。
	p.CooldownSoftForModel("u1", time.Minute, reset, "glm-5.3", "6004 model rate limit")
	p.CooldownSoftForModel("u2", time.Minute, reset, "glm-5.3", "6004 model rate limit")
	if st := p.ModelBlocked("glm-5.3"); st.Cause != BlockRateLimited {
		t.Errorf("6004 应判为限流（该等重置），got cause=%v status=%+v", st.Cause, st)
	}

	// 11102：经真实入口写入（无 ResetAt）。
	q := New("")
	q.Add(&auth.Auth{UID: "u1"})
	q.BlockModelBackoff("u1", "glm-5.3", "11102 model not available")
	if st := q.ModelBlocked("glm-5.3"); st.Cause != BlockUnavailable {
		t.Errorf("11102 应判为不可用（该换模型），got cause=%v status=%+v", st.Cause, st)
	}
}

// TestModelBlockedCauseMixedPrefersRateLimit 混合场景（一号限流、一号无此模型）：
// 整体报限流。限流窗口会自行到期，报「等到 X」对客户端成立；报「模型不可用」
// 会把它推去换模型，掩盖了本可以等回来的事实。
func TestModelBlockedCauseMixedPrefersRateLimit(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004 model rate limit")
	p.BlockModelBackoff("u2", "glm-5.3", "11102 model not available")

	st := p.ModelBlocked("glm-5.3")
	if !st.Blocked {
		t.Fatalf("两号都不可服务 → 应判为阻塞: %+v", st)
	}
	if st.Cause != BlockRateLimited {
		t.Errorf("混合时应判为限流（有可等回来的窗口），got %v", st.Cause)
	}
}

// TestModelBlockedAllCooled 全池都对该模型冷却 → Blocked=true，并带出账号数、
// 最早解封时间与上游原因（客户端据此决定「换模型」而不是「重试」）。
func TestModelBlockedAllCooled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	later := time.Now().Add(3 * time.Hour)
	sooner := time.Now().Add(1 * time.Hour)

	p.mu.Lock()
	p.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: later, Reason: "11102 model not available"},
	}
	p.byUID["u2"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: sooner, Reason: "11102 model not available"},
	}
	p.mu.Unlock()

	st := p.ModelBlocked("glm-5.3")
	if !st.Blocked {
		t.Fatalf("全池冷却应判为模型级阻塞，got %+v", st)
	}
	if st.Count != 2 {
		t.Errorf("Count=%d want 2", st.Count)
	}
	// 解封时间是**最早**的那个：客户端要的是「什么时候可能好」。
	if d := st.Until.Sub(sooner); d > time.Minute || d < -time.Minute {
		t.Errorf("Until=%v want≈%v（应取最早解封时刻）", st.Until, sooner)
	}
	if !strings.Contains(st.Reason, "11102") {
		t.Errorf("Reason=%q 应带上游原文", st.Reason)
	}
}

// TestModelBlockedOneStillServes 只要有一个号还能服务该模型，就不是全池阻塞——
// 此时选号失败另有原因（在途占满/积分保底/账号级冷却），报 model_unavailable 是错的。
func TestModelBlockedOneStillServes(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.mu.Lock()
	p.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(time.Hour), Reason: "11102 model not available"},
	}
	p.mu.Unlock()

	if st := p.ModelBlocked("glm-5.3"); st.Blocked {
		t.Errorf("u2 仍可服务该模型，不得判为全池阻塞: %+v", st)
	}
}

// TestModelBlockedIgnoresDisabled 停用号的模型冷却不计入：它的不可用与模型无关，
// 若计入则「只剩停用号」会被误报成全池模型阻塞。
func TestModelBlockedIgnoresDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.mu.Lock()
	p.byUID["u1"].disabled = true
	p.byUID["u2"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(time.Hour), Reason: "11102 model not available"},
	}
	p.mu.Unlock()

	st := p.ModelBlocked("glm-5.3")
	if !st.Blocked {
		t.Fatalf("唯一可用号也对模型冷却 → 应为阻塞，got %+v", st)
	}
	if st.Count != 1 {
		t.Errorf("Count=%d want 1（停用号不参与计数）", st.Count)
	}
}

// TestModelBlockedIgnoresManualDisabled 运维手动停用号同样不参与判定（本仓
// manualDisabled 与 disabled 是并列的两个状态位，见 issue #138/#118）。
// 只查 disabled 会把「唯一可用号被手动摘流量」误报成模型问题。
func TestModelBlockedIgnoresManualDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	if found, _ := p.SetManualDisabled("u2", true, "运维摘流量"); !found {
		t.Fatal("fixture: 设置手动停用失败")
	}

	p.mu.Lock()
	p.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(time.Hour), Reason: "11102 model not available"},
	}
	p.mu.Unlock()

	st := p.ModelBlocked("glm-5.3")
	if !st.Blocked {
		t.Fatalf("唯一在役号也对模型冷却 → 应判为阻塞，got %+v", st)
	}
	if st.Count != 1 {
		t.Errorf("Count=%d want 1（手动停用号不计入）", st.Count)
	}
}

// TestModelBlockedExpiredNotBlocking 冷却已到期不算阻塞：否则解封后客户端仍被
// 劝去换模型，而实际上池子已经能服务了。
func TestModelBlockedExpiredNotBlocking(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	p.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(-time.Minute), Reason: "11102 model not available"},
	}
	p.mu.Unlock()

	if st := p.ModelBlocked("glm-5.3"); st.Blocked {
		t.Errorf("冷却已过期不得判为阻塞: %+v", st)
	}
}

// TestModelBlockedEmptyInputs 空模型名 / 空池返回零值：
// 空模型名无从判定（选号此时也不做模型过滤），空池的不可用与模型无关。
func TestModelBlockedEmptyInputs(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if st := p.ModelBlocked(""); st.Blocked {
		t.Errorf("空模型名应返回零值: %+v", st)
	}

	empty := New("")
	if st := empty.ModelBlocked("glm-5.3"); st.Blocked {
		t.Errorf("空池应返回零值（无账号 ≠ 模型阻塞）: %+v", st)
	}
}

// TestModelBlockedClearsAfterSuccess 口径跟选号走：BlockModelClear 之后
// ModelBlocked 必须同步转为不阻塞（同一谓词 modelCooled，不得各写一份）。
func TestModelBlockedClearsAfterSuccess(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "glm-5.3", "11102 model not available")

	if st := p.ModelBlocked("glm-5.3"); !st.Blocked {
		t.Fatalf("写入负缓存后应为阻塞，got %+v", st)
	}
	p.BlockModelClear("u1", "glm-5.3")
	if st := p.ModelBlocked("glm-5.3"); st.Blocked {
		t.Errorf("清除负缓存后不得再判为阻塞: %+v", st)
	}
}
