package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// ClearModelBlocks 清掉**所有账号**上该模型的 11102 负缓存（全池自愈入口）。
//
// 覆盖四条口径：
//  1. 两个账号各有一条 → 返回 2（一次清干净整个池）；
//  2. 重复清 → 0（幂等，不会把「没东西可清」报成清过，调用方据此决定要不要重试）；
//  3. 只清 11102：同一账号上的 6004 条目（额度重置语义）必须原样留着；
//  4. 空模型名 → 0（防御：裸名为空时全池扫描没有意义）。
func TestClearModelBlocks(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	now := time.Now()
	block := func() modelCooldown {
		return modelCooldown{Until: now.Add(modelBlockBaseTTL), Reason: "11102 model not available"}
	}
	p.mu.Lock()
	p.byUID["u1"].modelCooldowns = map[string]modelCooldown{
		"glm-5.3":             block(),
		"deepseek-v4.1-flash": block(),
	}
	p.byUID["u2"].modelCooldowns = map[string]modelCooldown{
		"deepseek-v4.1-flash": block(),
		// 6004 是「等配额重置」语义，不是「该模型没有」，不能被本方法顺带清掉。
		"glm-5.3": {Until: now.Add(time.Hour), ResetAt: now.Add(time.Hour), Reason: "6004 model rate limit"},
	}
	p.mu.Unlock()

	if n := p.ClearModelBlocks("deepseek-v4.1-flash"); n != 2 {
		t.Fatalf("ClearModelBlocks = %d, want 2", n)
	}
	if n := p.ClearModelBlocks("deepseek-v4.1-flash"); n != 0 {
		t.Fatalf("重复清理 = %d, want 0", n)
	}
	if n := p.ClearModelBlocks("glm-5.3"); n != 1 {
		t.Fatalf("glm-5.3 清理 = %d, want 1（只清 u1 的 11102 条目）", n)
	}
	// 清空后 map 必须置 nil（与 BlockModelClear 同一写法），否则 persist 会持续
	// 序列化一张空表、且 len==0 与 nil 两种形态在同一结构里混用。
	// u1 两条 11102 都清完 → 空 map 应被置 nil；u2 还留着 6004 条目，故非 nil。
	p.mu.RLock()
	u1Left := p.byUID["u1"].modelCooldowns
	p.mu.RUnlock()
	if u1Left != nil {
		t.Fatalf("u1 的 modelCooldowns 应被置 nil, got %+v", u1Left)
	}
	p.mu.RLock()
	kept, ok := p.byUID["u2"].modelCooldowns["glm-5.3"]
	p.mu.RUnlock()
	if !ok {
		t.Fatal("u2 的 6004 冷却不应被清掉")
	}
	if kept.Reason != "6004 model rate limit" {
		t.Fatalf("u2 的 6004 条目被改写: %+v", kept)
	}

	if n := p.ClearModelBlocks(""); n != 0 {
		t.Fatalf("空模型名 = %d, want 0", n)
	}
}

// ClearModelBlocks 清理后必须置 dirty（否则重启会从旧快照把负缓存读回来，
// 自愈效果活不过一次重启）。
func TestClearModelBlocksSetsDirty(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "m", "11102 model not available")
	p.dirty.Store(false)
	if n := p.ClearModelBlocks("m"); n != 1 {
		t.Fatalf("ClearModelBlocks = %d, want 1", n)
	}
	if !p.dirty.Load() {
		t.Fatal("有清理时必须置 dirty")
	}
	// 无清理时不得置 dirty（避免无谓落盘）。
	p.dirty.Store(false)
	if n := p.ClearModelBlocks("m"); n != 0 {
		t.Fatalf("重复清理 = %d, want 0", n)
	}
	if p.dirty.Load() {
		t.Fatal("无清理时不应置 dirty")
	}
}

// ModelBlockedInRealms 与 ModelBlocked 的关系：空域集合退化为全池（逐字一致的旧行为），
// 指定域时只看该域账号——这是 11102 自愈判据必须带的维度（双 realm 部署下
// 全池口径恒为「未阻塞」，自愈永不触发）。
func TestModelBlockedInRealms(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "gl1", Domain: "www.workbuddy.ai"})
	for _, uid := range []string{"cn1", "cn2"} {
		p.BlockModelBackoff(uid, "m", "11102 model not available")
	}

	if st := p.ModelBlockedInRealms("m", SingleRealm("cn")); !st.Blocked || st.Count != 2 {
		t.Fatalf("cn 域应报告全挡: %+v", st)
	}
	if st := p.ModelBlockedInRealms("m", SingleRealm("global")); st.Blocked {
		t.Fatalf("global 域仍有可用号，不该报阻塞: %+v", st)
	}
	if st := p.ModelBlockedInRealms("m", nil); st.Blocked {
		t.Fatalf("全池口径下 global 号可用，不该报阻塞: %+v", st)
	}
	if st := p.ModelBlocked("m"); st.Blocked {
		t.Fatalf("ModelBlocked 必须保持全池口径（零行为变化）: %+v", st)
	}
	if st := p.ModelBlockedInRealms("", nil); st.Blocked {
		t.Fatal("空模型名不该报阻塞")
	}
}
