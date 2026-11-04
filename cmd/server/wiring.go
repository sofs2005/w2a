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
// 走 pool.UrgentUIDsForModelRealms——仅当该域集合/模型下存在紧急窗口（24h / 72h
// 两层）内到期的可用号时返回非空子集，否则 nil。session 路由只在**新建/失效重绑**
// 时用它缩小哈希候选集（已有有效绑定走 fast path，不受影响）。
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

// modelRateWithAliases 给上游目录倍率查表加上「对外名 → 该域物理名」的别名解析。
//
// 为什么必须有这层：上游目录倍率表的存储键是**物理模型 ID**（internal/upstream/client.go：
// rates[mi.ID] = rate），而 pool 的成本分层（credits.go 的 costTierOf）与积分保底
// （pick.go 的 floorBlockedForModel）拿到的 model 是**对外名**。两套键空间在
// 「某域出站名与对外名不同」的条目上对不上，真实配置里就有这样一条：
//
//	{"name": "deepseek-v4.1-flash", "cn": "deepseek-v4.1-flash", "global": "deepseek-v4.1-flash-sg"}
//
// global 号用裸名去查，命中目录里裸名的牌价 "0"（实际计费走 -sg）→ 判 tier 0「免费」；
// 查不到则判 tier 1「未知」。两者都排在「已实测收费」（tier 2）之前，而成本分层是硬过滤
// 取最优层 ⇒ 国内收费号被整体滤掉、流量压到国际号。同一查表还被积分保底用来判收费，
// 误判「未知」等于放行 ⇒ 保底失效、可能打穿余额。
//
// 修复点在**注入侧**：pool 的语义（拿到什么 model 就查什么）是对的，键空间对齐属于
// "上游目录 vs 网关对外名"这一层的职责，天然落在接线处。
//
// 语义（三条分支互斥，且与引入包装前逐字兼容）：
//   - 命中别名且该域出站名非空 → 用**该域出站名**（物理名）查；
//   - 命中别名但该域出站名为空（ok==false，"该域没有这个模型"）→ **原样**查。
//     不吞成空串：空串在 catalogRateTier 里是 tier 1「未知」，比保守更松（保底会放行）；
//     而该域账号本就不会被选中（RealmSet 在选号侧已排除），原样查即"保持既有 Resolve
//     语义"的防御性兜底；
//   - 未命中别名（或空表）→ 原样查，与引入前逐字一致（无别名部署零变化）。
//
// 热加载：store 是 *aliases.Store，startAliasWatcher 原地 Set 新表，闭包捕获指针即可
// 自动跟随——不需要在 configreload.go 里再注入一次。
//
// nil 安全（接线处写错不该 panic 掉选号热路径）：store == nil 时无别名可依 → 原样转发；
// rateOf == nil 时返回空串（=「未知」，与 pool 侧 modelRateOf==nil 的退化同向）。
func modelRateWithAliases(store *aliases.Store, rateOf func(realm, model string) string) func(realm, model string) string {
	return func(realm, model string) string {
		if rateOf == nil {
			return "" // 无从查倍率：判「未知」（catalogRateTier("")==1），与 pool 侧 nil 退化同向
		}
		if store == nil {
			return rateOf(realm, model) // 无别名表：与引入包装前逐字一致
		}
		if physical, ok := store.Resolve(model, realm); ok && physical != "" {
			return rateOf(realm, physical)
		}
		return rateOf(realm, model) // 未命中别名 / 该域无此模型：原样查（不得吞成空串）
	}
}

// realmAwareWeightedPick 构造会话粘性**首绑**的加权选号闭包（issue #265）。
//
// 背景：粘性未命中时旧实现用 FNV-1a 取模在可用 UID 里选，与积分余额、快过期积分、
// 闲置时长都无关；而默认开启的粘性让带会话标识的请求首轮就被绑定并滚动续期，永远
// 走不到 Pool.pick 的三因子加权。于是「快过期积分优先消耗」（pool.expiring_soon）
// 在默认配置下基本失效——一个没有快过期积分的号可以独占整段会话流量。
//
// 候选集已由 realmAwareAvailableForModel 按 realm + 模型可用性筛过，这里只需剥掉
// realm 前缀拿裸模型名：成本台账与 6004 模型级冷却都以裸名记账。
func realmAwareWeightedPick(p *pool.Pool, store *aliases.Store) func(model string, candidates []string) string {
	return func(model string, candidates []string) string {
		// 剥掉 realm 前缀 / 解析别名拿裸模型名（与 realmAwareAvailableForModel 同一
		// 路由裁决，两处口径必须一致）；解析失败时按原样传，让 pool 的模型级冷却
		// 查询退化为「未知模型」而不是错配到别的模型上。
		bare := model
		if rt, err := server.ResolveRoute(store, model); err == nil {
			bare = rt.Name
		}
		return p.WeightedPickAmong(candidates, bare)
	}
}
