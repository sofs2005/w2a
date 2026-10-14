// 冷却与熔断：Cooldown（固定时长账号级冷却）、CooldownSoftRate（账号级软冷却，对齐
// 上游重置时间或有界退避）、CooldownSoftForModel（模型级软冷却，对齐重置墙钟）、
// 软冷却封顶、熔断失败累计、签到解冻（ReenableIfCredits/reviveCoolingLocked）。
package pool

import (
	"strings"
	"time"
)

// SetCredits 只更新账号余额总量，不动快过架子集（creditsExpiring）——
// 上游未回报分桶时的兼容入口，语义与 SetCreditsDetailed 的区别已由
// expiring_test.go:TestSetCreditsLeavesExpiringUnchanged 锚定。
// DeptestOnly: 全库仅测试引用（pool 的 cost/expiring/inflight_global/race_stress/
// bench 与 server 的 handler_test 等）；生产写余额全走 SetCreditsDetailed
// （scheduler.go 签到与启动刷新时调用），本入口无生产调用方。保留是因为它是
// 「总量更新」与「总量+分桶更新」的语义对照锚点（删掉则 expiring 的向后兼容
// 行为失去断言）。
//
// 但**钳制不能省**：creditsExpiring 被钳到 [0, credits]。这不是「分桶更新」，
// 而是维护「expiring 是 credits 的子集」这条不变量——它现在有四处守卫
// （applyAccountsLocked 加载、SetCreditsExpiring / SetCreditsDetailed 写入、
// 本入口），缺一处就会漏。
// 漏掉的后果是真实的：weightOf 里 expiring/credits 是无保护的比值项
// （pick.go），比值 >1 会把 ×expiringWeight 的加成放大到远超设计上限，
// 让该账号被异常频繁地选中。场景：签到留下 expiring>0 后，权威余额因上游侧
// 消耗而降到 expiring 之下，此时经本入口回写一个更小的总量即触发。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if credits < 0 {
			credits = 0 // 负余额无意义（与 SetCreditsDetailed/加载侧同口径）
		}
		e.credits = credits
		if e.creditsExpiring > credits {
			e.creditsExpiring = credits
		}
		p.dirty.Store(true)
	}
}

// NoteCheckinDone 标记账号今日已签到（签到成功与上游"今天已签到"幂等拒绝均算）。
// 记录本地日期，跨零点自然过期；不触碰冷却/禁用状态（签到与冷却域正交）。
func (p *Pool) NoteCheckinDone(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		day := time.Now().Format("2006-01-02")
		if e.lastCheckinDay != day {
			e.lastCheckinDay = day
			p.dirty.Store(true)
		}
	}
}

// SetCreditsDetailed 更新账号余额总量 + 快过架子集（签到时调用，供优先消耗快过期积分）。
// expiring 会被钳到 [0, credits]：上游分桶异常时不污染权重。
// 兼容入口：不更新逐包到期快照（creditBatches 保持不变）——生产已改用
// SetCreditsExpiring 一并传入权威批次；保留本入口供「只有总量+桶」的调用方使用。
func (p *Pool) SetCreditsDetailed(uid string, credits, expiring int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if expiring < 0 {
			expiring = 0
		}
		if expiring > credits {
			expiring = credits
		}
		e.credits = credits
		e.creditsExpiring = expiring
		p.dirty.Store(true)
	}
}

// SetCreditsExpiring 更新账号余额总量 + 快过架子集 + 逐包到期快照（签到/启动刷新调用）。
//
// 这是**权威覆盖**入口：batches 来自上游 get-user-resource（upstream.UserResourceExpiry），
// 整体替换本地快照，把两次刷新之间的本地递减估算漂移（debitBatchesLocked 的"最早到期
// 优先被扣"假设）收敛掉。expiring 钳到 [0, credits] 的不变量同 SetCreditsDetailed；
// batches 经 normalizeBatches 过滤（零值/非正剩余/已过期剔除，按到期升序，累计钳到
// credits）——上游脏数据不得让明细反过来放大选号决策。
// batches 为空是合法输入（该号当前没有未到期积分包）→ 清空快照，账号失去临期优先资格。
func (p *Pool) SetCreditsExpiring(uid string, credits, expiring int64, batches []CreditBatch) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	if expiring < 0 {
		expiring = 0
	}
	if expiring > credits {
		expiring = credits
	}
	e.credits = credits
	e.creditsExpiring = expiring
	e.creditBatches = normalizeBatches(batches, credits, time.Now())
	p.dirty.Store(true)
}

