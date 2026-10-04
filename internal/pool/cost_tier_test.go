// 成本分层的域口径（realm-aware cost tiering）回归。
//
// 背景（统一调度改造 d2c52be 之后）：对外模型目录合并成裸名、一个模型名同时对应
// CN 与 global 两个池子，同一个 pick 里两域账号一起参选。而倍率是**分域**的
// （upstream.modelRates 按 "cn"/"global" 分桶，见 upstream.storeModelRates）——
// 同一个模型名在国内收费、在国际免费是常态。
//
// 因此成本分层必须按**账号自身所属域**查目录倍率（不是按域集合键，也不是用某一
// 侧的价统一替两域），否则会出现两种错配：
//   - 拿域集合 Key()（如 "cn+global"）查：表里没这个键 → 恒"未知" → 兜底失效；
//   - 用 CN 的价给 global 账号计价：国际免费号被当收费号过滤掉，白白不用。
// 本文件锁定这两种错配都不会再出现。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestCostTierUsesAccountRealm 同一模型名在两域倍率不同时，成本层按各账号**自身域**
// 判定：CN 侧收费 → tier 2，global 侧免费 → tier 0，同一次改动、同一个模型名。
func TestCostTierUsesAccountRealm(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetModelRateOf(rates(map[string]map[string]string{
		"cn":     {"dual-model": "2.00"}, // 国内：收费
		"global": {"dual-model": "0.00"}, // 国际：限免
	}))

	cn := &auth.Auth{UID: "cn-a"}
	cn.SetRealm("cn")
	gl := &auth.Auth{UID: "g-a"}
	gl.SetRealm("global")
	p.Add(cn)
	p.Add(gl)

	p.mu.Lock()
	now := time.Now()
	cnTier, cnCost := p.costTierOf(p.byUID["cn-a"], "dual-model", now)
	glTier, glCost := p.costTierOf(p.byUID["g-a"], "dual-model", now)
	p.mu.Unlock()

	if cnTier != 2 {
		t.Errorf("CN 号对 CN 收费倍率 x2.00 应为 tier 2，got %d", cnTier)
	}
	if glTier != 0 {
		t.Errorf("global 号对 global 免费倍率 x0.00 应为 tier 0（不得被 CN 的价带偏），got %d", glTier)
	}
	// 目录判出的层不带单价：倍率是倍数、cost1k 是每千 token 积分数，量纲不同不能混。
	if cnCost != 0 || glCost != 0 {
		t.Errorf("目录判层不得填 cost1k（量纲不同），got cn=%v gl=%v", cnCost, glCost)
	}
}

// TestCostTierCatalogFreeBeatsCatalogPaid 全池 pick 的可见行为：同一模型名国内收费、
// 国际免费时，tier 0（国际免费号）硬过滤掉 tier 2（国内收费号）——免费号必须被选中，
// 不能因为"两域一起选号"就按某一侧的价把免费号一起算成收费。
func TestCostTierCatalogFreeBeatsCatalogPaid(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.SetModelRateOf(rates(map[string]map[string]string{
		"cn":     {"dual-model": "2.00"},
		"global": {"dual-model": "0.00"},
	}))

	cn := &auth.Auth{UID: "cn-a"}
	cn.SetRealm("cn")
	gl := &auth.Auth{UID: "g-a"}
	gl.SetRealm("global")
	p.Add(cn)
	p.Add(gl)
	// 给 CN 号更高的积分权重（否则可能本就抽不到它，测不出分层）：权重高仍不该中选
	// ——成本分层是**硬过滤**，压过积分因子。
	p.SetCredits("cn-a", 100000)
	p.SetCredits("g-a", 10)

	a := p.PickExcludingForRealm(nil, "dual-model", "")
	if a == nil {
		t.Fatal("全池 pick 应返回账号，got nil")
	}
	if a.UID != "g-a" {
		t.Fatalf("国际免费号（tier 0）应压过国内收费号（tier 2），got %s", a.UID)
	}
}

// TestCostTierCatalogUnknownStaysTier1 目录未覆盖的模型保持 tier 1（未知），
// 不被目录兜底误判成收费：未知号仍可参选（"无观测"不等于"收费"）。
func TestCostTierCatalogUnknownStaysTier1(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetModelRateOf(rates(map[string]map[string]string{
		"cn": {"other-model": "1.00"},
	}))

	cn := &auth.Auth{UID: "cn-a"}
	cn.SetRealm("cn")
	p.Add(cn)

	p.mu.Lock()
	tier, cost := p.costTierOf(p.byUID["cn-a"], "unlisted-model", time.Now())
	p.mu.Unlock()

	if tier != 1 || cost != 0 {
		t.Fatalf("目录未覆盖的模型应为 tier 1（未知），got tier=%d cost=%v", tier, cost)
	}
}

// TestCostTierLocalLedgerBeatsCatalog 本地实测恒优先于目录：某号实测该模型收费
// （cost>0），即使目录标免费，也仍按实测判 tier 2 并带上真实单价——实测是真实扣费
// 证据，目录只是牌价。
func TestCostTierLocalLedgerBeatsCatalog(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetModelRateOf(rates(map[string]map[string]string{
		"global": {"hy4-preview-f": "0.00"}, // 目录标免费
	}))

	gl := &auth.Auth{UID: "g-a"}
	gl.SetRealm("global")
	p.Add(gl)
	p.NoteModelCost("g-a", "hy4-preview-f", 0.29, 1000) // 实测收费

	p.mu.Lock()
	tier, cost := p.costTierOf(p.byUID["g-a"], "hy4-preview-f", time.Now())
	p.mu.Unlock()

	if tier != 2 {
		t.Fatalf("本地实测收费应压过目录标免费，tier want 2 got %d", tier)
	}
	if cost != 0.29 {
		t.Fatalf("本地实测应带出真实单价 0.29，got %v", cost)
	}
}

// TestCostTierNoRateTableKeepsLegacy 未注入倍率表（nil）时退化为纯本地台账口径：
// 无观测即 tier 1——老部署零回归。
func TestCostTierNoRateTableKeepsLegacy(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	// 不调 SetModelRateOf。

	p.Add(&auth.Auth{UID: "a"})

	p.mu.Lock()
	tier, cost := p.costTierOf(p.byUID["a"], "any-model", time.Now())
	p.mu.Unlock()

	if tier != 1 || cost != 0 {
		t.Fatalf("未注入倍率表时应为 tier 1（无观测），got tier=%d cost=%v", tier, cost)
	}
}
