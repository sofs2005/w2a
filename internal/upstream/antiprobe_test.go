package upstream

import (
	"strings"
	"testing"
)

// bareAntiProbeCode 以上游线上实测形态构造业务码："11" + "128" 五位裸数字，
// 用码点拼接而非字面量——源码里的连字符在不同渲染/编辑链路上极易混淆，
// 而这条测试的全部价值就在于区分「带不带连字符」。
func bareAntiProbeCode() string {
	return "11" + string(rune(49)) + string(rune(50)) + string(rune(56))
}

// hyphenAntiProbeCode 历史误认形态："11" + "-" + "128"。
func hyphenAntiProbeCode() string {
	return "11" + string(rune(45)) + "128"
}

// TestIsAntiProbeBlockRealUpstreamShape 回归：上游真实返回的 error body 是
// `{"code":11-128,...}`（无连字符，od 实测字节 22 3a 31 31 31 32 38）。
// 早先按带连字符形态实现 hitAntiProbe，导致真实拦截被放行到 accountFaultRule →
// ErrAccountFault → handler 硬禁用 → 整池被打空、后续全 503。
func TestIsAntiProbeBlockRealUpstreamShape(t *testing.T) {
	bare := `{"code":` + bareAntiProbeCode() + `,"msg":"request illegal"}`
	hy := `{"code":` + hyphenAntiProbeCode() + `,"msg":"request illegal"}`

	if !IsAntiProbeBlock(bare) {
		t.Fatalf("真实上游形态 %q 未被识别为反探测拦截", bare)
	}
	// 带连字符形态不再产生，但保留兼容，避免上游偶发变体退化回硬禁用。
	if !IsAntiProbeBlock(hy) {
		t.Fatalf("兼容形态 %q 未被识别为反探测拦截", hy)
	}
	if got := Classify(400, bare); got != ErrContentBlocked {
		t.Fatalf("Classify(400, 真实形态)=%v want ErrContentBlocked", got)
	}
	// 11140 真封禁必须仍走 accountFault，不能被上面两支持续误伤。
	if got := Classify(403, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`); got != ErrAccountFault {
		t.Fatalf("11140 真封禁被误分类为 %v", got)
	}
}

// TestBareAndHyphenCodesDiffer 守住测试自身的前提：两种构造确实不同的字节序列。
// 若这条失败，说明构造方式被渲染/编辑链路改写，上面那条测试也就失去意义。
func TestBareAndHyphenCodesDiffer(t *testing.T) {
	bare := bareAntiProbeCode()
	hy := hyphenAntiProbeCode()
	if bare == hy {
		t.Fatalf("构造失败：两种形态相同 %q", bare)
	}
	if strings.Contains(bare, "-") {
		t.Fatalf("裸数字形态被改写成带连字符：%q", bare)
	}
	if !strings.Contains(hy, "-") {
		t.Fatalf("兼容形态构造失败：%q", hy)
	}
}
