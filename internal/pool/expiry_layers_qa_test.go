// 24h/72h 到期分层的**独立**补充验收（QA 侧，专打 expiry_layers_test.go 没覆盖的边界）。
//
// 与 expiry_layers_test.go 的分工：
//   - 那边锁的是"分层够不够"（24h 压 72h、免费跨层、窗口可配、并列确定性）；
//   - 这边打的是"分层在真实负载/边界下会不会漏"：
//     a) 持续使用语义（单号大批次 + 反复扣分，1000 次必须一直是它，且不返回 nil）
//     b) 批次扣穿后前移（critical → urgent 的真实迁移，而不是读代码推断）
//     c) 窗口边界的闭区间一致性（24h 与 72h 必须同构，不能一个闭一个开）
//     d) 在途放宽路径的副作用（tried/healthy/floorBlocked 三条约束必须不被放宽）
//     e) 在途放宽路径的 realm 域过滤（域外号不得被"放宽"捞进来）
//     f) 粘性侧在紧急层饱和时的语义（会不会把新会话绑到满载号上）
//     g) 粘性侧是否读**池子生效值**的窗口（热重载注入的另一个消费点）
//     并发）SetExpiryWindows 与 pick/粘性并发（-race 下必须无数据竞争）
//
// 断言口径：一律用 t.Fatalf/t.Errorf 真失败，不使用 t.Log 软断言；
// 随机源只用于让"常规逻辑"这条路可预测（权重首位），分层分支本身不做加权随机，
// 故分层断言不依赖注入源（不是同义反复）。
package pool

import (
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// qaPool 构造确定性选号环境（与 newLayerPool 同口径，独立命名避免依赖别的测试文件）。
func qaPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	t.Cleanup(p.Close)
	return p
}

// qaPaid 把账号在该模型上标成"实测收费"（tier 2），避免落进"免费层"分支。
// 注意 NoteModelCost 会顺带扣减余额/批次，故调用顺序必须是「先标价 → 再写权威批次」。
func qaPaid(t *testing.T, p *Pool, uid string) {
	t.Helper()
	p.NoteModelCost(uid, "m", 2.9, 1000)
}

// ---------------------------------------------------------------------------
// (a) 持续使用语义：24h 层单号 + 批次量大 + 反复请求
// ---------------------------------------------------------------------------

// TestQAExpiryCriticalSustainedUseUntilBatchChanges 用户原话的直译验收：
// "把一天内有积分到期的按到期最快的账号持续使用，直到到期时间有变化"。
//
// 场景：c20h（20h 后到期，Remain=100000，每请求扣 1 分）+ rich（无到期批次，
// 积分是 c20h 的 1 万倍）。若"持续使用"语义被破坏（比如退到加权随机、或按
// minPickGap/LRU 分散），rich 会分担相当一部分请求。
//
// 断言：1000 次 pick **全部**命中 c20h、无一返回 nil、且批次与余额同步递减 1000
// （证明"持续使用"是靠在途/权重之外的硬优先实现，而不是靠余额权重偶然倾斜）。
func TestQAExpiryCriticalSustainedUseUntilBatchChanges(t *testing.T) {
	p := qaPool(t)
	p.Add(&auth.Auth{UID: "c20h"})
	p.Add(&auth.Auth{UID: "rich"})
	qaPaid(t, p, "c20h")
	qaPaid(t, p, "rich")
	exp := inHours(20)
	setBatches(p, "c20h", 100000, 100000, CreditBatch{ExpiresAt: exp, Remain: 100000})
	setBatches(p, "rich", 1_000_000_000, 0)

	const rounds = 1000
	for i := 0; i < rounds; i++ {
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil {
			t.Fatalf("第 %d 次 pick 返回 nil：24h 层有号可用时不得选不出号", i)
		}
		if a.UID != "c20h" {
			t.Fatalf("第 %d 次选中 %s，want c20h（24h 层必须被持续使用，直到它的到期时间变化）", i, a.UID)
		}
		p.NoteModelCost(a.UID, "m", 1, 1000) // 每请求真实扣 1 分
	}

	p.mu.RLock()
	e := p.byUID["c20h"]
	credits := e.credits
	var remain int64
	var batches int
	for _, b := range e.creditBatches {
		remain += b.Remain
		batches++
	}
	at, ok := e.earliestExpiryAt(time.Now())
	_, stillCritical := e.criticalExpiryAt(time.Now(), p.criticalExpiry)
	p.mu.RUnlock()

	if credits != 100000-rounds {
		t.Errorf("1000 次扣 1 分后余额 = %d，want %d", credits, 100000-rounds)
	}
	if remain != 100000-rounds || batches != 1 {
		t.Errorf("批次剩余合计 = %d（%d 条），want %d（1 条）", remain, batches, 100000-rounds)
	}
	if !ok || !at.Equal(exp) {
		t.Errorf("到期时间被改动：got %v ok=%v，want %v（未扣穿前不得前移）", at, ok, exp)
	}
	if !stillCritical {
		t.Error("批次未扣穿（剩 99000）时应仍在 24h 层内")
	}
}

