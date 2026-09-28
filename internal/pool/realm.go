// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"sort"
	"time"

	"workbuddy2api/internal/auth"
)

// PickExcludingForRealm 按 realm 过滤的轮换选号：候选仅限 Realm()==realm 的账号。
// realm=="" 退化为 PickExcluding（现状语义，老调用零改动）。
// 可选做请求级轮换（tried）与模型感知（reqModel，6004 模型豁免照常生效）；
// reqModel 非空时健康口径换成 healthyForModel。realm 不匹配的全冷却兜底同样排除。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm)
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// DeptestOnly: 仅 realm_test.go 引用；生产经 wiring.go 走
// AvailableUIDsForModelRealm。保留作 ForModelRealm 的模型维度退化
// （model=""）语义锚点测试。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	return p.availableUIDsLocked(realm, func(e *entry, now time.Time) bool { return e.healthy(now) })
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	return p.availableUIDsLocked(realm,
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
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	cands := make([]*entry, 0, len(p.byUID))
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
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
