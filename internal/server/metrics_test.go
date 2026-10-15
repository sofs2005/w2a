package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// resetMetricsForTest 隔离用例间的全局聚合状态。
func resetMetricsForTest(t *testing.T) {
	t.Helper()
	ResetMetrics()
	t.Cleanup(ResetMetrics)
}

// TestMetricsAggregatesByModel 同一模型的多次请求累加，派生字段按口径折算。
func TestMetricsAggregatesByModel(t *testing.T) {
	resetMetricsForTest(t)

	// 两次成功 + 一次失败，同一模型。
	mk := func(status int, ttfbMS, toks, prompt, hit, miss, wr int, credit float64, mode string) *chatStat {
		return &chatStat{
			model: "global:deepseek-v4.1-flash", mode: mode, status: status,
			ttfb: time.Duration(ttfbMS) * time.Millisecond,
			toks: toks, hasUsage: true, prompt: prompt,
			cacheHit: hit, cacheMiss: miss, cacheWr: wr,
			credit: credit, hasCredit: true,
		}
	}
	recordChatMetric(mk(200, 1000, 100, 50, 800, 200, 0, 0.02, "stream"), 5*time.Second)
	recordChatMetric(mk(200, 2000, 200, 60, 900, 100, 0, 0.04, "stream"), 7*time.Second)
	recordChatMetric(mk(429, 0, -1, 0, 0, 0, 0, 0, "sync"), 1*time.Second)

	snap := MetricsSnapshotOf()
	if len(snap.Models) != 1 {
		t.Fatalf("models=%d want 1", len(snap.Models))
	}
	m := snap.Models[0]
	if m.Requests != 3 || m.Success != 2 || m.Failed != 1 {
		t.Errorf("req/succ/fail = %d/%d/%d want 3/2/1", m.Requests, m.Success, m.Failed)
	}
	if m.Streaming != 2 {
		t.Errorf("streaming=%d want 2", m.Streaming)
	}
	// 端到端均值 = (5000+7000+1000)/3 = 4333.33ms
	if got := m.AvgLatencyMS; got < 4333 || got > 4334 {
		t.Errorf("avg_latency=%.2f want ~4333.33", got)
	}
	// TTFB 只统计有观测的两次：(1000+2000)/2 = 1500ms
	if got := m.AvgTTFBMS; got != 1500 {
		t.Errorf("avg_ttfb=%.2f want 1500", got)
	}
	// token 只累加 hasUsage 的两次
	if m.PromptTokens != 110 || m.CompletionTokens != 300 {
		t.Errorf("prompt/comp = %d/%d want 110/300", m.PromptTokens, m.CompletionTokens)
	}
	// 命中率 = 1700/(1700+300) = 0.85
	if got := m.CacheHitRate; got < 0.8499 || got > 0.8501 {
		t.Errorf("cache_hit_rate=%.4f want 0.85", got)
	}
	if m.Credit != 0.06 {
		t.Errorf("credit=%.4f want 0.06", m.Credit)
	}
}

// TestMetricsMissingUsageNotCountedAsZero usage 缺失时不得把 0 计进 token/缓存。
func TestMetricsMissingUsageNotCountedAsZero(t *testing.T) {
	resetMetricsForTest(t)

	// hasUsage=false（上游没回 usage）：toks=-1 是哨兵，不该被当成 token 累加。
	recordChatMetric(&chatStat{
		model: "m1", mode: "sync", status: 200, toks: -1,
	}, time.Second)
	// hasUsage=true 且显式全 0：合法观测，参与累加（分母不为零才有意义）。
	recordChatMetric(&chatStat{
		model: "m1", mode: "sync", status: 200, toks: 0, hasUsage: true,
	}, time.Second)

	snap := MetricsSnapshotOf()
	m := snap.Models[0]
	if m.Requests != 2 {
		t.Fatalf("requests=%d want 2", m.Requests)
	}
	if m.CompletionTokens != 0 {
		t.Errorf("completion=%d want 0（-1 哨兵不得计入）", m.CompletionTokens)
	}
	if m.CacheHitRate != 0 {
		t.Errorf("cache_hit_rate=%f want 0（无观测时不做除法）", m.CacheHitRate)
	}
}