// Cooldown 冷却账号至 now+d（即时冷却：CoolSoft 429 / CoolHard 余额耗尽）。
//
// 重构后本入口是「固定时长的账号级冷却」，不再做两件旧事：
//   - 不再喂熔断器失败计数：熔断器只对「反复失败」（NoteError，5xx）退避。
//     软限流/余额耗尽各有权威恢复时刻（重置墙钟 / 04:00 签到），再并入"连续失败"
//     会让用户正常重试越堆越厚。熔断语义由 NoteError 唯一驱动（与 until 正交保持）。
//   - 不再做 softStreak 指数堆加：固定 d 即最终时长。CoolSoft 的精确对齐请用
//     CooldownSoftUntil（有界、对齐上游重置时间）。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.coolKind = kind
		e.reason = reason
		// 非模型级冷却入口：清空 6004 模型级独立冷却表（modelCooldowns），
		// 避免上一次模型级限流的模型豁免泄漏到本次**账号级**限流上
		// （否则换模型请求会错误绕过本次冷却）。
		e.modelCooldowns = nil
		p.dirty.Store(true)
	}
}

// CooldownSoftForModel 429 的**模型级**软冷却入口（issue #31）：把该模型的冷却截止
// 精确对齐到上游重置墙钟（不做指数堆加、不做 softStreak 计数）。
//
//   - resetAt 非零（带解析时间）→ modelCooldowns[model].Until = min(resetAt,
//     now+softRateMax)，ResetAt 记录上游原始墙钟（台账 ResetAt）。不写 until
//     （全账号级冷却不受模型级限流污染），切模型即可用（模型豁免）。
//   - resetAt 零值（无时间文案）→ 有界退避：base 起按 softStreak 翻倍、封顶
//     softRateMax，且**在软冷却中**（until 未到期）时不推进/不延长（兜底探测不再把
//     冷却越堆越厚）。不记录模型（不豁免）。
//
// 与旧实现的差异：有上游重置时间时绝对不做指数堆加（旧实现 softStreak 计数含
// 死逻辑，纯属库房麻痹）；无重置时间时，「冷却中兜底探测再 429」不再 softStreak++
// 翻倍——这正是用户「全池被推到 2h 封顶」的元凶。
//
// also 可选附加键（别名改造）：6004 台账键是选号用的**对外名**，而出站物理名可能
// 与之不同；把出站名一并登记同一条 Until，使「直接请求该物理名」的流量也吃到本次
// 限流（同一账号换个入口再撞一次没有意义）。无 resetAt 的分支是**账号级**退避，
// 没有模型键，附加键自然忽略。
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string, also ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if !resetAt.IsZero() {
			// 有上游重置时间：冷却截止 = min(resetAt, now+softRateMax)，不做指数放大。
			if e.modelCooldowns == nil {
				e.modelCooldowns = map[string]modelCooldown{}
			}
			mc := modelCooldown{
				Until:   p.cappedSoftUntilLocked(now, resetAt),
				ResetAt: resetAt,
				Reason:  reason,
			}
			e.modelCooldowns[model] = mc
			for _, m := range also {
				if m == "" || m == model {
					continue
				}
				e.modelCooldowns[m] = mc // 同一 Until/ResetAt：与物理名共享同一次限流窗口
			}
		} else {
			// 无解析时间（普通软冷却）：有界退避（base 起按 softStreak 翻倍、封顶
			// softRateMax）。注意：**在软冷却中**（until 未到期）时不推进/不延长。
			if e.coolKind != CoolSoft || !now.Before(e.until) {
				d := p.softDurationLocked(base, e.softStreak+1)
				e.softStreak++
				e.until = now.Add(d)
			}
			e.coolKind = CoolSoft
			e.reason = reason
			e.modelCooldowns = nil
		}
		p.dirty.Store(true)
	}
}