// ---------------------------------------------------------------------------
// (a2) 「免费跨层优先」的**可判别**用例（用户最在意的那条）
// ---------------------------------------------------------------------------

// TestQAFreeLayerCrossesLayerAndOrdersByExpiry 为什么必须单独写一条：
//
// 变异实验发现「免费号压过收费 24h 号」在**删掉免费层之后依然成立**——因为返回 nil
// 会落到常规路径的 tier 0 硬过滤，免费号照样赢。也就是说
// TestExpiryFreeUrgentStillBeatsCriticalPaid 的结论对实现无判别力（它只锁结果，
// 锁不住"免费 ∩ 临期"这一层本身）。
//
// 能判别实现是否真的做了「免费 ∩ (24h ∪ 72h) 并集 + 层内按最早到期」的场景：
//   - f50h（免费，50h，只有 100 分）、f60h（免费，60h，积分 1e6）——两个免费号
//     都在 72h 层、都不在 24h 层；
//   - p20h（收费，20h）——不在免费层。
//
// 正确实现：freeUrgent = [f50h, f60h] → 按最早到期 → f50h 恒中（p20h 一分流量都
// 拿不到，f60h 也拿不到）。
// 若免费层退化（只认 24h 窗口 / 直接返回 nil）：常规路径按权重挑 f60h（积分多
// 1 万倍）→ 本用例立即变红。故本用例同时锁住两件事：
//  1. 免费号跨层压过收费 24h 号（用户原话：不许为了烧临期积分去选收费号）；
//  2. 免费层内部仍按**最早到期**排序（不是按余额权重）。
func TestQAFreeLayerCrossesLayerAndOrdersByExpiry(t *testing.T) {
	p := qaPool(t)
	p.Add(&auth.Auth{UID: "f50h"})
	p.Add(&auth.Auth{UID: "f60h"})
	p.Add(&auth.Auth{UID: "p20h"})
	// tier 0：实测免费（cost=0）。
	p.NoteModelCost("f50h", "m", 0, 1000)
	p.NoteModelCost("f60h", "m", 0, 1000)
	qaPaid(t, p, "p20h")
	setBatches(p, "f50h", 100, 100, CreditBatch{ExpiresAt: inHours(50), Remain: 100})
	setBatches(p, "f60h", 1_000_000, 1_000_000, CreditBatch{ExpiresAt: inHours(60), Remain: 1_000_000})
	setBatches(p, "p20h", 100, 100, CreditBatch{ExpiresAt: inHours(20), Remain: 100})

	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil {
			t.Fatalf("第 %d 次 pick 返回 nil", i)
		}
		if a.UID != "f50h" {
			t.Fatalf("第 %d 次选中 %s，want f50h（免费层跨层优先 + 层内最早到期；"+
				"若落到常规权重路径会按余额选 f60h，若免费层被 24h 窗口截断则会给 p20h）", i, a.UID)
		}
	}

	// 粘性侧同口径：新会话的哈希候选集应只含免费 ∩ 临期的两个号，收费 24h 号不出现。
	uids := p.UrgentUIDsForModelRealm("m", "")
	sortStrings(uids)
	if len(uids) != 2 || uids[0] != "f50h" || uids[1] != "f60h" {
		t.Fatalf("粘性候选集 = %v，want [f50h f60h]（收费 24h 号不得混入免费层）", uids)
	}
}