// TestMetricsTotalIsSumOfModels total 必须等于各模型累加，不另算一份。
func TestMetricsTotalIsSumOfModels(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "a", mode: "sync", status: 200, toks: 10, hasUsage: true, prompt: 5}, time.Second)
	recordChatMetric(&chatStat{model: "b", mode: "sync", status: 500, toks: 20, hasUsage: true, prompt: 7}, time.Second)

	snap := MetricsSnapshotOf()
	if snap.Total.Requests != 2 || snap.Total.Success != 1 || snap.Total.Failed != 1 {
		t.Errorf("total req/succ/fail = %d/%d/%d want 2/1/1",
			snap.Total.Requests, snap.Total.Success, snap.Total.Failed)
	}
	if snap.Total.PromptTokens != 12 || snap.Total.CompletionTokens != 30 {
		t.Errorf("total tokens = %d/%d want 12/30", snap.Total.PromptTokens, snap.Total.CompletionTokens)
	}
}

// TestMetricsResetClears 重置后归零。
func TestMetricsResetClears(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "a", mode: "sync", status: 200, toks: 10, hasUsage: true}, time.Second)
	if MetricsSnapshotOf().Total.Requests != 1 {
		t.Fatal("前置：应有 1 条")
	}
	ResetMetrics()
	snap := MetricsSnapshotOf()
	if snap.Total.Requests != 0 || len(snap.Models) != 0 {
		t.Errorf("重置后 requests=%d models=%d want 0/0", snap.Total.Requests, len(snap.Models))
	}
}

// TestMetricsEmptyModelFallsBack 空模型名归入 "-"，不丢弃观测。
func TestMetricsEmptyModelFallsBack(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "", mode: "sync", status: 200}, time.Second)
	snap := MetricsSnapshotOf()
	if len(snap.Models) != 1 || snap.Models[0].Model != "-" {
		t.Fatalf("空模型名应归入 \"-\"，得到 %+v", snap.Models)
	}
	if snap.Total.Requests != 1 {
		t.Errorf("观测不得因模型名为空而丢弃")
	}
}

// TestMetricsCapBounded 超容量上限时丢弃新键且不 panic。
func TestMetricsCapBounded(t *testing.T) {
	resetMetricsForTest(t)

	for i := 0; i < metricsCap+50; i++ {
		recordChatMetric(&chatStat{model: string(rune('a'+i%26)) + string(rune('0'+i%10)) + string(rune('A'+i/260)), mode: "sync", status: 200}, time.Second)
	}
	snap := MetricsSnapshotOf()
	if len(snap.Models) > metricsCap {
		t.Errorf("models=%d 超过上限 %d", len(snap.Models), metricsCap)
	}
}

// ─── 按域分账（realm-aware stats）────────────────────────────────────────
//
// 统一调度后一个裸名会在 CN 与 global 账号间调度：两域单价、限免、上下文都不同，
// 混成一个累加器只剩加权平均。以下用例锁定「裸名一行 + 分域两行」的形状。