// BlockModelBackoff 11102「该后端无此模型」的 (账号, 模型) 负缓存入口（handler.applyErrorPolicy
// 调用）。复用 modelCooldowns 机制（不新建平行状态）：写 modelCooldowns[model]，Until 为指数退避
// TTL，选号侧 healthyForModel 自动对该账号避开该模型。
//
// 语义与 6004 正交：6004 是「模型被限流、对齐重置墙钟」，本入口是「官方确定该后端无此模型、
// 重试无意义，只能换模型/换账号」。退避按 reference model_blocks.py 口径（吸收语义、不抄实现）：
//   - 首次命中 TTL = modelBlockBaseTTL（6h）；
//   - 半开到期后允许放行重试；再次命中 Hits++ → TTL = base × 2^min(hits-1, modelBlockShift)；
//   - 封顶 modelBlockMaxTTL（24h，最多一天再试一次）；
//   - 该模型请求成功即由 BlockModelClear 清除（注意：6004 的 NoteSuccess 不清 modelCooldowns，
//     故清理必须由 handler 在成功且 model 命中 11102 条目时显式调用，见 BlockModelClear）。
//
// resetAt 无需传（11102 无重置文案），ResetAt 保持零值，与 6004 台账（rateLimitedModelsLocked）
// 共用 Until 判定——11102 条目会以 11102 reason 出现在 /status 台账，运维可见。
//
// also 可选附加键（别名改造）：别名请求的选号键是**对外名**（primary model），而
// 出站物理名可能与它不同；把出站名一并写入同一条 TTL，使「直接请求该物理名」的流量
// 也吃到本次负缓存（否则同一账号会用另一个入口再撞一次 11102）。hits 计数只按
// primary 键累计，别名键跟随同一 TTL——不引入第二条退避曲线。
func (p *Pool) BlockModelBackoff(uid, model, reason string, also ...string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	hits := 0
	if e.modelCooldowns != nil {
		hits = e.modelCooldowns[model].Hits
	}
	hits++
	ttl := modelBlockBaseTTL
	if d := ttl * (1 << uint(min(hits-1, modelBlockShift))); d < modelBlockMaxTTL {
		ttl = d
	} else {
		ttl = modelBlockMaxTTL
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	e.modelCooldowns[model] = modelCooldown{
		Until:  now.Add(ttl),
		Reason: reason,
		Hits:   hits,
	}
	for _, m := range also {
		if m == "" || m == model {
			continue
		}
		e.modelCooldowns[m] = modelCooldown{
			Until:  now.Add(ttl),
			Reason: reason,
			Hits:   hits,
		}
	}
	p.dirty.Store(true)
}

// BlockModelClear 清除 (账号, 模型) 的 11102 负缓存条目（该模型实测又通了）。半开探测或正常
// 请求对该模型成功后调用（handler 成功路径）。只清 11102 条目、不碰 6004 独立冷却表——
// 6004 有自身上游重置墙钟语义，成功不该抹掉（见 NoteSuccess 注释）。reason 前缀判定区分两者：
// 11102 条目的 reason 恒以 "11102" 开头（见上游 BlockModelReason）。
// also 可选附加键（别名改造）：与 BlockModelBackoff 写入的附加键对称，成功时一并清除。
func (p *Pool) BlockModelClear(uid, model string, also ...string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelCooldowns) == 0 {
		return
	}
	cleared := false
	for _, m := range append([]string{model}, also...) {
		if m == "" {
			continue
		}
		mc, exists := e.modelCooldowns[m]
		if !exists || !strings.HasPrefix(mc.Reason, "11102") {
			continue // 不存在的键，或 6004 条目（reason 前缀非 11102）
		}
		delete(e.modelCooldowns, m)
		cleared = true
	}
	if !cleared {
		return
	}
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
	p.dirty.Store(true)
}

// BlockCause 说明全池模型级阻塞的**性质**——决定客户端该等还是该换模型。
//
// 为什么必须区分：6004（模型限流）与 11102（该后端无此模型）共用 modelCooldowns
// 一张表，冷却期间选号都返回 nil。但前者是**临时**状态（等到上游重置墙钟即恢复），
// 后者只能换模型。此前两者都报 no_healthy_account（「服务端过载」），客户端无从
// 区分，只会不停重试。
type BlockCause int

const (
	// BlockUnavailable 上游明确答复「该后端无此模型」（11102）：重试无意义，只能换模型。
	//
	// 取零值 = 判不出性质时的兜底侧，理由见 blockCauseOf：把不可重试误报成可等待，
	// 客户端会一直等在永不到期的窗口上；反过来代价只是多提示一句换个模型。
	BlockUnavailable BlockCause = iota
	// BlockRateLimited 全池都在该模型的上游限流窗口内（6004 带重置墙钟）：临时状态，
	// 正确处置是等到 ResetAt。换模型也能用，但不该被说成「模型不可用」。
	BlockRateLimited
)

