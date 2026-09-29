// 账号状态机迁移的唯一权威实现。
//
// entry 的「可选择性」由五个正交维度决定：禁用(disabled)、手动停用(manualDisabled)、
// 账号级冷却(until/coolKind)、模型级冷却(modelCooldowns)、熔断(breakerUntil)。
// 维度之间以「迁移原语」收拢，禁止在其他文件散写这些字段——所有入口（applyErrorPolicy /
// refresh / keepalive / 签到 / 选号 / 运维端点）对状态的改动都必须经本文件的原语或经
// Cooldown/NoteError/NoteSuccess 等封装（它们在持锁下调用本文件原语）。
//
// 迁移矩阵（事件 → 动作 → 字段）：
//
//	disabled           ← disableLocked（Disable / NoteSessionDead 达阈）
//	manualDisabled     ← setManualDisabledLocked（运维端点 / CLI；只置位不清其他维度）
//	until/coolKind     ← Cooldown(CoolSoft/Hard，固定时长) / CooldownSoftRate / CooldownSoftForModel 无解析分支
//	                     （reviveCoolingLocked 只解冻 CoolHard，软限流保留）
//	modelCooldowns     ← CooldownSoftForModel 有解析分支；被 disableLocked/Cooldown/CooldownSoftRate 清
//	                     （reviveCoolingLocked 不清——余额恢复不构成限流解除证据）
//	breakerUntil       ← recordBreakerFailureLocked（NoteError 喂入）；NoteSuccess 清
//	softStreak         ← CooldownSoftRate / CooldownSoftForModel 无解析分支；NoteSuccess 清
//	                     （reviveCoolingLocked 保留——退避计数与余额无关）
//	sessionDeadFails   ← NoteSessionDead；ClearSessionDead/NoteSuccess/ReviveDisabled 清
//
// 关键正交性（疑点 4 修正）：
//   - 冷却域（until/coolKind/softStreak/modelCooldowns）与熔断器（fails/retryCount/
//     breakerUntil）正交：冷却管「近期被限流/余额耗尽」，熔断管「反复 5xx 失败」。
//     disableLocked 只清冷却域、不动熔断——禁用是授权/session 终态，不应覆盖熔断观测。
//   - clearCoolingLocked 是「冷却域归零」的单一来源，只被 disableLocked 使用
//     （禁用是终态，冷却随之作废）。reviveCoolingLocked（签到/余额刷新解冻）**不再**
//     走全清：余额恢复只解冻 CoolHard，软限流退避与模型级台账各有自身恢复时刻
//     （详见 reviveCoolingLocked 注释）。
//   - manualDisabled 与 disabled 各自独立：前者是运维意图（只能由运维入口清除），
//     后者是系统判定（可被签到解冻/refresh 等路径自动撤销）。二者都不清对方，
//     并存时 /status 分别透出（见 entry.go Status.manual_disabled/disabled 注释）。
package pool

import "time"

// clearCoolingLocked 清冷却域：until/coolKind/softStreak/modelCooldowns 全归零，
// reason 一并清空。熔断器（fails/retryCount/breakerUntil）不属冷却域，不动。
// 调用方必须已持有 p.mu。
func (e *entry) clearCoolingLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // 冷却域清零时一并清模型级独立冷却（模型豁免随之消失）
}

// disableLocked 禁用迁移：置 disabled 并清冷却域（禁用是比冷却更强的不可用终态）。
//
// 疑点 4 修正：旧 Disable 只置 disabled+reason，不碰 until/modelCooldowns/softStreak，
// 会出现「disabled=true 但 cooling=true / 残留 modelCooldowns」的一致性问题——一个
// 先被硬冷却（到次日 04:00）再被禁用的账号会同时呈现两种状态。禁用后冷却无意义
// （账号已退出选号，冷却截止不再被读取），故一并清空。
//
// 熔断器保留：熔断是「连续 5xx 失败」信号（与授权/会话无关），禁用后再复活时
// 熔断观测仍有效，不应被禁用覆盖。
func (p *Pool) disableLocked(e *entry, reason string) {
	e.clearCoolingLocked()
	e.disabled = true
	e.reason = reason
	p.dirty.Store(true)
}

