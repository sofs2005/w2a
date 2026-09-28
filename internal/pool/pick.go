// 选号：Pick 簇（healthy 三因子加权 Top5 短名单 + 加权随机 + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"log"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// Pick 单一选号入口（无请求级轮换、无 realm 过滤，模型感知）。
// DeptestOnly: 全库仅 pool 包测试引用；生产选号全走 PickExcludingForRealm /
// PickByUIDForModel。保留是因为测试需要无轮换/无 realm 的最小选号原语；
// 迁 export_test.go 不可行——export_test 对包外不可见，而本方法的语义文档
// （挑选策略全文）对生产簇（pick 私有实现）仍有维护参考价值。
// 挑选策略：healthy 账号中按三因子权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
// model 非空时启用 6004 模型级冷却豁免（healthyForModel）；空则等价账号级 healthy。
// 需要请求级轮换（tried）或分池（realm）时用 PickExcludingForRealm。
func (p *Pool) Pick(model string) *auth.Auth {
	return p.pick(nil, model, "")
}

// pick 在 healthy 候选集中按三因子权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// 候选集是 top5 近似：先按三因子权重（weightOf）降序取前 5（credits 只是权重的一个因子，
// 闲置补偿与成功率同样决定谁进短名单），再在 top5 内做防撞号过滤。
// 并发防雪崩：跳过 lastUsed 距今 < minPickGap 的账号（除非 top5 全部刚被用过，
// 此时退回最近最少使用 LRU 账号），迫使高并发请求发散，而不是全部撞同一高分账号。
// minPickGap=0（测试用）时过滤恒通过，退化为纯加权随机。
// reqModel 非空时把健康口径换成 healthyForModel（6004 模型豁免生效；PickExcluding 传 ""）。
// realm 非空时候选过滤叠加 Realm()==realm 谓词（分池选号域；PickExcluding 传 ""）。
// 注意：模型豁免只进 normal 选号（候选 healthy 判定）；全冷却兜底不参与模型豁免——
// 兜底本来就是在"无任何 direct 可用"时的降级，切模型可用性已在 normal 阶段体现。
func (p *Pool) pick(tried map[string]bool, reqModel, realm string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// 惰性清理过期的 6004 模型级冷却与成本台账（两者的 map 都不无限膨胀；
	// status 只读遍历天然跳过过期项，但内存条目必须在此真正删除）。
	for _, e := range p.byUID {
		e.pruneExpiredModelCooldowns(now)
		e.pruneExpiredModelCosts(now)
	}
	realmOK := func(e *entry) bool { return realm == "" || e.a.Realm() == realm }
	healthyOf := func(e *entry) bool { return realmOK(e) && e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return realmOK(e) && e.healthyForModel(now, reqModel) }
	}
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !healthyOf(e) {
			continue
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底。
		return p.pickEarliestExpiryLocked(tried, now, realm)
	}
	// 紧急到期分支（issue:积分过期，见 credits.go）：候选里出现「72 小时内到期」的
	// 账号时，按真实到期时刻硬优先（最早优先），已实测免费的临期号排在最前。
	// **只在有紧急候选时启用**——窗口外（含"全池最早到期但还很久"）一律走下方原有
	// 逻辑，否则最早到期的那个号会被永久垄断（用户明确要求：3 天内才提优先级）。
	// 分支独立于成本分层/探索/Top5/加权随机：这些机制都可能把选号拉回非临期号，
	// 与"硬优先"的语义冲突（探索不得改道到非免费候选）。
	// minPickGap 不参与本分支：紧急集通常只有一两个号，跳过会直接退回非临期号、
	// 违背硬优先语义；并发扩散由"批次耗尽即前移"（debitBatchesLocked）承担。
	if pref := p.preferredCandidatesLocked(cands, reqModel, now); len(pref) > 0 {
		e := p.pickEarliestExpiryAmongLocked(pref, reqModel, now)
		e.lastUsed = now
		p.pickSeq++
		e.usedSeq = p.pickSeq
		return e.a
	}
	// top5 短名单按三因子权重降序截断（而非 credits 单纯降序）：否则闲置补偿
	// 根本进不了短名单决策，低 credits 但久置的账号会永远排不进 top5。
	// maxCredits 统一用**全集口径**（tier 过滤前的全部 healthy 候选）：截断排序与
	// 抽签权重共享同一基准，两个阶段权重可比（旧实现 pickWeighted 在 eligible 子集
	// 上重取 max，全集最大 credits 号被 minPickGap 挤出后子集 max 偏小，剩余号的
	// credits 比例整体膨胀 k=全集max/子集max，credits 项相对 idle/success 被放大）。
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	// 权重与成本分层**各算一次、全程复用**（weighted 结构体定义见包级注释）：
	// weightOf 每候选一次（预计算存入 ws.w），costTier/modelCostOf 同样每候选一次
	// （存入 ws.tier/ws.cost1k）——sort 比较器与 pickWeighted 都只读缓存字段，
	// 不再现算。比较器内现算会翻成 O(n log n) 次冗余浮点/map 查找（46 账号约
	// 500 次比较），旧实现在此翻过车。
	// 成本分层：reqModel 非空时，按该模型的实测扣费把候选分层，只保留最优层。
	//   0 = 已实测免费（限免期/夜间免费的号，最强偏好）
	//   1 = 无观测（含观测过期）
	//   2 = 已实测收费
	// 为什么"无观测"排在"已实测收费"之前：新号的限免状态只能靠实测发现，
	// 若已知收费的号恒压过未知号，那台免费的号永远轮不到，也就永远学不到。
	// 为什么用硬过滤而非仅排序：pickWeighted 会在候选内加权随机，只排序的话
	// 收费号仍有机会抽中，达不到"优先免费"的语义。
	// 分层口径收在 credits.go 的 costTierOf（紧急分支的"免费层优先"复用同一实现）。
	costTier := func(e *entry) (int, float64) { return costTierOf(e, reqModel, now) }
	bestTier := 2
	hasTier1 := false
	explored := false // 本次 pick 是否切了探索层（事件日志在选中号确定后打）
	for _, e := range cands {
		if ti, _ := costTier(e); ti < bestTier {
			bestTier = ti
		}
	}
	// 条件探索（issue #136 方案 a′，语义契约七条见设计报告 §2.1）：tier 0 垄断层
	// 存在（bestTier==0 且 reqModel 非空）且候选含 tier 1（冻结存在）且距上次
	// 探索 ≥ 窗口（零值 timer=从未探索→首次满足即探）时，本次 pick 生效层切
	// tier 1-only——探索=搭车改道，把一个既有真实用户请求改道给未知号（零新增
	// 上游请求；IP 维度零增量，WAF 友好）。成功 → NoteModelCost 首观测 → 毕业
	// （tier 0/2，下一轮 pick 立即生效）；失败 → 既有错误策略照常，无探测风暴。
	// hasTier1 复用本循环上方 costTier 的预计算口径（每候选一次的契约不变）——
	// 下方 ws 构建循环顺带置位，不在判定处再算一遍。timer 同锁写入：并发 pick
	// 串行进入写锁，只有一个进入者能通过窗口判定（天然防重复探索）。
	// key = realm + "\x1f" + reqModel：同模型名可跨域，探索节奏按 (域, 模型)
	// 独立；realm==""（Pick 老语义）单独成键。
	if p.costExploreInterval > 0 && bestTier == 0 && reqModel != "" {
		for _, e := range cands {
			if ti, _ := costTier(e); ti == 1 {
				hasTier1 = true
				break
			}
		}
		key := realm + "\x1f" + reqModel
		if hasTier1 && now.Sub(p.exploreLast[key]) >= p.costExploreInterval {
			p.exploreLast[key] = now
			p.costExploreEvents++
			bestTier = 1
			explored = true
		}
	}
	ws := make([]weighted, 0, len(cands))
	for _, e := range cands {
		ti, ci := costTier(e)
		if ti == bestTier {
			ws = append(ws, weighted{e: e, w: p.weightOf(e, maxCredits, now), tier: ti, cost1k: ci})
		}
	}
	// 等权重洗牌：仅当存在权重并列（epsilon 比较，防浮点微差让洗牌静默失效）且
	// 候选数超过 top5 时，才对 ws 做 Fisher-Yates 洗牌（且**不消耗 p.randInt64N
	// 注入源**，避免改变 pickWeighted 的确定性语义，见 TestPickDeterministicViaSet
	// RandomSource）。权重全等或存在并列时，按字典序截断会让 uid 靠后的账号永远
	// 进不了 top5（惊群测试 c00 集中 79/100 的根因：c05..c09 被字典序截断、LRU
	// 兜底又只在 top5 内转）。洗牌用独立的 time-seeded 源，只在截断边界制造等
	// 权重随机次序，不影响加权抽签本身的确定性。
	if len(ws) > 5 {
		eq := false
		for i := 1; i < len(ws); i++ {
			if math.Abs(ws[i].w-ws[0].w) < weightEpsilon {
				eq = true
				break
			}
		}
		if eq {
			shuf := rand.New(rand.NewPCG(uint64(now.UnixNano()), uint64(len(ws))))
			shuf.Shuffle(len(ws), func(i, j int) { ws[i], ws[j] = ws[j], ws[i] })
		}
	}
	sort.SliceStable(ws, func(i, j int) bool {
		// costTier 硬过滤后 ws 全员同层，但仍按 cost1k 升序排（tier 2 层内单价低者
		// 在前；tier 0/1 层 cost1k 恒 0，本比较退化为权重比较）——读缓存字段不现算。
		if ws[i].cost1k != ws[j].cost1k {
			return ws[i].cost1k < ws[j].cost1k // 收费层：单价低的在前
		}
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID // 稳定兜底（洗牌后此项几乎不触发）
	})
	cands = cands[:0]
	for _, c := range ws {
		cands = append(cands, c.e)
	}
	// candsAll 保留截断前的全候选（权重降序），供 LRU 兜底在全量范围选最旧者，
	// 避免 top5 字典序截断把等权重靠后账号饿死（惊群根因之一）。
	candsAll := cands
	if len(cands) > 5 {
		cands = cands[:5]
	}
	// 防并发撞号：在持锁内基于「上次选中时刻」过滤，但同一批并发 goroutine 会串行进入
	// 本函数（写锁），每个进入者都把 lastUsed 置为 now —— 于是同一瞬间的第 2..N 个
	// 进入者看到前一个账号 lastUsed==now（距今 0 < minPickGap），被自然挤向其他账号。
	// 关键：lastUsed 在锁内赋值，使时间窗口判定在并发下可重入（此前 Acquire 在锁外，
	// 多个 goroutine 在窗口内同时通过校验造成惊群，TestPickAntiThunderingHerd 实证）。
	// 注意：先截断后过滤（截断边界内被挤出的号不回填）——与旧实现语义严格一致。
	eligible := make([]weighted, 0, len(cands))
	for _, we := range ws[:min(len(ws), 5)] {
		if now.Sub(we.e.lastUsed) >= minPickGap {
			eligible = append(eligible, we)
		}
	}
	var e *entry
	if len(eligible) == 0 {
		// top5 全部刚被用过：LRU 兜底，在**全候选 candsAll**（非仅 top5）里选最旧者。
		// 用 usedSeq 单调序号而非 lastUsed 墙钟比较：Windows 等平台 time.Now() 精度
		// ~0.5ms，快速连续选号时所有 lastUsed 完全相等，Before 全 false 会恒选
		// candsAll[0] 导致集中。usedSeq 严格全序，与时间精度无关。
		e = candsAll[0]
		for _, c := range candsAll[1:] {
			if c.usedSeq < e.usedSeq {
				e = c
			}
		}
	} else {
		e = p.pickWeighted(eligible) // eligible 保序 = top5 降序子集，权重直接用预计算值
	}
	if explored {
		// 探索事件日志（§5 可观测性）：选中号此时才确定，故在选中点打出。
		// 毕业结果由相邻的既有日志闭环（免费号无日志、收费号走 NoteModelCost
		// 常规路径）。
		log.Printf("[pool] cost explore model=%s realm=%q acct=%s window=%s",
			reqModel, realm, logfmt.Label(e.a.UID, e.a.Nickname), p.costExploreInterval)
	}
	e.lastUsed = now // 锁内即时标记：下一个进入 pick 的 goroutine 立即看到本号已用
	p.pickSeq++
	e.usedSeq = p.pickSeq // 单调序号：保证 usedSeq 严格全序（防惊群/LRU 的权威依据）
	return e.a
}