// sortStrings 就地升序排序（避免为一条断言引入 sort 依赖包别名冲突）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ---------------------------------------------------------------------------
// (b) 批次扣穿后前移：critical → urgent 的真实迁移
// ---------------------------------------------------------------------------

// TestQABatchDepletionDemotesCriticalToUrgent 用真实扣分路径证明分层迁移发生了。
//
// a20h：两条批次（20h/Remain=50 + 55h/Remain=100），初始在 24h 层；
// b40h：单条批次（40h/Remain=1000），初始只在 72h 层。
//
// 阶段 1：a20h 在 24h 层 → 独占流量（b40h 更晚 + 在低优先级层）。
// 扣 51 分 → 20h 那条被扣穿移除，a20h 的最早到期推进到 55h → 掉出 24h 层。
// 阶段 2：24h 层为空 → 72h 层内按最早到期排序 → b40h(40h) 接管。
//
// 这条用例的价值：迁移不是靠读代码推断——它由 p.NoteModelCost（真实扣分 →
// debitBatchesLocked）触发，并把"扣穿后还算不算临期"这条最容易写错的口径钉死。
func TestQABatchDepletionDemotesCriticalToUrgent(t *testing.T) {
	p := qaPool(t)
	p.Add(&auth.Auth{UID: "a20h"})
	p.Add(&auth.Auth{UID: "b40h"})
	qaPaid(t, p, "a20h")
	qaPaid(t, p, "b40h")

	b1 := CreditBatch{ExpiresAt: inHours(20), Remain: 50}
	b2 := CreditBatch{ExpiresAt: inHours(55), Remain: 100}
	setBatches(p, "a20h", 150, 150, b1, b2)
	setBatches(p, "b40h", 1000, 1000, CreditBatch{ExpiresAt: inHours(40), Remain: 1000})

	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "a20h" {
		t.Fatalf("阶段 1：pick = %v，want a20h（20h 在 24h 层内，压过 40h）", a)
	}

	// 扣 51 分：恰好扣穿 20h 那条（Remain=50）并把 55h 那条降到 99。
	p.NoteModelCost("a20h", "m", 51, 1000)

	now := time.Now()
	p.mu.RLock()
	ea := p.byUID["a20h"]
	at, ok := ea.earliestExpiryAt(now)
	_, isCritical := ea.criticalExpiryAt(now, p.criticalExpiry)
	_, isUrgent := ea.urgentExpiryAt(now, p.urgentExpiry)
	nb := len(ea.creditBatches)
	p.mu.RUnlock()

	if !ok {
		t.Fatal("扣穿 20h 批次后仍应有未到期批次（55h 那条）")
	}
	if nb != 1 {
		t.Fatalf("扣穿后应剩 1 条批次，got %d", nb)
	}
	if !at.Equal(b2.ExpiresAt) {
		t.Fatalf("最早到期应前移到 55h 那条：got %v，want %v", at, b2.ExpiresAt)
	}
	if isCritical {
		t.Fatal("最早到期已推进到 55h，不该仍在 24h 层")
	}
	if !isUrgent {
		t.Fatal("55h 仍在 72h 层内，应降级到 urgent 而不是彻底退出紧急层")
	}

	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "b40h" {
		t.Fatalf("阶段 2：pick = %v，want b40h（24h 层已空，72h 层内 40h 早于 55h，应接管）", a)
	}
}