// TestMetricsSplitsByAccountRealm 同一裸名、两个域各若干请求 → 一行两域明细，
// 父行是两域原始量之和（派生量由合计反算，不是"均值的均值"）。
func TestMetricsSplitsByAccountRealm(t *testing.T) {
	resetMetricsForTest(t)

	mk := func(realm string, toks, prompt int, credit float64) *chatStat {
		return &chatStat{
			model: "dual", realm: realm, mode: "sync", status: 200,
			toks: toks, hasUsage: true, prompt: prompt,
			credit: credit, hasCredit: true,
		}
	}
	// CN：2 次（5s / 7s），global：1 次（1s）。
	recordChatMetric(mk("cn", 100, 50, 0.02), 5*time.Second)
	recordChatMetric(mk("cn", 200, 60, 0.04), 7*time.Second)
	recordChatMetric(mk("global", 10, 5, 0.01), 1*time.Second)

	snap := MetricsSnapshotOf()
	if len(snap.Models) != 1 {
		t.Fatalf("同一裸名的两域应合成一行，models=%+v", snap.Models)
	}
	row := snap.Models[0]
	if row.Model != "dual" || row.Bare != "dual" {
		t.Errorf("父行应为裸名 dual，got model=%q bare=%q", row.Model, row.Bare)
	}
	if row.Requests != 3 || row.Credit < 0.0699 || row.Credit > 0.0701 {
		t.Errorf("父行应为两域之和 req=3 credit≈0.07，got req=%d credit=%v", row.Requests, row.Credit)
	}
	// 父行延迟 = (5000+7000+1000)/3 = 4333.33ms——由原始量合计反算，
	// 不是 (6000+1000)/2 = 3500（"均值的均值"会把 1 次的那域抬到 50% 权重）。
	if row.AvgLatencyMS < 4333 || row.AvgLatencyMS > 4334 {
		t.Errorf("父行 avg_latency=%.2f want ~4333.33（合计反算，非均值再平均）", row.AvgLatencyMS)
	}

	if len(row.Realms) != 2 {
		t.Fatalf("应有 cn/global 两行明细，got %+v", row.Realms)
	}
	if row.Realms[0].Realm != "cn" || row.Realms[1].Realm != "global" {
		t.Fatalf("域序应为 cn → global（刷新不换位），got %q,%q", row.Realms[0].Realm, row.Realms[1].Realm)
	}
	cn, gl := row.Realms[0], row.Realms[1]
	if cn.Requests != 2 || cn.Credit < 0.0599 || cn.Credit > 0.0601 || cn.AvgLatencyMS != 6000 {
		t.Errorf("cn 明细 want req=2 credit≈0.06 lat=6000，got %+v", cn)
	}
	if gl.Requests != 1 || gl.Credit != 0.01 || gl.AvgLatencyMS != 1000 {
		t.Errorf("global 明细 want req=1 credit=0.01 lat=1000，got %+v", gl)
	}
	if cn.TotalTokens != 300+110 || gl.TotalTokens != 10+5 {
		t.Errorf("分域 token 应为各域自身合计，got cn=%d gl=%d", cn.TotalTokens, gl.TotalTokens)
	}
}

// TestMetricsBareKeyCollapsesRealmPrefix 前缀请求与裸名请求归入同一裸名行：
// "cn:x" 与裸名 "x" 指的是同一个底层模型，只有裸名能让它们合成表格的一行。
func TestMetricsBareKeyCollapsesRealmPrefix(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "cn:hy3", realm: "cn", mode: "sync", status: 200}, time.Second)
	recordChatMetric(&chatStat{model: "hy3", realm: "global", mode: "sync", status: 200}, time.Second)

	snap := MetricsSnapshotOf()
	if len(snap.Models) != 1 {
		t.Fatalf("cn:hy3 与裸名 hy3 应为同一行，models=%+v", snap.Models)
	}
	row := snap.Models[0]
	if row.Model != "hy3" || row.Bare != "hy3" {
		t.Errorf("行键应为裸名 hy3，got model=%q bare=%q", row.Model, row.Bare)
	}
	if len(row.Realms) != 2 || row.Realms[0].Realm != "cn" || row.Realms[1].Realm != "global" {
		t.Errorf("两个域各一行，got %+v", row.Realms)
	}
}