// blockCauseOf 判定一条模型级冷却的性质。判据按可靠性排序，任一命中即足够：
//
//  1. ResetAt 非零 → 限流。只有 6004 带得上游重置墙钟（11102 无重置文案，ResetAt
//     恒零值，见 modelCooldown.ResetAt 注释）；该字段跨持久化往返保全（persist.go
//     的 ModelCooldowns 存取两侧都带 ResetAt），重启后判据不失效。
//  2. reason 以 "6004" 开头 → 限流。防御分支：6004 只经 CooldownSoftForModel 的
//     重置墙钟分支写入、必带 ResetAt，理论上走不到这里；但条目可能来自旧版本落盘
//     的 state.json——那里若 ResetAt 缺失，仅凭零值会把限流误判成「模型不可用」。
//  3. 兜底 → 不可用。11102 条目恒由 BlockModelBackoff 以 upstream.ModelBlockReason
//     写入，与该判断一致；取零值也是判不出时的安全侧（见 BlockUnavailable 注释）。
func blockCauseOf(mc modelCooldown) BlockCause {
	if !mc.ResetAt.IsZero() {
		return BlockRateLimited
	}
	if strings.HasPrefix(mc.Reason, "6004") {
		return BlockRateLimited
	}
	return BlockUnavailable
}

// ModelBlockStatus 描述某模型在全池范围内因模型级冷却而不可选的情况。
type ModelBlockStatus struct {
	Blocked bool      // true = 每个非禁用账号都对该模型处于冷却中
	Cause   BlockCause // 阻塞性质：限流（该等）还是不可用（该换模型）
	Reason  string    // 冷却原因（通常为上游原文，如 "11102 model ... not found"）
	Until   time.Time // 最早解封时间（零值 = 上游未给重置时刻）
	Count   int       // 因此被挡的账号数
}

// ModelBlocked 报告该模型是否在全池范围内被模型级冷却挡住。
//
// 为什么需要它：选号失败时客户端只会拿到 no_healthy_account（「没有可用账号」），
// 但真实原因常常是「号都在、只是都对这个模型关闭」。两者对调用方的处置完全不同
// ——前者该等，后者换个模型才有用——此前却无法区分：首次请求还能看到上游原文
// （lastErr 非空），一旦负缓存写入，后续请求 lastErr 为空，就只剩「池子没号」。
// 上游原文与解封时间在那里被丢掉。
//
// 跨域判定：调用方失败前已依次尝试过各域，所以只要有**任意**账号还能服务该模型，
// 就不能算全池阻塞 —— 此时返回 Blocked=false，让上层继续用原有的
// no_healthy_account 文案（选号失败另有原因：在途占满/积分保底/账号级冷却）。
//
// 口径必须与选号一致：用 modelCooled（与 healthyForModel 同一谓词），不直接查 map，
// 将来该谓词加了豁免（如只审计不拦截的条目）这里自动跟随。
//
// 注意 Blocked 不等于「模型不可用」：6004（限流，会自行到期）同样写 modelCooldowns、
// 同样让选号返回 nil。调用方必须再看 Cause 分别处置，别一律报 model_unavailable。
func (p *Pool) ModelBlocked(model string) ModelBlockStatus {
	if model == "" {
		return ModelBlockStatus{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()

	var st ModelBlockStatus
	rateLimited := false
	for _, e := range p.byUID {
		if e.disabled || e.manualDisabled {
			continue // 停用号不参与：它的不可用与模型无关
		}
		if !e.modelCooled(now, model) {
			// 还有账号能服务这个模型 → 不是模型级阻塞。
			return ModelBlockStatus{}
		}
		st.Count++
		mc, ok := e.modelCooldowns[model]
		if !ok {
			continue
		}
		if blockCauseOf(mc) == BlockRateLimited {
			// 只要有一个号是「限流」，整体性质就是限流：限流窗口会自行到期，
			// 说「等到 X」对客户端成立；而报「模型不可用」会把它推去换模型，
			// 掩盖了本可以等回来的事实。
			rateLimited = true
		}
		if st.Reason == "" {
			st.Reason = mc.Reason
		}
		// 取最早解封：那才是「再等多久值得重试」的答案。
		if !mc.Until.IsZero() && (st.Until.IsZero() || mc.Until.Before(st.Until)) {
			st.Until = mc.Until
		}
	}
	if st.Count == 0 {
		// 池里压根没有非停用账号：这是「真的没号」，不是模型问题。
		return ModelBlockStatus{}
	}
	st.Blocked = true
	if rateLimited {
		st.Cause = BlockRateLimited
	}
	return st
}

// CooldownSoftRate 429/限流文案的**账号级**软冷却入口（handler.applyErrorPolicy 调用）。
//
// 语义：
//   - resetAt 非零（上游带权威重置时间，无论 6004 还是 11140 rate-limiting）→
//     账号级直到该墙钟（截断到 softRateMax，绝不指数堆加）；**不**在
//     modelCooldowns 记模型（账号级语义，不产生切模型豁免，保持与旧
//     CooldownSoftForModel 无解析分支一致——普通账号级限流不该因切模型绕过）。
//   - resetAt 零值且**不在冷却中**（首次/恢复后的新限流）→ 有界退避：按 softStreak
//     指数退避并封顶 softRateMax（默认 2h，并经 softDurationLocked 统一封顶）。
//     softStreak 只在真正进入一次新冷却时计数，由 NoteSuccess/reviveCoolingLocked
//     清零（既有恢复语义）。
//   - resetAt 零值且**已在软冷却中**（兜底探测再次撞 429）→ 不推进 streak、不延长
//     until：用户重试/并发兜底探测不得把冷却越堆越厚——这正是旧实现「越重试越冷、
//     全池被推到 2h 封顶」的元凶（每次探测都 softStreak++ 指数翻倍）。
func (p *Pool) CooldownSoftRate(uid string, base time.Duration, resetAt time.Time, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if !resetAt.IsZero() {
			e.until = p.cappedSoftUntilLocked(now, resetAt)
		} else if e.coolKind != CoolSoft || !now.Before(e.until) {
			// 新限流（不在有效软冷却中）：推进有界退避；兜底探测（仍在软冷却中）不翻倍。
			d := p.softDurationLocked(base, e.softStreak+1)
			e.softStreak++
			e.until = now.Add(d)
		}
		e.coolKind = CoolSoft
		e.reason = reason
		e.modelCooldowns = nil // 账号级软冷却：清空模型豁免（切模型不绕过）
		p.dirty.Store(true)
	}
}

// cappedSoftUntilLocked 把上游重置墙钟截断到 softRateMax（now+softRateMax 与 resetAt
// 取较早者）。resetAt 已过期（时钟偏移/文案过期）时日长短钳到时间零点附近，立即恢复。
// 调用方必须已持有 p.mu。
func (p *Pool) cappedSoftUntilLocked(now, resetAt time.Time) time.Time {
	cap := now.Add(p.softRateMaxOr())
	if resetAt.After(cap) {
		return cap
	}
	if resetAt.After(now) {
		return resetAt
	}
	return now.Add(time.Millisecond)
}

// softRateMaxOr 返回生效的 softRateMax（未注入时按默认 2h），供封顶计算。
// 调用方必须已持有 p.mu。
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}
	return defaultSoftRateMax
}