// ---------------------------------------------------------------------------
// (c) 窗口边界精确性：两层判据必须同构（同为闭区间）
// ---------------------------------------------------------------------------

// TestQAExpiryWindowBoundaryIsClosedAndConsistent 边界口径：
//   - 到期时刻**恰好等于** now+window 时命中（判据是 `at.After(now+window)` 判否 ⇒ 闭区间）；
//   - +1ns 即落窗外；
//   - 24h 层与 72h 层走同一个 expiryWithin，**不允许**出现"24h 闭、72h 开"的不一致。
//
// 为什么必须测：边界差一个 ns 在测试里看不出，但在生产上表现为"整点批量赠送的积分
// 到底算不算一天内到期"——批量赠送的到期时刻常常是完全相同的整点，实际影响面不小。
func TestQAExpiryWindowBoundaryIsClosedAndConsistent(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "b"})
	now := time.Now()

	// 1) 逐窗口验证"恰好命中 / 差 1ns 不命中"，两层共用同一个判据函数。
	for _, w := range []time.Duration{
		time.Minute, time.Hour, p.criticalExpiry, 48 * time.Hour, p.urgentExpiry,
	} {
		exact := now.Add(w)
		setBatches(p, "b", 100, 100, CreditBatch{ExpiresAt: exact, Remain: 100})
		p.mu.RLock()
		at, ok := p.byUID["b"].expiryWithin(now, w)
		p.mu.RUnlock()
		if !ok || !at.Equal(exact) {
			t.Fatalf("window=%v：到期恰好等于 now+window 应命中（闭区间），got ok=%v at=%v", w, ok, at)
		}

		setBatches(p, "b", 100, 100, CreditBatch{ExpiresAt: exact.Add(time.Nanosecond), Remain: 100})
		p.mu.RLock()
		_, ok = p.byUID["b"].expiryWithin(now, w)
		p.mu.RUnlock()
		if ok {
			t.Fatalf("window=%v：now+window+1ns 必须落窗外（上界开）", w)
		}
	}

	// 2) 分层边界矩阵：24h 恰好 = 两层都命中；72h 恰好 = 只有外层命中。
	cases := []struct {
		name       string
		offset     time.Duration
		wantCritic bool
		wantUrgent bool
	}{
		{"1分钟", time.Minute, true, true},
		{"23h59m", 23*time.Hour + 59*time.Minute, true, true},
		{"恰好24h", p.criticalExpiry, true, true},
		{"24h+1ns", p.criticalExpiry + time.Nanosecond, false, true},
		{"48h", 48 * time.Hour, false, true},
		{"71h59m", 71*time.Hour + 59*time.Minute, false, true},
		{"恰好72h", p.urgentExpiry, false, true},
		{"72h+1ns", p.urgentExpiry + time.Nanosecond, false, false},
		{"30天", 30 * 24 * time.Hour, false, false},
	}
	for _, c := range cases {
		setBatches(p, "b", 100, 100, CreditBatch{ExpiresAt: now.Add(c.offset), Remain: 100})
		p.mu.RLock()
		e := p.byUID["b"]
		_, isCritic := e.criticalExpiryAt(now, p.criticalExpiry)
		_, isUrgent := e.urgentExpiryAt(now, p.urgentExpiry)
		p.mu.RUnlock()
		if isCritic != c.wantCritic || isUrgent != c.wantUrgent {
			t.Errorf("%s：critical=%v urgent=%v，want critical=%v urgent=%v",
				c.name, isCritic, isUrgent, c.wantCritic, c.wantUrgent)
		}
		if isCritic && !isUrgent {
			t.Errorf("%s：破坏了 critical ⊆ urgent 不变量", c.name)
		}
	}
}

// ---------------------------------------------------------------------------
// (d) 在途放宽路径的副作用：只能放宽 inFlightFull，不能放宽 tried/healthy/floor
// ---------------------------------------------------------------------------

