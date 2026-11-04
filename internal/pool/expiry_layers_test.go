// 到期硬优先的**两层**语义验收（24h 层 / 72h 层 / 免费跨层优先 / 在途占满回退）。
//
// 与 credits_test.go 的 TestUrgent* / TestFreeBeats* 互补：那批用例锁的是"临期 > 常规"
// 与"免费 > 临期收费"两条老规则（本轮改动不得动它们），本文件锁的是本轮新增的
// 「24h 压过 72h」「窗口可配」「在途占满时降级而不是走冷却兜底」。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// newLayerPool 构造确定性选号环境：关闭探索（避免 tier 切换干扰）、随机源恒取 0
// （常规逻辑下必落权重首位）、关闭防撞号窗口（同号可连续被选中，便于计数断言）。
func newLayerPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	t.Cleanup(p.Close)
	return p
}

// pickCounts 连续选 n 次并统计命中分布。
func pickCounts(p *Pool, model string, n int) map[string]int {
	seen := map[string]int{}
	for i := 0; i < n; i++ {
		if a := p.PickExcludingForRealm(nil, model, ""); a != nil {
			seen[a.UID]++
		}
	}
	return seen
}

// TestExpiryCriticalLayerBeatsUrgentLayer 第 1 条：无免费号时，24h 层整体压过 72h 层。
//
// 本用例**分两段**断言，因为「切层」在两个观测面上的可判别性完全不同，必须写清楚，
// 否则下一个人会重蹈覆辙（把 pick 分布当成分层的证明）：
//
//	(1) pick 侧（选中分布）：收费 20h 与收费 60h 竞争，20h 全中——60h 号即便积分
//	    多得多也没有机会。这一段**不能**证明"24h 层压过 72h 层"：pick 走的是
//	    pickEarliestExpiryAmongLocked，其第一排序键就是「最早到期时刻」，而
//	    critical ⊆ urgent 保证 24h 号的到期时刻必然早于任何只在外层的号——所以
//	    "切层"与"不切层"在 pick 上选出的是同一个号，天生不可判别（实测：把
//	    credits.go 里 critical 独占那几行删掉，本段依然全绿）。它锁的是**层内
//	    最早到期优先**这条老规则的回归，价值在此，别指望它测分层。
//	(2) 粘性侧（哈希候选集）：UrgentUIDsForModelRealms 只返回 ["a20h"]。这才是
//	    「24h 层独占」的唯一判别面——粘性新建/重绑直接拿候选集做哈希分配、根本不
//	    经过 pick，72h 号只要混进候选集就会被哈希到，绕过 24h 层的硬优先。
func TestExpiryCriticalLayerBeatsUrgentLayer(t *testing.T) {
	p := newLayerPool(t)
	p.Add(&auth.Auth{UID: "a20h"})
	p.Add(&auth.Auth{UID: "b60h"})
	p.NoteModelCost("a20h", "m", 2.9, 1000)
	p.NoteModelCost("b60h", "m", 2.9, 1000)
	setBatches(p, "a20h", 100, 100, CreditBatch{ExpiresAt: inHours(20), Remain: 100})
	setBatches(p, "b60h", 1_000_000, 1_000_000, CreditBatch{ExpiresAt: inHours(60), Remain: 1_000_000})

	// (1) 层内最早到期优先（对"分层"无判别力，见上方注释）。
	seen := pickCounts(p, "m", 30)
	if seen["a20h"] != 30 || seen["b60h"] != 0 {
		t.Fatalf("24h 层应独占流量，got %v（want a20h=30 b60h=0）", seen)
	}

	// (2) 判别面：粘性候选集只含 24h 层。删掉 critical 独占分支（或把两层混成
	// 一张表）时这里会拿到 [a20h b60h]，本断言立即变红——pick 侧做不到这一点。
	if uids := p.UrgentUIDsForModelRealm("m", ""); len(uids) != 1 || uids[0] != "a20h" {
		t.Fatalf("粘性候选集 = %v，want [a20h]（72h 号 b60h 不得混入 24h 层）", uids)
	}
}

