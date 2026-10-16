package ops

import (
	"context"
	"errors"
	"testing"

	"workbuddy2api-gui/internal/config"
)

// stubAdmin 记录调用参数的极简桥实现，供 ops 层单测使用。
type stubAdmin struct {
	calls []string
	// found=false 模拟 uid 不在池中。
	found bool
	// manual / auto 为回显用的状态位。
	manual bool
	auto   bool
}

func (s *stubAdmin) SetManualDisabled(uid string, disabled bool, reason string) (bool, bool) {
	s.calls = append(s.calls, "set")
	s.manual = disabled
	return s.found, true
}

func (s *stubAdmin) ManualDisabledState(string) (bool, string, bool) {
	return s.manual, "", s.found
}

func (s *stubAdmin) ReviveDisabled(string) bool {
	s.calls = append(s.calls, "revive")
	changed := s.auto
	s.auto = false
	return changed
}

func (s *stubAdmin) AccountStatus(string) (bool, bool) { return s.auto, s.found }

// newTestService 构造只带必要配置的 Service（桥随后由用例注入）。
func newTestService(readOnly bool) *Service {
	cfg := config.Default()
	cfg.ReadOnly = readOnly
	return New(cfg, nil, nil, nil)
}

// TestAccountAdminUnavailable 宿主未注入桥时（独立面板模式）三个操作都必须返回
// ErrAdminUnavailable，且不 panic——不静默失败，也不假装成功。
func TestAccountAdminUnavailable(t *testing.T) {
	s := newTestService(false)
	ctx := context.Background()

	if _, err := s.DisableAccount(ctx, "u1", ""); !errors.Is(err, ErrAdminUnavailable) {
		t.Errorf("DisableAccount err=%v want ErrAdminUnavailable", err)
	}
	if _, err := s.EnableAccount(ctx, "u1"); !errors.Is(err, ErrAdminUnavailable) {
		t.Errorf("EnableAccount err=%v want ErrAdminUnavailable", err)
	}
	if _, err := s.ReviveAccount(ctx, "u1"); !errors.Is(err, ErrAdminUnavailable) {
		t.Errorf("ReviveAccount err=%v want ErrAdminUnavailable", err)
	}
}

// TestAccountAdminReadOnlyPrecedesBridge 只读守卫必须先于桥调用：只读面板不该
// 有机会碰到池。
func TestAccountAdminReadOnlyPrecedesBridge(t *testing.T) {
	s := newTestService(true)
	stub := &stubAdmin{found: true}
	s.SetAccountAdmin(stub)
	ctx := context.Background()

	for name, err := range map[string]error{
		"disable": func() error { _, err := s.DisableAccount(ctx, "u1", ""); return err }(),
		"enable":  func() error { _, err := s.EnableAccount(ctx, "u1"); return err }(),
		"revive":  func() error { _, err := s.ReviveAccount(ctx, "u1"); return err }(),
	} {
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s err=%v want ErrReadOnly", name, err)
		}
	}
	if len(stub.calls) != 0 {
		t.Errorf("只读模式下不应调桥: %v", stub.calls)
	}
}

// TestDisableAccountDefaultsReason 空 reason 落到池里必须是可读的默认文案，
// 否则面板上「为什么停的」永远空白。
func TestDisableAccountDefaultsReason(t *testing.T) {
	s := newTestService(false)
	var gotReason string
	s.SetAccountAdmin(reasonRecorder{&gotReason})

	if _, err := s.DisableAccount(context.Background(), "u1", ""); err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}
	if gotReason != "manual" {
		t.Errorf("默认 reason=%q want manual", gotReason)
	}
}

// TestDisableAccountKeepsGivenReason 显式传入的 reason 必须原样透传，不被默认值覆盖。
func TestDisableAccountKeepsGivenReason(t *testing.T) {
	s := newTestService(false)
	var gotReason string
	s.SetAccountAdmin(reasonRecorder{&gotReason})

	if _, err := s.DisableAccount(context.Background(), "u1", "观察几天"); err != nil {
		t.Fatalf("DisableAccount: %v", err)
	}
	if gotReason != "观察几天" {
		t.Errorf("reason=%q want 观察几天", gotReason)
	}
}

// TestReviveAccountIgnoresManualBit 复活只清系统位：运维明确摘除的号不该被一次
// revive 悄悄放回选号池。
func TestReviveAccountIgnoresManualBit(t *testing.T) {
	s := newTestService(false)
	stub := &stubAdmin{found: true, manual: true, auto: true}
	s.SetAccountAdmin(stub)

	st, err := s.ReviveAccount(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if !st.ManualDisabled {
		t.Error("revive 不应清除手动停用位")
	}
	if st.Disabled {
		t.Error("revive 应清除系统自动禁用位")
	}
}

// TestAccountOpsUnknownUID 桥回 found=false 时必须报「不在池中」类错误（api 层
// 据此映射 404），而不是成功。
func TestAccountOpsUnknownUID(t *testing.T) {
	s := newTestService(false)
	s.SetAccountAdmin(&stubAdmin{found: false})
	ctx := context.Background()

	for name, err := range map[string]error{
		"disable": func() error { _, err := s.DisableAccount(ctx, "u9", ""); return err }(),
		"enable":  func() error { _, err := s.EnableAccount(ctx, "u9"); return err }(),
		"revive":  func() error { _, err := s.ReviveAccount(ctx, "u9"); return err }(),
	} {
		if err == nil {
			t.Fatalf("%s: 期望报错", name)
		}
		if !errors.Is(err, ErrAccountNotFound) {
			t.Errorf("%s err=%v want ErrAccountNotFound", name, err)
		}
	}
}

// reasonRecorder 只关心 SetManualDisabled 收到的 reason。
type reasonRecorder struct{ got *string }

func (r reasonRecorder) SetManualDisabled(_ string, _ bool, reason string) (bool, bool) {
	*r.got = reason
	return true, true
}
func (reasonRecorder) ManualDisabledState(string) (bool, string, bool) { return false, "", true }
func (reasonRecorder) ReviveDisabled(string) bool                      { return false }
func (reasonRecorder) AccountStatus(string) (bool, bool)               { return false, true }
