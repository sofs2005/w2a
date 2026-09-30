package pool

import (
	"reflect"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// realmPool 构造一个含 cn/global 账号的池，并确保 globalEnabled 开关开启（缺省）。
func realmPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	// CN 账号（显式 cn realm 或空 realm+cn domain 均可）→ Realm()=="cn"
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: ""}) // 空 domain → cn
	// global 账号 → Realm()=="global"
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "g2", Domain: "workbuddy.ai"})
	return p
}

// realmOf 直接读账号 realm（等价 e.a.Realm()）。
func realmOf(a *auth.Auth) string { return a.Realm() }

func TestAvailableUIDsForRealm(t *testing.T) {
	p := realmPool(t)

	// 全部 healthy → cn 集合只有 cn1/cn2，global 集合只有 g1/g2。
	got := p.AvailableUIDsForRealm("cn")
	want := []string{"cn1", "cn2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(cn)=%v want %v", got, want)
	}
	got = p.AvailableUIDsForRealm("global")
	want = []string{"g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(global)=%v want %v", got, want)
	}

	// realm=="" 退化为现状（等价 AvailableUIDs：全部 healthy）。
	got = p.AvailableUIDsForRealm("")
	want = []string{"cn1", "cn2", "g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm()=%v want %v", got, want)
	}
}

func TestAvailableUIDsForModelRealm(t *testing.T) {
	p := realmPool(t)
	// 6004 模型冷却只落在 cn-1 上 → AvailableUIDsForModelRealm("glm-5.2","cn") 排除它，
	// 但 cn 集合至少还保底 cn-2；global 集合不受 cn 冷却影响。
	p.CooldownSoftForModel("cn1", 10*time.Minute, time.Now().Add(30*time.Minute), "glm-5.2", "model 6004")

	got := p.AvailableUIDsForModelRealm("glm-5.2", "cn")
	if len(got) != 1 || got[0] != "cn2" {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2,cn)=%v want [cn2]", got)
	}
	got = p.AvailableUIDsForModelRealm("glm-5.2", "global")
	want := []string{"g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2,global)=%v want %v", got, want)
	}
	// realm=="" → 等价 AvailableUIDsForModel（模型豁免照常：cn1 仍被该模型冷却排除）。
	got = p.AvailableUIDsForModelRealm("glm-5.2", "")
	want = []string{"cn2", "g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2)=%v want %v", got, want)
	}
}

func TestPickExcludingForRealm(t *testing.T) {
	p := realmPool(t)
	// realm=cn → 只从 cn 集合选。
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "", "cn")
		if a == nil {
			t.Fatal("PickExcludingForRealm(cn) returned nil")
		}
		seen[a.UID] = true
		if realmOf(a) != "cn" {
			t.Fatalf("PickExcludingForRealm(cn) returned global account %s", a.UID)
		}
	}
	// global 同样只从 global 集合。
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "", "global")
		if a == nil {
			t.Fatal("PickExcludingForRealm(global) returned nil")
		}
		if realmOf(a) != "global" {
			t.Fatalf("PickExcludingForRealm(global) returned cn account %s", a.UID)
		}
	}
	// realm=="" → 退化现状：所有 healthy 都能选。
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "", "")
		if a == nil {
			t.Fatal("PickExcludingForRealm() returned nil")
		}
	}
}

func TestPickExcludingForRealmTried(t *testing.T) {
	p := realmPool(t)
	// tried 排除在 realm 过滤之后（维持请求级轮换语义）。
	a := p.PickExcludingForRealm(map[string]bool{"cn1": true}, "", "cn")
	if a == nil {
		t.Fatal("tried cn1 → nil")
	}
	if a.UID == "cn1" {
		t.Fatalf("tried cn1 still picked: %s", a.UID)
	}
	if realmOf(a) != "cn" {
		t.Fatalf("picked non-cn %s", a.UID)
	}
}

// ---------------------------------------------------------------------------
// RealmSet 集合谓词（统一调度改造）
// ---------------------------------------------------------------------------

