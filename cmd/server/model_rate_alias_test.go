// 模型别名 × 上游目录倍率：键空间对齐测试（cmd/server/model_rate_alias_test.go）。
//
// 缺陷回顾：internal/pool/credits.go 的 costTierOf / pick.go 的 floorBlockedForModel 在
// 「本地无实测台账」时会退到上游目录倍率兜底，键是**对外名**（如 deepseek-v4.1-flash）；
// 而倍率表的存储键是上游目录里的**物理模型 ID**（internal/upstream/client.go 的
// rates[mi.ID] = rate）。真实配置里 deepseek-v4.1-flash 的 global 出站名是
// deepseek-v4.1-flash-sg，于是 global 号查裸名得到 "0"（裸名牌价为 0）→ 判 tier 0「免费」，
// 或查不到 → tier 1「未知」；两者都排在「已实测收费」（tier 2）之前，成本分层硬过滤
// 取最优层 ⇒ 国内号被整体滤掉、流量压到国际号。同一查表还被积分保底判收费复用，
// 误判「未知」= 放行 ⇒ 保底失效、可能打穿余额。
//
// 修复点在**注入侧**（cmd/server/wiring.go 的 modelRateWithAliases）：把对外名按域解析成
// 物理名再查倍率。pool 侧语义不动（它拿到什么 model 就查什么）。
package main

