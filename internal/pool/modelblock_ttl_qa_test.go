package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 11102 负缓存 TTL 的**字面值**护栏（2026-10-28 决策：首次 2m、封顶 10m）。
//
// 与 TestBlockModelBackoffExponentialTtl 的分工：那条按常量推算（改了常量它自动跟随，
// 保证「代码与自己的常量一致」），本用例把**常量本身**钉成字面值（保证「常量与线上
// 决策一致」）。两者缺一不可：
//   - 只有按常量推算 → 有人把 base 改回 6h，测试照绿，而线上退回到「一个模型锁死
//     整池一整天」的形态（这正是本批同步要修的问题）；
//   - 只有字面值 → 常量与使用处（blockModelTTL 之类的换算）不一致时测不出来。
//
// 行为面同时钉住：首次 ≈2m、二次 ≈4m、连打封顶 ≈10m 且**不超过**封顶值
// （超过说明封顶没生效，等于退避会一路翻倍到 2^6×2m=128m）。
func TestModelBlockTTLLiteralValues(t *testing.T) {
	const (
		wantBase = 2 * time.Minute
		wantMax  = 10 * time.Minute
	)
	if modelBlockBaseTTL != wantBase {
		t.Errorf("modelBlockBaseTTL=%v want %v（首次退避基数，2026-10-28 决策）", modelBlockBaseTTL, wantBase)
	}
	if modelBlockMaxTTL != wantMax {
		t.Errorf("modelBlockMaxTTL=%v want %v（封顶，2026-10-28 决策）", modelBlockMaxTTL, wantMax)
	}
	// 封顶必须真的比「基数按 shift 翻倍」小，否则 modelBlockShift 的存在没有意义
	// （注释里写的是 2m×2^6=128m > 10m，所以实际封顶锚定在 10m）。
	if maxByShift := modelBlockBaseTTL << modelBlockShift; maxByShift <= modelBlockMaxTTL {
		t.Errorf("base<<shift=%v 应大于封顶 %v，否则封顶分支不可达", maxByShift, modelBlockMaxTTL)
	}

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	ttl := func() time.Duration {
		t.Helper()
		p.mu.RLock()
		defer p.mu.RUnlock()
		mc, ok := p.byUID["u1"].modelCooldowns["m"]
		if !ok {
			t.Fatalf("u1 上没有 model=m 的负缓存条目")
		}
		return time.Until(mc.Until)
	}
	near := func(got, want time.Duration, what string) {
		t.Helper()
		if d := got - want; d > 2*time.Second || d < -2*time.Second {
			t.Errorf("%s TTL=%v want ~%v", what, got, want)
		}
	}

	p.BlockModelBackoff("u1", "m", "11102 model not available")
	near(ttl(), wantBase, "首次")

	p.BlockModelBackoff("u1", "m", "11102 model not available")
	near(ttl(), 2*wantBase, "二次")

	for i := 0; i < 10; i++ {
		p.BlockModelBackoff("u1", "m", "11102 model not available")
	}
	got := ttl()
	near(got, wantMax, "连打封顶")
	// 封顶是上界，不是「大约」：超过 1 秒即说明封顶失效（会一路翻倍到 128m）。
	if got > wantMax+time.Second {
		t.Errorf("连打后 TTL=%v 超过封顶 %v（封顶失效）", got, wantMax)
	}
}
