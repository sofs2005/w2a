package api

import (
	"encoding/json"
	"testing"

	"workbuddy2api-gui/internal/gateway"
	"workbuddy2api-gui/internal/upstream"
)

// TestModelRealmsPassthrough 钉住 `realms` 字段在面板侧的**双向**存活。
//
// 为什么值得单独一条测试：`gateway.Model` 是白名单式结构体，encoding/json 对未声明的
// 字段**静默丢弃**——网关下发的 `realms` 就这么消失过一次，且没有任何报错、没有任何
// 日志，症状只是「模型与倍率」页的域分栏全落到国内版（前端还有按 id 前缀推断的回退，
// 于是连"字段没了"都看不出来）。这条测试让「漏声明字段」变成编译期之后的立即失败。
//
// 覆盖两段真实链路：
//  1. 解码 —— 网关 /v1/models 的 JSON → gateway.Model（Client.Models 的做法）；
//  2. 编码 —— modelView 序列化回 JSON（/api/models 响应，前端据此分栏）。
func TestModelRealmsPassthrough(t *testing.T) {
	// 与网关 modelList 输出同形：裸名 + realms 标注可用域。
	raw := []byte(`{"data":[
		{"id":"glm-5.2","object":"model","created":1,"owned_by":"workbuddy",
		 "context_length":1000000,"realms":["cn","global"]},
		{"id":"hunyuan-only-cn","object":"model","created":1,"owned_by":"workbuddy",
		 "context_length":128000,"realms":["cn"]}
	]}`)

	var decoded struct {
		Data []gateway.Model `json:"data"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解码 /v1/models 失败: %v", err)
	}
	if len(decoded.Data) != 2 {
		t.Fatalf("条目数=%d want 2", len(decoded.Data))
	}
	if got := decoded.Data[0].Realms; len(got) != 2 || got[0] != "cn" || got[1] != "global" {
		t.Fatalf("realms 在解码时被丢弃: %v（gateway.Model 漏声明 json tag？）", got)
	}
	if got := decoded.Data[1].Realms; len(got) != 1 || got[0] != "cn" {
		t.Fatalf("单域条目 realms 错误: %v", got)
	}

	// 老网关不下发 realms → nil，且不得在响应里编造一个空数组
	// （前端靠「字段缺席」判断无法按域区分，空数组会被误读为"哪个域都不支持"）。
	var legacy struct {
		Data []gateway.Model `json:"data"`
	}
	if err := json.Unmarshal([]byte(`{"data":[{"id":"x","object":"model"}]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Data[0].Realms != nil {
		t.Errorf("缺省 realms 应为 nil，得到 %v", legacy.Data[0].Realms)
	}
	blob, err := json.Marshal(modelView{Model: legacy.Data[0]})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if _, present := back["realms"]; present {
		t.Errorf("缺省 realms 不应出现在响应里（前端据此判断老网关）: %s", blob)
	}

	// 重新编码后 realms 必须原样透出，否则前端拿不到。
	blob, err = json.Marshal(modelView{Model: decoded.Data[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	rs, ok := back["realms"].([]any)
	if !ok || len(rs) != 2 {
		t.Fatalf("重新编码后 realms 丢失或形态错误: %s", blob)
	}
	if rs[0] != "cn" || rs[1] != "global" {
		t.Errorf("realms 顺序/内容被改动: %v", rs)
	}
}

// TestModelCreditsPerRealmPassthrough 钉住按域倍率字段（credits_cn / credits_global）
// 在面板侧的**双向**存活，与 TestModelRealmsPassthrough 同一动机：
// gateway.Model 是白名单结构体，未声明的字段静默丢弃；而分域倍率丢了不会报错，
// 症状只是「模型与倍率」页两个 tab 显示同一个价（前端 realmCredits 回退到合并的
// credits）。这条测试让漏声明字段当场失败。
func TestModelCreditsPerRealmPassthrough(t *testing.T) {
	// 与网关 /v1/models 输出同形：合并的 credits 是 CN 优先的展示兼容字段，
	// 分域真值在 credits_cn / credits_global。
	raw := []byte(`{"data":[
		{"id":"glm-5.2","object":"model","credits":"x0.06",
		 "credits_cn":"x0.06","credits_global":"x0.31"},
		{"id":"cn-only","object":"model","credits":"x1.00","credits_cn":"x1.00"}
	]}`)

	var decoded struct {
		Data []gateway.Model `json:"data"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解码 /v1/models 失败: %v", err)
	}
	if got := decoded.Data[0].CreditsCN; got != "x0.06" {
		t.Fatalf("credits_cn 在解码时被丢弃: %q（gateway.Model 漏声明 json tag？）", got)
	}
	if got := decoded.Data[0].CreditsGlobal; got != "x0.31" {
		t.Fatalf("credits_global 在解码时被丢弃: %q", got)
	}
	// 单域条目：另一侧缺席（不是空串），前端据此回退到 credits。
	if got := decoded.Data[1].CreditsGlobal; got != "" {
		t.Errorf("cn-only 不应有 credits_global，got %q", got)
	}

	// 重新编码后两个字段都必须原样透出，否则前端分栏拿不到。
	blob, err := json.Marshal(modelView{Model: decoded.Data[0]})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if back["credits_cn"] != "x0.06" || back["credits_global"] != "x0.31" {
		t.Fatalf("重新编码后分域倍率丢失或错值: %s", blob)
	}
	// 缺席侧不得被序列化出来（omitempty），否则前端会把空串当"该域免费"。
	// 注意用新的 map：json.Unmarshal 往非 nil map 里是**合并**而非清空，
	// 复用上面的 back 会把上一轮的 credits_global 留在里面，断言就假绿了。
	blob, err = json.Marshal(modelView{Model: decoded.Data[1]})
	if err != nil {
		t.Fatal(err)
	}
	back = map[string]any{}
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if _, present := back["credits_global"]; present {
		t.Errorf("缺席侧 credits_global 不应出现: %s", blob)
	}
}

// TestBuildModelViewsPromotionsPerRealm 钉住促销**按域配对**，而非按 id 前缀。
//
// 回归背景：旧实现按 "global:" 前缀判断域，统一调度后目录只剩裸名，于是所有模型
// 都被当成 CN，国际版 tab 恒显示国内活动（这是个真 bug，不只是展示偏好）。
// 本例让同一个裸名的两域活动完全不同，断言各域各取各的、互不串味。
func TestBuildModelViewsPromotionsPerRealm(t *testing.T) {
	cnPromo := upstream.ModelPromotion{
		ID: "cn-night", ModelIDs: []string{"glm-5.2"}, Enabled: true, Kind: "off_peak", Priority: 1,
	}
	cnTop := upstream.ModelPromotion{
		ID: "cn-free", ModelIDs: []string{"glm-5.2"}, Enabled: true, Kind: "limited_free", Priority: 9,
	}
	glPromo := upstream.ModelPromotion{
		ID: "gl-free", ModelIDs: []string{"glm-5.2"}, Enabled: true, Kind: "limited_free", Priority: 3,
	}
	// 只覆盖 CN 独有模型的 global 活动：不得挂到别处。
	glOther := upstream.ModelPromotion{
		ID: "gl-other", ModelIDs: []string{"gpt-5.4"}, Enabled: true,
	}

	models := []gateway.Model{
		{ID: "glm-5.2", Realms: []string{"cn", "global"}}, // 双域
		{ID: "cn-only-x", Realms: []string{"cn"}},         // 仅国内
		{ID: "gpt-5.4", Realms: []string{"global"}},       // 仅国际
		{ID: "legacy-global:old-model"},                   // 老网关：无 realms，靠前缀
	}
	views := buildModelViews(models, map[string][]upstream.ModelPromotion{
		"cn":     {cnPromo, cnTop},
		"global": {glPromo, glOther},
	})
	if len(views) != len(models) {
		t.Fatalf("视图数=%d want %d", len(views), len(models))
	}

	// 双域模型：两域各挂各的，且各自按优先级降序（CN 的 free 排在 night 前）。
	glm := views[0]
	if len(glm.PromotionsCN) != 2 {
		t.Fatalf("glm-5.2 CN 促销数=%d want 2", len(glm.PromotionsCN))
	}
	if glm.PromotionsCN[0].ID != "cn-free" {
		t.Errorf("CN 促销应按优先级降序（free 在前），got %s", glm.PromotionsCN[0].ID)
	}
	if len(glm.PromotionsGlobal) != 1 || glm.PromotionsGlobal[0].ID != "gl-free" {
		t.Fatalf("glm-5.2 global 促销应只含 gl-free（不得混入 CN 活动），got %+v", glm.PromotionsGlobal)
	}

	// 单域条目：不承接另一域的活动。
	if len(views[1].PromotionsGlobal) != 0 {
		t.Errorf("仅国内模型不应有 global 促销，got %+v", views[1].PromotionsGlobal)
	}
	if len(views[1].PromotionsCN) != 0 {
		t.Errorf("cn-only-x 无匹配活动，CN 促销应为空，got %+v", views[1].PromotionsCN)
	}
	if len(views[2].PromotionsCN) != 0 {
		t.Errorf("仅国际模型不应有 CN 促销，got %+v", views[2].PromotionsCN)
	}
	if len(views[2].PromotionsGlobal) != 1 || views[2].PromotionsGlobal[0].ID != "gl-other" {
		t.Errorf("gpt-5.4 应挂 gl-other，got %+v", views[2].PromotionsGlobal)
	}

	// 老网关前缀条目：按前缀推断域，且裸名剥掉前缀后才与促销 modelIds 对齐。
	legacy := views[3]
	if len(legacy.PromotionsGlobal) != 0 {
		t.Errorf("old-model 无匹配活动，应为空，got %+v", legacy.PromotionsGlobal)
	}
	if len(legacy.PromotionsCN) != 0 {
		t.Errorf("老网关 global: 前缀条目不得被当成 CN，got %+v", legacy.PromotionsCN)
	}
}

// TestModelBareAndRealms 单测两个域解析助手：前缀剥离与优先级顺序。
//
// modelRealms 的顺序是契约的一部分——前端按 [cn, global] 顺序渲染分栏，
// 网关 realms 输出的顺序也以此为约定（见网关 handler.go 的写出循环）。
func TestModelBareAndRealms(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"global:x", "x"},
		{"cn:y", "y"},
		{"plain", "plain"},
		{"global:", ""},
		{"", ""},
	} {
		if got := modelBare(tc.in); got != tc.want {
			t.Errorf("modelBare(%q)=%q want %q", tc.in, got, tc.want)
		}
	}

	for _, tc := range []struct {
		name   string
		id     string
		realms []string
		want   []string
	}{
		{"网关 realms 两域", "m", []string{"global", "cn"}, []string{"cn", "global"}}, // 输入乱序也应归一到 cn, global
		{"网关 realms 单域", "m", []string{"global"}, []string{"global"}},
		{"无 realms 裸名回退 cn", "m", nil, []string{"cn"}},
		{"无 realms global 前缀", "global:m", nil, []string{"global"}},
		{"无 realms cn 前缀", "cn:m", nil, []string{"cn"}},
		{"realms 含未知值被忽略", "m", []string{"mars"}, []string{"cn"}}, // 非法域 → 退回前缀推断
	} {
		got := modelRealms(tc.id, tc.realms)
		if len(got) != len(tc.want) {
			t.Errorf("%s: modelRealms(%q,%v)=%v want %v", tc.name, tc.id, tc.realms, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: modelRealms(%q,%v)=%v want %v", tc.name, tc.id, tc.realms, got, tc.want)
				break
			}
		}
	}
}
