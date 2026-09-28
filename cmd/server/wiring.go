package main

import (
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
//
// realm 为空串时 pool.AvailableUIDsForModelRealm 退化为现状（AvailableUIDsForModel），
// 老调用（无前缀模型名）语义零改动。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.AvailableUIDsForModelRealm(bare, realm)
	}
}

// realmAwareUrgentForModel 构造会话粘性路由「紧急到期优先」候选集的 realm 感知闭包。
//
// 与 realmAwareAvailableForModel 同构（同样的前缀剥离 + 分池过滤），差别只在数据来源：
// 走 pool.UrgentUIDsForModelRealm——仅当该 realm/模型下存在 72 小时内到期的可用号时
// 返回非空子集，否则 nil。session 路由只在**新建/失效重绑**时用它缩小哈希候选集
// （已有有效绑定走 fast path，不受影响）。
func realmAwareUrgentForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.UrgentUIDsForModelRealm(bare, realm)
	}
}