// TestExpiryFreeUrgentStillBeatsCriticalPaid 第 2 条：**免费优先是跨层的**，
// 且免费层内部仍按最早到期排（不是按余额权重）。
//
// 构造必须含**两个免费号**，否则本用例没有判别力：只放「免费 50h + 收费 20h」时，
// 免费层若整体失效（preferredCandidatesLocked 返回 nil），选号会落到常规路径的
// tier 0 硬过滤，免费号照样全中——"免费号赢了"证明不了"免费 ∩ 临期这一层在起作用"。
//
// 判别构造（正确实现下 f50h 恒中）：
//   - f50h（免费，50h 到期，100 分）+ f60h（免费，60h 到期，1e6 分）：两个免费号
//     都在 72h 层、都不在 24h 层；
//   - p20h（收费，20h 到期）：不在免费层里。
//
// 一条断言同时锁住两件事：
//  1. 免费号跨层压过收费 24h 号（f60h 与 p20h 一分流量都拿不到）；
//  2. 免费层内按**最早到期**排序——若退化成按余额权重，1e6 分的 f60h 会赢。
//
// 判别力实测：把 freeUrgent 的窗口从「72h 并集」收窄成「只认 24h」（变异 M6）后，
// freeUrgent 为空 → 返回 nil → 常规权重路径按余额选中 f60h，本用例立即变红。
func TestExpiryFreeUrgentStillBeatsCriticalPaid(t *testing.T) {
	p := newLayerPool(t)
	p.Add(&auth.Auth{UID: "f50h"})
	p.Add(&auth.Auth{UID: "f60h"})
	p.Add(&auth.Auth{UID: "p20h"})
	p.NoteModelCost("f50h", "m", 0, 1000)   // tier 0：实测免费
	p.NoteModelCost("f60h", "m", 0, 1000)   // tier 0：实测免费
	p.NoteModelCost("p20h", "m", 2.9, 1000) // tier 2：实测收费
	setBatches(p, "f50h", 100, 100, CreditBatch{ExpiresAt: inHours(50), Remain: 100})
	setBatches(p, "f60h", 1_000_000, 1_000_000, CreditBatch{ExpiresAt: inHours(60), Remain: 1_000_000})
	setBatches(p, "p20h", 100, 100, CreditBatch{ExpiresAt: inHours(20), Remain: 100})

	seen := pickCounts(p, "m", 100)
	if seen["f50h"] != 100 || seen["f60h"] != 0 || seen["p20h"] != 0 {
		t.Fatalf("免费临期号应跨层压过收费 24h 层，且免费层内按最早到期排序，"+
			"got %v（want f50h=100 f60h=0 p20h=0；"+
			"若落到常规权重路径会按余额选 f60h，若免费层被 24h 窗口截断会给 p20h）", seen)
	}
}

// TestExpiryFreeNotUrgentKeepsTier0Filter 第 3 条：免费号都不临期时，紧急层放手，
// 但常规成本分层（tier 0 硬过滤）仍然保证免费号占优——收费 20h 号不得因为"更临期"
// 而翻盘（它压根进不了 tier 0 层）。
func TestExpiryFreeNotUrgentKeepsTier0Filter(t *testing.T) {
	p := newLayerPool(t)
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "paid20h"})
	p.NoteModelCost("free", "m", 0, 1000)
	p.NoteModelCost("paid20h", "m", 2.9, 1000)
	setBatches(p, "free", 1000, 0) // 无到期批次 = 不临期
	setBatches(p, "paid20h", 100, 100, CreditBatch{ExpiresAt: inHours(20), Remain: 100})

	seen := pickCounts(p, "m", 30)
	if seen["free"] != 30 || seen["paid20h"] != 0 {
		t.Fatalf("免费不临期时应退回 tier0 硬过滤，got %v（want free=30 paid20h=0）", seen)
	}
}

