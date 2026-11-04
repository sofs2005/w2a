// 积分到期批次：逐包真实到期快照（签到/启动刷新写入）+ 24h/72h 两层紧急层候选筛选。
//
// 背景（issue:积分过期）：权重（weightOf 的快过期占比项 ×8）只能让"快过期多的号"
// 更可能被选中，不能保证**最早到期**的号先被消耗；成本分层、Top5 截断、加权随机、
// 会话粘性都会盖过它。本文件引入硬优先级：只有当积分确实落在紧急窗口（24h / 72h
// 两层）内到期时才按真实到期时刻优先，24h 层整体压过 72h 层（一天内作废的来不及
// 烧完，三天内的还有一轮签到可能覆盖）；窗口外保持原逻辑，否则"永远最早到期的那
// 一个号"会被垄断。且已实测免费的号可以排到临期号之前（免费号本来就该优先，不该
// 为了烧临期积分去付钱——用户明确纠正过这一点，且这条优先是**跨层**的）。
package pool

import (
	"sort"
	"strconv"
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

// urgentExpiryWindow / criticalExpiryWindow 两层到期窗口的**默认值**：只有到期时刻
// 落在 (now, now+窗口] 内的积分才参与硬优先（用户规则：3 天内到期才按过期时间最高
// 优先级）。窗口外（含无到期）一律走原有的成本分层 / 权重 / 防撞号逻辑——否则
// "全池最早到期"的那个号会被永久垄断。
//
// 为什么分两层而不是一个 72h 窗口：两个窗口的紧迫度差一个数量级。24h 内到期的积分
// 几乎必然作废（剩下不到一天，来不及烧完），而 72h 内到期的还有一整轮签到/活动可能
// 覆盖。只用一个窗口时，20h 与 60h 到期的号在紧急层里只按"谁更早"排序——一旦 20h 号
// 被在途上限或请求级 tried 挤出候选集，60h 号就顶上来吃掉本该属于 20h 号的流量，
// 而 20h 号的积分照样作废。分层后 24h 层整体压过 72h 层：先榨干一天内要作废的，
// 再轮到三天内的。
//
// 两个值只是**默认值**，生效值在 Pool.urgentExpiry / Pool.criticalExpiry（运维可经
// pool.urgent_expiry / pool.critical_expiry 调整，见 SetExpiryWindows）。
const (
	urgentExpiryWindow   = 72 * time.Hour
	criticalExpiryWindow = 24 * time.Hour
)

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

// expiryWithin 两层窗口共用的判据：账号最早的未过期批次是否落在 (now, now+window] 内。
// 窗口由调用方传入而不是读包级常量：窗口是运维可调参数，而 entry 不知道自己属于
// 哪个池——收在 Pool 字段里是唯一能让热重载（SetExpiryWindows）生效的形态。
func (e *entry) expiryWithin(now time.Time, window time.Duration) (time.Time, bool) {
	at, ok := e.earliestExpiryAt(now)
	if !ok || at.After(now.Add(window)) {
		return time.Time{}, false
	}
	return at, true
}

// urgentExpiryAt 报告账号是否处于紧急到期状态（3 天层），并返回最早到期时刻。
// 判据：存在未过期批次且到期时刻 <= now+window（window = Pool.urgentExpiry）。
func (e *entry) urgentExpiryAt(now time.Time, window time.Duration) (time.Time, bool) {
	return e.expiryWithin(now, window)
}

// criticalExpiryAt 报告账号是否处于**顶层**（一天层）到期状态，判据与 urgentExpiryAt
// 同构，只是窗口更紧。语义上 critical ⊆ urgent（SetExpiryWindows 保证 critical <= urgent，
// 否则"1 天层比 3 天层还宽"会得出与命名相反的分层结果）。
func (e *entry) criticalExpiryAt(now time.Time, window time.Duration) (time.Time, bool) {
	return e.expiryWithin(now, window)
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
// 三层语义（用户确认过的规则，优先级从高到低）：
//  1. 免费 ∩ 临期（24h ∪ 72h 的并集）：只要存在**免费且临期**的号，就只用它们
//     （"免费号不该为了烧临期积分去付钱"，用户明确纠正过：不是整个最低成本层优先，
//     而是只有免费号能排到最早到期前面）。这一层是**跨层**的——免费号哪怕 50h 才
//     到期，也压过收费号的 20h：免费积分过期没有金钱损失，为了烧它去选收费号才是
//     实打实的支出。
//  2. 24h 层（含收费）：一天内到期的积分几乎必然作废，整体压过 72h 层。
//  3. 72h 层（含收费）：三天内到期的号按真实到期时刻优先。
//  4. 都为空 → nil，交给常规成本分层 / Top5 / 权重 / 防撞号。
//
// 层内不按 tier 分层（未知与收费一视同仁，由到期时刻决定）——"不让未知成本层
// 单凭 tier 排在更早到期账号前"。层间切分只发生在"没有免费号"时。
//
// 两种「返回 nil」的退化（都必须保留，否则会引入垄断）：
//   - 无紧急候选：窗口外一切照旧。
//   - 有免费号但免费号都不临期：此时"免费层"的结果与常规成本分层（tier 0 硬过滤）
//     完全一致，接管只会把加权随机换成"永远选字典序最小的免费号"（免费号可能长期
//     无到期批次，硬选会永久钉死同一账号）。交给原逻辑（含 Top5 + 加权随机 + 防撞号）。
//
// 返回值语义是「本次该只用这一批号」：调用方要么在批内选最早到期者（pick），要么
// 把批当哈希候选集（粘性新建分配），都不能混入批外的号。
//
// 调用方必须已持有 p.mu（只读遍历用的也是同一把锁，读锁亦可）。
func (p *Pool) preferredCandidatesLocked(cands []*entry, reqModel string, now time.Time) []*entry {
	urgentWin, criticalWin := p.urgentExpiry, p.criticalExpiry
	// 第一遍只算两层紧急集：costTierOf 有 map 查找，无紧急候选的热路径上不能白算
	// （窗口外是绝大多数请求，这一步是零成本退出的地方）。critical 惰性声明：窗口外
	// 它是 nil，不给热路径多一次分配（urgent 的预分配是改造前就有的口径，保留）。
	urgent := make([]*entry, 0, len(cands))
	var critical []*entry
	for _, e := range cands {
		if _, ok := e.urgentExpiryAt(now, urgentWin); !ok {
			continue
		}
		urgent = append(urgent, e)
		if _, ok := e.criticalExpiryAt(now, criticalWin); ok {
			critical = append(critical, e)
		}
	}
	if len(urgent) == 0 {
		return nil // 无紧急候选：调用方走原有成本分层/权重/哈希逻辑
	}
	free := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if ti, _ := p.costTierOf(e, reqModel, now); ti == 0 {
			free = append(free, e)
		}
	}
	if len(free) == 0 {
		if len(critical) > 0 {
			return critical // 24h 层压过 72h 层（两层都非空时只放顶层进来）
		}
		return urgent
	}
	freeUrgent := make([]*entry, 0, len(free))
	for _, e := range free {
		// 免费层用 72h 窗口（并集）：critical ⊆ urgent，故这一步就是「免费 ∩ (24h ∪ 72h)」。
		if _, ok := e.urgentExpiryAt(now, urgentWin); ok {
			freeUrgent = append(freeUrgent, e)
		}
	}
	if len(freeUrgent) == 0 {
		return nil // 免费号都不临期：交还原逻辑（见上方"两种退化"注释）
	}
	return freeUrgent
}

// costTierOf 计算 (账号, 模型) 的成本层与单价：
//   - 0 = 免费（本地实测免费，或本地无观测但目录标免费）
//   - 1 = 未知（本地无观测且目录未下发该模型/倍数不可数值化）
//   - 2 = 收费（本地实测收费，或本地无观测但目录标收费）
//
// 判据两级，本地实测恒优先于目录（实测是真实扣费证据，目录只是牌价）：
//  1. 本地实测台账（e.modelCostOf）——有观测即定论，不看目录。
//  2. 上游目录倍率兜底（p.modelRateOf）——本地无观测/观测过期时按牌价判级。
//     只凭实测会让「没学过」恒等于「未知」：新模型全池无观测，免费的号学不到
//     「免费」从而永远落后于已知收费号（pick 的硬过滤会把它滤掉），收费的号却
//     可能被当成未知而放行（积分保底那条路径的 kimi-k3-1 实案）。
//
// 倍率按**账号自身所属域**（e.a.Realm()）查，与 floorBlockedForModel 同键：
// 账号只会路由到自己域的上游、按该域计费，倍率表正是按 "cn"/"global" 分桶存储。
// 选号是域集合形态（RealmSet）没有单一路由域，账号自身域是唯一正确的键——
// 全池 pick 里 CN 号与 global 号各查各域，同一模型名在两域可判出不同层。
//
// cost1k 只来自本地实测：目录倍率是「牌价的倍数」，与 costPer1k（每千 token 的
// 积分数）不同量纲，混进同一个排序键会得出无意义的次序（1.62 的倍数 vs 0.05 分/千
// token 无从比较）。故目录判出的 tier 2 一律回 0，同层内退化为按 UID 确定性排序
// ——真实单价本来也只有实测过才有。未知模型（model==""）恒按 tier 1。
//
// 调用方必须已持有 p.mu（读 p.modelRateOf；e.a.Realm() 内部取 a.mu，同
// floorBlockedForModel 的锁序，无反向嵌套）。
func (p *Pool) costTierOf(e *entry, model string, now time.Time) (tier int, cost1k float64) {
	if mc, ok := e.modelCostOf(model, now); ok {
		if mc.CostPer1k <= 0 {
			return 0, 0
		}
		return 2, mc.CostPer1k
	}
	if p.modelRateOf == nil {
		return 1, 0
	}
	return catalogRateTier(p.modelRateOf(e.a.Realm(), model)), 0
}

// catalogRateTier 把上游目录倍率原文判成成本层：0 = 目录标免费，2 = 目录标收费，
// 1 = 未知（未下发 / 非数值 / 负数）。
//
// 为什么负数也算未知而不是免费：倍率是倍数语义，负值上游不该下发，遇到只能当异常
// 形态处理——按免费放行等于让一个畸形值把账号的保底拦截至零，风险方向不安全。
// 与 floorBlockedForModel 共用本函数，保证「判收费」两处永远同口径。
func catalogRateTier(rate string) int {
	if rate == "" {
		return 1
	}
	v, err := strconv.ParseFloat(rate, 64)
	if err != nil || v < 0 {
		return 1
	}
	if v == 0 {
		return 0
	}
	return 2
}
