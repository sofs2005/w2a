package aliases

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseValid 合法表：两域条目 + 单域条目（另一域留空）。
func TestParseValid(t *testing.T) {
	tbl, err := Parse([]byte(`{"aliases":[
		{"name":"glm-5.2","cn":"glm-5.2","global":"glm-5.2-intl"},
		{"name":"my-hidden","cn":"","global":"internal-x"},
		{"name":"cn-only","cn":"only-cn","global":""}
	]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tbl.Len() != 3 {
		t.Fatalf("Len=%d want 3", tbl.Len())
	}
	if got := tbl.Names(); len(got) != 3 || got[0] != "cn-only" || got[2] != "my-hidden" {
		t.Errorf("Names=%v want 升序 [cn-only glm-5.2 my-hidden]", got)
	}
}

// TestParseEmpty 空文件 / 空数组都是合法输入（清空表），不是错误。
func TestParseEmpty(t *testing.T) {
	for _, in := range []string{"", "   \n", "{}", `{"aliases":[]}`} {
		tbl, err := Parse([]byte(in))
		if err != nil {
			t.Errorf("Parse(%q) 应合法: %v", in, err)
			continue
		}
		if tbl.Len() != 0 {
			t.Errorf("Parse(%q) Len=%d want 0", in, tbl.Len())
		}
	}
}

// TestParseRejects 非法输入整体拒绝（保持旧表的契约前提）。
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, in string
	}{
		{"非法 JSON", `{"aliases":`},
		{"name 为空", `{"aliases":[{"name":"","cn":"x"}]}`},
		{"name 含冒号", `{"aliases":[{"name":"cn:foo","cn":"x"}]}`},
		{"两域同空", `{"aliases":[{"name":"x","cn":"","global":""}]}`},
		{"name 重复", `{"aliases":[{"name":"x","cn":"a"},{"name":"x","cn":"b"}]}`},
	}
	for _, c := range cases {
		if _, err := Parse([]byte(c.in)); err == nil {
			t.Errorf("%s: Parse 应报错但通过了", c.name)
		}
	}
}

// TestParseAllowsColonInTargets 出站名允许含冒号（既有裸名如 deepseek:v3 带冒号）：
// 它们直接进上游请求体，不经网关前缀解析，拒绝反而是误伤。
func TestParseAllowsColonInTargets(t *testing.T) {
	tbl, err := Parse([]byte(`{"aliases":[{"name":"ds","cn":"deepseek:v3"}]}`))
	if err != nil {
		t.Fatalf("出站名含冒号应合法: %v", err)
	}
	if got, ok := tbl.Resolve("ds", "cn"); !ok || got != "deepseek:v3" {
		t.Errorf("Resolve(ds,cn)=(%q,%v) want (deepseek:v3,true)", got, ok)
	}
}

// TestResolve 三态：未命中透传 / 命中取对应域名 / 命中但该域为空 → notOK。
func TestResolve(t *testing.T) {
	tbl, err := Parse([]byte(`{"aliases":[
		{"name":"both","cn":"c-name","global":"g-name"},
		{"name":"g-only","global":"g-only-name"},
		{"name":"c-only","cn":"c-only-name"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, realm, want string
		ok                bool
	}{
		{"both", "cn", "c-name", true},
		{"both", "global", "g-name", true},
		{"g-only", "global", "g-only-name", true},
		{"g-only", "cn", "", false}, // 单域条目：CN 域不存在
		{"c-only", "global", "", false},
		{"c-only", "cn", "c-only-name", true},
		{"untouched", "cn", "untouched", true},     // 未命中：原样透传
		{"untouched", "global", "untouched", true}, // 两域都透传
		{"untouched", "", "untouched", true},       // 空 realm（全池）也透传
		{"deepseek:v3", "cn", "deepseek:v3", true}, // 带冒号的裸名不是别名键，透传
	}
	for _, c := range cases {
		got, ok := tbl.Resolve(c.name, c.realm)
		if got != c.want || ok != c.ok {
			t.Errorf("Resolve(%q,%q)=(%q,%v) want (%q,%v)", c.name, c.realm, got, ok, c.want, c.ok)
		}
	}
}

// TestAllowedRealms 候选域收紧：未命中 → 两域都允许（全池语义）；命中 → 按非空域名。
func TestAllowedRealms(t *testing.T) {
	tbl, _ := Parse([]byte(`{"aliases":[
		{"name":"both","cn":"a","global":"b"},
		{"name":"g-only","global":"b"},
		{"name":"c-only","cn":"a"}
	]}`))
	cases := []struct {
		name             string
		wantCN, wantGlob bool
	}{
		{"both", true, true},
		{"g-only", false, true},
		{"c-only", true, false},
		{"untouched", true, true},
	}
	for _, c := range cases {
		cn, gl := tbl.AllowedRealms(c.name)
		if cn != c.wantCN || gl != c.wantGlob {
			t.Errorf("AllowedRealms(%q)=(%v,%v) want (%v,%v)", c.name, cn, gl, c.wantCN, c.wantGlob)
		}
	}
}

// TestLoadMissingIsEmpty 文件不存在 = 空表（默认部署没有别名文件，不该报错）。
func TestLoadMissingIsEmpty(t *testing.T) {
	tbl, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("缺失文件应视作空表: %v", err)
	}
	if tbl.Len() != 0 {
		t.Errorf("Len=%d want 0", tbl.Len())
	}
	// 空路径（禁用落盘形态）同样空表。
	if tbl, err := Load(""); err != nil || tbl.Len() != 0 {
		t.Errorf("Load(\"\")=(%v,%v) want (空表,nil)", tbl, err)
	}
}

// TestLoadReadsFile 落盘 → 读取往返。
func TestLoadReadsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "model_aliases.json")
	if err := os.WriteFile(p, []byte(`{"aliases":[{"name":"x","global":"gx"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tbl, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, ok := tbl.Resolve("x", "global"); !ok || got != "gx" {
		t.Errorf("Resolve(x,global)=(%q,%v) want (gx,true)", got, ok)
	}
}

// TestLoadBadJSONErrors 非法 JSON 必须返回错误（调用方据此保持旧表 + WARN）。
func TestLoadBadJSONErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "model_aliases.json")
	if err := os.WriteFile(p, []byte(`{"aliases":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("非法 JSON 应报错（否则热加载会把别名静默清空）")
	}
}

// TestStoreSnapshotSwitch 热加载核心契约：Set 新表后读方立刻看到新映射，
// 旧表快照不被修改（不可变发布）。
func TestStoreSnapshotSwitch(t *testing.T) {
	s := NewStore()
	// 初始空表：一切透传。
	if got, ok := s.Resolve("m", "cn"); !ok || got != "m" {
		t.Fatalf("空表 Resolve=(%q,%v) want (m,true)", got, ok)
	}

	old, _ := Parse([]byte(`{"aliases":[{"name":"m","cn":"old-cn"}]}`))
	s.Set(old)
	if got, ok := s.Resolve("m", "cn"); !ok || got != "old-cn" {
		t.Fatalf("第一次 Set 后 Resolve=(%q,%v) want (old-cn,true)", got, ok)
	}

	newer, _ := Parse([]byte(`{"aliases":[{"name":"m","cn":"new-cn","global":"new-gl"}]}`))
	s.Set(newer)
	if got, _ := s.Resolve("m", "cn"); got != "new-cn" {
		t.Errorf("第二次 Set 后 Resolve=(%q) want new-cn", got)
	}
	if got, ok := s.Resolve("m", "global"); !ok || got != "new-gl" {
		t.Errorf("第二次 Set 后 global=(%q,%v) want (new-gl,true)", got, ok)
	}
	// 旧快照仍是旧内容（不可变：Set 只发布新表，不改旧表）。
	if got, _ := old.Resolve("m", "global"); got != "" {
		t.Errorf("旧表被就地修改了: global=%q", got)
	}
	// Set(nil) 清空。
	s.Set(nil)
	if got, _ := s.Resolve("m", "cn"); got != "m" {
		t.Errorf("Set(nil) 后 Resolve=(%q) want m（透传）", got)
	}
}

// TestSignature 指纹：缺失 → "missing"；存在 → size|mtime；内容变化 → 指纹变化。
func TestSignature(t *testing.T) {
	p := filepath.Join(t.TempDir(), "model_aliases.json")
	if got := Signature(p); got != "missing" {
		t.Errorf("缺失文件 Signature=%q want missing", got)
	}
	if got := Signature(""); got != "" {
		t.Errorf("空路径 Signature=%q want 空串", got)
	}
	if err := os.WriteFile(p, []byte(`{"aliases":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sig1 := Signature(p)
	if sig1 == "missing" || sig1 == "" {
		t.Fatalf("存在文件 Signature=%q 不合法", sig1)
	}
	if err := os.WriteFile(p, []byte(`{"aliases":[{"name":"x","cn":"y"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if sig2 := Signature(p); sig2 == sig1 {
		t.Errorf("内容变化后指纹未变（%q）：热加载会漏检", sig1)
	}
}

// TestEntriesRoundTrip Entries 升序输出且字段完整（面板读写用）。
func TestEntriesRoundTrip(t *testing.T) {
	tbl, _ := Parse([]byte(`{"aliases":[
		{"name":"b","cn":"bc","global":"bg"},
		{"name":"a","global":"ag"}
	]}`))
	es := tbl.Entries()
	if len(es) != 2 || es[0].Name != "a" || es[1].Name != "b" {
		t.Fatalf("Entries=%+v want [a b] 升序", es)
	}
	if es[0].CN != "" || es[0].Global != "ag" {
		t.Errorf("a 条目=%+v want {a, \"\", ag}", es[0])
	}
	if es[1].CN != "bc" || es[1].Global != "bg" {
		t.Errorf("b 条目=%+v want {b, bc, bg}", es[1])
	}
}

// TestParsePanelWrittenFormat 钉住面板产物的具体形态。
//
// 面板 ops.WriteAliases 用 json.MarshalIndent 写出带缩进、字段顺序固定、**空域写成
// 空串**（而不是省略该键）的 JSON。这两个模块是独立 Go module（根 vs panel/），
// 没有共享类型，格式一致性只能靠测试钉住——若哪天面板改成 omitempty 省略空域键，
// 本用例会失败在这里，而不是在生产上表现为"某模型莫名只在 global 可选"。
func TestParsePanelWrittenFormat(t *testing.T) {
	// 逐字取自 ops.WriteAliases 的输出形态（含末尾换行）。
	raw := []byte("{\n" +
		"  \"aliases\": [\n" +
		"    {\n" +
		"      \"name\": \"my-hidden\",\n" +
		"      \"cn\": \"\",\n" +
		"      \"global\": \"gl-phys\"\n" +
		"    },\n" +
		"    {\n" +
		"      \"name\": \"my-dual\",\n" +
		"      \"cn\": \"cn-phys\",\n" +
		"      \"global\": \"gl-phys\"\n" +
		"    }\n" +
		"  ]\n" +
		"}\n")
	tbl, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析面板产物失败（面板保存的表网关读不进来）: %v", err)
	}
	if tbl.Len() != 2 {
		t.Fatalf("Len=%d want 2", tbl.Len())
	}
	// 空域必须解释为"该域无此模型"，而不是"该域用同名"。
	if cn, gl := tbl.AllowedRealms("my-hidden"); cn || !gl {
		t.Errorf("my-hidden AllowedRealms=(cn=%v, global=%v) want (false, true)", cn, gl)
	}
	if _, ok := tbl.Resolve("my-hidden", "cn"); ok {
		t.Error("my-hidden 在 cn 域应不可用（面板留空 = 该域没有此模型）")
	}
	if cn, gl := tbl.AllowedRealms("my-dual"); !cn || !gl {
		t.Errorf("my-dual AllowedRealms=(cn=%v, global=%v) want (true, true)", cn, gl)
	}
}
