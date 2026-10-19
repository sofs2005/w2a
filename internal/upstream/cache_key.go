// cache_key.go 注入上游 prompt_cache_key 字段（P0 费用优化）。
//
// ⚠️ 当前**不在生产链路上**（prepareBody 已不再调用 InjectPromptCacheKey）：
// 官方国际版全量分发包（D:\PG\workbuddyAI 的 main/server.js + cli/dist /
// renderer 全量文件）里 prompt_cache_key 的 grep 计数为 0，且我们注入的键带
// `wb2a-` 前缀（同样计数 0）。即客户端从不在 body 里发这个字段——我们发了就是
// 「我们发了、官方从没发过」的自造形态，属 11-128 渠道校验的高危特征。
// 费用优化让位于不被风控，故该优化暂时停用；函数体保留以便日后确认安全后重启。
//
// 原始设计（停用前的逆向结论）：上游服务端支持 prompt_cache_key，
// 同一段 8k token 前缀：
//   - 不带 → prompt_cache_hit_tokens=0, credit≈0.34
//   - 带   → prompt_cache_hit_tokens=7808, credit≈0.02（费用降 ~17×）
//
// 网关曾为每个出站 chat 请求注入一个稳定、按账号隔离的 cache key。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"workbuddy2api/internal/session"
)

// InjectPromptCacheKey 在已改写的出站 body 上注入 prompt_cache_key 字段。
//
// 优先级：
//  1. body 已带 prompt_cache_key → 原值保留（客户端自知复用哪个键）
//  2. body 已带 conversation_id / conversationId → 用它做会话哈希源
//  3. 都没有 → 用入站 conversationID 参数（来自 X-Conversation-ID 头解析）
//  4. 仍为空 → 用首条 user 消息派生的会话级键（session.StickyFallbackKey）
//
// 第 4 条是 Anthropic / Claude Code 类流量的必需兜底：这类入站请求既无
// conversation_id（ResolveConversationID 刻意只认该字段，见 ids.go）也无
// X-Conversation-ID 头，前三条全部落空 → convHex 退化成「只由 uid 决定的定值」，
// 即**每账号一个恒定 cache key**。同账号下所有客户端（多个 Claude Code 会话、
// 桌面端等）于是共用一个上游前缀缓存槽，互相覆盖 → 前缀命中不稳定、常整段 miss，
// 费用降 ~17× 的优化在这个最主流的客户端上基本失效。
//
// 为什么用 StickyFallbackKey 而不是复制一份「system + 首条 user」逻辑：该函数
// 就是为「无会话标识客户端」派生会话级稳定键而写的（取首条 user 消息文本哈希——
// 会话内历史不断追加而首条恒定，故同会话恒同键、开新会话自然换键），语义完全一致。
// 它自带的抑制条件也一并继承：body 带 user_id 时返回 ""（user 维度粒度过粗，
// 会把一个 user 的全部并行对话钉到同一个缓存槽，反而加剧互相覆盖），此时回落
// 到第 3 条之后的行为（定值键）——即「判不出来就不猜」，与既有保守口径一致。
//
// 注意本函数**只影响 prompt_cache_key 取值**，不动 ChatMeta.ConversationID：
// 后者同时是出站 X-Conversation-ID 头的来源（headers.go），语义是「透传客户端
// 原值，空则不发（不伪造）」——上游后台按该头聚合对话，网关不得替客户端编造。
// 因此兜底必须落在这里，而不是把派生键回填进 ConversationID。
//
// 安全约束——按账号隔离：
//   - 生成键格式 `wb2a-<uid8>-<convHex>`
//   - uid8 是账号 UID 前 8 字符，跨账号绝不相同 → 跨账号缓存键绝不碰撞
//   - 跨账号复用同一 cache key 会让上游命中错账号的前缀缓存、泄露对方对话，故 uid 是硬隔离因子
//
// 入参 uid 为账号 UID（空则用 "-"，但仍会注入键；调用方应保证传真实 UID）。
// 入参 conversationID 为网关解析出的会话标识（body 里没有 conversation_id 时用它做哈希源）。
// 四源全空（无会话标识、无 user_id、无首条 user 文本）时 convHex 为定值（每次新会话
// 不复用前缀，但仍保留账号隔离段）。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不二次错误化）。
func InjectPromptCacheKey(body []byte, uid, conversationID string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	// 优先级 1：客户端已显式带 key → 绝不覆盖。
	if existing, ok := obj["prompt_cache_key"].(string); ok && existing != "" {
		return body
	}
	// 优先级 2：body 里的 conversation_id / conversationId 做会话哈希源。
	conv := conversationID
	if v := strField(obj, "conversation_id"); v != "" {
		conv = v
	} else if v := strField(obj, "conversationId"); v != "" {
		conv = v
	}
	// 优先级 4：仍无会话标识 → 用首条 user 消息派生（见上方注释）。
	if conv == "" {
		conv = session.StickyFallbackKey(body)
	}
	key := buildCacheKey(uid, conv)
	obj["prompt_cache_key"] = key
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// buildCacheKey 生成 `wb2a-<uid8>-<convHex>` 格式的稳定 cache key。
//
// uid8 提供账号隔离段；convHex = sha256(uid + conversationID)[:16] 的 hex 提供会话段
// （同账号同会话稳定、不同会话不同）。会话源为空时 convHex 仍由 uid 单独哈希，
// 保证跨账号绝不碰撞但同一空会话不复用（空会话 = 新会话语义）。
func buildCacheKey(uid, conversation string) string {
	uid8 := uid
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	if uid8 == "" {
		uid8 = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	convHex := hex.EncodeToString(sum[:16])
	return "wb2a-" + uid8 + "-" + convHex
}

// strField 从 map 取 string 字段，非 string 或空串返回 ""。
func strField(obj map[string]any, key string) string {
	v, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
