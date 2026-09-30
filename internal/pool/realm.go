// 分池选号域：按 realm（cn/global）过滤选号与可用集合。空集退化为现状（全池）。
package pool

import (
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// RealmSet 选号域过滤集合（统一调度改造）：
//
//	nil / 空集        → 全池（不限域，既有 realm=="" 语义，零改动）
//	{"cn"}            → 钉 CN（cn: 前缀；纯 CN 部署的裸名旧语义）
//	{"global"}        → 钉 global（global: 前缀）
//	{"cn","global"}   → 两域并集（语义等价全池；别名双域条目用）
//
// 为什么是集合而不是单值：**单域别名条目**（"隐藏模型只在一域存在"）必须把另一域
// 账号排除出候选——否则那批账号被选中只会吃一次 11102 再换号，白费一轮上游往返。
// 单值谓词表达不了"排除某一域"这个需求，集合可以，且空集天然退化为全池。
//
// 注意 Key() 的取值：两域并集**不**归一化为空集（那会让别名单域与全池共用同一个
// 成本探索 timer）。并集与全池在候选集上等价，但键不同只是多一个 timer 条目，
// 无正确性影响；归一化反而要求每次比较都做集合相等判断，不值。
type RealmSet map[string]bool

// SingleRealm 单域集合构造（"" → nil = 全池，与既有 realm=="" 退化语义严格一致）。
func SingleRealm(realm string) RealmSet {
	if realm == "" {
		return nil
	}
	return RealmSet{realm: true}
}

// Allows 报告某账号 realm 是否在允许集合内（空集恒允许 = 全池）。
func (s RealmSet) Allows(realm string) bool {
	if len(s) == 0 {
		return true
	}
	return s[realm]
}

// Key 稳定域键（成本探索 timer 用）：全池 ""（与既有键零漂移）、单域即该域本身、
// 多域排序后以 "+" 连接（map 遍历无序，必须排序才能跨调用稳定，否则同一集合
// 会散成多个 timer 条目、探索节奏被打乱）。
func (s RealmSet) Key() string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		for k := range s {
			return k
		}
	}
	ks := make([]string, 0, len(s))
	for k := range s {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, "+")
}

// PickExcludingForRealm 按 realm 过滤的轮换选号：候选仅限 Realm()==realm 的账号。
// realm=="" 退化为 PickExcluding（现状语义，老调用零改动）。
// 可选做请求级轮换（tried）与模型感知（reqModel，6004 模型豁免照常生效）；
// reqModel 非空时健康口径换成 healthyForModel。realm 不匹配的全冷却兜底同样排除。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pickInRealms(tried, reqModel, SingleRealm(realm))
}

// PickExcludingForRealms 是 PickExcludingForRealm 的集合形态（统一调度改造）：
// realms 为 nil/空 = 全池（等价 realm==""）；非空 = 仅集合内域参与（含全冷却兜底）。
// 供别名单域条目排除另一域账号用（见 RealmSet 注释）。
func (p *Pool) PickExcludingForRealms(tried map[string]bool, reqModel string, realms RealmSet) *auth.Auth {
	return p.pickInRealms(tried, reqModel, realms)
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// DeptestOnly: 仅 realm_test.go 引用；生产经 wiring.go 走
// AvailableUIDsForModelRealm。保留作 ForModelRealm 的模型维度退化
// （model=""）语义锚点测试。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	return p.availableUIDsLocked(SingleRealm(realm),
		func(e *entry, now time.Time) bool { return e.healthy(now) })
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	return p.availableUIDsLocked(SingleRealm(realm),
		func(e *entry, now time.Time) bool { return e.healthyForModel(now, model) })
}

// AvailableUIDsForModelRealms 是 AvailableUIDsForModelRealm 的集合形态：realms 为
// nil/空 = 全池（等价 realm==""），非空 = 仅集合内域的账号（别名单域条目的可用集）。
// 供会话粘性路由按（对外名, 允许域集合）取可用账号（见 cmd/server/wiring.go）。
func (p *Pool) AvailableUIDsForModelRealms(model string, realms RealmSet) []string {
	return p.availableUIDsLocked(realms,
		func(e *entry, now time.Time) bool { return e.healthyForModel(now, model) })
}

// UrgentUIDsForModelRealm 返回「紧急到期优先」的可用 UID 子集（issue:积分过期 的
// 粘性初次分配侧）：仅当池里存在 72 小时内到期的可用候选时非空，否则返回 nil。
//
// 语义与选号侧共用同一实现（credits.preferredCandidatesLocked）：已实测免费层可整体
// 优先于临期非免费号；无免费层时返回全部紧急候选（**不做"只留最早那一个"的截断**——
// 粘性侧要在这批里哈希打散，只留一个会让所有新会话钉死在同一账号）。
//
// 调用方（session 路由）只在**新建或失效重绑**时用它缩小哈希候选集；已有有效绑定
// 走原 fast path，不因积分到期被切号（用户要求保留粘性）。
// 返回 nil 表示"无紧急候选，调用方保持原有分配逻辑"。
func (p *Pool) UrgentUIDsForModelRealm(model, realm string) []string {
	return p.UrgentUIDsForModelRealms(model, SingleRealm(realm))
}

// UrgentUIDsForModelRealms 是 UrgentUIDsForModelRealm 的集合形态（realms 为
// nil/空 = 全池）。语义与判据见 UrgentUIDsForModelRealm。
func (p *Pool) UrgentUIDsForModelRealms(model string, realms RealmSet) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	cands := make([]*entry, 0, len(p.byUID))
	for _, e := range p.byUID {
		if !realms.Allows(e.a.Realm()) {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		cands = append(cands, e)
	}
	pref := p.preferredCandidatesLocked(cands, model, now)
	if len(pref) == 0 {
		return nil
	}
	uids := make([]string, 0, len(pref))
	for _, e := range pref {
		uids = append(uids, e.a.UID)
	}
	sort.Strings(uids) // 稳定输出（与 availableUIDsLocked 同口径）
	return uids
}
