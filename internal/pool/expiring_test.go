package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestExpiringCreditBoostsWeight 快过期积分占比高的号权重大于占比低/无的号（同总量下）。
func TestExpiringCreditBoostsWeight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "expiring-heavy"})          // 总量相同,快过期占比高
	p.Add(&auth.Auth{UID: "stable-heavy"})            // 总量相同,快过期占比低
	p.SetCreditsDetailed("expiring-heavy", 1000, 900) // 90% 快过期
	p.SetCreditsDetailed("stable-heavy", 1000, 50)    // 5% 快过期

	p.mu.RLock()
	eExp := p.byUID["expiring-heavy"]
	eSta := p.byUID["stable-heavy"]
	maxC := int64(1000)
	now := time.Now()
	wExp := p.weightOf(eExp, maxC, now)
	wSta := p.weightOf(eSta, maxC, now)
	p.mu.RUnlock()

	if wExp <= wSta {
		t.Errorf("expiring-heavy weight %.3f <= stable-heavy %.3f; 快过期积分应加权", wExp, wSta)
	}
	// 差值应约等于 (0.9-0.05)*expiringWeight = 0.85*8 = 6.8
	diff := wExp - wSta
	if diff < 6.0 || diff > 7.5 {
		t.Errorf("weight diff %.3f, want ~6.8 (0.85*expiringWeight)", diff)
	}
}

// TestExpiringClampedToCredits expiring 超过总量/负值时被钳制,不污染权重。
func TestExpiringClampedToCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 100, 9999) // expiring > credits
	p.mu.RLock()
	if p.byUID["u1"].creditsExpiring != 100 {
		t.Errorf("creditsExpiring=%d want 100 (clamped to credits)", p.byUID["u1"].creditsExpiring)
	}
	p.mu.RUnlock()

	p.SetCreditsDetailed("u1", 100, -5) // 负值
	p.mu.RLock()
	if p.byUID["u1"].creditsExpiring != 0 {
		t.Errorf("creditsExpiring=%d want 0 (negative clamped)", p.byUID["u1"].creditsExpiring)
	}
	p.mu.RUnlock()
}

// TestSetCreditsLeavesExpiringUnchanged 旧 SetCredits 只更新总量,不清 expiring(向后兼容)。
func TestSetCreditsLeavesExpiringUnchanged(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 500, 200)
	p.SetCredits("u1", 600) // 旧入口只更新总量
	p.mu.RLock()
	e := p.byUID["u1"]
	if e.credits != 600 {
		t.Errorf("credits=%d want 600", e.credits)
	}
	if e.creditsExpiring != 200 {
		t.Errorf("creditsExpiring=%d want 200 (SetCredits 不应清)", e.creditsExpiring)
	}
	p.mu.RUnlock()
}

// TestSetCreditsClampsExpiring 权威余额低于 expiring 时须钳到 [0, credits]：
// 维护「expiring 是 credits 子集」的不变量（applyAccountsLocked / SetCreditsDetailed
// / SetCredits 三处守卫齐全），否则 weightOf 的 expiring/credits 比值项会 >1，
// 把 ×expiringWeight 加成放大到远超设计上限。
func TestSetCreditsClampsExpiring(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 1000, 900) // 快过期占比 90%
	p.SetCredits("u1", 100)               // 权威余额降到 expiring 之下

	p.mu.RLock()
	e := p.byUID["u1"]
	if e.credits != 100 {
		t.Errorf("credits=%d want 100", e.credits)
	}
	if e.creditsExpiring != 100 {
		t.Errorf("creditsExpiring=%d want 100 (应被钳到 credits)", e.creditsExpiring)
	}
	// 不变量：expiring <= credits，比值项 <= 1。
	if e.creditsExpiring > e.credits {
		t.Fatalf("不变量破坏：expiring=%d > credits=%d", e.creditsExpiring, e.credits)
	}
	w := p.weightOf(e, 1000, time.Now())
	if w > 1+10+expiringWeight+defaultIdleWeightMax+1 {
		t.Errorf("weight=%.3f 超出设计上限（expiring 比值应 <= 1）", w)
	}
	p.mu.RUnlock()

	// 负余额钳 0。
	p.SetCredits("u1", -5)
	p.mu.RLock()
	if e := p.byUID["u1"]; e.credits != 0 || e.creditsExpiring != 0 {
		t.Errorf("负值应钳 0：credits=%d expiring=%d", e.credits, e.creditsExpiring)
	}
	p.mu.RUnlock()
}