// TestQARelaxedInFlightKeepsFloorBlock d 的主用例：积分保底不得被放宽路径绕开。
//
// 构造"候选集为空"（只有触底 24h 号 + 一个冷却号），这样 pick 一定会走到
// pickRelaxedEmergencyLocked。若该函数放宽了 floorBlocked，触底号会被捞回来
// 接收费模型——等于绕过积分保底，直接把余额打穿。
//
// 子用例顺序有意为之：先给**阳性对照**（同一场景但余额充足）证明放宽路径确实被
// 触发且能选中 24h 号，再给阴性用例证明"余额低于 floor"这一个变量就能把它挡住——
// 否则阴性用例可能只是"路径没走到"。
func TestQARelaxedInFlightKeepsFloorBlock(t *testing.T) {
	t.Run("阳性对照-余额充足时放宽路径生效", func(t *testing.T) {
		p := qaPool(t)
		p.SetMaxInFlight(1)
		p.SetCreditFloor(100, 100)
		p.Add(&auth.Auth{UID: "h20h"})
		p.Add(&auth.Auth{UID: "cool"})
		qaPaid(t, p, "h20h")
		qaPaid(t, p, "cool")
		setBatches(p, "h20h", 500, 500, CreditBatch{ExpiresAt: inHours(20), Remain: 500})
		setBatches(p, "cool", 1000, 1000, CreditBatch{ExpiresAt: inHours(1), Remain: 1000})
		p.Cooldown("cool", CoolSoft, 10*time.Minute, "test")
		if !p.Acquire("h20h") {
			t.Fatal("h20h 应能占到在途名额")
		}

		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil || a.UID != "h20h" {
			t.Fatalf("pick = %v，want h20h（唯一健康候选在途占满时应放宽上限，不得落到冷却号 cool）", a)
		}
	})

	t.Run("余额低于floor时不得被放宽路径捞回", func(t *testing.T) {
		p := qaPool(t)
		p.SetMaxInFlight(1)
		p.SetCreditFloor(100, 100)
		p.Add(&auth.Auth{UID: "low20h"})
		p.Add(&auth.Auth{UID: "cool"})
		qaPaid(t, p, "low20h")
		qaPaid(t, p, "cool")
		// 余额 3 < floor 100 且模型实测收费 ⇒ floorBlocked 为真。
		setBatches(p, "low20h", 3, 3, CreditBatch{ExpiresAt: inHours(20), Remain: 3})
		setBatches(p, "cool", 1000, 1000, CreditBatch{ExpiresAt: inHours(1), Remain: 1000})
		p.Cooldown("cool", CoolSoft, 10*time.Minute, "test")
		if !p.Acquire("low20h") {
			t.Fatal("low20h 应能占到在途名额")
		}

		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil {
			t.Fatal("pick 返回 nil：有可用冷却兜底号时不该 nil（兜底只应跳过触底号）")
		}
		if a.UID == "low20h" {
			t.Fatal("触底号（credits=3 < floor=100 + 收费模型）被在途放宽路径捞回来了，绕过积分保底")
		}
		if a.UID != "cool" {
			t.Fatalf("pick = %s，want cool（保底拦掉触底号后应由冷却兜底承接）", a.UID)
		}
	})

	t.Run("tried不被放宽", func(t *testing.T) {
		p := qaPool(t)
		p.SetMaxInFlight(1)
		p.Add(&auth.Auth{UID: "h20h"})
		p.Add(&auth.Auth{UID: "cool"})
		qaPaid(t, p, "h20h")
		qaPaid(t, p, "cool")
		setBatches(p, "h20h", 500, 500, CreditBatch{ExpiresAt: inHours(20), Remain: 500})
		setBatches(p, "cool", 1000, 1000, CreditBatch{ExpiresAt: inHours(1), Remain: 1000})
		p.Cooldown("cool", CoolSoft, 10*time.Minute, "test")
		if !p.Acquire("h20h") {
			t.Fatal("h20h 应能占到在途名额")
		}

		a := p.PickExcludingForRealm(map[string]bool{"h20h": true}, "m", "")
		if a != nil && a.UID == "h20h" {
			t.Fatal("请求级 tried 被在途放宽路径忽略：同一请求内会重复打同一个失败号")
		}
		if a == nil || a.UID != "cool" {
			t.Fatalf("pick = %v，want cool（tried 的 h20h 不得被放宽捞回）", a)
		}
	})

	t.Run("healthy不被放宽", func(t *testing.T) {
		p := qaPool(t)
		p.SetMaxInFlight(1)
		p.Add(&auth.Auth{UID: "cool20h"})
		p.Add(&auth.Auth{UID: "x60h"})
		qaPaid(t, p, "cool20h")
		qaPaid(t, p, "x60h")
		setBatches(p, "cool20h", 500, 500, CreditBatch{ExpiresAt: inHours(20), Remain: 500})
		setBatches(p, "x60h", 500, 500, CreditBatch{ExpiresAt: inHours(60), Remain: 500})
		p.Cooldown("cool20h", CoolSoft, 10*time.Minute, "test")
		if !p.Acquire("x60h") {
			t.Fatal("x60h 应能占到在途名额")
		}

		// 若 healthy 被放宽，20h 的冷却号会赢过 60h 的健康号——正是本用例要排除的形态。
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil || a.UID != "x60h" {
			t.Fatalf("pick = %v，want x60h（冷却中的 20h 号即便更早到期也不得被放宽捞回）", a)
		}
	})
}

