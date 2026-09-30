package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api-gui/internal/authstore"
	"workbuddy2api-gui/internal/config"
	"workbuddy2api-gui/internal/gateway"
	"workbuddy2api-gui/internal/ops"
)

// TestAccountStatusLedgerPassthrough 钉住账号台账字段在面板侧的存活。
//
// 为什么值得单独一条测试（与 TestModelRealmsPassthrough 同因）：
// gateway.AccountStatus 是白名单式结构体，encoding/json 对未声明字段**静默丢弃**
// ——不报错、不告警，症状只是面板上那栏永远为空。`realms` 已经这么消失过一次，
// 而这两份台账（模型级限流 + 每模型实测成本）恰恰是「按号分账」在面板上**唯一**的
// 出口：/v1/stats 只按请求体模型字符串聚合、不区分账号，统一调度后裸名又跨域选号，
// 丢了这两个字段就再没有别处能看出「哪个号在哪个模型上花了多少」。
//
// 覆盖两段：解码（网关 /status → gateway.AccountStatus）+ 编码（AccountView → JSON）。
func TestAccountStatusLedgerPassthrough(t *testing.T) {
	// 与网关 pool.Status 输出同形（omitempty 字段在无记录时缺席，此处给全）。
	raw := []byte(`{"accounts":[{
		"uid":"u1","realm":"global","credits":100,"disabled":false,
		"rate_limited_models":[
			{"model":"deepseek-v4.1-flash","until":"2026-09-30T10:00:00Z",
			 "reset_at":"2026-09-30T12:00:00Z","reason":"429 rate limit"}
		],
		"model_costs":[
			{"model":"deepseek-v4.1-flash","cost_per_1k":0.0299,
			 "last_seen":"2026-09-30T09:00:00Z","samples":7},
			{"model":"free-model","cost_per_1k":0,"last_seen":"2026-09-30T09:00:00Z"}
		]
	}]}`)

	var decoded struct {
		Accounts []gateway.AccountStatus `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解码 /status 失败: %v", err)
	}
	if len(decoded.Accounts) != 1 {
		t.Fatalf("账号数=%d want 1", len(decoded.Accounts))
	}
	ga := decoded.Accounts[0]

	if ga.Realm != "global" {
		t.Errorf("realm 在解码时被丢弃: %q（gateway.AccountStatus 漏声明 json tag？）", ga.Realm)
	}
	if len(ga.RateLimitedModels) != 1 {
		t.Fatalf("rate_limited_models 被丢弃: %+v", ga.RateLimitedModels)
	}
	if got := ga.RateLimitedModels[0]; got.Model != "deepseek-v4.1-flash" || got.Reason != "429 rate limit" {
		t.Errorf("限流条目字段错误: %+v", got)
	}
	// Until 与 ResetAt 是**两个不同**的时点（前者被 soft_rate_max 截断），
	// 混用会让运维看到错误的重置时间，故分别断言。
	if !ga.RateLimitedModels[0].Until.Equal(time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("until 解析错误: %v", ga.RateLimitedModels[0].Until)
	}
	if !ga.RateLimitedModels[0].ResetAt.Equal(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("reset_at 解析错误: %v", ga.RateLimitedModels[0].ResetAt)
	}

	if len(ga.ModelCosts) != 2 {
		t.Fatalf("model_costs 被丢弃: %+v", ga.ModelCosts)
	}
	if got := ga.ModelCosts[0]; got.Model != "deepseek-v4.1-flash" || got.CostPer1k != 0.0299 || got.Samples != 7 {
		t.Errorf("成本条目字段错误: %+v", got)
	}
	// cost_per_1k = 0 是**实测免费**（tier 0），与「无观测」不同——必须能解出 0
	// 而不是被当作缺省丢弃（前端据此显示「免费」并解释选号偏好）。
	if got := ga.ModelCosts[1]; got.CostPer1k != 0 || got.Model != "free-model" {
		t.Errorf("免费观测（cost_per_1k=0）未被保留: %+v", got)
	}

	// 编码：AccountView 是 /api/accounts 与 /api/accounts/{uid} 的响应体。
	view := ops.AccountView{
		UID:               "u1",
		Realm:             ga.Realm,
		RateLimitedModels: ga.RateLimitedModels,
		ModelCosts:        ga.ModelCosts,
	}
	blob, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	if back["realm"] != "global" {
		t.Errorf("realm 未透出到 /api/accounts 响应: %s", blob)
	}
	rlm, ok := back["rate_limited_models"].([]any)
	if !ok || len(rlm) != 1 {
		t.Fatalf("rate_limited_models 未透出: %s", blob)
	}
	mc, ok := back["model_costs"].([]any)
	if !ok || len(mc) != 2 {
		t.Fatalf("model_costs 未透出: %s", blob)
	}

	// 空台账必须**缺席**而非空数组：前端据此区分「无记录」与「查询失败」，
	// 空数组会被渲染成一张空表格，看着像坏了。
	empty, err := json.Marshal(ops.AccountView{UID: "u2"})
	if err != nil {
		t.Fatal(err)
	}
	var emptyBack map[string]any
	if err := json.Unmarshal(empty, &emptyBack); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"realm", "rate_limited_models", "model_costs"} {
		if _, present := emptyBack[k]; present {
			t.Errorf("无记录时 %s 不应出现在响应里: %s", k, empty)
		}
	}
}

// TestAccountsMergesLedgerFields 覆盖 Service.Accounts 里 /status → AccountView 的
// **合并赋值**那几行。
//
// 为什么单靠上面的编解码测试不够：那是手工构造 AccountView，只钉住「结构体声明了
// 这些字段」。而真正的丢失点在 merge 循环里——网关下发的值要逐字段搬到 AccountView，
// 漏搬一行同样静默：结构体测试照样绿，面板上那栏照样空。这条测试真起一个假网关
// /status，走完 Accounts() 全路径，故能钉住 merge。
func TestAccountsMergesLedgerFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accounts":[{
			"uid":"u1","realm":"global","credits":42,
			"rate_limited_models":[{"model":"m1","until":"2026-09-30T10:00:00Z","reason":"429"}],
			"model_costs":[{"model":"m1","cost_per_1k":0.5,"last_seen":"2026-09-30T09:00:00Z","samples":3}]
		}],"total":1,"healthy":1}`))
	}))
	defer srv.Close()

	store, err := authstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("构建凭证 store: %v", err)
	}
	// 只用到 /status，upstream 传 nil 即可（Accounts 不碰上游）。
	svc := ops.New(&config.Config{}, store, gateway.New(srv.URL, func() string { return "" }, time.Second), nil)

	// 凭证目录为空 → 账号全部来自 /status 的「池中存在但磁盘无凭证」分支，
	// 该分支同样要带上台账字段，正好一并覆盖。
	accounts, status, _, err := svc.Accounts(t.Context())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if status == nil {
		t.Fatal("未取到网关 /status")
	}
	if len(accounts) != 1 {
		t.Fatalf("账号数=%d want 1", len(accounts))
	}
	a := accounts[0]

	if a.Realm != "global" {
		t.Errorf("realm 未从 /status 合并到 AccountView: %q", a.Realm)
	}
	if len(a.RateLimitedModels) != 1 || a.RateLimitedModels[0].Model != "m1" {
		t.Errorf("rate_limited_models 未合并: %+v", a.RateLimitedModels)
	}
	if len(a.ModelCosts) != 1 || a.ModelCosts[0].Model != "m1" || a.ModelCosts[0].Samples != 3 {
		t.Errorf("model_costs 未合并: %+v", a.ModelCosts)
	}
}