// TestMetricsUnroutedRealmIsOwnGroup 未路由（选号失败 503 / 模型名解析不出）记 ""，
// 独立成组排在最后——编造归属（并进 cn 或 global）比空着更糟。
func TestMetricsUnroutedRealmIsOwnGroup(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "m", realm: "cn", mode: "sync", status: 200}, time.Second)
	recordChatMetric(&chatStat{model: "m", realm: "", mode: "sync", status: 503}, time.Second)

	snap := MetricsSnapshotOf()
	row := snap.Models[0]
	if len(row.Realms) != 2 {
		t.Fatalf("应有 cn + 未路由两行，got %+v", row.Realms)
	}
	if row.Realms[0].Realm != "cn" {
		t.Errorf("cn 应排首位，got %+v", row.Realms)
	}
	if row.Realms[1].Realm != "" || row.Realms[1].Failed != 1 {
		t.Errorf("未路由组应为空域且含那次 503，got %+v", row.Realms[1])
	}
	// 空域不得并进 cn：cn 只有那 1 次成功。
	if row.Realms[0].Requests != 1 || row.Realms[0].Failed != 0 {
		t.Errorf("未路由请求不得计入 cn，got %+v", row.Realms[0])
	}
}

// TestMetricsRealmOrderUnknownRealmLast 非 cn/global 的异常域值按字典序补在最后，
// 保证输出确定性（map 遍历无序，不排序会让表格行每次刷新换位）。
func TestMetricsRealmOrderUnknownRealmLast(t *testing.T) {
	perRealm := map[string]*modelMetrics{
		"zeta": {}, "global": {}, "cn": {}, "": {}, "alpha": {},
	}
	got := metricRealmOrder(perRealm)
	want := []string{"cn", "global", "", "alpha", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("order=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order=%v want %v", got, want)
		}
	}
}