// TestQARelaxedInFlightRespectsRealm e 的用例：放宽路径必须保留 realm 域过滤。
//
// cn20h（cn 域，20h 到期，在途占满）+ gl10h（global 域，10h 到期，在途空闲）。
// 钉 cn 域请求 → 候选集为空 → 放宽重扫。若 healthyOf 里的 realm 判断失效，
// gl10h（更早到期）会抢走 cn 请求，等于把请求路由到域外账号（多吃一次 11102）。
//
// 对照：同一池子不钉域（全池）时必须选中 gl10h——证明 gl10h 本身是可选的、
// 且确实比 cn20h 更早到期，从而证明 cn 域那次被排除的唯一原因是域过滤。
func TestQARelaxedInFlightRespectsRealm(t *testing.T) {
	p := qaPool(t)
	p.SetMaxInFlight(1)
	cn := &auth.Auth{UID: "cn20h"}
	cn.SetRealm("cn")
	gl := &auth.Auth{UID: "gl10h"}
	gl.SetRealm("global")
	p.Add(cn)
	p.Add(gl)
	qaPaid(t, p, "cn20h")
	qaPaid(t, p, "gl10h")
	setBatches(p, "cn20h", 500, 500, CreditBatch{ExpiresAt: inHours(20), Remain: 500})
	setBatches(p, "gl10h", 500, 500, CreditBatch{ExpiresAt: inHours(10), Remain: 500})
	if !p.Acquire("cn20h") {
		t.Fatal("cn20h 应能占到在途名额")
	}

	// 钉 cn：候选集只剩（占满的）cn20h → 为空 → 放宽重扫 → 只能是 cn20h。
	a := p.PickExcludingForRealm(nil, "m", "cn")
	if a == nil {
		t.Fatal("钉 cn 时 pick 返回 nil：放宽重扫应保住域内的 24h 号")
	}
	if a.UID == "gl10h" {
		t.Fatalf("钉 cn 的请求被路由到 global 域号（放宽路径丢了 realm 过滤）")
	}
	if a.UID != "cn20h" {
		t.Fatalf("pick = %s，want cn20h", a.UID)
	}

	// 对照：全池口径下 gl10h（10h，更早到期）本就该赢——说明上一条的排除原因是域。
	b := p.PickExcludingForRealm(nil, "m", "")
	if b == nil || b.UID != "gl10h" {
		t.Fatalf("全池 pick = %v，want gl10h（10h 早于 20h，可作为对照基线）", b)
	}
}