// TestExpiryWindowsConfigurable 第 4 条：窗口是**配置**而不是常量。
// 把窗口收到 6h/2h 后，原本 20h 到期的号不再临期，选号退回常规权重逻辑
// （积分远高的 60h 号胜出）——证明生效值确实来自 Pool 字段而非包级常量。
func TestExpiryWindowsConfigurable(t *testing.T) {
	p := newLayerPool(t)
	p.Add(&auth.Auth{UID: "a20h"})
	p.Add(&auth.Auth{UID: "b60h"})
	p.NoteModelCost("a20h", "m", 2.9, 1000)
	p.NoteModelCost("b60h", "m", 2.9, 1000)
	setBatches(p, "a20h", 100, 100, CreditBatch{ExpiresAt: inHours(20), Remain: 100})
	setBatches(p, "b60h", 1_000_000, 1_000_000, CreditBatch{ExpiresAt: inHours(60), Remain: 1_000_000})

	// 默认 72h/24h：20h 在 24h 层内 → 硬优先。
	if seen := pickCounts(p, "m", 10); seen["a20h"] != 10 {
		t.Fatalf("默认窗口下 a20h 应全中，got %v", seen)
	}

	p.SetExpiryWindows(6*time.Hour, 2*time.Hour)
	if u, c := p.ExpiryWindows(); u != 6*time.Hour || c != 2*time.Hour {
		t.Fatalf("生效窗口 = (%v, %v)，want (6h, 2h)", u, c)
	}
	// 20h / 60h 都在 6h 窗口外 → 无紧急候选 → 常规权重（随机源取 0 = 权重首位 = b60h）。
	if seen := pickCounts(p, "m", 10); seen["b60h"] != 10 || seen["a20h"] != 0 {
		t.Fatalf("窗口收到 6h 后应退回常规权重逻辑（b60h 占优），got %v", seen)
	}
}

// TestExpiryWindowsClampCriticalToUrgent 第 5 条：critical > urgent 必须钳平。
// "1 天层"比"3 天层"还宽是自相矛盾的配置（顶层会吞掉整个外层），入口处钳到 urgent，
// 否则分层语义反转且没有任何运行期报错。
func TestExpiryWindowsClampCriticalToUrgent(t *testing.T) {
	p := newLayerPool(t)
	p.SetExpiryWindows(1*time.Hour, 48*time.Hour)
	if u, c := p.ExpiryWindows(); u != time.Hour || c != time.Hour {
		t.Fatalf("critical 应被钳到 urgent：got (%v, %v)，want (1h, 1h)", u, c)
	}

	// 行为验证：1h 窗口下，3h 到期的号已不临期，只有 30min 的号在紧急层里。
	p.Add(&auth.Auth{UID: "a30m"})
	p.Add(&auth.Auth{UID: "b3h"})
	p.NoteModelCost("a30m", "m", 2.9, 1000)
	p.NoteModelCost("b3h", "m", 2.9, 1000)
	setBatches(p, "a30m", 100, 100, CreditBatch{ExpiresAt: inHours(0.5), Remain: 100})
	setBatches(p, "b3h", 1_000_000, 1_000_000, CreditBatch{ExpiresAt: inHours(3), Remain: 1_000_000})
	if seen := pickCounts(p, "m", 10); seen["a30m"] != 10 {
		t.Fatalf("1h 窗口下 30min 号应全中，got %v", seen)
	}

	// 非正值 = 未设置：保留现值（与 SetBreaker 等 Set* 同口径）。
	p.SetExpiryWindows(0, -time.Hour)
	if u, c := p.ExpiryWindows(); u != time.Hour || c != time.Hour {
		t.Fatalf("非正值应保留原值：got (%v, %v)，want (1h, 1h)", u, c)
	}
}

