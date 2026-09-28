// 积分到期批次：逐包真实到期快照（签到/启动刷新写入）+ 72 小时紧急层候选筛选。
//
// 背景（issue:积分过期）：权重（weightOf 的快过期占比项 ×8）只能让"快过期多的号"
// 更可能被选中，不能保证**最早到期**的号先被消耗；成本分层、Top5 截断、加权随机、
// 会话粘性都会盖过它。本文件引入硬优先级：只有当积分确实在 72 小时内到期时，
// 才按真实到期时刻优先（用户要求：3 天内到期才走最高优先级，否则保持原逻辑，
// 否则"永远最早到期的那一个号"会被垄断）；且已实测免费的号可以排到临期号之前
// （免费号本来就该优先，不该为了烧临期积分去付钱——用户明确纠正过这一点）。
package pool

import (
	"sort"
	"time"
)

// CreditBatch 单个积分包的到期批次（到期时刻 + 该包剩余可用积分），
// 快照来自上游 get-user-resource 的权威口径（upstream.CreditExpiry）。
//
// 与运行态字段一一对应（单一表示，内存与落盘同构）：entry.creditBatches 与
// stateAccount.CreditBatches 共用本类型，JSON tag 直接落盘（同 modelCost 口径的
// 教训——两套类型要手工同步，容易漂移）。
//
// Remain 是**快照值 + 本地消耗估算**：上游 usage.credit 只说本次扣了多少总量，
// 不指出扣的是哪个包，故 pool 侧按"最早到期批次优先被扣"的假设递减。这个假设有实证支撑：
// 实测「个人体验版」（周期包，CycleEndTime=月末）的余额在动，而到期更晚的包分文未动
// ——上游正是先扣最早到期的包（见 upstream.expiry.go 的口径取舍）。这个假设不需要精确——
// 每次签到/启动刷新都用上游快照整体替换（SetCreditsExpiring），漂移最多存活一个刷新周期，
// 且批次的用途是"谁更早到期"的相对排序，递减误差只影响同一账号内的批次边界。
type CreditBatch struct {
	ExpiresAt time.Time `json:"expires_at"`
	Remain    int64     `json:"remain"`
}

// urgentExpiryWindow 紧急到期窗口：只有到期时刻落在 (now, now+72h] 内的积分才参与
// 硬优先（用户规则：3 天内到期才按过期时间最高优先级）。窗口外（含无到期）一律
// 走原有的成本分层 / 权重 / 防撞号逻辑——否则"全池最早到期"的那个号会被永久垄断。
const urgentExpiryWindow = 72 * time.Hour

// earliestExpiryAt 返回账号最早的、**尚未过期**的批次到期时刻；无有效批次时 ok=false。
// 批次在写入/恢复时已按到期升序排序，故取首个未过期条目即可（见 normalizeBatches）。
func (e *entry) earliestExpiryAt(now time.Time) (time.Time, bool) {
	for _, b := range e.creditBatches {
		if b.Remain <= 0 || b.ExpiresAt.IsZero() {
			continue
		}
		if !b.ExpiresAt.After(now) {
			continue // 已过期：上游不会再扣，无优先价值
		}
		return b.ExpiresAt, true
	}
	return time.Time{}, false
}

// urgentExpiryAt 报告账号是否处于紧急到期状态，并返回最早到期时刻。
// 判据：存在未过期批次且到期时刻 <= now+urgentExpiryWindow。
func (e *entry) urgentExpiryAt(now time.Time) (time.Time, bool) {
	at, ok := e.earliestExpiryAt(now)
	if !ok || at.After(now.Add(urgentExpiryWindow)) {
		return time.Time{}, false
	}
	return at, true
}

