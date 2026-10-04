// 积分保底（credit floor）：账号余额低于 floor 时，对**收费**模型不再参与选号
// ——防止收费请求把余额打穿、连免费模型都 402 冷却到次日签到（最坏约 11.5 小时）。
//
// 收费判据两级（见 pick.floorBlockedForModel）：
//  1. 本地实测台账（该号该模型实测 cost>0）；
//  2. 上游目录倍率兜底（本地无观测时用牌价判——否则「没学过」恒等于「放行」，
//     高价新模型会把触底号一笔打穿）。
//
// 免费模型（实测 cost=0 或目录倍率 0）不受影响：保底保的是「还有余额可用」，
// 不是「什么都别调」。签到回血（SetCreditsDetailed）后自动恢复。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// rates 构造 (realm, 模型) → 上游目录倍率的查表函数（倍率是字符串，如 "1.62"）。
func rates(m map[string]map[string]string) func(realm, model string) string {
	return func(realm, model string) string { return m[realm][model] }
}

// TestCreditFloorBlocksPaidBelowFloor 触底号被拦在实测收费模型之外：
// 低于 floor 的号即便积分权重再高，也不能对实测收费模型出票。
func TestCreditFloorBlocksPaidBelowFloor(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "poor"})
	p.Add(&auth.Auth{UID: "rich"})

	// poor 触底 + 实测收费；rich 余额充足 + 实测收费。
	p.SetCredits("poor", 30)
	p.SetCredits("rich", 1_000_000)
	p.NoteModelCost("poor", "hy4-preview", 2.9, 1000)
	p.NoteModelCost("rich", "hy4-preview", 5.0, 1000)

	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealm(nil, "hy4-preview", "")
		if a == nil {
			t.Fatalf("第 %d 次选号返回 nil，want rich（触底号被 floor 拦截，rich 应承接）", i)
		}
		if a.UID == "poor" {
			t.Fatalf("第 %d 次选中 poor（credits=30 < floor=100 且模型实测收费），floor 应拦截", i)
		}
	}
}

// TestCreditFloorAllowsFreeBelowFloor 触底号对实测免费模型照常可选：
// 保底的目的恰是「留点余额让免费模型还能用」，免费请求 credit=0 不再扣减。
func TestCreditFloorAllowsFreeBelowFloor(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 30)
	p.NoteModelCost("poor", "hy4-preview", 0, 1000) // 实测免费

	a := p.PickExcludingForRealm(nil, "hy4-preview", "")
	if a == nil {
		t.Fatal("触底号对免费模型应照常可选，got nil")
	}
	if a.UID != "poor" {
		t.Fatalf("选中 %v，want poor（免费模型不受 floor 限制）", a.UID)
	}
}

// TestCreditFloorAllowsUnknownBelowFloor 触底号对「台账无观测且目录未覆盖」的模型
// 照常可选：无法判收费时按放行处理（未知不惩罚——目录未覆盖的多为内部/别名模型，
// 拦了会让号永久失联）。
func TestCreditFloorAllowsUnknownBelowFloor(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 30)
	// 无任何 NoteModelCost，也未注入倍率表：hy4-preview 对 poor 完全未知。

	a := p.PickExcludingForRealm(nil, "hy4-preview", "")
	if a == nil {
		t.Fatal("触底号对未知模型应照常可选，got nil")
	}
	if a.UID != "poor" {
		t.Fatalf("选中 %v，want poor（未知不受 floor 限制）", a.UID)
	}
}

// TestCreditFloorAllBelowReturnsNil 全池触底 + 全收费时选号返回 nil：
// floor 是硬语义，宁可 503 也不放行收费请求打穿保底（放行=回到「烧到 0」现状）。
func TestCreditFloorAllBelowReturnsNil(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	for _, uid := range []string{"a", "b"} {
		p.SetCredits(uid, 10)
		p.NoteModelCost(uid, "hy4-preview", 2.9, 1000)
	}

	if a := p.PickExcludingForRealm(nil, "hy4-preview", ""); a != nil {
		t.Fatalf("全池触底且全收费，want nil，got %v（floor 硬语义：不放行）", a.UID)
	}
}

