package main

import (
	"workbuddy2api/internal/aliases"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）或命中别名表：
// 必须经 server.ResolveRoute 一次性得出「对外名 + 候选域集合」，再交给分池选号域过滤
// ——否则显式钉域的请求会被粘性分配到另一域账号（跨 realm 泄漏），单域别名也会被
// 分配到根本没有该模型的域（选中即 11102）。
//
// 与 handler 共用同一个 ResolveRoute：两处口径必须完全一致，否则会出现"选号不会选的域
// 被粘性钉住"——粘性命中校验走本闭包，选号走 handler，任何分歧都表现为会话被钉在
// 一个永远不会被选中的账号上反复失败。
//
// 裸模型名（无前缀）经统一调度改造后 Realm==RealmUnified（空串），
// CandidateRealms() 为 nil → AvailableUIDsForModelRealms(bare, nil) 即**全池**
// （CN + global 共用粘性池），与 pool 层空集谓词的既有"不限域"语义天然一致。
// pool.unified_routing=false（逃生门）时裸名回落 "cn"，闭包同样零特判。
func realmAwareAvailableForModel(p *pool.Pool, store *aliases.Store) func(model string) []string {
	return func(model string) []string {
		rt, err := server.ResolveRoute(store, model)
		if err != nil {
			// 单域别名被钉到不存在的域：无任何账号可用（handler 对同一请求直接 400）。
			return nil
		}
		return p.AvailableUIDsForModelRealms(rt.Name, rt.CandidateRealms())
	}
}

// realmAwareUrgentForModel 构造会话粘性路由「紧急到期优先」候选集的 realm 感知闭包。
//
// 与 realmAwareAvailableForModel 同构（同样的路由裁决 + 分池过滤），差别只在数据来源：
// 走 pool.UrgentUIDsForModelRealms——仅当该域集合/模型下存在 72 小时内到期的可用号时
// 返回非空子集，否则 nil。session 路由只在**新建/失效重绑**时用它缩小哈希候选集
// （已有有效绑定走 fast path，不受影响）。
// 裸名（RealmUnified）→ 全池口径：跨域的临期号一起参与"紧急优先"竞争。
func realmAwareUrgentForModel(p *pool.Pool, store *aliases.Store) func(model string) []string {
	return func(model string) []string {
		rt, err := server.ResolveRoute(store, model)
		if err != nil {
			return nil
		}
		return p.UrgentUIDsForModelRealms(rt.Name, rt.CandidateRealms())
	}
}
