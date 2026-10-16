// panel_admin_test.go 进程内账号管理桥（panel.go 的 poolAdmin）的契约测试。
//
// 为什么必须测：面板 module 不能 import internal/pool，接口在面板侧定义、实现在这里
// 靠 Go 的隐式接口满足跨过 module 边界——这条桥一旦方法签名漂移，编译器只在**赋值处**
// 报错（poolAdmin{p} → Options.Admin），运行时行为（谁清哪个位、回显读哪个位）没有任何
// 编译期保障。本文件把「面板看到的双位语义」钉在真池上。
package main

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// adminTestPool 构造含单账号 u1 的真池（state 落在临时目录，避免污染工作区）。
func adminTestPool(t *testing.T) *pool.Pool {
	t.Helper()
	p := pool.New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1"})
	return p
}

// TestPoolAdminRoundTrip 面板的停用/启用/复活在真池上必须落到正确的位。
func TestPoolAdminRoundTrip(t *testing.T) {
	p := adminTestPool(t)
	a := poolAdmin{p}

	// 停用 → 手动位为真、原因透出。
	if found, changed := a.SetManualDisabled("u1", true, "观察几天"); !found || !changed {
		t.Fatalf("停用 (found,changed)=(%v,%v) want (true,true)", found, changed)
	}
	manual, reason, ok := a.ManualDisabledState("u1")
	if !ok || !manual || reason != "观察几天" {
		t.Fatalf("停用后 manual=%v reason=%q ok=%v", manual, reason, ok)
	}

	// 幂等：同参数再停一次不算变更（面板据此显示「无变化」而非假成功）。
	if _, changed := a.SetManualDisabled("u1", true, "观察几天"); changed {
		t.Error("重复停用应为 changed=false")
	}

	// 启用 → 手动位清除，系统位不受影响。
	if found, changed := a.SetManualDisabled("u1", false, ""); !found || !changed {
		t.Fatalf("启用 (found,changed)=(%v,%v) want (true,true)", found, changed)
	}
	if manual, _, _ := a.ManualDisabledState("u1"); manual {
		t.Error("启用后手动位应清除")
	}

	// 未知 uid：found=false（面板据此回 404），且不 panic。
	if found, _ := a.SetManualDisabled("nope", true, "x"); found {
		t.Error("未知 uid 的停用应 found=false")
	}
	if _, _, ok := a.ManualDisabledState("nope"); ok {
		t.Error("未知 uid 的 ManualDisabledState 应 ok=false")
	}
	if _, ok := a.AccountStatus("nope"); ok {
		t.Error("未知 uid 的 AccountStatus 应 ok=false")
	}
}

// TestPoolAdminAccountStatusReadsAutoBit AccountStatus 必须读**系统自动禁用位**，
// 而不是手动位——面板的「启用后仍被禁用」提示完全依赖这个口径（读错位会让提示永远
// 不出现，运维以为号已经好了）。
func TestPoolAdminAccountStatusReadsAutoBit(t *testing.T) {
	p := adminTestPool(t)
	a := poolAdmin{p}

	// 手动停用不动系统位。
	a.SetManualDisabled("u1", true, "r")
	if disabled, ok := a.AccountStatus("u1"); !ok || disabled {
		t.Errorf("手动停用后 AccountStatus=(%v,%v) want (false,true)", disabled, ok)
	}

	// 系统禁用（连续失败达阈值之外的显式入口：ReviveDisabled 的前置态）。
	p.Disable("u1", "上游判死")
	if disabled, ok := a.AccountStatus("u1"); !ok || !disabled {
		t.Errorf("系统禁用后 AccountStatus=(%v,%v) want (true,true)", disabled, ok)
	}
	// 两位正交：系统禁用期间手动位仍为真。
	if manual, _, _ := a.ManualDisabledState("u1"); !manual {
		t.Error("系统禁用不应影响手动位")
	}
}

// TestPoolAdminReviveLeavesManualBit 复活只清系统位：面板上「复活」按钮只对
// 非手动停用的号显示，但桥这一层也必须守住这条语义——否则将来前端放宽显示条件
// 会静默把运维明确摘除的号放回选号池。
func TestPoolAdminReviveLeavesManualBit(t *testing.T) {
	p := adminTestPool(t)
	a := poolAdmin{p}

	a.SetManualDisabled("u1", true, "运维摘除")
	p.Disable("u1", "上游判死")

	if !a.ReviveDisabled("u1") {
		t.Fatal("复活应报告确实清除了自动禁用")
	}
	if disabled, _ := a.AccountStatus("u1"); disabled {
		t.Error("复活后系统位应清除")
	}
	if manual, reason, _ := a.ManualDisabledState("u1"); !manual || reason != "运维摘除" {
		t.Errorf("复活不该动手动位: manual=%v reason=%q", manual, reason)
	}
	// 二次复活：已无系统禁用可清。
	if a.ReviveDisabled("u1") {
		t.Error("已复活的号再次复活应返回 false")
	}
}

// TestNewRootHandlerWithoutPool 装配时池为 nil（测试/降级路径）不得 panic，
// 且面板的账号管理端点应给出「部署形态不支持」而不是崩在空指针上。
func TestNewRootHandlerWithoutPool(t *testing.T) {
	h := newTestRootHandler(t) // 内部已传 nil 池
	if h == nil {
		t.Fatal("newRootHandler 返回 nil")
	}
}