// normalizeBatches 规范化到期批次快照：剔除零值/非正剩余/已过期条目，按到期升序排序，
// 并把累计剩余钳到 credits（批次是 credits 的明细分解，加起来不得超过总量——上游
// 脏数据或本地递减 bug 都不能让明细反过来放大决策）。
// 调用方必须已持有 p.mu（或处于构造/恢复路径）。
func normalizeBatches(batches []CreditBatch, credits int64, now time.Time) []CreditBatch {
	if len(batches) == 0 {
		return nil
	}
	out := make([]CreditBatch, 0, len(batches))
	for _, b := range batches {
		if b.Remain <= 0 || b.ExpiresAt.IsZero() || !b.ExpiresAt.After(now) {
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ExpiresAt.Before(out[j].ExpiresAt) })
	// 累计钳制：从最早到期开始累加，超出 credits 的部分截断（负余额无意义）。
	var acc int64
	for i := range out {
		room := credits - acc
		if room <= 0 {
			out = out[:i]
			break
		}
		if out[i].Remain > room {
			out[i].Remain = room
		}
		acc += out[i].Remain
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// debitBatchesLocked 按"最早到期批次优先被扣"递减批次剩余（与 NoteModelCost 的余额
// 扣减同一次调用内完成，口径一致）。扣穿的批次从表中移除，从而把账号的最早到期时刻
// 推进到下一条——以免批次用完后账号仍被当作"临期"而持续获得优先。
// 这个递减是本地估算（上游不回报包级归属），下次签到/启动刷新由权威快照覆盖。
// 调用方必须已持有 p.mu 写锁。
func (e *entry) debitBatchesLocked(d int64) {
	if d <= 0 || len(e.creditBatches) == 0 {
		return
	}
	kept := e.creditBatches[:0]
	for _, b := range e.creditBatches {
		if d <= 0 {
			kept = append(kept, b)
			continue
		}
		if b.Remain <= d {
			d -= b.Remain
			continue // 批次耗尽：移除
		}
		b.Remain -= d
		d = 0
		kept = append(kept, b)
	}
	// kept 复用底层数组：截断后把尾部置零，避免残留指针/数据被后续 append 误读。
	for i := len(kept); i < len(e.creditBatches); i++ {
		e.creditBatches[i] = CreditBatch{}
	}
	if len(kept) == 0 {
		e.creditBatches = nil
		return
	}
	e.creditBatches = kept
}

// preferredCandidatesLocked 计算「紧急到期优先」的候选子集；无紧急候选时返回 nil
// （调用方保持原有逻辑不变——这是"窗口外零回归"的关键开关）。
//
// 语义（用户确认过的规则）：
//   - 触发条件：候选里**至少有一个**账号的积分在 72 小时内到期。
//   - 若该紧急集合里存在**已实测免费**（tier 0）的账号 → 只返回「免费 ∩ 紧急」
//     （"免费号不该为了烧临期积分去付钱"，用户明确纠正过：不是整个最低成本层优先，
//     而是只有免费号能排到最早到期前面）。
//   - 否则 → 返回全部紧急候选（未知成本与已实测收费之间**不按 tier 分层**，由到期
//     时间决定——"不让未知成本层单凭 tier 排在更早到期账号前"）。
//
// 两种「返回 nil」的退化（都必须保留，否则会引入垄断）：
//   - 无紧急候选：窗口外一切照旧。
//   - 有免费号但免费号都不临期：此时"免费层"的结果与常规成本分层（tier 0 硬过滤）
//     完全一致，接管只会把加权随机换成"永远选字典序最小的免费号"（免费号可能长期
//     无到期批次，硬选会永久钉死同一账号）。交给原逻辑（含 Top5 + 加权随机 + 防撞号）。
//
// 调用方必须已持有 p.mu（只读遍历用的也是同一把锁，读锁亦可）。
func (p *Pool) preferredCandidatesLocked(cands []*entry, reqModel string, now time.Time) []*entry {
	urgent := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if _, ok := e.urgentExpiryAt(now); ok {
			urgent = append(urgent, e)
		}
	}
	if len(urgent) == 0 {
		return nil // 无紧急候选：调用方走原有成本分层/权重/哈希逻辑
	}
	free := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if ti, _ := costTierOf(e, reqModel, now); ti == 0 {
			free = append(free, e)
		}
	}
	if len(free) == 0 {
		return urgent
	}
	freeUrgent := make([]*entry, 0, len(free))
	for _, e := range free {
		if _, ok := e.urgentExpiryAt(now); ok {
			freeUrgent = append(freeUrgent, e)
		}
	}
	if len(freeUrgent) == 0 {
		return nil // 免费号都不临期：交还原逻辑（见上方"两种退化"注释）
	}
	return freeUrgent
}

// costTierOf 计算 (账号, 模型) 的成本层与单价：
//   - 0 = 已实测免费（CostPer1k <= 0）
//   - 1 = 无观测（含观测过期）
//   - 2 = 已实测收费
//
// 与 pick 内的闭包同一口径（pick 直接调用本函数），未知模型（model==""）按 tier 1。
func costTierOf(e *entry, model string, now time.Time) (tier int, cost1k float64) {
	mc, ok := e.modelCostOf(model, now)
	if !ok {
		return 1, 0
	}
	if mc.CostPer1k <= 0 {
		return 0, 0
	}
	return 2, mc.CostPer1k
}