// TestRealmSetAllows 集合谓词三态：空集 = 全池（恒允许）；单域 = 只允许该域；
// 两域并集 = 都允许（语义等价全池，但 Key 不同）。
func TestRealmSetAllows(t *testing.T) {
	cases := []struct {
		name   string
		s      RealmSet
		realm  string
		expect bool
	}{
		{"nil 全池允许 cn", nil, "cn", true},
		{"nil 全池允许 global", nil, "global", true},
		{"空集全池允许 global", RealmSet{}, "global", true},
		{"cn 单域允许 cn", RealmSet{"cn": true}, "cn", true},
		{"cn 单域拒绝 global", RealmSet{"cn": true}, "global", false},
		{"global 单域允许 global", RealmSet{"global": true}, "global", true},
		{"global 单域拒绝 cn", RealmSet{"global": true}, "cn", false},
		{"并集允许 cn", RealmSet{"cn": true, "global": true}, "cn", true},
		{"并集允许 global", RealmSet{"cn": true, "global": true}, "global", true},
	}
	for _, c := range cases {
		if got := c.s.Allows(c.realm); got != c.expect {
			t.Errorf("%s: Allows(%q)=%v want %v", c.name, c.realm, got, c.expect)
		}
	}
}

// TestSingleRealm 单域构造："" → nil（全池，与既有 realm=="" 退化语义严格一致）；
// 非空 → 单元素集合。
func TestSingleRealm(t *testing.T) {
	if got := SingleRealm(""); got != nil {
		t.Errorf("SingleRealm(\"\")=%v want nil（全池）", got)
	}
	s := SingleRealm("global")
	if len(s) != 1 || !s["global"] {
		t.Errorf("SingleRealm(global)=%v want {global}", s)
	}
}

// TestRealmSetKey Key 稳定且与单值语义对齐：空 → ""（成本探索 timer 键零漂移）、
// 单域 → 该域本身、并集 → 排序后 "+" 连接（map 遍历无序，必须排序才跨调用稳定）。
func TestRealmSetKey(t *testing.T) {
	cases := []struct {
		name string
		s    RealmSet
		want string
	}{
		{"nil", nil, ""},
		{"空集", RealmSet{}, ""},
		{"cn", RealmSet{"cn": true}, "cn"},
		{"global", RealmSet{"global": true}, "global"},
		{"并集", RealmSet{"cn": true, "global": true}, "cn+global"},
		{"并集（插入序相反）", RealmSet{"global": true, "cn": true}, "cn+global"},
	}
	for _, c := range cases {
		if got := c.s.Key(); got != c.want {
			t.Errorf("%s: Key()=%q want %q", c.name, got, c.want)
		}
	}
	// 并集键不等于全池键（成本探索 timer 分开，见 RealmSet 注释）。
	union := RealmSet{"cn": true, "global": true}
	if union.Key() == RealmSet(nil).Key() {
		t.Error("并集 Key 不得等于全池 Key（探索 timer 会混用）")
	}
}

// TestPickExcludingForRealms 集合形态选号：单域集合只从该域选（含全冷却兜底）；
// 并集与全池都能选到两域账号。
func TestPickExcludingForRealms(t *testing.T) {
	p := realmPool(t) // cn1/cn2/g1/g2，已关 minPickGap
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealms(nil, "", RealmSet{"cn": true})
		if a == nil {
			t.Fatal("PickExcludingForRealms({cn}) returned nil")
		}
		if realmOf(a) != "cn" {
			t.Fatalf("集合 {cn} 选出 global 账号 %s", a.UID)
		}
	}
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealms(nil, "", RealmSet{"global": true})
		if a == nil {
			t.Fatal("PickExcludingForRealms({global}) returned nil")
		}
		if realmOf(a) != "global" {
			t.Fatalf("集合 {global} 选出 cn 账号 %s", a.UID)
		}
	}
	// 并集：两域都可能出现（不逐次断言，只要求非 nil 且域合法）。
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		a := p.PickExcludingForRealms(nil, "", RealmSet{"cn": true, "global": true})
		if a == nil {
			t.Fatal("并集选号 returned nil")
		}
		seen[realmOf(a)] = true
	}
	if !seen["cn"] || !seen["global"] {
		t.Errorf("并集选号未覆盖两域: %v", seen)
	}
}

