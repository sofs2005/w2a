// 模型积分倍率快照（modelRates）测试：消费方是 pool 的积分保底——
// 本地实测台账无观测时，用它判「这个模型收不收费」。
package upstream

import (
	"testing"
	"time"
)

// TestModelRateCacheEffectiveAndNormalized 倍率快照的写入/规范化/整体替换语义：
// 生效价优先于牌价、原文形态归一为数值、刷新时整桶替换（旧条目不得残留）。
func TestModelRateCacheEffectiveAndNormalized(t *testing.T) {
	c := New()
	factor := 0.5
	c.storeModelRates("cn", []ModelInfo{
		{ID: "base", Credits: "x0.50 credits"},
		{ID: "promo", Credits: "x0.80", PromoFactor: &factor, PromoCredits: "0.50x"},
		{ID: "free", Credits: "x0.29", PromoFactor: &factor, PromoCredits: "0x"},
		{ID: "no-rate", Credits: ""},
	})
	if got := c.ModelRate("cn", "base"); got != "0.5" {
		t.Fatalf("base rate=%q want 0.5", got)
	}
	// 生效价优先：牌价 x0.80，但限时五折生效 → 0.5。
	if got := c.ModelRate("cn", "promo"); got != "0.5" {
		t.Fatalf("promo rate=%q want 0.5（生效价应优先于牌价）", got)
	}
	// 限时免费：折扣价 0x → "0"，积分保底据此放行（不得按牌价 x0.29 误拦）。
	if got := c.ModelRate("cn", "free"); got != "0" {
		t.Fatalf("free rate=%q want 0（限时免费生效价）", got)
	}
	// 无可解析倍率的条目不入桶。
	if got := c.ModelRate("cn", "no-rate"); got != "" {
		t.Fatalf("no-rate=%q want empty", got)
	}
	if got := normalizeModelRate("x0.05 credits"); got != "0.05" {
		t.Fatalf("normalizeModelRate=%q want 0.05", got)
	}

	// 整体替换：刷新后旧条目（含已失效的优惠）不得残留，否则保底会拿旧价判收费。
	c.storeModelRates("cn", []ModelInfo{{ID: "base", Credits: "x0.79"}})
	if got := c.ModelRate("cn", "base"); got != "0.79" {
		t.Fatalf("refreshed base rate=%q want 0.79", got)
	}
	if got := c.ModelRate("cn", "promo"); got != "" {
		t.Fatalf("stale promo rate=%q want empty after full refresh", got)
	}
}

// TestModelRateRealmIsolation 倍率按域分桶：同名模型在 CN / global 的倍率互不串味
// （积分保底按账号自身所属域查表，串味会导致另一域误拦/漏拦）。
func TestModelRateRealmIsolation(t *testing.T) {
	c := New()
	c.storeModelRates("cn", []ModelInfo{{ID: "dual", Credits: "x2.00"}})
	c.storeModelRates("global", []ModelInfo{{ID: "dual", Credits: "x0.00"}})
	if got := c.ModelRate("cn", "dual"); got != "2" {
		t.Errorf("cn dual=%q want 2", got)
	}
	if got := c.ModelRate("global", "dual"); got != "0" {
		t.Errorf("global dual=%q want 0", got)
	}
	// 空 realm 归一为 cn（与 efforts 桶同口径，老调用零漂移）。
	if got := c.ModelRate("", "dual"); got != "2" {
		t.Errorf("empty realm dual=%q want 2 (归一 cn)", got)
	}
	if got := c.ModelRate("cn", "missing"); got != "" {
		t.Errorf("missing=%q want empty", got)
	}
	if got := c.ModelRate("cn", ""); got != "" {
		t.Errorf("empty model=%q want empty", got)
	}
}

// TestPromoActiveWindow 优惠生效窗口：enabled / daily 时段（含跨午夜）/ validFrom。
func TestPromoActiveWindow(t *testing.T) {
	enabled := func() *v3ModelPromotion {
		return &v3ModelPromotion{Enabled: true}
	}
	at := func(h, m int) time.Time {
		return time.Date(2026, 9, 23, h, m, 0, 0, promoZone)
	}

	// 无 schedule = 全天生效；enabled=false 恒不生效。
	if !promoActive(enabled(), at(12, 0)) {
		t.Error("无 schedule 应全天生效")
	}
	if promoActive(&v3ModelPromotion{Enabled: false}, at(12, 0)) {
		t.Error("enabled=false 不应生效")
	}

	// 跨午夜窗口 23:00→7:50（glm-5.2 夜间折扣实测形态）。
	night := enabled()
	night.Schedule = &v3PromoSchedule{
		Daily:    []v3PromoWindow{{Start: "23:00", End: "7:50"}},
		Timezone: "Asia/Shanghai",
	}
	if !promoActive(night, at(23, 30)) {
		t.Error("23:30 应落在跨午夜窗口内")
	}
	if !promoActive(night, at(3, 0)) {
		t.Error("03:00 应落在跨午夜窗口内（跨零点侧）")
	}
	if promoActive(night, at(12, 0)) {
		t.Error("12:00 不应落在夜间窗口内")
	}

	// validFrom 未到 → 不生效（即使落在 daily 窗口内）。
	future := enabled()
	future.Schedule = &v3PromoSchedule{ValidFrom: "2027-01-01T00:00:00+08:00"}
	if promoActive(future, at(12, 0)) {
		t.Error("validFrom 未到不应生效")
	}
}

// TestApplyModelPromotionsPriority 同模型多条命中取 priority 最高
// （实测 glm-5.2 白天 badge-only 与夜间五折靠 priority+daily 双轨切换）；
// 无 discount 的条目不改价（本仓库不透出展示文案，故直接跳过）。
func TestApplyModelPromotionsPriority(t *testing.T) {
	f50, f0 := 0.5, 0.0
	out := map[string]ModelInfo{
		"glm-5.2": {ID: "glm-5.2", Credits: "x0.80"},
		"other":   {ID: "other", Credits: "x1.00"},
	}
	promos := []v3ModelPromotion{
		{Enabled: true, Priority: 50, ModelIDs: []string{"glm-5.2"}}, // 无 discount：应被跳过
		{Enabled: true, Priority: 100, ModelIDs: []string{"glm-5.2"},
			Discount: &v3PromoDiscount{DiscountedCredits: "0.40x", Factor: f50}},
		{Enabled: true, Priority: 100, ModelIDs: []string{"absent"},
			Discount: &v3PromoDiscount{DiscountedCredits: "0x", Factor: f0}},
	}
	applyModelPromotions(out, promos)

	mi := out["glm-5.2"]
	if mi.PromoFactor == nil || *mi.PromoFactor != 0.5 || mi.PromoCredits != "0.40x" {
		t.Fatalf("glm-5.2 promo=%v/%q want 0.5/0.40x", mi.PromoFactor, mi.PromoCredits)
	}
	if out["other"].PromoFactor != nil {
		t.Error("未命中的模型不应挂优惠")
	}
	if _, ok := out["absent"]; ok {
		t.Error("目录外模型不应被插入")
	}
}