// TestCreditFloorRecoversAfterCheckin 签到回血后自动恢复：floor 只读当前 credits，
// SetCreditsDetailed 刷回权威余额越过 floor 即刻放行，无需任何复位操作。
func TestCreditFloorRecoversAfterCheckin(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "only"})
	p.SetCredits("only", 30)
	p.NoteModelCost("only", "hy4-preview", 2.9, 1000)

	if a := p.PickExcludingForRealm(nil, "hy4-preview", ""); a != nil {
		t.Fatalf("触底期应被拦，got %v", a.UID)
	}
	// 签到回血：权威余额刷新到 floor 之上。
	p.SetCreditsDetailed("only", 500, 0)
	a := p.PickExcludingForRealm(nil, "hy4-preview", "")
	if a == nil {
		t.Fatal("回血后（credits=500 ≥ floor=100）应恢复可选，got nil")
	}
	if a.UID != "only" {
		t.Fatalf("选中 %v，want only", a.UID)
	}
}

// TestCreditFloorZeroDisables floor=0（默认）完全关闭：行为与引入前逐字一致。
func TestCreditFloorZeroDisables(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	// 不调用 SetCreditFloor：零值即默认关闭。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 1)
	p.NoteModelCost("poor", "hy4-preview", 2.9, 1000)

	a := p.PickExcludingForRealm(nil, "hy4-preview", "")
	if a == nil || a.UID != "poor" {
		t.Fatalf("floor=0 应关闭保底（触底收费号照常可选），got %v", a)
	}
}

// TestCreditFloorBoundaryAtFloor credits 恰好等于 floor 时不拦：
// 语义是「低于 floor 才拦」（credits < floor），等于 floor 仍在安全线之上。
func TestCreditFloorBoundaryAtFloor(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "edge"})
	p.NoteModelCost("edge", "hy4-preview", 2.9, 1000)
	// 顺序注意：NoteModelCost 会实扣余额（本地插值），设置边界值必须放在观测之后。
	p.SetCredits("edge", 100)

	a := p.PickExcludingForRealm(nil, "hy4-preview", "")
	if a == nil || a.UID != "edge" {
		t.Fatalf("credits=100 == floor=100 应放行（低于才拦），got %v", a)
	}
}

// TestCreditFloorStickyPathBlocked 粘性路径同样被 floor 约束：
// 触底号即便被会话粘住，对收费模型也不得继续出票（PickByUIDForModel 返回 nil，
// handler 侧解绑换号）。
func TestCreditFloorStickyPathBlocked(t *testing.T) {
	p := New("")
	p.SetCreditFloor(100)
	p.Add(&auth.Auth{UID: "sticky"})
	p.SetCredits("sticky", 30)
	p.NoteModelCost("sticky", "hy4-preview", 2.9, 1000)

	if a := p.PickByUIDForModel("sticky", "hy4-preview"); a != nil {
		t.Fatalf("粘性号触底 + 收费模型应被 floor 拦（want nil），got %v", a.UID)
	}
	// 免费模型不受影响：同一粘性号照常出票。
	p.NoteModelCost("sticky", "free-model", 0, 1000)
	if a := p.PickByUIDForModel("sticky", "free-model"); a == nil {
		t.Fatal("粘性号触底 + 免费模型应照常出票，got nil")
	}
}

// ---------------------------------------------------------------------------
// 目录倍率兜底：堵住「无观测 = 放行」漏洞
// ---------------------------------------------------------------------------

// TestCreditFloorBlocksUnobservedPaidModelByCatalogRate 实案回归（kimi-k3-1）：
// 某模型**全池无实测观测**时，仅看本地台账会恒判「放行」——而该模型在上游目录里
// 是明确收费的（x1.62），两笔就能把 100 分的号打穿到 0 并硬冷却到次日 04:00。
// 目录倍率必须堵住这个洞。
//
// 用**单账号**断言而非「多账号看选中谁」：后者会被权重/随机源干扰——即便 floor
// 失效，rich 也可能因为权重高而被选中，测试假绿（实测踩到过）。单账号池里
// 「返回 nil」与「返回 poor」是 floor 生效与否的干净二分。
func TestCreditFloorBlocksUnobservedPaidModelByCatalogRate(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	// 上游目录：kimi-k3-1 = x1.62 收费。
	p.SetModelRateOf(rates(map[string]map[string]string{
		"global": {"kimi-k3-1": "1.62"},
	}))

	g := &auth.Auth{UID: "poor"}
	g.SetRealm("global")
	p.Add(g)
	p.SetCredits("poor", 30) // 触底
	// 刻意**不**调 NoteModelCost：无实测观测（实案形态）。

	if a := p.PickExcludingForRealm(nil, "kimi-k3-1", "global"); a != nil {
		t.Fatalf("触底号应被目录倍率 x1.62 拦住（无观测不得等同放行），got %v", a)
	}

	// 对照：回血越过 floor 即恢复可选（证明拦截确实由 floor 触发，而非模型被禁）。
	p.SetCredits("poor", 500)
	if a := p.PickExcludingForRealm(nil, "kimi-k3-1", "global"); a == nil {
		t.Fatal("回血越过 floor 后应恢复可选，got nil")
	}
}

