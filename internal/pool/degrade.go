// 临时出池（degradeUntil 通道）：两种成因 + 成功清零。
//   - NoteFailures：「连败降权」（issue #114），计数 consecutiveFails，成因 "fails"。
//   - NoteTimeout：「连续上游超时（慢号）」，计数 consecutiveTimeouts，成因 "timeout"。
//
// 两者共用同一个 degradeUntil 截止，与冷却/熔断「并存取更长者不叠加」由 entry.healthy
// 的并列或门天然保证（三个截止是或门，任一未到期即不可选，生效的一定是最远者
// ——见 entry.degradeUntil 注释）。
package pool

import "time"

// NoteFailures 记录一次「不罚号的失败」（issue #114 连败兜底的喂入口）：
// consecutiveFails++，达到 degradeThreshold 触发降权（degradeUntil = now +
// 降权时长，计数清零供下一轮重新累计——同 recordBreakerFailureLocked 哲学）。
// **已在降权期内**（degradeUntil 未到期）再次达阈：不延长、不翻倍（防用户重试
// 把降权越堆越厚，与 CooldownSoftRate 兜底探测同哲学）——计数清零，到期放行后
// 需重新连败满阈值才再降。成功（NoteSuccess）清零计数并清降权截止。
//
// 谁该喂：ErrClient（未知 4xx，applyErrorPolicy default 只换号不罚）与传输层
// 失败（handler 网络抖动分支，同样只换号不罚）——两者都是「不知道原因的失败」，
// 正是连败兜底的目标形态。带权威分类的错误（429/WAF/402/5xx/12153/11140/11102）
// 各有「知道原因的惩罚」（冷却/熔断/禁用/负缓存），不喂本计数器——惩罚已存在，
// 再喂连败是重复计罚（任务书纪律：两机制并存取更长者，不叠加）。
func (p *Pool) NoteFailures(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.consecutiveFails++
	if e.consecutiveFails < p.degradeThreshold {
		p.dirty.Store(true)
		return
	}
	e.consecutiveFails = 0
	if !e.degradeUntil.IsZero() && time.Now().Before(e.degradeUntil) {
		// 降权期内的连败达阈：不延长（取更长者语义由 healthy 或门保证）。计数
		// 已清零——到期放行后需重新连败满阈值才再降。
		p.dirty.Store(true)
		return
	}
	e.degradeUntil = time.Now().Add(p.degradeDurationLocked())
	e.degradeCause = degradeCauseFails
	p.dirty.Store(true)
}

// NoteTimeout 记录一次「上游超时（慢号）」信号，返回该账号是否因此被临时出池。
//
// 为什么需要这条通道（线上故障复盘）：handler 的超时分支是**止损**设计（150af36：
// 不换号、不罚号、不解绑粘性），本意正确——超时多半是上游抖动，换号只会把客户端
// 拖到 MaxRotate × header_timeout。但它有两个叠加盲区：
//  1. 超时信号完全不进池子（不 NoteFailures/NoteError/applyErrorPolicy），于是
//     「持续挂起的号」在选号三因子里权重不变、没有连败降权、没有冷却，会被反复
//     抽中，每次白烧一个 header_timeout；
//  2. 超时不解绑粘性，而粘性命中**优先于**评分选号——OpenAI 兼容客户端的粘性键
//     是首条 user 消息派生的会话级 fallback 键，等于一个会话终生钉在一个号上，
//     于是「超时 → 503 → 客户端重试 → 又命中同一个慢号」形成死循环。
//
// 本函数只解决「同一个号**连续**超时」这一种强信号，语义边界严格：
//   - 单次超时：consecutiveTimeouts++ 后返回 false —— **不罚**（保持原设计，
//     罚号的门槛必须高于一次抖动）；
//   - 达阈（timeoutStreak，默认 2）：计数清零（供下一轮重新累计，同 NoteFailures）
//     并复用 degradeUntil 临时出池（degradeCause="timeout"）。调用方据此解绑粘性，
//     否则会话会被钉在一个已出池的号上，下一次请求又要走一遍「粘性命中失败→解绑
//     →回落轮换」，白白多一次往返。
//
// 不缩短既有降权（与 NoteFailures 同哲学）：degradeUntil 取 max(既有未到期值,
// now+timeoutCooldown)。若该号已因连败降权 10m，一次超时达阈不该把它「洗」成
// 60s——惩罚只能加重不能减轻，否则客户端重试可以当减刑器用。
func (p *Pool) NoteTimeout(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	now := time.Now()
	e.consecutiveTimeouts++
	streak := p.timeoutStreak
	if streak <= 0 {
		streak = defaultTimeoutStreak
	}
	if e.consecutiveTimeouts < streak {
		p.dirty.Store(true)
		return false
	}
	// 达阈：清零供下一轮累计（出池期结束后再连续超时才会再次出池）。
	e.consecutiveTimeouts = 0
	until := now.Add(p.timeoutCooldownLocked())
	if e.degradeUntil.After(until) {
		// 既有降权更远：保持原截止与成因（不缩短、不改文案，防"减刑"）。
		// 仍返回 true —— 该号此刻**确实**在池外（不管是哪个成因把它摘出去的），
		// 调用方必须据此解绑粘性，否则会话会被钉在一个不可选的号上。
		p.dirty.Store(true)
		return true
	}
	e.degradeUntil = until
	e.degradeCause = degradeCauseTimeout
	p.dirty.Store(true)
	return true
}

// timeoutCooldownLocked 返回超时出池时长（未注入/非法值时回落默认 60s）。
// **不做上限钳制**：上界由配置层钳好再注入——键是 pool.timeout_cooldown_max
// （默认 10m），钳制点在 cmd/server 的 normalize，**只有那一处**。这里只消费已
// 归一的值：两处各钳一次会让「生效值」有两个事实来源，而池子又拿不到上限键
// （SetTimeout 只收一个时长），钳制逻辑放在这里必然与 config 的默认值漂移。
// 不做指数升级：与连败降权同哲学（见 degradeDurationLocked 注释）——成因计数在
// 达阈时清零、历史触发次数不持久化，档位不可推导；且超时是「慢」不是「坏」，
// 上游一恢复该号就该立刻可用。调用方必须已持有 p.mu。
func (p *Pool) timeoutCooldownLocked() time.Duration {
	d := p.timeoutCooldown
	if d <= 0 {
		d = defaultTimeoutCooldown
	}
	return d
}

// degradeDurationLocked 返回降权时长（当前实现为固定 degradeCooldown，钳到
// [defaultDegradeCooldown, degradeCooldownMax]）。不做指数升级：触发计数在达阈时
// 清零、不持久化历史触发次数，档位不可推导；固定时长已满足「临时出池一段时间」
// 的语义（真持续坏号会在下一轮 5 连败里再降一次）。调用方必须已持有 p.mu。
func (p *Pool) degradeDurationLocked() time.Duration {
	d := p.degradeCooldown
	if d <= 0 {
		d = defaultDegradeCooldown
	}
	max := p.degradeCooldownMax
	if max <= 0 {
		max = defaultDegradeCooldownMax
	}
	if d > max {
		d = max
	}
	return d
}