// ---------------------------------------------------------------------------
// (f) 粘性侧在紧急层饱和时的语义
// ---------------------------------------------------------------------------

// TestQAStickyUrgentSetUnderSaturation 钉住 UrgentUIDsForModelRealms（粘性新建分配
// 的候选集）在饱和下的实际返回，回答"新会话会不会被绑到满载号上"。
//
// 实际行为（本用例证明）：
//   - 24h 层唯一号在途占满 → 粘性侧把它**过滤掉**（cands 构建阶段就排除了
//     inFlightFull），返回 72h 层 → 新会话绑到 72h 号，不会绑到满载号；
//   - 两层都占满 → 返回 nil（会话路由退回常规分配）；
//   - 但 pick 侧在同一时刻会**放宽**在途上限继续烧 24h 号。
//
// 两条路径在饱和下不一致（粘性退回常规、pick 放宽），后果见报告"设计存疑"。
func TestQAStickyUrgentSetUnderSaturation(t *testing.T) {
	p := qaPool(t)
	p.SetMaxInFlight(1)
	p.Add(&auth.Auth{UID: "c20h"})
	p.Add(&auth.Auth{UID: "u60h"})
	qaPaid(t, p, "c20h")
	qaPaid(t, p, "u60h")
	setBatches(p, "c20h", 500, 500, CreditBatch{ExpiresAt: inHours(20), Remain: 500})
	setBatches(p, "u60h", 500, 500, CreditBatch{ExpiresAt: inHours(60), Remain: 500})

	if uids := p.UrgentUIDsForModelRealm("m", ""); len(uids) != 1 || uids[0] != "c20h" {
		t.Fatalf("空闲时粘性候选集 = %v，want [c20h]", uids)
	}
	if !p.Acquire("c20h") {
		t.Fatal("c20h 应能占到在途名额")
	}
	uids := p.UrgentUIDsForModelRealm("m", "")
	if len(uids) != 1 || uids[0] != "u60h" {
		t.Fatalf("24h 层占满时粘性候选集 = %v，want [u60h]（不得把新会话绑到满载号上）", uids)
	}
	if !p.Acquire("u60h") {
		t.Fatal("u60h 应能占到在途名额")
	}
	if uids := p.UrgentUIDsForModelRealm("m", ""); len(uids) != 0 {
		t.Fatalf("两层都占满时粘性候选集 = %v，want 空（应退回常规分配）", uids)
	}
	// 对比：同一时刻 pick 侧会放宽在途上限继续烧 24h 号。
	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "c20h" {
		t.Fatalf("pick = %v，want c20h（饱和时 pick 侧放宽，与粘性侧行为不同）", a)
	}
}

// TestQAStickyUrgentSetIgnoresFloor 钉住粘性候选集**不做**积分保底判定的现状：
// 触底 + 收费的 24h 号仍会进入粘性候选集。
//
// 这不是本轮改动引入的（老 72h 层同形），但 24h 层恰恰是"低余额 + 快到期"的号，
// 命中概率显著变高，故明确记录其后果与自愈链路：会话被绑到它之后，
// PickByUIDForModel 会因同一条保底判据返回 nil → handler 解绑粘性 → 走普通轮换。
func TestQAStickyUrgentSetIgnoresFloor(t *testing.T) {
	p := qaPool(t)
	p.SetCreditFloor(100, 100)
	p.Add(&auth.Auth{UID: "poor20h"})
	qaPaid(t, p, "poor20h")
	setBatches(p, "poor20h", 3, 3, CreditBatch{ExpiresAt: inHours(20), Remain: 3})

	uids := p.UrgentUIDsForModelRealm("m", "")
	if len(uids) != 1 || uids[0] != "poor20h" {
		t.Fatalf("粘性候选集 = %v，want [poor20h]（现状：粘性侧不判 floor）", uids)
	}
	// 但真正取号时会被保底拦住（→ handler 解绑粘性），这是自愈链路的关键一环。
	if a := p.PickByUIDForModel("poor20h", "m"); a != nil {
		t.Fatalf("触底号被 PickByUIDForModel 放行（%s）：保底在粘性命中路径失效", a.UID)
	}
}