// TestMetricsSingleDomainStillTagged 单域条目（生产常态：绝大多数模型只在一域跑）
// 仍带一条**域明细**，只是数组长度为 1。这是刻意的：它确实知道自己在哪一域跑的，
// 标成"未知域"是信息倒退；表格据此显示「国内版」徽章而不是「未标注域」。
func TestMetricsSingleDomainStillTagged(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "solo", realm: "cn", mode: "sync", status: 200}, time.Second)

	snap := MetricsSnapshotOf()
	row := snap.Models[0]
	if row.Bare != "solo" {
		t.Errorf("bare 应始终下发（面板按裸名成组），got %q", row.Bare)
	}
	if len(row.Realms) != 1 || row.Realms[0].Realm != "cn" {
		t.Fatalf("单域条目应带一条 cn 明细，got %+v", row.Realms)
	}
	// 父行与唯一子条目必须等值——前端在单域时可能只渲染子行，两处对不上就是 bug。
	if row.Requests != row.Realms[0].Requests || row.AvgLatencyMS != row.Realms[0].AvgLatencyMS {
		t.Errorf("单域时父行应与子条目等值，got 父=%+v 子=%+v", row, row.Realms[0])
	}
	// 老网关（整段缺席）才该走「未标注域」退化：这里显式确认 JSON 里确实带上了 realms。
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"realms"`) {
		t.Errorf("单域条目也应序列化 realms（含域徽章信息），得到 %s", raw)
	}
}

// ─── 出站模型名（别名是否生效的唯一直接证据）────────────────────────────
//
// 行键（裸名）与流水行都只回显**入站**名，响应体的 model 又是上游回显（上游会把
// 自己的别名规范化掉），三处都看不出网关到底发了什么。以下用例钉住 out_models 的
// 三个关键性质：只在真的改写时出现、落在**域子条目**、普通请求载荷逐字节不变。

// TestMetricsOutModelsOnlyWhenRewritten 只有「出站名 ≠ 裸名」才输出 out_models：
// 非别名请求（出站名 == 裸名）与未路由请求（没有出站名）都不得输出。
func TestMetricsOutModelsOnlyWhenRewritten(t *testing.T) {
	resetMetricsForTest(t)

	// 别名请求：入站 hy4-preview，出站 hy4-preview-f。
	recordChatMetric(&chatStat{
		model: "hy4-preview", outModel: "hy4-preview-f",
		realm: "cn", mode: "sync", status: 200,
	}, time.Second)
	// 非别名请求：出站名就是入站名。
	recordChatMetric(&chatStat{
		model: "glm-5.2", outModel: "glm-5.2",
		realm: "cn", mode: "sync", status: 200,
	}, time.Second)
	// 未路由（选号失败 503）：根本没有出站名。
	recordChatMetric(&chatStat{
		model: "ghost", outModel: "",
		realm: "", mode: "sync", status: 503,
	}, time.Second)

	snap := MetricsSnapshotOf()

	aliased := findModelRow(t, snap, "hy4-preview")
	if len(aliased.Realms) != 1 {
		t.Fatalf("hy4-preview 应只有一条 cn 明细，got %+v", aliased.Realms)
	}
	if got := aliased.Realms[0].OutModels; len(got) != 1 || got[0] != "hy4-preview-f" {
		t.Errorf("别名请求应记下出站名 hy4-preview-f，got %v", got)
	}

	plain := findModelRow(t, snap, "glm-5.2")
	if got := plain.Realms[0].OutModels; got != nil {
		t.Errorf("非别名请求（出站名==裸名）不得输出 out_models，got %v", got)
	}

	ghost := findModelRow(t, snap, "ghost")
	if got := ghost.Realms[0].OutModels; got != nil {
		t.Errorf("未路由请求没有出站名，不得输出 out_models，got %v", got)
	}
}

// TestMetricsOutModelsPerRealm 出站名落在**域子条目**：同一对外名在两域映射到不同
// 物理名时，各自显示各自的——并到父条目上就只剩一个并集，正好丢掉要查的东西。
func TestMetricsOutModelsPerRealm(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{
		model: "kimi-k3", outModel: "kimi-k3-1",
		realm: "cn", mode: "sync", status: 200,
	}, time.Second)
	recordChatMetric(&chatStat{
		model: "kimi-k3", outModel: "kimi-k3",
		realm: "global", mode: "sync", status: 200,
	}, time.Second)

	snap := MetricsSnapshotOf()
	row := findModelRow(t, snap, "kimi-k3")
	if len(row.Realms) != 2 {
		t.Fatalf("应有 cn/global 两行，got %+v", row.Realms)
	}
	// cn 出站 kimi-k3-1（与裸名不同 → 输出）；global 出站 kimi-k3（== 裸名 → 不输出）。
	if got := row.Realms[0].OutModels; len(got) != 1 || got[0] != "kimi-k3-1" {
		t.Errorf("cn 域出站名应为 kimi-k3-1，got %v", got)
	}
	if got := row.Realms[1].OutModels; got != nil {
		t.Errorf("global 域出站名与裸名同名，不得输出，got %v", got)
	}
}

// TestMetricsOutModelsOmittedFromJSON 普通请求的载荷必须与引入本字段**前逐字节
// 一致**（omitempty + 过滤到 nil）：面板据此把「同名」与「网关没上报」区分开，
// 若恒输出空数组，老面板/新面板都会把每条请求读成"别名生效过"。
func TestMetricsOutModelsOmittedFromJSON(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "plain", outModel: "plain", realm: "cn", mode: "sync", status: 200}, time.Second)
	snap := MetricsSnapshotOf()
	raw, err := json.Marshal(snap.Models[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "out_models") {
		t.Errorf("非别名请求不得序列化 out_models（否则面板读不出「未改写」），得到 %s", raw)
	}

	// 对照：别名请求必须序列化出来，且是数组形态。
	recordChatMetric(&chatStat{model: "al", outModel: "al-phys", realm: "cn", mode: "sync", status: 200}, time.Second)
	snap = MetricsSnapshotOf()
	raw, err = json.Marshal(findModelRow(t, snap, "al"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"out_models":["al-phys"]`) {
		t.Errorf("别名请求应序列化 out_models:[\"al-phys\"]，得到 %s", raw)
	}
}
