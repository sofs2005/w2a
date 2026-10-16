// account_admin.go 账号运维（停用 / 启用 / 复活）的进程内实现。
//
// 为什么不再走 HTTP：这些操作原先经面板 HTTP 客户端打网关 /admin/accounts/{uid}/*
// 端点，而该端点与 /v1 共用同一把 api_key——任何持有 key 的下游都能把账号摘出选号池。
// 现在宿主（workbuddy2api）与面板同进程，直接在装配处把池桥进来即可：
//
//	面板 /api/accounts/{uid}/disable（session 鉴权）
//	  → ops.Service → AccountAdmin 接口 → *pool.Pool
//
// 网关 mux 上不再注册 /admin/ 整组路由，curl + api_key 无路可走。
//
// 接口定义在消费方（本包）而非实现方：Go internal 规则不允许 workbuddy2api-gui
// module import workbuddy2api/internal/pool，故由宿主在 cmd/server 里以适配器实现。
package ops

import (
	"context"
	"errors"
	"fmt"
)

// ErrAdminUnavailable 面板未接入网关账号管理能力。
//
// 独立面板模式（panel/cmd/server 连远端网关）下会命中：那种部署没有同进程的池，
// 停用/启用/复活无从下手。文案要说清是"部署形态不支持"，而不是让人去翻配置。
var ErrAdminUnavailable = errors.New("当前部署形态不支持该操作：面板未与网关同进程运行")

// ErrAccountNotFound 目标 uid 不在网关账号池中（api 层据此映射 404）。
var ErrAccountNotFound = errors.New("账号不存在于网关账号池中")

// AccountAdmin 网关侧的账号管理能力（宿主进程内注入）。
//
// 四个方法一一对应 pool 上的既有实现，签名刻意保持逐字一致，宿主侧适配器可以
// 写成一行转发而不做任何转换（避免两处口径漂移）。
type AccountAdmin interface {
	// SetManualDisabled 置/清手动停用位，返回 (found, changed)。
	SetManualDisabled(uid string, disabled bool, reason string) (found, changed bool)
	// ManualDisabledState 读手动停用位，uid 不存在时 ok=false。
	ManualDisabledState(uid string) (disabled bool, reason string, ok bool)
	// ReviveDisabled 解除系统自动禁用（不动手动位），返回是否确实清除。
	ReviveDisabled(uid string) bool
	// AccountStatus 读系统自动禁用位（回显用），uid 不存在时 ok=false。
	AccountStatus(uid string) (disabled bool, ok bool)
}

// AdminState 停用/启用/复活的统一回显（面板据此直接更新 UI，不必再打一次 /status）。
//
// 字段与前端 types.ts 的 AdminState 一一对应；改成进程内调用后**形状不变**，
// 前端契约零改动。
type AdminState struct {
	UID            string `json:"uid"`
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason,omitempty"`
	// Disabled 系统自动禁用位。启用（enable）后它可能仍为真——那说明账号还被系统
	// 判死，需先解决故障再 revive。面板据此给「已恢复但仍被禁用」的提示。
	Disabled bool `json:"disabled"`
	// Changed 本次调用是否真的改动了状态（幂等重试时为 false）。
	Changed bool `json:"changed"`
}

// SetAccountAdmin 注入网关侧账号管理能力（宿主同进程部署时调用）。
// 未注入时三个操作都返回 ErrAdminUnavailable——不静默失败，也不假装成功。
func (s *Service) SetAccountAdmin(a AccountAdmin) { s.admin = a }

// adminOrErr 取注入的管理能力，未注入时给出可读错误。
func (s *Service) adminOrErr() (AccountAdmin, error) {
	if s.admin == nil {
		return nil, ErrAdminUnavailable
	}
	return s.admin, nil
}

// DisableAccount 手动停用账号（摘出对话流量）。日常任务（签到/保活/猫猫）照常，
// 只有对话选号不再用该号——语义完全在网关侧的池里实现（pool.manualDisabled），
// 面板只做转发，不复制判断。
func (s *Service) DisableAccount(_ context.Context, uid, reason string) (*AdminState, error) {
	if err := s.ensureWritable(); err != nil {
		return nil, err
	}
	admin, err := s.adminOrErr()
	if err != nil {
		return nil, err
	}
	if reason == "" {
		reason = "manual" // 与网关端点时代的默认文案一致（无体调用是最常见形态）
	}
	found, changed := admin.SetManualDisabled(uid, true, reason)
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, uid)
	}
	return s.adminStateOf(admin, uid, changed), nil
}

// EnableAccount 解除手动停用。只解 manualDisabled 位，**不**复活系统禁用——
// 返回的 AdminState.Disabled 若仍为真，说明账号还被系统判死，需另行 revive。
func (s *Service) EnableAccount(_ context.Context, uid string) (*AdminState, error) {
	if err := s.ensureWritable(); err != nil {
		return nil, err
	}
	admin, err := s.adminOrErr()
	if err != nil {
		return nil, err
	}
	found, changed := admin.SetManualDisabled(uid, false, "")
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, uid)
	}
	return s.adminStateOf(admin, uid, changed), nil
}

// ReviveAccount 解除系统自动禁用（清 disabled + reason + 连续 12153 计数）。
// 不碰手动停用位：运维明确摘除的号不应被一次 revive 悄悄放回选号池。
func (s *Service) ReviveAccount(_ context.Context, uid string) (*AdminState, error) {
	if err := s.ensureWritable(); err != nil {
		return nil, err
	}
	admin, err := s.adminOrErr()
	if err != nil {
		return nil, err
	}
	// 先判存在性：ReviveDisabled 对不存在的 uid 返回 false，与「本来就是好的」
	// 无法区分，故用 ManualDisabledState 的存在性作为 uid 是否在池中的判据。
	if _, _, ok := admin.ManualDisabledState(uid); !ok {
		return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, uid)
	}
	changed := admin.ReviveDisabled(uid)
	return s.adminStateOf(admin, uid, changed), nil
}

// adminStateOf 组装操作后的双位回显（两个状态位各读一次，口径与网关端点时代一致）。
func (s *Service) adminStateOf(admin AccountAdmin, uid string, changed bool) *AdminState {
	manual, reason, _ := admin.ManualDisabledState(uid)
	auto, _ := admin.AccountStatus(uid)
	return &AdminState{
		UID:            uid,
		ManualDisabled: manual,
		ManualReason:   reason,
		Disabled:       auto,
		Changed:        changed,
	}
}