// softDurationLocked 按连续软冷却次数把基数 d 指数放大：d << (streak-1)，封顶 softRateMax。
// softRateMax 未注入（<=0）时按 defaultSoftRateMax 算。streak<=1 时原样返回 d。
// 左移位数受 softStreakShiftMax 限制，避免 streak 极大时移位溢出。
// 调用方必须已持有 p.mu。
func (p *Pool) softDurationLocked(d time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return d
	}
	shift := streak - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	d <<= shift
	max := p.softRateMax
	if max <= 0 {
		max = defaultSoftRateMax
	}
	if d > max || d <= 0 { // d<=0：左移溢出成负数/零，同样按封顶兜底
		d = max
	}
	return d
}

// recordBreakerFailureLocked 累计一次熔断失败；达到阈值则按指数退避熔断。
// 熔断与冷却（until）解耦：冷却按错误类别给固定时长，熔断则对"反复失败"逐次加长封禁。
// 调用方必须已持有 p.mu。
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// 触发熔断：重置失败计数供下一轮重新累计；retryCount 递增放大退避指数。
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
}

// CooldownUntilTomorrow4AM 冷却到下一个 04:00（本地时区）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00）恢复。
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM 返回 now 之后最近的一个 04:00（与 now 同一时区）。
// now 在当天 04:00 之前（凌晨 00:00~04:00）时返回当天 04:00——此时签到尚未执行，
// 该窗内触发的硬冷却等当天签到即可恢复；返回次日会白冷约一天。
// 04:00 整及之后返回次日 04:00。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// ReenableIfCredits 签到/余额刷新后解冻：仅当 remain > 0 且账号非禁用时，解冻
// **余额耗尽冷却**（CoolHard）。软限流（CoolSoft）与模型级台账（modelCooldowns）
// 不在此清除——它们的恢复证据是上游重置墙钟到期或探测成功，不是余额恢复
// （余额刷新周期任务每 5 分钟到达这里，全清会把限流冷却实际寿命压到一个刷新周期内）。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
// reviveCoolingLocked 已迁至 transition.go（状态机迁移唯一权威实现）。