// TestPickExcludingForRealmsFallbackRespectsSet 全冷却兜底同样受集合约束：
// 只有 global 号冷却时，{cn} 集合兜底不得把 global 号捞回来（否则单域别名
// 仍会打到另一域，白费一轮 11102）。
func TestPickExcludingForRealmsFallbackRespectsSet(t *testing.T) {
	p := New("")
	withNoPickGap(t)
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	// 两号都进冷却 → normal 无候选，走全冷却兜底分支。
	p.Cooldown("cn1", CoolSoft, time.Hour, "429")
	p.Cooldown("g1", CoolSoft, time.Hour, "429")

	if got := p.PickExcludingForRealms(nil, "", RealmSet{"global": true}); got == nil || got.UID != "g1" {
		t.Fatalf("兜底（集合 {global}）应选 g1, got %+v", got)
	}
	if got := p.PickExcludingForRealms(nil, "", RealmSet{"cn": true}); got == nil || got.UID != "cn1" {
		t.Fatalf("兜底（集合 {cn}）应选 cn1, got %+v", got)
	}
}

// TestAvailableUIDsForModelRealms 可用集合的集合形态：单域集合只列该域账号，
// 模型级 6004 豁免照常生效；并集 = 两域之和。
func TestAvailableUIDsForModelRealms(t *testing.T) {
	p := realmPool(t)
	p.CooldownSoftForModel("cn1", 10*time.Minute, time.Now().Add(30*time.Minute), "glm-5.2", "model 6004")

	if got := p.AvailableUIDsForModelRealms("glm-5.2", RealmSet{"cn": true}); !reflect.DeepEqual(got, []string{"cn2"}) {
		t.Errorf("集合 {cn}: %v want [cn2]（cn1 被模型冷却排除）", got)
	}
	if got := p.AvailableUIDsForModelRealms("glm-5.2", RealmSet{"global": true}); !reflect.DeepEqual(got, []string{"g1", "g2"}) {
		t.Errorf("集合 {global}: %v want [g1 g2]", got)
	}
	if got := p.AvailableUIDsForModelRealms("glm-5.2", RealmSet{"cn": true, "global": true}); !reflect.DeepEqual(got, []string{"cn2", "g1", "g2"}) {
		t.Errorf("并集: %v want [cn2 g1 g2]", got)
	}
	if got := p.AvailableUIDsForModelRealms("glm-5.2", nil); !reflect.DeepEqual(got, []string{"cn2", "g1", "g2"}) {
		t.Errorf("全池: %v want [cn2 g1 g2]（与并集候选等价）", got)
	}
}

// TestUrgentUIDsForModelRealms 紧急到期候选集的集合形态：域外临期号不得入选
// （单域别名下把 cn 的临期号拉进 global 候选会把会话导向不可用域）。
func TestUrgentUIDsForModelRealms(t *testing.T) {
	p := New("")
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	soon := time.Now().Add(3 * time.Hour)
	p.SetCreditsExpiring("cn1", 100, 100, []CreditBatch{{ExpiresAt: soon, Remain: 100}})
	p.SetCreditsExpiring("g1", 100, 100, []CreditBatch{{ExpiresAt: soon.Add(time.Hour), Remain: 100}})

	if got := p.UrgentUIDsForModelRealms("m", RealmSet{"global": true}); !reflect.DeepEqual(got, []string{"g1"}) {
		t.Errorf("集合 {global}: %v want [g1]（cn1 虽更临期但域外）", got)
	}
	if got := p.UrgentUIDsForModelRealms("m", RealmSet{"cn": true}); !reflect.DeepEqual(got, []string{"cn1"}) {
		t.Errorf("集合 {cn}: %v want [cn1]", got)
	}
	if got := p.UrgentUIDsForModelRealms("m", nil); !reflect.DeepEqual(got, []string{"cn1", "g1"}) {
		t.Errorf("全池: %v want [cn1 g1]（两域临期号一起竞争）", got)
	}
}