// TestExpiryLayerRelaxedInFlightFallback 第 6 条（本任务最容易写漏的用例）：
//
//	(a) 24h 层唯一的号在途占满 + 存在 72h 号 → 必须回退到 72h 号；
//	(b) 两层都占满 → 放宽在途上限重扫，仍按层内最早到期选中 24h 号，
//	    **不得**落到全冷却兜底（兜底会把流量扔给正在冷却的号，而 24h 号的积分干等作废）。
//
// 判据靠 "cool" 这个冷却号：它不在健康候选集里，只有走 pickEarliestExpiryLocked
// 才会被选中。断言 c20h 中选，等于同时证明"没走兜底"。
func TestExpiryLayerRelaxedInFlightFallback(t *testing.T) {
	p := newLayerPool(t)
	p.SetMaxInFlight(1)
	p.Add(&auth.Auth{UID: "c20h"})
	p.Add(&auth.Auth{UID: "u60h"})
	p.Add(&auth.Auth{UID: "cool"})
	for _, u := range []string{"c20h", "u60h", "cool"} {
		p.NoteModelCost(u, "m", 2.9, 1000)
	}
	setBatches(p, "c20h", 100, 100, CreditBatch{ExpiresAt: inHours(20), Remain: 100})
	setBatches(p, "u60h", 1000, 1000, CreditBatch{ExpiresAt: inHours(60), Remain: 1000})
	// 冷却号：到期最早（1h），一旦走兜底就会被它吃掉流量——正是我们要排除的形态。
	setBatches(p, "cool", 1000, 1000, CreditBatch{ExpiresAt: inHours(1), Remain: 1000})
	p.Cooldown("cool", CoolSoft, 10*time.Minute, "test")

	// (a) 24h 层唯一号占满 → 降级到 72h 层。
	if !p.Acquire("c20h") {
		t.Fatal("c20h 应能占到在途名额")
	}
	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "u60h" {
		t.Fatalf("24h 层占满时应回退到 72h 层，got %v（want u60h）", a)
	}

	// (b) 两层都占满 → 放宽在途上限，仍选 24h 层（最早到期），而不是冷却兜底。
	if !p.Acquire("u60h") {
		t.Fatal("u60h 应能占到在途名额")
	}
	a := p.PickExcludingForRealm(nil, "m", "")
	if a == nil {
		t.Fatal("紧急层全部在途占满时不得返回 nil（应放宽在途上限）")
	}
	if a.UID == "cool" {
		t.Fatal("走了全冷却兜底：紧急层在途占满时应放宽上限，而不是把流量扔给冷却号")
	}
	if a.UID != "c20h" {
		t.Fatalf("放宽重扫后应选 24h 层最早到期的号，got %s（want c20h）", a.UID)
	}
}

// TestExpiryLayerTieDeterministic 第 7 条：同一到期时刻的并列必须确定性（按 UID 兜底），
// 不能让 map 遍历顺序决定选号——批量赠送的积分到期时刻往往完全相同。
func TestExpiryLayerTieDeterministic(t *testing.T) {
	p := newLayerPool(t)
	p.Add(&auth.Auth{UID: "a20h"})
	p.Add(&auth.Auth{UID: "b20h"})
	p.Add(&auth.Auth{UID: "c60h"})
	for _, u := range []string{"a20h", "b20h", "c60h"} {
		p.NoteModelCost(u, "m", 2.9, 1000)
	}
	same := inHours(20) // 同一时刻：故意只算一次，避免纳秒差把"并列"变成"先后"
	setBatches(p, "a20h", 100, 100, CreditBatch{ExpiresAt: same, Remain: 100})
	setBatches(p, "b20h", 100, 100, CreditBatch{ExpiresAt: same, Remain: 100})
	setBatches(p, "c60h", 1_000_000, 1_000_000, CreditBatch{ExpiresAt: inHours(60), Remain: 1_000_000})

	seen := pickCounts(p, "m", 30)
	if seen["a20h"] != 30 || seen["b20h"] != 0 || seen["c60h"] != 0 {
		t.Fatalf("并列应按 UID 确定性选 a20h，且 72h 号不参与，got %v", seen)
	}
}