// pickEarliestExpiryAmongLocked 在紧急到期候选里选最早到期者（issue:积分过期）。
//
// 排序口径（三级，全序且稳定）：
//  1. 最早到期时刻升序——**核心判据**，让 72 小时内作废的积分先被烧掉。
//  2. 成本层升序（0 免费 / 1 未知 / 2 收费）——同一到期时刻（或都没有到期批次）时
//     优先免费号；preferredCandidatesLocked 已保证不会出现"非免费压过免费"。
//  3. 单价升序 + UID 字典序——兜底确定性（同一时刻到期的批次常见：批量赠送的积分
//     到期时刻相同，此时不能让 map 遍历顺序决定选号）。
//
// 为什么不做加权随机：本分支的语义是"必须先把最早到期的积分烧掉"，随机化会重新
// 引入"临期积分放过期"的可能。防集中由"批次耗尽即前移"（debitBatchesLocked）
// 与 72 小时窗口自然约束——批次用完后该号退出紧急集，选号回到正常逻辑。
// 调用方必须已持有 p.mu 写锁（本函数只读，但选号路径统一在写锁内）。
func (p *Pool) pickEarliestExpiryAmongLocked(cands []*entry, reqModel string, now time.Time) *entry {
	best := cands[0]
	bestAt, bestOK := best.earliestExpiryAt(now)
	bestTier, bestCost := costTierOf(best, reqModel, now)
	for _, e := range cands[1:] {
		at, ok := e.earliestExpiryAt(now)
		tier, cost := costTierOf(e, reqModel, now)
		if betterExpiryPick(at, ok, tier, cost, e.a.UID,
			bestAt, bestOK, bestTier, bestCost, best.a.UID) {
			best, bestAt, bestOK, bestTier, bestCost = e, at, ok, tier, cost
		}
	}
	return best
}