// TestCreditFloorUnobservedFreeModelStillAllowed 目录判免费的无观测模型照常放行：
// 兜底只拦收费，不得把「没学过」一刀切禁掉（否则新上的免费模型没人接）。
func TestCreditFloorUnobservedFreeModelStillAllowed(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.SetModelRateOf(rates(map[string]map[string]string{
		"global": {"new-free": "0.00"},
	}))

	g := &auth.Auth{UID: "poor"}
	g.SetRealm("global")
	p.Add(g)
	p.SetCredits("poor", 30)

	a := p.PickExcludingForRealm(nil, "new-free", "global")
	if a == nil || a.UID != "poor" {
		t.Fatalf("目录倍率 x0.00 的免费模型应放行触底号，got %v", a)
	}
}

// TestCreditFloorUnknownRateStillAllowed 目录未覆盖该模型（倍率未知）→ 放行。
// 有意保守：目录未覆盖的多为内部/别名模型，拦了会让号永久失联。
func TestCreditFloorUnknownRateStillAllowed(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.SetModelRateOf(rates(map[string]map[string]string{
		"global": {"known-paid": "3.31"}, // 目录里没有 unknown-model
	}))

	g := &auth.Auth{UID: "poor"}
	g.SetRealm("global")
	p.Add(g)
	p.SetCredits("poor", 30)

	a := p.PickExcludingForRealm(nil, "unknown-model", "global")
	if a == nil || a.UID != "poor" {
		t.Fatalf("目录未覆盖的模型应放行（倍率未知不惩罚），got %v", a)
	}
}

// TestCreditFloorNoRateTableKeepsLegacyBehavior 未注入倍率表（nil）时退化为
// 仅本地台账判定——零回归：老部署/未装配场景下行为与引入前一致。
func TestCreditFloorNoRateTableKeepsLegacyBehavior(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	// 不调 SetModelRateOf。

	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 30)

	a := p.PickExcludingForRealm(nil, "unobserved", "")
	if a == nil || a.UID != "poor" {
		t.Fatalf("未注入倍率表时应退化为放行（零回归），got %v", a)
	}
}

// TestCreditFloorLocalLedgerFreeBeatsCatalogPaid 本地实测优先于目录：某号实测该
// 模型免费（限免/夜间优惠），目录牌价收费时以实测为准——实测是更强的证据。
func TestCreditFloorLocalLedgerFreeBeatsCatalogPaid(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.SetModelRateOf(rates(map[string]map[string]string{
		"global": {"hy4-preview-f": "0.29"}, // 目录牌价收费
	}))

	g := &auth.Auth{UID: "poor"}
	g.SetRealm("global")
	p.Add(g)
	p.SetCredits("poor", 30)
	p.NoteModelCost("poor", "hy4-preview-f", 0, 1000) // 实测免费（限免中）

	a := p.PickExcludingForRealm(nil, "hy4-preview-f", "global")
	if a == nil || a.UID != "poor" {
		t.Fatalf("实测免费应优先于目录牌价收费，got %v", a)
	}
}

