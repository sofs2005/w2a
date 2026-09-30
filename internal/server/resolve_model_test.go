package server

import (
	"reflect"
	"testing"

	"workbuddy2api/internal/aliases"
	"workbuddy2api/internal/pool"
)

// aliasTableForTest 由 JSON 字面量构造别名表（解析失败直接 Fatal）。
func aliasTableForTest(t *testing.T, raw string) *aliases.Store {
	t.Helper()
	tbl, err := aliases.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("解析测试别名表: %v", err)
	}
	s := aliases.NewStore()
	s.Set(tbl)
	return s
}

// TestResolveModel 覆盖 PLAN D6 前缀解析协议（统一调度改造后）：
// 取第一个 ":"，前段为 cn/global 才剥离（钉域）；否则视为裸名 → RealmUnified（全池）。
func TestResolveModel(t *testing.T) {
	SetUnifiedRouting(true)
	t.Cleanup(func() { SetUnifiedRouting(true) })

	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"cn:glm-5.2", "cn", "glm-5.2"},
		{"global:gpt-5.4", "global", "gpt-5.4"},
		{"glm-5.2", RealmUnified, "glm-5.2"}, // 裸名 → 全池（本改造核心语义）
		{"deepseek:v3", RealmUnified, "deepseek:v3"}, // 冒号前段不在枚举内，不剥离
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}

// TestResolveModelEdge 边界：空串、仅冒号、空前缀、大小写。
func TestResolveModelEdge(t *testing.T) {
	SetUnifiedRouting(true)
	t.Cleanup(func() { SetUnifiedRouting(true) })

	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"", RealmUnified, ""},
		{":", RealmUnified, ":"},
		{":model", RealmUnified, ":model"}, // 空前缀不匹配 cn/global，不剥离
		{"GLOBAL:gpt-5", RealmUnified, "GLOBAL:gpt-5"}, // 大小写敏感：不做归一
		{"global:", "global", ""},                      // 前缀合法 + 空裸名仍剥离
		{"global:gpt-5.4", "global", "gpt-5.4"},
		{"cn:", "cn", ""},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}