// betterExpiryPick 报告候选 a 是否优于当前最优 b（排序口径见 pickEarliestExpiryAmongLocked）。
// 抽成独立函数便于单测直接锁定比较语义，也避免在循环里重复三层嵌套分支。
func betterExpiryPick(
	aAt time.Time, aOK bool, aTier int, aCost float64, aUID string,
	bAt time.Time, bOK bool, bTier int, bCost float64, bUID string,
) bool {
	// 1. 到期时刻：有批次的恒优于无批次的（有批次 = 确实有未到期积分要烧）。
	if aOK != bOK {
		return aOK
	}
	if aOK && bOK && !aAt.Equal(bAt) {
		return aAt.Before(bAt)
	}
	// 2. 成本层（0 免费最优）。
	if aTier != bTier {
		return aTier < bTier
	}
	// 3. 单价 + UID 字典序（确定性兜底）。
	if aCost != bCost {
		return aCost < bCost
	}
	return aUID < bUID
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time, realm string) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue // 域过滤：池内跨 realm 的冷却账号不参与本 realm 兜底
		}
		if e.disabled || e.manualDisabled {
			continue // 禁用/手动停用的账号永不参与兜底
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // 余额耗尽号（处于有效 hard 冷却期）不参与兜底：等签到恢复，调了必 402
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry acct=%s until=%s kind=%s", logfmt.Label(best.a.UID, best.a.Nickname), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	// 兜底同样是「选中」，必须与 pick() 正常路径、粘性命中路径（PickByUIDForModel）
	// 一样推进 usedSeq/pickSeq：否则被兜底反复选中的账号 usedSeq 恒为 0，在 pick 的
	// LRU 兜底（按 usedSeq 取最旧）眼里永远是「最旧」，刚被用过就被立刻再选——
	// 防集中/防惊群失效（entry.usedSeq 契约：每次被选中时取 p.pickSeq 自增值）。
	p.pickSeq++
	best.usedSeq = p.pickSeq
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（max=0 不限 → 恒 false）。
// 调用方需已持 p.mu（读锁或写锁均可，本方法只读上限字段）。上限按 realm
// 分档（global 档 maxInFlightGlobal，WAF 403 修复 P1-1；未设置回落 maxInFlight）。
func (p *Pool) inFlightFull(e *entry) bool {
	limit := p.inFlightLimit(e)
	if limit <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(limit)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// weighted 单个候选的选号预计算结果：权重（weightOf，每候选一次）+ 成本分层
// （tier/cost1k，每候选一次）。sort 比较器与 pickWeighted 抽签都只读缓存字段，
// 任何一处现算（旧实现在比较器内重算 costTier、在 pickWeighted 内重算 weightOf）
// 都会翻成 O(n log n) 次冗余浮点/map 查找，且引入两阶段口径分裂。
type weighted struct {
	e      *entry
	w      float64
	tier   int     // costTier 结果缓存（0 免费 / 1 无观测 / 2 收费）
	cost1k float64 // CostPer1k 缓存（tier 2 排序用；tier 0/1 恒 0）
}

// expiringWeight 快过期积分占比的权重系数（三因子之一，issue:积分过期）。
// 取 8：略低于 credits 总量项（×10），足以在"快过期多"与"总量相近"的号之间拉开差距，
// 又不至于压过总量项让"总量大但快过期少"的号被完全饿死。
const expiringWeight = 8.0

// pickWeighted 三因子加权随机（claude-api selectWeightedRandom 参考口径）：
//
//		weight = credits 比例 × 10 + 快过期积分占比 × expiringWeight + idleWeight
//
//	  - credits 比例 = 该号 credits / 全集最大 credits（避免量纲爆炸；全集口径：
//	    tier 过滤前的全部 healthy 候选，与截断排序共享基准——见 weighted 预计算注释）
//	  - 快过期积分占比 = creditsExpiring/credits（×8，issue:积分过期）
//	  - idleWeight = min(距 lastUsed 小时数 × idleWeightPerHour, idleWeightMax)；从未使用给满分
//
// credits 全 0 时仍按 idle+expiring 加权（不退化均匀随机）。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
// 候选携带调用方预计算的权重（weighted.w，maxCredits/now 均已按全集口径算好），
// 本函数**不再调用 weightOf**——单次 pick 内每个候选的权重只算一次，预计算与抽签
// 共用同一数值（旧实现在 eligible 子集上用子集 maxCredits 重算第二遍，两次口径
// 分裂：全集最大 credits 号被 minPickGap 挤出后，子集内 credits 比例整体膨胀）。
func (p *Pool) pickWeighted(cands []weighted) *entry {
	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	weights := make([]int64, len(cands))
	var total int64
	for i, c := range cands {
		// 四舍五入并保底权重 ≥1：向零截断会让 w<1/scale 的低权重号权重归零，
		// 彻底失去被抽中机会（候选少时加剧选号集中，惊群测试的放大因子之一）。
		// 保底 ≥1 同时保证 total ≥ len(cands) > 0（抽签分支无需 total<=0 兜底）。
		wi := int64(c.w*scale + 0.5)
		if wi < 1 {
			wi = 1
		}
		weights[i] = wi
		total += wi
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	r := rnd(total)
	var acc int64
	for i := range cands {
		acc += weights[i]
		if r < acc {
			return cands[i].e
		}
	}
	return cands[len(cands)-1].e
}

// weightEpsilon 等权重判定的浮点容差：权重公式微调后权重不再位级相等，
// 精确相等比较会让等权重洗牌静默失效、回到字典序截断饿死问题。
const weightEpsilon = 1e-9

// weightOf 计算单个账号的三因子权重。单次 pick 内对每个候选只调用一次
// （预计算存入 weighted.w，sort 比较器与 pickWeighted 均读缓存不重算）。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	if p.weightOfHook != nil {
		p.weightOfHook() // DeptestOnly 观测：验证单次 pick 只算一次
	}
	if p.weightOfMaxHook != nil {
		p.weightOfMaxHook(maxCredits) // DeptestOnly 观测：验证全集口径
	}
	w := 1.0
	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 1b. 快过期积分加成（issue:积分过期）：官方活动赠送的奖励积分按批过期，
	// 不用就作废。creditsExpiring 占总量比例越高，越应优先被消耗——把"快过期
	// 占比"作为一个独立的强权重项（×expiringWeight），让快过期积分多的号优先选。
	// 与成本分层（costTier 优先免费）正交：那是按"实测扣费"分层，这是按"过期紧迫度"。
	if e.credits > 0 && e.creditsExpiring > 0 {
		w += float64(e.creditsExpiring) / float64(e.credits) * expiringWeight
	}
	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}
	// （原第 3 因子「成功率 EMA ×3」已删，success-ema-review §4：双饱和计数器对
	// 真实成功率不敏感、成熟池退化为与默认值重合的常数 1.5、失败侧与熔断器
	// 100% 同源——机制名存实亡且冗余，删除后 weightOf 为三因子。）
	return w
}
