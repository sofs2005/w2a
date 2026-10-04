// metrics.go 请求统计聚合（/v1/stats 数据源）。
//
// 设计要点：
//   - **单一埋点**：唯一写入口是 chatStat.done()，流式/非流式/错误路径全汇于此，
//     天然覆盖全路径，不需要在每个 return 前重复记账。
//   - **只采信上游 usage**：token / cache / credit 一律来自上游末帧 usage，缺失时
//     用 hasUsage 区分「缺观测」与「显式 0」，不做 rune 估算（与成本账本同纪律）。
//   - **按裸名 × 域聚合**：键是剥掉 cn:/global: 前缀的裸名，第二层是**承接请求的
//     账号域**——同一个模型名在两域的单价/限免不同，必须分开看（见 metricsStore）。
//   - **有界内存**：模型键数量受上游目录限制（不是无界增长）；另设容量上限兜底，
//     超限时丢弃新键并记一次 WARN，避免异常模型名刷爆内存。
//   - **零外部依赖**：纯内存累加，进程重启即清零（since 随进程启动时间）。
package server

import (
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

// metricsCap 模型键容量上限。上游目录规模远小于此值；上限只为兜底异常模型名。
const metricsCap = 512

// modelMetrics 单模型的累加器（全字段原子性由 metricsMu 保证，无需 atomic）。
type modelMetrics struct {
	requests  int64
	success   int64
	failed    int64
	streaming int64

	ttfbSumMS  float64 // TTFB 累计（仅成功且有观测的请求）
	ttfbCount  int64
	latSumMS   float64 // 端到端耗时累计（全部请求）
	genSecSum  float64 // 生成秒数累计（供 tokens/s）
	promptTok  int64
	compTok    int64
	cacheHit   int64
	cacheMiss  int64
	cacheWrite int64
	credit     float64

	lastSeen time.Time

	// realm 该条目承接请求的账号域（"cn"/"global"/""=未路由）。
	//
	// 这是「按域分账」的载体：统一调度后裸名会在 CN 与 global 账号间调度，
	// 同一个模型名两域的单价、限免活动、上下文长度都不同，混在一个累加器里
	// 只能看到一个加权平均，既看不出哪域花了多少、也解释不了扣费为什么变。
	// 老路径（无账号上下文：选号失败 503、模型名解析不出）记 ""，独立成组，
	// 不并进任何一侧——编造归属比空着更糟。
	realm string
}

// accumulateMetric 把一次请求的观测累加进一个累加器。抽成独立函数是为了让
// 「累加」与「选哪个累加器」（metricFor 的裸名×域寻址）分开——前者是纯算术，
// 后者才是策略，混在一起会让口径改动（如新增一个按域字段）无从下手。
func accumulateMetric(mm *modelMetrics, s *chatStat, total time.Duration) {
	mm.requests++
	if s.status == 200 {
		mm.success++
	} else {
		mm.failed++
	}
	if s.mode == "stream" {
		mm.streaming++
	}

	totalMS := float64(total.Milliseconds())
	mm.latSumMS += totalMS

	// TTFB 只在有观测时累加（流式首帧才有；非流式恒 0，不计入均值分母，
	// 否则会把非流式的 0 拉低均值，失真）。
	if s.ttfb > 0 {
		mm.ttfbSumMS += float64(s.ttfb.Milliseconds())
		mm.ttfbCount++
	}

	// token / cache / credit 只在 hasUsage 时累加：缺失≠0。
	if s.hasUsage {
		mm.promptTok += int64(s.prompt)
		// toks<0 是「观测缺失」哨兵（非流式路径：usage 存在但缺 completion_tokens 时
		// completionTokens 返回 -1，此时 hasUsage 仍为真）。不设此防护会把 -1 累加进
		// 总量，越积越偏——真值只可能 ≥0，故负值一律不计。
		if s.toks > 0 {
			mm.compTok += int64(s.toks)
		}
		mm.cacheHit += int64(s.cacheHit)
		mm.cacheMiss += int64(s.cacheMiss)
		mm.cacheWrite += int64(s.cacheWr)
		// 生成吞吐分母：总耗时减去 TTFB（纯生成时间）。TTFB 缺失时退回总耗时。
		gen := totalMS
		if s.ttfb > 0 {
			gen = totalMS - float64(s.ttfb.Milliseconds())
		}
		if gen > 0 {
			mm.genSecSum += gen / 1000.0
		}
	}
	if s.hasCredit {
		mm.credit += s.credit
	}

	mm.lastSeen = time.Now()
}

// metricsStore 全局聚合表。
//
// 一棵树，两层分组：**裸名 → 域 → 累加器**（byBare）。为什么是这个形状：
//
//   - 键用**裸名**而非请求体原样串：统一调度后客户端写 "deepseek-v4.1-flash"（全池）
//     或 "cn:deepseek-v4.1-flash"（钉域）指的是同一个底层模型，只有裸名能让它们
//     归到同一行——这正是表格要的「一个模型一行」。
//   - 第二层是**域**：同一个裸名在国内号与国际号上单价、限免活动、上下文都不同，
//     混成一个累加器只能看到加权平均，既看不出哪域花了多少、也解释不了扣费为什么变。
//     域取**选中账号的 Realm()**（不是请求体的前缀）——前缀只表达"允许打哪"，实际
//     落在哪个域由选号决定；账要记在实际提供服务的那一侧。
//   - ""（空域）表示请求未被路由到任何账号（选号失败 503、模型名解析不出）。独立成
//     一组，不并进任何一侧——编造归属比空着更糟。
//
// 容量按**裸名个数**计（metricsCap）：子条目数 = 裸名数 × 域数（≤3），有界。
type metricsStore struct {
	mu     sync.Mutex
	since  time.Time
	byBare map[string]map[string]*modelMetrics
	warned bool // 容量超限只告警一次，避免刷屏
}

var globalMetrics = &metricsStore{
	since:  time.Now(),
	byBare: make(map[string]map[string]*modelMetrics),
}

// metricFor 取（或首次创建）指定裸名 × 域的累加器；裸名数超限返回 nil（丢弃本次
// 观测，与旧行为一致——cap 只为兜底异常模型名，不是为了精确统计）。
// 调用方必须已持有 m.mu。
func (m *metricsStore) metricFor(bare, realm string) *modelMetrics {
	perRealm, ok := m.byBare[bare]
	if !ok {
		if len(m.byBare) >= metricsCap {
			if !m.warned {
				m.warned = true
				logMetricsCapWarn(bare)
			}
			return nil
		}
		perRealm = make(map[string]*modelMetrics)
		m.byBare[bare] = perRealm
	}
	if mm, ok := perRealm[realm]; ok {
		return mm
	}
	mm := &modelMetrics{realm: realm}
	perRealm[realm] = mm
	return mm
}

// recordChatMetric 把一次请求的观测累加进聚合表。由 chatStat.done() 调用。
//
// total 为端到端耗时（TTFB 与生成吞吐的分母口径均由此派生）。模型名为空/"-" 时
// 归入 "-" 键（仍计入 total，不丢弃观测）。
func recordChatMetric(s *chatStat, total time.Duration) {
	model := s.model
	if model == "" {
		model = "-"
	}

	// 归口用裸名（见 metricsStore 注释）。"-"（模型名缺失）保持原样：它不是有效
	// 模型名，且不该与某个真的叫 "-" 的模型混淆——resolveModel("-") 本就返回裸名
	// "-"（无冒号），故无需特判。
	_, bare := resolveModel(model)

	m := globalMetrics
	m.mu.Lock()
	defer m.mu.Unlock()

	if mm := m.metricFor(bare, s.realm); mm != nil {
		accumulateMetric(mm, s, total)
	}
}

// MetricsSnapshot 是 /v1/stats 的响应载荷（字段名与社区面板约定一致）。
type MetricsSnapshot struct {
	Enabled   bool               `json:"enabled"`
	Message   string             `json:"message,omitempty"`
	Since     time.Time          `json:"since"`
	Now       time.Time          `json:"now"`
	UptimeSec int64              `json:"uptime_sec"`
	Total     ModelStatPayload   `json:"total"`
	Models    []ModelStatPayload `json:"models"`
}

// ModelStatPayload 单模型派生统计。
type ModelStatPayload struct {
	Model string `json:"model"`

	Requests  int64 `json:"requests"`
	Success   int64 `json:"success"`
	Failed    int64 `json:"failed"`
	Streaming int64 `json:"streaming"`

	AvgTTFBMS    float64 `json:"avg_ttfb_ms"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	TokensPerSec float64 `json:"tokens_per_sec"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	CacheHitTokens   int64   `json:"cache_hit_tokens"`
	CacheMissTokens  int64   `json:"cache_miss_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`

	Credit       float64 `json:"credit"`
	CreditPerReq float64 `json:"credit_per_req"`

	// Bare 该条目的裸模型名（剥掉 cn:/global: 前缀）。
	//
	// 面板的「官方价」按**裸名**索引价格表（单价与域无关，是国内外的厂商定价），
	// 而 Model 可能是 "cn:xxx" 这种钉域名。让网关直接给出裸名，面板据此查表即可，
	// 不必自己去 split(":") —— 按前缀猜域/猜名正是促销配对那处出过的 bug 类型。
	Bare string `json:"bare,omitempty"`

	// Realms 该条目在池中实际被哪些账号域承接（"cn"/"global"，按固定序），
	// 以及各域**独立**的子统计。
	//
	// 为什么行内再嵌一层而没有拉平成顶层的多个数组：官方价（Bare，与域无关）必须
	// 与分域明细同一行才能"官方价放裸名后面、其余列按域分行"地渲染，拉平会让前端
	// 自己按模型名 join 两个数组，等于把配对的活推给展示层（同一类 bug 的温床）。
	//
	// **恒非空**：每个条目至少有一个域子条目（哪怕只有 cn 一次请求），故单域模型
	// 也会输出一个单元素数组、渲染成带「国内版」徽章的一行——它确实知道自己在哪一域
	// 跑的，标成"未知域"反而是信息倒退。omitempty 只服务于老网关/手写载荷这类
	// 整段缺席的情形，前端那时才退化为单行「未标注域」。
	Realms []RealmStat `json:"realms,omitempty"`

	// Credits 上游积分倍率原文（如 "x0.06"），与 /v1/models 的 credits 同源同值；
	// 目录未下发 / 缓存冷 → 空串，JSON 整体省略（缺失≠免费，不输出 "x0.00"）。
	// 由 stats handler 从模型目录只读缓存合入（enrichCredits），不参与聚合。
	//
	// 注意口径：它取的是**合并后的单值（CN 优先）**，两域倍率不同时只代表国内版。
	// 分域真值见 /v1/models 的 credits_cn / credits_global（本表的倍率列暂无分域
	// 版本——它纯展示、且当前无消费方，故未随本次分域改造一起拆）。
	Credits string `json:"credits,omitempty"`

	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// RealmStat 单个域的统计明细。字段集是所有按域派生量都有的那些（均值、比率、
// 吞吐、扣费），不含 Model/Bare/Realms/LastSeen 这些跨域的标识与记账字段——
// 它们属于父条目，子条目重复输出只会让前端困惑该信哪一个。
type RealmStat struct {
	// Realm "cn" 或 "global"；"" = 请求未被路由到任何账号（选号失败、模型名解析不出）。
	Realm string `json:"realm"`

	Requests  int64 `json:"requests"`
	Success   int64 `json:"success"`
	Failed    int64 `json:"failed"`
	Streaming int64 `json:"streaming"`

	AvgTTFBMS    float64 `json:"avg_ttfb_ms"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	TokensPerSec float64 `json:"tokens_per_sec"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	CacheHitTokens   int64   `json:"cache_hit_tokens"`
	CacheMissTokens  int64   `json:"cache_miss_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`

	Credit       float64 `json:"credit"`
	CreditPerReq float64 `json:"credit_per_req"`
}

// MetricsSnapshotOf 生成当前聚合快照。models 按请求数降序（面板表格默认序）。
func MetricsSnapshotOf() MetricsSnapshot {
	now := time.Now()
	m := globalMetrics
	m.mu.Lock()
	defer m.mu.Unlock()

	out := MetricsSnapshot{
		Enabled:   true,
		Since:     m.since,
		Now:       now,
		UptimeSec: int64(now.Sub(m.since).Seconds()),
		Models:    make([]ModelStatPayload, 0, len(m.byBare)),
	}

	// 一次遍历产出：每个裸名一行（含分域明细），同时把各域累加器汇总成 total。
	//
	// 求和遍历的是**域子条目**：父行是跨域合计（= 各子条目之和），只是展示便利，
	// 不该承担语义。直接对子条目求和是唯一没有歧义的口径。
	//
	// 父行的派生量（均值/比率/吞吐）由**原始量合计反算**，而不是把各域的均值再
	// 平均——后者是「均值的均值」，请求数不同的两域会被等权稀释（CN 3 次 + global
	// 300 次，等权平均会把 3 次那笔的延迟抬到 50% 权重）。故父行自己也留一份
	// modelMetrics 累加原始量，最后统一折算，与 total 同一口径。
	var tot modelMetrics
	for bare, perRealm := range m.byBare {
		var row modelMetrics
		subs := make([]RealmStat, 0, len(perRealm))
		// 域按固定序输出（cn → global → ""）：map 遍历顺序随机，不排序会让
		// 「国内版」「国际版」两行每次刷新互换位置。
		for _, realm := range metricRealmOrder(perRealm) {
			mm := perRealm[realm]
			addMetric(&tot, mm)
			addMetric(&row, mm)
			subs = append(subs, realmStatOf(realm, mm))
		}
		rowPayload := deriveModelStat(bare, &row)
		rowPayload.Bare = bare
		rowPayload.Realms = subs
		out.Models = append(out.Models, rowPayload)
	}
	out.Total = deriveModelStat("total", &tot)

	sort.Slice(out.Models, func(i, j int) bool {
		if out.Models[i].Requests != out.Models[j].Requests {
			return out.Models[i].Requests > out.Models[j].Requests
		}
		return out.Models[i].Model < out.Models[j].Model
	})
	return out
}

// metricRealmOrder 返回 perRealm 里各域的固定输出序：cn → global → ""（未路由）。
//
// 固定序是为了面板稳定——map 遍历顺序随机，不排序会让同一模型的两行每次刷新
// 互换位置。空域排最后：它是异常路径（选号失败），不该占据「第一行」的位置。
func metricRealmOrder(perRealm map[string]*modelMetrics) []string {
	order := make([]string, 0, len(perRealm))
	for _, real := range []string{"cn", "global"} {
		if _, ok := perRealm[real]; ok {
			order = append(order, real)
		}
	}
	// 其余（含 ""）按字典序补在最后，保证确定性。
	rest := make([]string, 0, len(perRealm))
	for realm := range perRealm {
		if realm != "cn" && realm != "global" {
			rest = append(rest, realm)
		}
	}
	sort.Strings(rest)
	return append(order, rest...)
}

// addMetric 把 src 的原始累加量并入 dst（total 与父行共用）。
func addMetric(dst, src *modelMetrics) {
	dst.requests += src.requests
	dst.success += src.success
	dst.failed += src.failed
	dst.streaming += src.streaming
	dst.ttfbSumMS += src.ttfbSumMS
	dst.ttfbCount += src.ttfbCount
	dst.latSumMS += src.latSumMS
	dst.genSecSum += src.genSecSum
	dst.promptTok += src.promptTok
	dst.compTok += src.compTok
	dst.cacheHit += src.cacheHit
	dst.cacheMiss += src.cacheMiss
	dst.cacheWrite += src.cacheWrite
	dst.credit += src.credit
	if src.lastSeen.After(dst.lastSeen) {
		dst.lastSeen = src.lastSeen
	}
}

// realmStatOf 把某域的累加器折算成 RealmStat（与 deriveModelStat 同口径，
// 只是少了跨域的标识字段）。
func realmStatOf(realm string, mm *modelMetrics) RealmStat {
	r := RealmStat{
		Realm:            realm,
		Requests:         mm.requests,
		Success:          mm.success,
		Failed:           mm.failed,
		Streaming:        mm.streaming,
		PromptTokens:     mm.promptTok,
		CompletionTokens: mm.compTok,
		TotalTokens:      mm.promptTok + mm.compTok,
		CacheHitTokens:   mm.cacheHit,
		CacheMissTokens:  mm.cacheMiss,
		CacheWriteTokens: mm.cacheWrite,
		Credit:           mm.credit,
	}
	if mm.requests > 0 {
		r.AvgLatencyMS = mm.latSumMS / float64(mm.requests)
		r.CreditPerReq = mm.credit / float64(mm.requests)
	}
	if mm.ttfbCount > 0 {
		r.AvgTTFBMS = mm.ttfbSumMS / float64(mm.ttfbCount)
	}
	if mm.genSecSum > 0 {
		r.TokensPerSec = float64(mm.compTok) / mm.genSecSum
	}
	if denom := mm.cacheHit + mm.cacheMiss; denom > 0 {
		r.CacheHitRate = float64(mm.cacheHit) / float64(denom)
	}
	return r
}

// deriveModelStat 把累加器折算为派生统计（均值、比率、吞吐）。
func deriveModelStat(name string, mm *modelMetrics) ModelStatPayload {
	p := ModelStatPayload{
		Model:            name,
		Requests:         mm.requests,
		Success:          mm.success,
		Failed:           mm.failed,
		Streaming:        mm.streaming,
		PromptTokens:     mm.promptTok,
		CompletionTokens: mm.compTok,
		TotalTokens:      mm.promptTok + mm.compTok,
		CacheHitTokens:   mm.cacheHit,
		CacheMissTokens:  mm.cacheMiss,
		CacheWriteTokens: mm.cacheWrite,
		Credit:           mm.credit,
	}
	if mm.requests > 0 {
		p.AvgLatencyMS = mm.latSumMS / float64(mm.requests)
		p.CreditPerReq = mm.credit / float64(mm.requests)
	}
	if mm.ttfbCount > 0 {
		p.AvgTTFBMS = mm.ttfbSumMS / float64(mm.ttfbCount)
	}
	if mm.genSecSum > 0 {
		p.TokensPerSec = float64(mm.compTok) / mm.genSecSum
	}
	// 命中率分母 = 命中 + 未命中（不含 write：写入是「为后续命中付的费」，
	// 计入分母会把首次请求的命中率压低，失真）。
	if denom := mm.cacheHit + mm.cacheMiss; denom > 0 {
		p.CacheHitRate = float64(mm.cacheHit) / float64(denom)
	}
	if !mm.lastSeen.IsZero() {
		t := mm.lastSeen
		p.LastSeen = &t
	}
	return p
}

// ResetMetrics 清空聚合（/v1/stats/reset），便于观察增量。since 重置为当前时刻。
func ResetMetrics() {
	m := globalMetrics
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byBare = make(map[string]map[string]*modelMetrics)
	m.since = time.Now()
	m.warned = false
}

// logMetricsCapWarn 容量超限告警（独立函数便于测试替换/断言，也避免 import log 污染
// 主体逻辑的阅读）。
func logMetricsCapWarn(model string) {
	log.Printf("WARN: [metrics] 模型键达上限 %d，丢弃新键 model=%q（异常模型名？）", metricsCap, model)
}

// enrichCredits 把上游积分倍率原文合入 stats 快照（/v1/stats 数据展示侧增强）。
//
// 数据源与 /v1/models 完全同源：CN 侧 cachedModelsSnapshot / global 侧
// GlobalModelInfosSnapshot，均为**只读快照**——缓存冷/过期 → nil，绝不发起上游
// 调用（maintainer 约束：网关只加工已有数据）。倍率是展示字段而非观测值，故
// 不进 recordChatMetric 聚合路径，快照出口统一合入。
//
// 键归一：stats 行键已是**裸名**（metricsStore 按裸名分树），目录 id 也是裸名——
// 直接查表即可。仍走 resolveModel 是为了兜住"裸名里带冒号"的异常串（如
// "global:global:x" 剥一层后仍带前缀），未知前缀/裸名含冒号/"-" 查不到 → 省略。
// total 行不参与（跨倍率聚合无意义）。
//
// 裸名（RealmUnified）两域目录都查，CN 优先：与 /v1/models 的目录合并口径一致
// （同名条目 CN 值优先、global 补缺），否则同一模型在目录里显示一个倍率、
// 在 stats 里显示另一个。global 表在无 upstream 时为 nil（读 nil map 安全）。
func (h *Handler) enrichCredits(snap *MetricsSnapshot) {
	cn := make(map[string]string) // bare id -> credits 原文
	for _, mi := range cachedModelsSnapshot() {
		if mi.Credits != "" {
			cn[mi.ID] = mi.Credits
		}
	}
	var global map[string]string
	if h.cfg.Upstream != nil {
		global = make(map[string]string)
		for _, mi := range h.cfg.Upstream.GlobalModelInfosSnapshot() {
			if mi.Credits != "" {
				global[mi.ID] = mi.Credits
			}
		}
	}
	for i := range snap.Models {
		realm, bare := resolveModel(snap.Models[i].Model)
		if bare == "" || bare == "-" {
			continue
		}
		switch realm {
		case "global":
			snap.Models[i].Credits = global[bare]
		case RealmUnified:
			if c := cn[bare]; c != "" {
				snap.Models[i].Credits = c
			} else {
				snap.Models[i].Credits = global[bare]
			}
		default:
			snap.Models[i].Credits = cn[bare]
		}
	}
}

// fillStatFromUsage 把非流式聚合响应的 usage 观测填进 chatStat（与流式路径同口径）。
//
// 与 usageCreditTotal 的分工：那个函数服务成本账本（只取 credit + 总 token），
// 本函数服务 metrics（还要 prompt/cache 三段）。两者都读同一份 usage，但目标字段
// 不同，故不复用——强行合并会让账本依赖 metrics 的字段集，反之亦然。
func fillStatFromUsage(st *chatStat, resp map[string]any) {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return
	}
	st.hasUsage = true
	st.prompt = intFromUsage(u, "prompt_tokens")
	st.cacheHit = intFromUsage(u, "prompt_cache_hit_tokens")
	st.cacheMiss = intFromUsage(u, "prompt_cache_miss_tokens")
	st.cacheWr = intFromUsage(u, "prompt_cache_write_tokens")
	if c, ok := u["credit"].(float64); ok {
		st.credit = c
		st.hasCredit = true
	}
}

// intFromUsage 从 usage map 取整数字段；缺失或类型不符返回 0。
func intFromUsage(u map[string]any, key string) int {
	if v, ok := u[key].(float64); ok {
		return int(v)
	}
	return 0
}

// stats 处理 GET /v1/stats：返回按模型聚合的请求统计（社区面板数据源）。
// 聚合口径不变；出口处只读合入模型目录的积分倍率（enrichCredits，无上游调用）。
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	snap := MetricsSnapshotOf()
	h.enrichCredits(&snap)
	writeJSON(w, http.StatusOK, snap)
}

// statsReset 处理 POST /v1/stats/reset：清空累计，便于观察增量。
func (h *Handler) statsReset(w http.ResponseWriter, r *http.Request) {
	ResetMetrics()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