// TestResolveModelUnifiedRoutingOff 逃生门：pool.unified_routing=false 时
// 裸名退回旧语义（只路由 CN），钉域前缀不受影响。
func TestResolveModelUnifiedRoutingOff(t *testing.T) {
	SetUnifiedRouting(false)
	t.Cleanup(func() { SetUnifiedRouting(true) })

	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"glm-5.2", "cn", "glm-5.2"},                  // 裸名 → cn（旧语义）
		{"deepseek:v3", "cn", "deepseek:v3"},          // 未知前缀同样回落 cn
		{"cn:glm-5.2", "cn", "glm-5.2"},               // 钉域不受开关影响
		{"global:gpt-5.4", "global", "gpt-5.4"},       // 钉域不受开关影响
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("unified_off: resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}

// TestResolveRouteNoAlias 无别名/无表：路由裁决退化为纯前缀解析——对外名 = 剥前缀后
// 的名字，候选域由 Realm 决定（nil=全池，单域=钉域），不标 aliased。
func TestResolveRouteNoAlias(t *testing.T) {
	SetUnifiedRouting(true)
	t.Cleanup(func() { SetUnifiedRouting(true) })

	for _, store := range []*aliases.Store{nil, aliases.NewStore()} {
		for _, c := range []struct{ in, wantRealm, wantName string }{
			{"glm-5.2", RealmUnified, "glm-5.2"},
			{"cn:glm-5.2", "cn", "glm-5.2"},
			{"global:gpt-5.4", "global", "gpt-5.4"},
		} {
			r, err := ResolveRoute(store, c.in)
			if err != nil {
				t.Fatalf("ResolveRoute(%q): %v", c.in, err)
			}
			if r.Realm != c.wantRealm || r.Name != c.wantName || r.Aliased {
				t.Errorf("ResolveRoute(%q)=%+v want realm=%q name=%q aliased=false", c.in, r, c.wantRealm, c.wantName)
			}
			if r.Realms != nil {
				t.Errorf("无别名时 Realms 应为 nil（不额外收紧）: %v", r.Realms)
			}
			if out, ok := r.Outbound("cn"); !ok || out != c.wantName {
				t.Errorf("无别名 Outbound(cn)=(%q,%v) want (%q,true)", out, ok, c.wantName)
			}
			if out, ok := r.Outbound("global"); !ok || out != c.wantName {
				t.Errorf("无别名 Outbound(global)=(%q,%v) want (%q,true)", out, ok, c.wantName)
			}
		}
	}
}

// TestResolveRouteAliasDualRealm 双域别名：对外名改写生效，候选域不收窄
//（两域都能承接），出站名按账号域各取一侧。
func TestResolveRouteAliasDualRealm(t *testing.T) {
	SetUnifiedRouting(true)
	t.Cleanup(func() { SetUnifiedRouting(true) })

	store := aliasTableForTest(t, `{"aliases":[{"name":"my-model","cn":"cn-phys","global":"gl-phys"}]}`)
	r, err := ResolveRoute(store, "my-model")
	if err != nil {
		t.Fatalf("ResolveRoute: %v", err)
	}
	if !r.Aliased || r.Name != "my-model" {
		t.Fatalf("route=%+v want aliased 且对外名 my-model", r)
	}
	if len(r.Realms) != 0 {
		t.Errorf("双域别名不额外收紧候选域: %v", r.Realms)
	}
	if !r.AllowsRealm("cn") || !r.AllowsRealm("global") {
		t.Errorf("双域别名两域都应允许: cn=%v global=%v", r.AllowsRealm("cn"), r.AllowsRealm("global"))
	}
	if out, ok := r.Outbound("cn"); !ok || out != "cn-phys" {
		t.Errorf("Outbound(cn)=(%q,%v) want (cn-phys,true)", out, ok)
	}
	if out, ok := r.Outbound("global"); !ok || out != "gl-phys" {
		t.Errorf("Outbound(global)=(%q,%v) want (gl-phys,true)", out, ok)
	}
}

// TestResolveRouteAliasSingleRealm 单域别名：候选域收紧到该域（另一域账号不参与选号），
// 且该域的出站名可用、另一域返回 notOK（防御性兜底）。
func TestResolveRouteAliasSingleRealm(t *testing.T) {
	SetUnifiedRouting(true)
	t.Cleanup(func() { SetUnifiedRouting(true) })

	store := aliasTableForTest(t, `{"aliases":[
		{"name":"only-global","global":"gl-phys"},
		{"name":"only-cn","cn":"cn-phys"}
	]}`)

	r, err := ResolveRoute(store, "only-global")
	if err != nil {
		t.Fatalf("ResolveRoute(only-global): %v", err)
	}
	if !reflect.DeepEqual(r.CandidateRealms(), pool.RealmSet{"global": true}) {
		t.Errorf("only-global 候选域=%v want {global}", r.CandidateRealms())
	}
	if r.AllowsRealm("cn") {
		t.Error("only-global 不得允许 CN 账号承接")
	}
	if _, ok := r.Outbound("cn"); ok {
		t.Error("only-global 在 CN 域出站名应为 notOK")
	}

	r, err = ResolveRoute(store, "only-cn")
	if err != nil {
		t.Fatalf("ResolveRoute(only-cn): %v", err)
	}
	if !reflect.DeepEqual(r.CandidateRealms(), pool.RealmSet{"cn": true}) {
		t.Errorf("only-cn 候选域=%v want {cn}", r.CandidateRealms())
	}
	if r.AllowsRealm("global") {
		t.Error("only-cn 不得允许 global 账号承接")
	}
}

// TestResolveRouteAliasPinnedConflict 单域别名与显式钉域矛盾 → Err（调用方 400，
// 不浪费一轮选号）：条目只有 global 名，请求却钉 CN（反之亦然）。
func TestResolveRouteAliasPinnedConflict(t *testing.T) {
	SetUnifiedRouting(true)
	t.Cleanup(func() { SetUnifiedRouting(true) })

	store := aliasTableForTest(t, `{"aliases":[
		{"name":"only-global","global":"gl-phys"},
		{"name":"only-cn","cn":"cn-phys"}
	]}`)
	if _, err := ResolveRoute(store, "cn:only-global"); err == nil {
		t.Error("cn:only-global 钉域与该条目不匹配，应报错")
	}
	if _, err := ResolveRoute(store, "global:only-cn"); err == nil {
		t.Error("global:only-cn 钉域与该条目不匹配，应报错")
	}
	// 钉到**匹配**的那一域：合法，候选域仍为该域。
	r, err := ResolveRoute(store, "global:only-global")
	if err != nil {
		t.Fatalf("global:only-global 应合法: %v", err)
	}
	if r.Realm != "global" || !reflect.DeepEqual(r.CandidateRealms(), pool.RealmSet{"global": true}) {
		t.Errorf("global:only-global route=%+v want realm=global 候选域 {global}", r)
	}
}

// TestRouteOutboundUsesSnapshot 出站名取 Route 自带的条目快照，不回查 store：
// 请求进行中别名表被热加载换掉，本次请求的候选域与出站名必须仍自洽（不会出现
// "候选集按旧表、出站名按新表"）。
func TestRouteOutboundUsesSnapshot(t *testing.T) {
	SetUnifiedRouting(true)
	t.Cleanup(func() { SetUnifiedRouting(true) })

	store := aliasTableForTest(t, `{"aliases":[{"name":"m","cn":"old-cn","global":"old-gl"}]}`)
	r, err := ResolveRoute(store, "m")
	if err != nil {
		t.Fatal(err)
	}
	// 请求进行中热加载换表。
	store.Set(aliasTableForTest(t, `{"aliases":[{"name":"m","cn":"new-cn","global":"new-gl"}]}`).Get())

	if out, ok := r.Outbound("cn"); !ok || out != "old-cn" {
		t.Errorf("Outbound(cn)=(%q,%v) want (old-cn,true)（快照自洽，不随热加载漂移）", out, ok)
	}
	if n := len(r.CandidateRealms()); n != 0 {
		t.Errorf("双域别名候选域应保持全池（快照口径）: %v", r.CandidateRealms())
	}
}

