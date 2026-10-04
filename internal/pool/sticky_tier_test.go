// 粘性可用集合（AvailableUIDsForModelRealms）的成本分层回归。
//
// 背景：会话粘性默认开启（config 的 session_sticky.enabled 缺省 true）。它的候选池
// 来自 pool.AvailableUIDsForModelRealms，而**只有粘性**用这条集合（wiring.go 的
// realmAwareAvailableForModel → session.Config.AvailableForModel）。该集合此前只做
// 「healthyForModel」过滤——不看成本分层。于是：
//
//	会话首轮 settle 到某个 tier 2（收费）号后，Bind 把它钉住；后续轮次走
//	ResolveForModel 快路径，只要该号 healthy 就直接放行——**绕过 pick 的成本分层
//	硬过滤**。免费号（tier 0）明明可用，却因为会话粘性而被持续跳过。
//
// 同文件的 UrgentUIDsForModelRealms（紧急到期路径）走 preferredCandidatesLocked、
// 自带免费优先，两处口径不一致——本测试锁定「可用集合也必须按成本分层过滤免费层」。
package pool

import (
	"reflect"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestAvailableUIDsTierAware 同一模型名国内收费、国际免费时，粘性可用集合必须
// 只列 tier 0 的国际免费号——否则会话粘性会绕过 pick 的成本分层，把请求钉死在
// 收费的国内号上（实案：国际免费号账本 1480 样本实测免费，国内号却承接 77% 流量）。
func TestAvailableUIDsTierAware(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetModelRateOf(rates(map[string]map[string]string{
		"cn":     {"dual-model": "2.00"}, // 国内收费 → tier 2
		"global": {"dual-model": "0.00"}, // 国际免费 → tier 0
	}))

	cn := &auth.Auth{UID: "cn-a"}
	cn.SetRealm("cn")
	gl := &auth.Auth{UID: "g-a"}
	gl.SetRealm("global")
	p.Add(cn)
	p.Add(gl)

	got := p.AvailableUIDsForModelRealms("dual-model", nil)
	// 期望：只有免费的国际号。若返回含 cn-a，则 tier 2 号进入了粘性候选池，
	// 会话一旦绑上它就再也回不到免费号——正是本案的病灶。
	for _, uid := range got {
		if uid == "cn-a" {
			t.Fatalf("粘性可用集合含 tier 2 收费号 cn-a（got %v）：会话粘性绕过了成本分层硬过滤", got)
		}
	}
	found := false
	for _, uid := range got {
		if uid == "g-a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("粘性可用集合应含免费号 g-a，got %v", got)
	}

	// 交叉验证：真正的选号路径（pick）在同口径下确实只留免费号——两处必须一致。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	if a := p.PickExcludingForRealm(nil, "dual-model", ""); a == nil || a.UID != "g-a" {
		t.Fatalf("pick 应选中免费号 g-a，got %v", a)
	}
}

// TestAvailableUIDsTierAwareFallsBackWhenNoFree 免费层整体不可用时，粘性可用集合
// 必须回落到「最优可用层」——这与 pick 的 bestTier 语义一致（只保留最优层，不是
// 只保留免费层）。否则免费号全部冷却时粘性会拿不到任何候选（会话彻底无号可用）。
func TestAvailableUIDsTierAwareFallsBackWhenNoFree(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetModelRateOf(rates(map[string]map[string]string{
		"cn":     {"dual-model": "2.00"},
		"global": {"dual-model": "0.00"},
	}))

	cn := &auth.Auth{UID: "cn-a"}
	cn.SetRealm("cn")
	gl := &auth.Auth{UID: "g-a"}
	gl.SetRealm("global")
	p.Add(cn)
	p.Add(gl)

	// 让免费的国际号在**该模型上**不可用（6004 模型级冷却）。
	p.mu.Lock()
	p.byUID["g-a"].modelCooldowns = map[string]modelCooldown{
		"dual-model": {Until: time.Now().Add(time.Hour)},
	}
	p.mu.Unlock()

	got := p.AvailableUIDsForModelRealms("dual-model", nil)
	if len(got) != 1 || got[0] != "cn-a" {
		t.Fatalf("免费层不可用时应回落到收费层 cn-a，got %v", got)
	}
}

// TestAvailableUIDsTierAwareHashSpreads 免费层有多个号时，粘性可用集合应把它们
// **全部**留下（供 session 哈希打散），而不是只留一个——只留一个会让所有新会话
// 钉死在同一免费号上。同时确认 tier 2 收费号一个都不在集合里。
func TestAvailableUIDsTierAwareHashSpreads(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetModelRateOf(rates(map[string]map[string]string{
		"cn":     {"dual-model": "2.00"},
		"global": {"dual-model": "0.00"},
	}))
	for _, uid := range []string{"cn-a", "cn-b"} {
		a := &auth.Auth{UID: uid}
		a.SetRealm("cn")
		p.Add(a)
	}
	for _, uid := range []string{"g-a", "g-b", "g-c"} {
		a := &auth.Auth{UID: uid}
		a.SetRealm("global")
		p.Add(a)
	}
	got := p.AvailableUIDsForModelRealms("dual-model", nil)
	want := []string{"g-a", "g-b", "g-c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("粘性可用集合应只剩三个免费国际号（哈希打散），got %v", got)
	}
}

// TestAvailableUIDsTierAwarePinnedRealm 钉域请求（cn:model / 单域别名）的最优层
// 必须在**域内**取：国际号在该模型上免费时，全局最优层是 0，但 CN 候选全是 tier 2
// ——若拿全局最优层去比，CN 结果集为空、粘性彻底失效（本该回落到 CN 收费层）。
// 这是「求 bestTier 的域集合必须与结果集一致」的回归。
func TestAvailableUIDsTierAwarePinnedRealm(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetModelRateOf(rates(map[string]map[string]string{
		"cn":     {"dual-model": "2.00"}, // 国内收费
		"global": {"dual-model": "0.00"}, // 国际免费
	}))
	cn := &auth.Auth{UID: "cn-a"}
	cn.SetRealm("cn")
	gl := &auth.Auth{UID: "g-a"}
	gl.SetRealm("global")
	p.Add(cn)
	p.Add(gl)

	// 钉 CN：必须回落到 CN 收费号，不能因为国际免费而返回空集。
	got := p.AvailableUIDsForModelRealms("dual-model", RealmSet{"cn": true})
	if !reflect.DeepEqual(got, []string{"cn-a"}) {
		t.Fatalf("钉 CN 应回落到收费的 cn-a（域内最优层），got %v", got)
	}
	// 钉 global：免费号照常。
	if got := p.AvailableUIDsForModelRealms("dual-model", RealmSet{"global": true}); !reflect.DeepEqual(got, []string{"g-a"}) {
		t.Fatalf("钉 global 应为 g-a，got %v", got)
	}
}
