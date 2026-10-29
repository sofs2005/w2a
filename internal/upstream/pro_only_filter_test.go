package upstream

import "testing"

// 实测载荷（2026-10-29 两域日志原样摘录）。两域 tier / requiredUserType 都是
// "standard"，差别只在 trackKey 与 badge 文案——这正是不能按 tier 判的原因。
const (
	proOnlyTiersGlobal = `[
		{"badge":{"color":"#009273","label":"Pro Only","labelEn":"Pro Only"},
		 "enabled":true,"id":"gpt-pro-only",
		 "modelIds":["gpt-6-astra","gpt-6-sol","grok-4.7","gemini-3.8-flash"],
		 "priority":200,"requiredUserType":"standard","tier":"standard",
		 "trackKey":"gpt_pro_only"}
	]`
	// CN：tier 同为 standard，但语义是「资源紧张时优先调度」，免费号照样可用。
	priorityTiersCN = `[
		{"badge":{"label":"订阅优先"},
		 "enabled":true,"id":"tier-standard-2026q3",
		 "modelIds":["glm-5.3","glm-5.3-flash","kimi-k2.8-preview"],
		 "priority":20,"requiredUserType":"standard","tier":"standard",
		 "trackKey":"model_tier_standard"}
	]`
)

// TestFilterProOnlyModels_Global 全局域：命中 Pro Only 的模型从 names / infos /
// efforts / defaults 四处一并剔除，其余原样保留。
func TestFilterProOnlyModels_Global(t *testing.T) {
	names := []string{"glm-5.2", "gpt-6-sol", "kimi-k3", "grok-4.7"}
	infos := []ModelInfo{{ID: "glm-5.2"}, {ID: "gpt-6-sol"}, {ID: "kimi-k3"}, {ID: "grok-4.7"}}
	efforts := map[string][]string{"glm-5.2": {"high"}, "gpt-6-sol": {"low"}, "grok-4.7": {"high"}}
	defaults := map[string]string{"glm-5.2": "high", "gpt-6-sol": "low", "grok-4.7": "high"}

	gotNames, gotInfos, gotEfforts, gotDefaults := filterProOnlyModels(
		parseV3ModelTiers([]byte(`{"data":{"modelTiers":` + proOnlyTiersGlobal + `}}`)),
		names, infos, efforts, defaults)

	if len(gotNames) != 2 || gotNames[0] != "glm-5.2" || gotNames[1] != "kimi-k3" {
		t.Fatalf("names=%v want [glm-5.2 kimi-k3]", gotNames)
	}
	if len(gotInfos) != 2 {
		t.Fatalf("infos=%+v want 2 条", gotInfos)
	}
	for _, mi := range gotInfos {
		if mi.ID == "gpt-6-sol" || mi.ID == "grok-4.7" {
			t.Errorf("Pro Only 模型 %q 仍在 infos 里", mi.ID)
		}
	}
	// effort 桶必须与模型列表同步，否则 prepareBody 会给已删模型挂档位。
	for _, id := range []string{"gpt-6-sol", "grok-4.7"} {
		if _, ok := gotEfforts[id]; ok {
			t.Errorf("efforts 仍含已剔除模型 %q", id)
		}
		if _, ok := gotDefaults[id]; ok {
			t.Errorf("defaults 仍含已剔除模型 %q", id)
		}
	}
	if len(gotEfforts["glm-5.2"]) == 0 {
		t.Error("保留模型的 effort 桶被误删")
	}
}

// TestFilterProOnlyModels_CNTierNotFiltered CN 的 tier-standard 是调度优先级而非
// 访问限制（tier/requiredUserType 同为 "standard"），不能被当成 Pro Only 剔除。
// 这是按 trackKey 而非 tier 判定的核心理由。
func TestFilterProOnlyModels_CNTierNotFiltered(t *testing.T) {
	names := []string{"glm-5.3", "glm-5.3-flash", "kimi-k2.8-preview", "hy3"}
	infos := []ModelInfo{{ID: "glm-5.3"}, {ID: "glm-5.3-flash"}, {ID: "kimi-k2.8-preview"}, {ID: "hy3"}}

	gotNames, gotInfos, _, _ := filterProOnlyModels(
		parseV3ModelTiers([]byte(`{"data":{"modelTiers":` + priorityTiersCN + `}}`)),
		names, infos, nil, nil)

	if len(gotNames) != 4 {
		t.Fatalf("CN 优先调度模型被误删：names=%v want 全部 4 条", gotNames)
	}
	if len(gotInfos) != 4 {
		t.Fatalf("CN 优先调度模型被误删：infos=%d want 4", len(gotInfos))
	}
}

// TestFilterProOnlyModels_DisabledTierIgnored enabled=false 的分层不生效。
func TestFilterProOnlyModels_DisabledTierIgnored(t *testing.T) {
	tiers := []v3ModelTier{{Enabled: false, TrackKey: "gpt_pro_only", ModelIDs: []string{"gpt-6-sol"}}}
	names := []string{"gpt-6-sol"}
	got, _, _, _ := filterProOnlyModels(tiers, names, nil, nil, nil)
	if len(got) != 1 {
		t.Fatalf("enabled=false 的 tier 不该生效：names=%v", got)
	}
}

// TestFilterProOnlyModels_EmptyTiers 企业端点族没有 modelTiers 段 → 原样返回，
// 不能因解析不出而清空目录。
func TestFilterProOnlyModels_EmptyTiers(t *testing.T) {
	names := []string{"a", "b"}
	got, _, _, _ := filterProOnlyModels(nil, names, nil, nil, nil)
	if len(got) != 2 {
		t.Fatalf("tiers 为空时不该过滤：names=%v", got)
	}
	// 非法 JSON 同样降级为空 tiers。
	if tiers := parseV3ModelTiers([]byte("not-json")); tiers != nil {
		t.Errorf("坏 JSON 应返回 nil，得 %v", tiers)
	}
}

// TestMergeTiersUnion 三路探测的 tiers 取并集：某一路缺该段时不影响最终过滤。
func TestMergeTiersUnion(t *testing.T) {
	a := []v3ModelTier{{Enabled: true, TrackKey: "gpt_pro_only", ModelIDs: []string{"m1"}}}
	b := []v3ModelTier{{Enabled: true, TrackKey: "gpt_pro_only", ModelIDs: []string{"m1", "m2"}}}

	got := mergeTiers(a, b)
	if len(got) != 2 {
		t.Fatalf("mergeTiers=%v want 2 条（同 id 去重）", got)
	}
	// 并集后 m1/m2 都应被剔除。
	names := []string{"m1", "m2", "ok"}
	kept, _, _, _ := filterProOnlyModels(got, names, nil, nil, nil)
	if len(kept) != 1 || kept[0] != "ok" {
		t.Fatalf("kept=%v want [ok]", kept)
	}
}
