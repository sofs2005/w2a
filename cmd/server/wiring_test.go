package main

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"workbuddy2api/internal/aliases"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
)

// aliasStore 由 JSON 字面量构造别名 Store（测试用；解析失败直接 Fatal）。
func aliasStore(t *testing.T, raw string) *aliases.Store {
	t.Helper()
	tbl, err := aliases.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("解析测试别名表: %v", err)
	}
	s := aliases.NewStore()
	s.Set(tbl)
	return s
}

// realmPool 构造含 cn/global 账号的池并确保 global 开关开启（缺省）。
func realmPool(t *testing.T) *pool.Pool {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	return p
}

// TestRealmAwareAvailableForModel 断言会话粘性的 realm 感知闭包（统一调度改造后）：
// 带前缀的模型名按 realm 过滤可用账号（钉域）；裸名 → 全池（CN + global 一起调度）。
func TestRealmAwareAvailableForModel(t *testing.T) {
	p := realmPool(t)
	fn := realmAwareAvailableForModel(p, aliases.NewStore())

	cases := []struct {
		model string
		want  []string
	}{
		{"glm-5.2", []string{"cn1", "g1"}}, // 裸名 → 全池（本改造核心：跨域调度）
		{"cn:glm-5.2", []string{"cn1"}},    // 显式 cn 前缀 → cn 集合（钉域）
		{"global:gpt-5.4", []string{"g1"}}, // global 前缀 → global 集合（钉域）
	}
	for _, c := range cases {
		if got := fn(c.model); !reflect.DeepEqual(got, c.want) {
			t.Errorf("AvailableForModel(%q)=%v want %v", c.model, got, c.want)
		}
	}
}

// TestRealmAwareAvailableForModelUnifiedOff 逃生门：pool.unified_routing=false 时
// 裸名退回旧语义（只取 cn 集合），钉域前缀不受影响。
func TestRealmAwareAvailableForModelUnifiedOff(t *testing.T) {
	p := realmPool(t)
	server.SetUnifiedRouting(false)
	t.Cleanup(func() { server.SetUnifiedRouting(true) })
	fn := realmAwareAvailableForModel(p, aliases.NewStore())

	if got := fn("glm-5.2"); !reflect.DeepEqual(got, []string{"cn1"}) {
		t.Errorf("unified off: AvailableForModel(glm-5.2)=%v want [cn1]（裸名只打 CN）", got)
	}
	if got := fn("global:gpt-5.4"); !reflect.DeepEqual(got, []string{"g1"}) {
		t.Errorf("unified off: AvailableForModel(global:gpt-5.4)=%v want [g1]（钉域不受影响）", got)
	}
}

// TestRealmAwareAvailableForModelGlobalDisabled 逃生门：全局开关显式关闭（config
// "enabled": false）时，即便 auth 写了 realm=global 也不路由 global——闭包对 global:
// 前缀返回空集（纯 CN 锁定语义与旧缺省等价）。
func TestRealmAwareAvailableForModelGlobalDisabled(t *testing.T) {
	auth.SetGlobalEnabled(false)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	fn := realmAwareAvailableForModel(p, aliases.NewStore())

	// 开关关闭 → 该 global 账号 Realm()=="cn"（逃生门），对 global: 前缀不可见。
	if got := fn("global:gpt-5.4"); len(got) != 0 {
		t.Errorf("global disabled: AvailableForModel(global:gpt-5.4)=%v want empty", got)
	}
	// 裸名 → cn：该账号被当作 cn 可见（现状等价，纯 CN 锁定）。
	if got := fn("glm-5.2"); !reflect.DeepEqual(got, []string{"g1"}) {
		t.Errorf("global disabled: AvailableForModel(glm-5.2)=%v want [g1]", got)
	}
}

// TestRealmAwareAvailableForModelDefaultOnCNZeroRegression 开关缺省开启（生产语义）时
// 纯 CN 部署零回归：cn 账号（cn domain / 空 domain）对裸名与 cn: 前缀都可见，
// global: 前缀对纯 CN 池不可见（池里无 global 账号）。
func TestRealmAwareAvailableForModelDefaultOnCNZeroRegression(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: ""}) // 空 domain → cn（老 CN 凭证）
	fn := realmAwareAvailableForModel(p, aliases.NewStore())

	cases := []struct {
		model string
		want  []string
	}{
		{"glm-5.2", []string{"cn1", "cn2"}},    // 裸名 → cn 集合（现状零回归）
		{"cn:glm-5.2", []string{"cn1", "cn2"}}, // cn 前缀 → cn 集合
		{"global:gpt-5.4", nil},                // global 前缀 → 纯 CN 池无可用（不对 spread）
	}
	for _, c := range cases {
		want := c.want
		if want == nil {
			want = []string{}
		}
		if got := fn(c.model); !reflect.DeepEqual(got, want) {
			t.Errorf("AvailableForModel(%q)=%v want %v", c.model, got, want)
		}
	}
}