// TestCreditFloorRateLookupUsesAccountRealm 倍率按**账号自身所属域**查，而非选号
// 的域集合键。
//
// 回归背景（本仓库特有）：选号是域集合形态（RealmSet），一个 pick 里可能同时有 CN
// 与 global 账号。倍率表按 "cn" / "global" 分桶存储，若拿域集合的 Key()（如
// "cn+global"）去查，表里根本没有这个键 → 恒返回空串 → 目录兜底形同虚设，
// 触底号被「收费未知」放行打穿（正是 39af6b7 要堵的那个洞）。
//
// 本例：同一模型名在两域倍率不同，只有 CN 收费。CN 号触底应被拦、global 号放行
// ——两者同处一个全池 pick，各自按自身域查表，互不串味。
func TestCreditFloorRateLookupUsesAccountRealm(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetModelRateOf(rates(map[string]map[string]string{
		"cn":     {"dual-model": "2.00"}, // CN 侧收费
		"global": {"dual-model": "0.00"}, // global 侧免费
	}))

	cnAcct := &auth.Auth{UID: "cn-poor"}
	cnAcct.SetRealm("cn")
	gAcct := &auth.Auth{UID: "g-poor"}
	gAcct.SetRealm("global")
	p.Add(cnAcct)
	p.Add(gAcct)
	p.SetCredits("cn-poor", 30)
	p.SetCredits("g-poor", 30)

	// 单账号域内断言（避免权重/随机源干扰）：CN 号触底 + CN 目录收费 → 拦。
	if a := p.PickExcludingForRealm(nil, "dual-model", "cn"); a != nil {
		t.Fatalf("CN 触底号应被 CN 目录倍率 x2.00 拦住，got %v", a.UID)
	}
	// global 号触底 + global 目录免费 → 放行（同一次改动里的另一半）。
	if a := p.PickExcludingForRealm(nil, "dual-model", "global"); a == nil || a.UID != "g-poor" {
		t.Fatalf("global 触底号对 global 目录免费模型应放行，got %v", a)
	}
}

// TestCreditFloorBlocksFallbackPath 全冷却兜底路径同样受保底约束。
// 背景：floor 把健康号全拦掉 → cands 为空 → 走 pickEarliestExpiryLocked 兜底，
// 而兜底原本不看保底 → 触底号被「捞回来」继续接收费模型，表现为同一条 floor WARN
// 反复刷同一个号（credits=1 < floor=150 仍持续中选）。兜底是最后一道选号路径，
// 保底在它之前挡不住就等于没挡。
func TestCreditFloorBlocksFallbackPath(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(150)
	p.SetModelRateOf(rates(map[string]map[string]string{"global": {"kimi-k3": "1.62"}}))

	poor := &auth.Auth{UID: "poor"}
	poor.SetRealm("global")
	p.Add(poor)
	p.SetCredits("poor", 1)
	// 置入软冷却：让 healthy 候选为空，强制走兜底路径。
	p.Cooldown("poor", CoolSoft, 2*time.Minute, "test")

	if a := p.PickExcludingForRealm(nil, "kimi-k3", "global"); a != nil {
		t.Fatalf("兜底路径应受保底约束：触底号 credits=1 < floor=150 不得被捞出，got %v", a)
	}
}

// TestCreditFloorFallbackAllowsFreeModel 兜底路径对**免费**模型照常放行：保底只拦
// 收费，不得让触底号连免费模型也接不到（那等于变相禁用）。
func TestCreditFloorFallbackAllowsFreeModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(150)
	p.SetModelRateOf(rates(map[string]map[string]string{"global": {"hy3": "0.00"}}))

	poor := &auth.Auth{UID: "poor"}
	poor.SetRealm("global")
	p.Add(poor)
	p.SetCredits("poor", 1)
	p.Cooldown("poor", CoolSoft, 2*time.Minute, "test")

	if a := p.PickExcludingForRealm(nil, "hy3", "global"); a == nil {
		t.Fatal("兜底路径对免费模型应放行触底号，got nil")
	}
}

// TestCreditFloorStickyBlockedByCatalogRate 粘性路径同判据：粘性号触底 + 目录判
// 收费 → PickByUIDForModel 返回 nil，handler 解绑换号（避免钉在打穿的号上）。
func TestCreditFloorStickyBlockedByCatalogRate(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetModelRateOf(rates(map[string]map[string]string{
		"global": {"kimi-k3-1": "1.62"},
	}))

	g := &auth.Auth{UID: "poor"}
	g.SetRealm("global")
	p.Add(g)
	p.SetCredits("poor", 30)

	if a := p.PickByUIDForModel("poor", "kimi-k3-1"); a != nil {
		t.Fatalf("粘性号触底且目录判收费应返回 nil（解绑换号），got %v", a)
	}
	// 回血后恢复：越过 floor 即放行。
	p.SetCredits("poor", 500)
	if a := p.PickByUIDForModel("poor", "kimi-k3-1"); a == nil {
		t.Fatal("回血越过 floor 后粘性号应恢复可选，got nil")
	}
}