// TestExpiryLayerOutOfWindowZeroRegression 第 8 条：全部号都在 72h 窗口外时，选号必须
// 回到常规权重逻辑（积分高的号占优），不得因"全池最早到期"而被垄断。
func TestExpiryLayerOutOfWindowZeroRegression(t *testing.T) {
	p := newLayerPool(t)
	p.Add(&auth.Auth{UID: "rich"})
	p.Add(&auth.Auth{UID: "poor"})
	p.NoteModelCost("rich", "m", 2.9, 1000)
	p.NoteModelCost("poor", "m", 2.9, 1000)
	// poor 的到期时刻更早（5 天 < 90 天），但两者都在 72h 窗口外 → 不应触发硬优先。
	setBatches(p, "rich", 1_000_000, 0, CreditBatch{ExpiresAt: inHours(24 * 90), Remain: 1_000_000})
	setBatches(p, "poor", 10, 0, CreditBatch{ExpiresAt: inHours(24 * 5), Remain: 10})

	seen := pickCounts(p, "m", 20)
	if seen["rich"] != 20 || seen["poor"] != 0 {
		t.Fatalf("窗口外应走常规权重（积分高者胜），got %v（want rich=20 poor=0）", seen)
	}
}

// TestExpiryLayerDefaultsFromPoolNew 默认值锚点：New 之后两层窗口就是 72h/24h，
// 且 24h 层的判据与 72h 层同构（critical ⊆ urgent）。
func TestExpiryLayerDefaultsFromPoolNew(t *testing.T) {
	p := New("")
	defer p.Close()
	u, c := p.ExpiryWindows()
	if u != urgentExpiryWindow || c != criticalExpiryWindow {
		t.Fatalf("New 后窗口 = (%v, %v)，want (%v, %v)", u, c, urgentExpiryWindow, criticalExpiryWindow)
	}
	if c > u {
		t.Fatalf("不变量被破坏：critical(%v) > urgent(%v)", c, u)
	}

	p.Add(&auth.Auth{UID: "e20h"})
	setBatches(p, "e20h", 100, 100, CreditBatch{ExpiresAt: inHours(20), Remain: 100})
	now := time.Now()
	p.mu.RLock()
	e := p.byUID["e20h"]
	_, isCritical := e.criticalExpiryAt(now, p.criticalExpiry)
	_, isUrgent := e.urgentExpiryAt(now, p.urgentExpiry)
	p.mu.RUnlock()
	if !isCritical || !isUrgent {
		t.Fatalf("20h 到期应同时在两层内：critical=%v urgent=%v", isCritical, isUrgent)
	}
}

// TestExpiryLayerStickyPathRespectsCriticalLayer 粘性新建分配侧同口径：
// 无免费号时，24h 层非空就只把 24h 层交给哈希分配，72h 号不得混进来。
func TestExpiryLayerStickyPathRespectsCriticalLayer(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "a20h"})
	p.Add(&auth.Auth{UID: "u60h"})
	p.NoteModelCost("a20h", "m", 2.9, 1000)
	p.NoteModelCost("u60h", "m", 2.9, 1000)
	setBatches(p, "a20h", 100, 100, CreditBatch{ExpiresAt: inHours(20), Remain: 100})
	setBatches(p, "u60h", 1000, 1000, CreditBatch{ExpiresAt: inHours(60), Remain: 1000})

	uids := p.UrgentUIDsForModelRealm("m", "")
	if len(uids) != 1 || uids[0] != "a20h" {
		t.Fatalf("粘性候选集应只含 24h 层，got %v（want [a20h]）", uids)
	}
}