// TestRealmAwareAvailableForModelAlias 别名表对候选域的收紧：单域条目必须把另一域账号
// 排除在可用集合之外（否则粘性会把会话钉到根本没有该模型的域，选中即 11102）；
// 双域条目等价全池；未命中条目（含"物理名直call"）保持全池，与引入别名前一致。
func TestRealmAwareAvailableForModelAlias(t *testing.T) {
	p := realmPool(t) // cn1 + g1
	store := aliasStore(t, `{"aliases":[
		{"name":"only-global","global":"internal-x"},
		{"name":"only-cn","cn":"glm-5.2-cn"},
		{"name":"both","cn":"glm-5.2","global":"glm-5.2-intl"}
	]}`)
	fn := realmAwareAvailableForModel(p, store)

	cases := []struct {
		model string
		want  []string
	}{
		// 单域条目：候选域只剩 global → CN 账号不可用。
		{"only-global", []string{"g1"}},
		// 单域条目：候选域只剩 CN → global 账号不可用。
		{"only-cn", []string{"cn1"}},
		// 双域条目：不额外收紧 → 全池（与裸名同）。
		{"both", []string{"cn1", "g1"}},
		// 未命中别名：透传 → 全池。
		{"glm-5.2", []string{"cn1", "g1"}},
		// 出站物理名直呼同样透传（不被别名表拦截）。
		{"internal-x", []string{"cn1", "g1"}},
		// 前缀钉域与别名条目冲突（"cn:" 钉域，而该别名没配 cn 名）→ 无可用账号：
		// ResolveRoute 直接判错，闭包返回 nil（handler 对同一请求回 400 而非静默换域）。
		{"cn:only-global", nil},
	}
	for _, c := range cases {
		// nil 与空切片对本闭包语义等价（都是"无可用账号"），统一成空切片再比。
		want, got := c.want, fn(c.model)
		if want == nil {
			want = []string{}
		}
		if len(got) == 0 {
			got = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("AvailableForModel(%q)=%v want %v", c.model, got, want)
		}
	}
}

// TestRealmAwareUrgentForModelAlias 紧急到期候选集与主闭包同口径（同走 ResolveRoute：
// 单域别名只在该域里挑临期号），避免两个闭包分歧导致"紧急优先"把会话导向不可用域。
func TestRealmAwareUrgentForModelAlias(t *testing.T) {
	p := realmPool(t)
	// 两号都在 72h 紧急窗口内（否则不参与硬优先，测试会退化成空集断言）。
	p.SetCreditsExpiring("cn1", 100, 100, []pool.CreditBatch{{ExpiresAt: time.Now().Add(3 * time.Hour), Remain: 100}})
	p.SetCreditsExpiring("g1", 100, 100, []pool.CreditBatch{{ExpiresAt: time.Now().Add(5 * time.Hour), Remain: 100}})

	// 无别名 → 全池口径：两域的临期号一起参与"紧急优先"竞争。
	all := realmAwareUrgentForModel(p, aliases.NewStore())("glm-5.2")
	if !reflect.DeepEqual(all, []string{"cn1", "g1"}) {
		t.Fatalf("无别名 UrgentForModel(glm-5.2)=%v want [cn1 g1]", all)
	}

	// 单域别名 → 候选域只剩 global：cn1 的临期积分不得把它拉回候选。
	store := aliasStore(t, `{"aliases":[{"name":"only-global","global":"internal-x"}]}`)
	if got := realmAwareUrgentForModel(p, store)("only-global"); !reflect.DeepEqual(got, []string{"g1"}) {
		t.Errorf("UrgentForModel(only-global)=%v want [g1]（域外临期号不得入选）", got)
	}
	if got := realmAwareAvailableForModel(p, store)("only-global"); !reflect.DeepEqual(got, []string{"g1"}) {
		t.Errorf("AvailableForModel(only-global)=%v want [g1]", got)
	}
}

// TestAliasFilePath 别名文件路径推导：与 state.json 同目录固定名 model_aliases.json；
// 空 state 路径（纯内存测试形态）→ 空串 = 别名功能关闭（一切透传）。
func TestAliasFilePath(t *testing.T) {
	cases := []struct {
		state, want string
	}{
		{"./data/state.json", "data/model_aliases.json"},
		{"/app/data/state.json", "/app/data/model_aliases.json"},
		{"state.json", "model_aliases.json"},
		{"", ""},
	}
	for _, c := range cases {
		if got := filepath.ToSlash(aliasFilePath(c.state)); got != c.want {
			t.Errorf("aliasFilePath(%q)=%q want %q", c.state, got, c.want)
		}
	}
}

// TestModelJSONPath model.json 路径推导（context_length 四级查找链第 3 级接线）：
// 与 state.json 同目录同名换缀（Docker ./data volume 持久化）；空 state 路径 →
// 空串（禁用落盘，内存 + 种子仍可用）。
func TestModelJSONPath(t *testing.T) {
	cases := []struct {
		state, want string
	}{
		{"./data/state.json", "data/model.json"},
		{"/app/data/state.json", "/app/data/model.json"},
		{"state.json", "model.json"},
		{"", ""},
	}
	for _, c := range cases {
		// 归一化 got 侧：modelJSONPath 走 filepath.Join，Windows 产出反斜杠；
		// want 本就是正斜杠字面量（跨平台规范形式），对 want 做 ToSlash 是无操作，
		// 反斜杠会原样留在 got 里导致断言在 Windows 必然失败。
		if got := filepath.ToSlash(modelJSONPath(c.state)); got != c.want {
			t.Errorf("modelJSONPath(%q)=%q want %q", c.state, got, c.want)
		}
	}
}