import (
	"reflect"
	"testing"

	"workbuddy2api/internal/aliases"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// fakeRateTable 构造按 (realm, model) → 倍率原文的假查表函数（模拟上游目录倍率表：
// 键是物理模型 ID，未下发的模型返回 ""）。
func fakeRateTable(m map[string]map[string]string) func(realm, model string) string {
	return func(realm, model string) string { return m[realm][model] }
}

// TestModelRateWithAliasesResolvesPhysicalName 用例 1（核心，修复前必红）：
// 别名命中时按**该域物理名**查倍率，而不是用对外名裸查。
//
// 场景就是线上真实配置：global 域裸名 deepseek-v4.1-flash 的牌价是 "0"（实际计费走
// 带 -sg 后缀的物理模型，牌价 "2.00"）。包装后必须拿到 "2.00"——否则 global 号被判
// 「免费」，成本分层（硬过滤取最优层）会把实测收费的国内号整体滤掉。
func TestModelRateWithAliasesResolvesPhysicalName(t *testing.T) {
	store := aliasStore(t, `{"aliases":[{"name":"ds","cn":"ds","global":"ds-sg"}]}`)
	rateOf := fakeRateTable(map[string]map[string]string{
		"cn":     {"ds": "1.50"},
		"global": {"ds": "0", "ds-sg": "2.00"}, // 裸名 tag 0（实际不走），物理名 tag 2.00
	})
	wrapped := modelRateWithAliases(store, rateOf)

	if got := wrapped("global", "ds"); got != "2.00" {
		t.Fatalf("wrapped(\"global\", \"ds\")=%q want %q（别名应解析到物理名 ds-sg；"+
			"得 \"0\" = 判免费、得 \"\" = 判未知，两者都会让国内收费号被成本分层滤掉）",
			got, "2.00")
	}
}

// TestModelRateWithAliasesCNUnaffected 用例 2：cn 域同名的别名（cn == 对外名）不受影响，
// 仍按 cn 域的值查——包装只做「对外名 → 该域物理名」的键替换，不改域的选取。
func TestModelRateWithAliasesCNUnaffected(t *testing.T) {
	store := aliasStore(t, `{"aliases":[{"name":"ds","cn":"ds","global":"ds-sg"}]}`)
	rateOf := fakeRateTable(map[string]map[string]string{
		"cn":     {"ds": "1.50"},
		"global": {"ds": "0", "ds-sg": "2.00"},
	})
	wrapped := modelRateWithAliases(store, rateOf)

	if got := wrapped("cn", "ds"); got != "1.50" {
		t.Fatalf("wrapped(\"cn\", \"ds\")=%q want %q（cn 域名与对外名相同，应原样命中）", got, "1.50")
	}
}

// TestModelRateWithAliasesPassthroughWhenNoAlias 用例 3：未命中别名时逐字透传，
// 与引入包装之前完全一致（无别名部署 / 未配别名的模型行为零变化）。
func TestModelRateWithAliasesPassthroughWhenNoAlias(t *testing.T) {
	store := aliasStore(t, `{"aliases":[{"name":"ds","cn":"ds","global":"ds-sg"}]}`)
	rateOf := fakeRateTable(map[string]map[string]string{
		"global": {"some-other-model": "0.79"},
	})
	wrapped := modelRateWithAliases(store, rateOf)

	if got, want := wrapped("global", "some-other-model"), rateOf("global", "some-other-model"); got != want {
		t.Fatalf("未命中别名应原样透传：wrapped=%q rateOf=%q", got, want)
	}
	if got := wrapped("global", "some-other-model"); got != "0.79" {
		t.Fatalf("wrapped(\"global\", \"some-other-model\")=%q want %q", got, "0.79")
	}
}

// TestModelRateWithAliasesSingleDomainAlias 用例 4：单域别名（该域出站名为空）。
//
// aliases.Table.Resolve 对「命中但该域名空」返回 ("", false)，语义是"该域没有这个模型"。
// 包装**不能**把空串当成要查的模型名吞成 ""（那等于判「未知」tier 1，比保守更松）：
// 按既有 Resolve 语义，这里应回落成原样查（restrict 发生在选号侧 RealmSet，
// 该域账号本就不会被选中）。
func TestModelRateWithAliasesSingleDomainAlias(t *testing.T) {
	store := aliasStore(t, `{"aliases":[{"name":"only-cn","cn":"only-cn","global":""}]}`)
	rateOf := fakeRateTable(map[string]map[string]string{
		"cn": {"only-cn": "0.50"},
		// global 域即便被问到（防御性路径），给出的值也用于区分"透传"与"吞成空串"。
		"global": {"only-cn": "1.11"},
	})
	wrapped := modelRateWithAliases(store, rateOf)

	got := wrapped("global", "only-cn") // 不应 panic
	if got != rateOf("global", "only-cn") {
		t.Fatalf("单域别名（global 为空）应原样透传：wrapped=%q rateOf=%q", got, rateOf("global", "only-cn"))
	}
	if got != "1.11" {
		t.Fatalf("wrapped(\"global\", \"only-cn\")=%q want %q（不得吞成空串）", got, "1.11")
	}
	if cn := wrapped("cn", "only-cn"); cn != "0.50" {
		t.Fatalf("wrapped(\"cn\", \"only-cn\")=%q want %q（非空的域名应正常解析）", cn, "0.50")
	}
}

// TestModelRateWithAliasesNilSafety 用例 5：store / rateOf 为 nil 时安全降级，不 panic。
// （rateOf 为 nil 时无从查倍率 → 空串 = 「未知」，与 pool 侧 modelRateOf==nil 的退化同向；
// store 为 nil 时无别名可依 → 原样转发，与空表语义一致。）
func TestModelRateWithAliasesNilSafety(t *testing.T) {
	rateOf := fakeRateTable(map[string]map[string]string{"global": {"ds": "0"}})

	if got := modelRateWithAliases(nil, rateOf)("global", "ds"); got != "0" {
		t.Fatalf("store=nil 应原样转发：got %q want %q", got, "0")
	}
	if got := modelRateWithAliases(aliases.NewStore(), rateOf)("global", "ds"); got != "0" {
		t.Fatalf("空表应原样转发：got %q want %q", got, "0")
	}
	if got := modelRateWithAliases(nil, nil)("global", "ds"); got != "" {
		t.Fatalf("rateOf=nil 应退化为空串（未知）：got %q", got)
	}
	if got := modelRateWithAliases(aliases.NewStore(), nil)("global", "ds"); got != "" {
		t.Fatalf("rateOf=nil 应退化为空串（未知）：got %q", got)
	}
}

// TestCostTierViaAliasRateIntegration 集成级用例：锁住"缺陷真的会被修掉"。
//
// 断言不看包装函数自身，而看 pool 的**可见行为**：成本分层（bestTierLocked）只保留
// 域内最优层的可用账号（AvailableUIDsForModelRealms ⇒ 会话粘性首绑候选集）。
//
//   - 国内号 cn1：该模型有本地实测台账（cost_per_1k>0）→ tier 2（收费）。
//   - 国际号 g1：**无任何本地观测**（模拟新账号首调 / 观测超 modelCostTTL 过期），
//     只能靠目录倍率判级。
//
// 修复前：g1 查对外名得 "0" → tier 0「免费」→ 全局最优层 0 → 候选只剩 {g1}，
// 国内收费号被整体滤掉（正是线上"流量压到国际号"的形态）。
// 修复后：别名把 g1 的查表键解析成物理名 ds-sg 得 "2.00" → tier 2，与 cn1 同层
// → 两个号都在候选集里（再由权重/UID 决定谁上）。
func TestCostTierViaAliasRateIntegration(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := pool.New("")
	p.SetCostExploreInterval(0) // 关掉 tier 探索窗口，避免时间因素干扰层判定

	cn := &auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"}
	cn.SetRealm("cn")
	gl := &auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"}
	gl.SetRealm("global")
	p.Add(cn)
	p.Add(gl)

	// 两个号余额充足（排除积分保底与健康度干扰；本用例只看成本层）。
	p.SetCredits("cn1", 1_000_000)
	p.SetCredits("g1", 1_000_000)

	// cn1 有近期本地实测：2.0 分 / 千 token（>0 ⇒ tier 2 收费）。
	p.NoteModelCost("cn1", "deepseek-v4.1-flash", 2.0, 1000)

	store := aliasStore(t, `{"aliases":[{"name":"deepseek-v4.1-flash",`+
		`"cn":"deepseek-v4.1-flash","global":"deepseek-v4.1-flash-sg"}]}`)
	rateOf := fakeRateTable(map[string]map[string]string{
		"cn":     {"deepseek-v4.1-flash": "1.00"},
		"global": {"deepseek-v4.1-flash": "0", "deepseek-v4.1-flash-sg": "2.00"},
	})
	p.SetModelRateOf(modelRateWithAliases(store, rateOf))

	// 域集合 nil = 全池（与"裸名跨域调度"的实际调用形态一致）。
	got := p.AvailableUIDsForModelRealms("deepseek-v4.1-flash", nil)
	want := []string{"cn1", "g1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AvailableUIDsForModelRealms=%v want %v：国际号无本地实测时应按别名解析出的"+
			"物理名查倍率（2.00 ⇒ tier 2 收费），与国内实测收费号同层；"+
			"只返回 {g1} 说明它被判成了 tier 0「免费」而把国内号整体滤掉（本缺陷）", got, want)
	}

	// 反向断言：不带别名解析（修复前的注入形态）必须复现缺陷——候选只剩国际号。
	// 这条让本用例成为"缺陷存在性"的活证据，而不是对实现的同义反复。
	p.SetModelRateOf(rateOf)
	if bug := p.AvailableUIDsForModelRealms("deepseek-v4.1-flash", nil); !reflect.DeepEqual(bug, []string{"g1"}) {
		t.Fatalf("修复前形态（裸名查倍率）应只剩国际号 %v，实得 %v：本缺陷的前提已变，请复核用例", []string{"g1"}, bug)
	}
}
