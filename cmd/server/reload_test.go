package main

import (
	"os"
	"path/filepath"
	"testing"

	"workbuddy2api/internal/aliases"
)

// TestReloadAliasesOnce 别名热加载一轮的全部契约：
//   - 文件从未有到有 → 载入（默认部署没有别名文件，创建后必须能生效）；
//   - 内容改变 → 换表（新映射立即生效、旧映射消失）；
//   - 非法 JSON → 保持旧表且不推进基线（修好后仍能载入）；
//   - 签名未变 → 无操作（不重复解析、不误清空）。
func TestReloadAliasesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model_aliases.json")
	store := aliases.NewStore()

	// 基线：文件还不存在。
	last := aliases.Signature(path)
	if last != "missing" {
		t.Fatalf("初始 Signature=%q want missing", last)
	}

	// 1) 从无到有 → 载入。
	if err := os.WriteFile(path, []byte(`{"aliases":[{"name":"m","cn":"cn-phys"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	last = reloadAliasesOnce(path, store, last)
	if got, ok := store.Resolve("m", "cn"); !ok || got != "cn-phys" {
		t.Fatalf("创建文件后 Resolve=(%q,%v) want (cn-phys,true)", got, ok)
	}

	// 2) 内容改变 → 换表（旧映射消失，防止"只增不减"）。
	if err := os.WriteFile(path, []byte(`{"aliases":[{"name":"m","global":"gl-phys"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	last = reloadAliasesOnce(path, store, last)
	if got, ok := store.Resolve("m", "global"); !ok || got != "gl-phys" {
		t.Errorf("改写后 Resolve(m,global)=(%q,%v) want (gl-phys,true)", got, ok)
	}
	if got, ok := store.Resolve("m", "cn"); ok || got != "" {
		t.Errorf("改写后 cn 侧应为不可用（该域不存在）: (%q,%v) want (\"\",false)", got, ok)
	}

	// 3) 非法 JSON → 保持旧表，且**不推进基线**（返回值仍是旧指纹）。
	badSig := aliases.Signature(path)
	if err := os.WriteFile(path, []byte(`{"aliases":`), 0o600); err != nil {
		t.Fatal(err)
	}
	last = reloadAliasesOnce(path, store, last)
	if got, ok := store.Resolve("m", "global"); !ok || got != "gl-phys" {
		t.Errorf("非法 JSON 后旧映射被破坏: Resolve=(%q,%v) want (gl-phys,true)", got, ok)
	}
	if last != badSig {
		t.Errorf("解析失败时基线被推进（last=%q want %q）：修好后会漏检", last, badSig)
	}

	// 4) 修好 → 正常载入（基线未推进，签名再变即触发）。
	if err := os.WriteFile(path, []byte(`{"aliases":[{"name":"m","cn":"fixed"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	last = reloadAliasesOnce(path, store, last)
	if got, ok := store.Resolve("m", "cn"); !ok || got != "fixed" {
		t.Errorf("修复后 Resolve=(%q,%v) want (fixed,true)", got, ok)
	}

	// 5) 签名未变 → 无操作（重复调用不改变结果）。
	prev := aliases.Signature(path)
	last2 := reloadAliasesOnce(path, store, prev)
	if last2 != prev {
		t.Errorf("签名未变时应原样返回基线: got %q want %q", last2, prev)
	}
	if got, ok := store.Resolve("m", "cn"); !ok || got != "fixed" {
		t.Errorf("重复调用破坏了表: Resolve=(%q,%v)", got, ok)
	}
}