// reviveCoolingLocked 余额恢复解冻：只解冻**余额耗尽冷却**（CoolHard 的
// until/coolKind/reason）并更新 credits，不动熔断器（fails/retryCount/breakerUntil）、
// 软限流退避（CoolSoft/softStreak）与模型级台账（modelCooldowns）。
//
// 为什么不再全清冷却域（对齐 fork linguo2625469 的 602ed1b）：
//   - CoolHard 的权威恢复证据正是余额恢复（remain>0），照旧解冻。
//   - CoolSoft/softStreak/modelCooldowns 的恢复证据是**上游重置墙钟到期或探测成功**，
//     与「余额有钱」无关。任何经 ReenableIfCredits 到达这里的路径（签到、余额刷新）
//     若顺手全清，限流冷却的实际寿命就被压到两条路径的间隔内：6004 台账被抹后撞限号
//     被误判健康，重新选号再撞 429，全池冷却保护形同虚设（fork 两号池实测复现：
//     expiring==0 的号每个刷新周期被抹一次，expiring>0 的走 SetCreditsDetailed 幸免，
//     两号行为不对称即根因指纹）。softStreak 同理保留，由 NoteSuccess（成功是最强
//     恢复证据）或自然到期收敛。
//   - 熔断域照旧不动：余额恢复只证明 billing 通道健康，不证明 chat 通道健康（C5 语义）。
//
// 与 clearCoolingLocked（disableLocked 专用）的分工：禁用是终态、冷却随之作废，故全清；
// 余额恢复只是「一个维度恢复」，不构成其他维度作废的理由。调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits int64) {
	e.credits = credits
	// 余额恢复时同步推进批次：批次明细是 credits 的分解，累计不得超过总量。
	// 签到路径随后会经 SetCreditsExpiring 用上游权威批次整体覆盖（权威优先），
	// 这里是**兜底**——reviveCoolingLocked 的调用方若没带批次（如测试直接调
	// ReenableIfCredits），钳制仍要成立，否则明细会长期大于总量。
	e.creditBatches = normalizeBatches(e.creditBatches, credits, time.Now())
	if e.coolKind == CoolHard {
		// 解冻硬冷却的时间域；softKind/reason 一并清（reason 是本次硬冷却的文案）。
		// modelCooldowns 不动——账号级硬冷却本就由 Cooldown 清过模型豁免（cooldown.go:108），
		// 这里无需也不应再动。
		e.until = time.Time{}
		e.coolKind = 0
		e.reason = ""
	}
}

// setManualDisabledLocked 手动停用迁移（运维入口）：只置 manualDisabled + 原因，
// **不清冷却域、不动熔断器**。
//
// 与 disableLocked（自动禁用）的关键差异——手动停用是「对话流量摘除」，不是
// 「账号冻结」：签到、token 保活、排程任务照常执行，账号凭证与积分状态都是活的。
// 因此刻意不碰 until/coolKind/modelCooldowns/breakerUntil：停用期间这些维度继续
// 按各自规律演进（冷却自然到期、熔断计数继续累计），恢复时拿到的是「停用期间
// 真实发生过什么」的完整状态，而不是被清空的一刀切。
//
// 为什么用独立状态位而非复用 disabled：自动禁用会被签到解冻、refresh 成功等路径
// 自动撤销，手动停用若复用同一字段，运维意图会被这些路径意外解除。两位独立、
// 各自清除，都清空才回到选号池。
// 调用方必须已持有 p.mu。
func (p *Pool) setManualDisabledLocked(e *entry, disabled bool, reason string) {
	e.manualDisabled = disabled
	if disabled {
		e.manualReason = reason
	} else {
		e.manualReason = ""
	}
	// 立即落盘而非置 dirty：手动停用是**运维意图**，而本功能的全部意义就是
	// 「重启保留运维意图」（issue #138/#118）——置 dirty 会让「停用后未及落盘
	// 即重启/强杀」丢失意图，账号自己回到选号池。低频运维操作，同步写盘的开销
	// 可忽略；与 SyncToDir 剔除账号的直接落盘（pool.go 的 if changed）同口径。
	// 其余置 dirty 点（余额扣减/计数/冷却）仍在请求热路径上走周期落盘：它们丢失
	// 的代价是自愈的（签到覆盖 / 重撞一次限流 / 重学一次退避）。
	p.saveLocked()
}