// ---------------------------------------------------------------------------
// (g) 粘性侧必须读池子生效窗口（热重载注入的第二消费点）
// ---------------------------------------------------------------------------

// TestQAStickyPathUsesLiveWindows UrgentUIDsForModelRealms 走的是 p.mu.RLock 路径，
// 与 pick 侧（写锁路径）不是同一段代码。只测 pick 侧无法证明窗口注入在粘性路径也生效。
func TestQAStickyPathUsesLiveWindows(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "a20h"})
	qaPaid(t, p, "a20h")
	setBatches(p, "a20h", 500, 500, CreditBatch{ExpiresAt: inHours(20), Remain: 500})

	if uids := p.UrgentUIDsForModelRealm("m", ""); len(uids) != 1 {
		t.Fatalf("默认窗口下粘性候选集 = %v，want 1 个", uids)
	}
	p.SetExpiryWindows(6*time.Hour, 2*time.Hour)
	if uids := p.UrgentUIDsForModelRealm("m", ""); len(uids) != 0 {
		t.Fatalf("窗口收到 6h/2h 后粘性候选集 = %v，want 空（20h 已不在紧急窗口内）", uids)
	}
	p.SetExpiryWindows(72*time.Hour, 24*time.Hour)
	if uids := p.UrgentUIDsForModelRealm("m", ""); len(uids) != 1 {
		t.Fatalf("窗口恢复默认后粘性候选集 = %v，want 1 个（应实时跟随池子生效值）", uids)
	}
}

// ---------------------------------------------------------------------------
// 并发：SetExpiryWindows 与 pick / 粘性并发（-race 必过）
// ---------------------------------------------------------------------------

// TestQAExpiryWindowsConcurrentSetAndPick 热重载（SetExpiryWindows，写锁）与
// 选号（pick，写锁）/ 粘性候选集（UrgentUIDs，读锁）并发时不得有数据竞争，
// 且不得出现"窗口处于半更新状态"导致的 nil 选号。
func TestQAExpiryWindowsConcurrentSetAndPick(t *testing.T) {
	p := qaPool(t)
	p.Add(&auth.Auth{UID: "a20h"})
	p.Add(&auth.Auth{UID: "b60h"})
	p.Add(&auth.Auth{UID: "c200h"})
	for _, u := range []string{"a20h", "b60h", "c200h"} {
		qaPaid(t, p, u)
	}
	setBatches(p, "a20h", 500, 500, CreditBatch{ExpiresAt: inHours(20), Remain: 500})
	setBatches(p, "b60h", 500, 500, CreditBatch{ExpiresAt: inHours(60), Remain: 500})
	setBatches(p, "c200h", 500, 500, CreditBatch{ExpiresAt: inHours(200), Remain: 500})

	var wg sync.WaitGroup
	const iters = 200

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if i%2 == 0 {
				p.SetExpiryWindows(72*time.Hour, 24*time.Hour)
			} else {
				p.SetExpiryWindows(2*time.Hour, time.Hour)
			}
		}
	}()

	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if a := p.PickExcludingForRealm(nil, "m", ""); a == nil {
					t.Errorf("并发改窗口期间 pick 返回 nil（池里始终有可用号）")
					return
				}
				_ = p.UrgentUIDsForModelRealm("m", "")
				u, c := p.ExpiryWindows()
				if u <= 0 || c <= 0 || c > u {
					t.Errorf("并发读取到非法窗口组合 urgent=%v critical=%v", u, c)
					return
				}
			}
		}()
	}
	wg.Wait()
}
