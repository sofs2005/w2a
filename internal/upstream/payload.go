// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
//  3. image_url 归一化（上游只认 OpenAI 对象形态，字符串会 400 code=11101）
//  4. 工具 schema pattern 的 `\_` 非标转义归一（上游严格文法校验会 400 code=11129）
package upstream

import (
	"encoding/json"
	"log"
	"math"
	"strings"
)

// PrepareBodyOpt 单 pass 改写；sanitize=false 时行为完全还原（仅强制 stream + 归一化 tool_choice）。
// DeptestOnly: 仅测试引用（upstream 各 _test + server 稳定性回归）；生产经
// prepareBody 走 PrepareBodyOptWithEffortsAndDefault。跨包测试引用，
// 迁 export_test.go 不可行。保留作三层封装的最底层语义锚点。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, nil, nil)
}

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级 reasoning_effort：
// 仅当请求显式携带且模型不支持该档位时，改为 ≤请求档位的最高支持档；支持档全部高于请求档时取最低档；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不降级）。
//
// DeptestOnly: 仅测试引用（upstream stability/thinking/cache_key/sse 族 +
// server 稳定性回归）；生产经 prepareBody 走 PrepareBodyOptWithEffortsAndDefault。
// 跨包测试引用，迁 export_test.go 不可行。保留作无默认档的降级管线锚点。
//
// 向后兼容封装：不传 defaultEfforts（无模型声明默认档），thinking.go 回退硬编码 high。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, efforts, nil)
}

// PrepareBodyOptWithEffortsAndDefault 在 PrepareBodyOptWithEfforts 基础上按模型
// reasoning.defaultEffort 补默认档（缺显式 effort 时优先用模型声明档，空串/未知回退硬编码）。
func PrepareBodyOptWithEffortsAndDefault(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	// max_completion_tokens → max_tokens 翻译（吸收 PR #116，Closes #117）：
	// OpenAI 规范里 max_tokens 已 deprecated、max_completion_tokens 是新字段
	// （o-series 起引入）；DeepSeek Harness 等新客户端只发别名。WorkBuddy 上游
	//（CN /v2 与 global /console 同源，见 context_catalog「两区是同一套 API 的两次
	// 部署」实测结论）只认 max_tokens——别名透传会被上游忽略后回落默认输出上限
	//（实测 32000），长流任务被截。
	//   - 显式 max_tokens 存在 → 原样保留（显式优先，别名只删不译）；
	//   - 别名值为 0/null/负数/非数值 → 不翻译（0/null 语义是「未设置」，走上游
	//     默认；负数是非法值，翻译等于把垃圾搬进 max_tokens）；
	//   - 翻译后删别名字段（上游 Go struct 未知字段宽松，但留着徒增 body 体积与
	//     排障噪音）。
	translateMaxCompletionTokens(obj)
	// 必须紧随翻译之后：别名 1 → 已译成 max_tokens=1，此处才能一并抬到下限。
	clampMinMaxTokens(obj)
	// stream_options 仅当 body 未显式带时补 {include_usage: true}（D7）：
	// 官方 CLI 流式必发该字段，上游据此在末帧返回 usage 用量；显式带则不覆盖。
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeToolChoice(obj)
	normalizeToolPatterns(obj)
	// Kimi 系模型对「只有 enum、没有 type」的工具属性做严格校验（上游 11133）：
	// 在出站前补 type，属于「让请求通过」的安全网，与 normalizeToolPatterns 同口径。
	normalizeToolEnumTypes(obj)
	normalizeRoles(obj)
	normalizeImageURL(obj)
	// tool 配对三步（见 tool_pairing.go）：先合并再重排再清理。所有模型一律执行（独立于
	// deepseek-only 的 sanitize 开关）。这是「让请求通过」的安全网——不完整配对的
	// tool_calls/tool 结果会让上游对之后每条消息都返 400，必须先行剔除；
	// 插在结果中间的非 tool 消息（Codex image_resize_notice）同样判配对断裂，
	// 先 repack 挪后，再 cleanup 删孤儿，两侧同口径。
	//
	// 顺序不能换：mergeAdjacentToolCalls 必须最先跑——它把「背靠背的两条
	// assistant.tool_calls」合成一条（部分 agent 客户端回放并行调用的报文形状），是上游
	// deepseek 系模型 11148 的正面修复；先合并再 repack，repack 才看得到完整的一批调用。
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = mergeAdjacentToolCalls(msgs)
		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		// 无改动时两步都返回原 slice，这里回写等于零操作；任一步重排/删除
		// （哪怕后续步骤零改动）也必须落到 obj——不能只在「最后一步改动」时回写，
		// 否则 repack 单独生效的结果会被原 slice 覆盖丢失。
		obj["messages"] = msgs
	}
	// DeepSeek 思维链开关（见 thinking.go）：注入 thinking.type=enabled + 缺档补默认档。
	// 先于 normalizeReasoningEffort 执行：补入的默认档也要走既有降级管线，
	// 模型不支持默认档时自动落到 ≤ 默认档的最高支持档（不出站不合规档位）。
	model, _ := obj["model"].(string)
	injectThinking(obj, lookupDefaultEffort(defaultEfforts, model))
	normalizeReasoningEffort(obj, efforts)
	// DeepSeek 多轮一致性：assistant 消息带 reasoning 痕迹时回填 reasoning_content
	// （requiresReasoningContentOnAssistantMessages，见 thinking.go）。
	backfillReasoningContent(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为上游
// 认的 max_tokens（吸收 PR #116）。调用点在 PrepareBodyOptWithEffortsAndDefault
// 管线 stream 强制之后（同一预处理管线挂载，任务书 prompt-too-long §3）。
// 规则：显式 max_tokens 优先（别名只删）；别名非正数值（0/null/负数）不翻译；
// 非数值别名（字符串等畸形）不翻译（原样透传由上游报 11101 参数错）。
// 两域同口径：CN /v2 与 global /console 是同一套 API（见 context_catalog 文件头
// 实测结论），翻译不分 realm——global 域上游同样只认 max_tokens。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens") // 无论翻译与否，别名一律删（见上方注释）
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return // 显式 max_tokens 优先：别名只删不译
	}
	// json.Unmarshal 数字 → float64（整数去整后回写，避免 1.28e5 科学计数法/小数尾
	// 巴进上游 body）；其他数值类型防御性兼容（int 家族——手构造 map 的调用方）。
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
}

// gptDefaultMinMaxTokens GPT 系模型在**无本仓库实测下限**时的保守抬升下限。
//
// 为什么是 16：fork（linguo2625469）对 gpt-6-sol / gpt-6-luna / gpt-5.6-sol 三个
// 型号实测——max_tokens 15 → 400 code=11133 model_param_invalid，16 → 200。该下限
// 已被同源分叉验证过，作为「有实测」型号之外 GPT 系的兜底值是唯一有证据支撑的选择。
const gptDefaultMinMaxTokens = 16

// gptMinMaxTokensByModel 逐模型 max_tokens 下限（本仓库实测优先，覆盖兜底值）。
//
// gpt-5.4 是**本仓库自己的实测**（commit c6e849f，真实账号锚定）：要求 ≥100，
// 否则 400 code=11133 integer_below_min_value。它与 fork 那三个型号不是同一档——
// 15/16 的边界在 gpt-5.4 上仍是失败区间（16..99 全被拒）。若照搬 fork 的一刀切 16，
// gpt-5.4 依旧切不过去，正是这次要修的症状。两处实测不冲突，是**不同型号不同下限**。
//
// 已知上限侧无需处理：本表最大值 100 远低于任一 gpt 型号的 maxOutput（128000）。
var gptMinMaxTokensByModel = map[string]int64{
	"gpt-5.4": 100,
}

// bareModelName 剥掉 realm 前缀（cn:/global:）并归一大小写与空白，只留模型名本体。
//
// 剥前缀是必须的，且**必须在查下限表之前**：出站 body 是裸名，但错误路径可能带
// "global:gpt-5.4" 进来；若按原串查表，gpt-5.4 会漏掉自己的 100 下限、错落 16 兜底。
func bareModelName(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(m, ':'); i >= 0 {
		m = m[i+1:]
	}
	return m
}

// isGPTFamily 判断模型是否属 GPT 系（下限规则只作用于它）。
func isGPTFamily(model string) bool {
	return strings.HasPrefix(bareModelName(model), "gpt-")
}

// minMaxTokensFor GPT 系模型接受的 max_tokens 下限；非 GPT 系返回 0（不钳）。
func minMaxTokensFor(model string) int64 {
	bare := bareModelName(model)
	if !strings.HasPrefix(bare, "gpt-") {
		return 0
	}
	if m, ok := gptMinMaxTokensByModel[bare]; ok {
		return m
	}
	return gptDefaultMinMaxTokens
}

// clampMinMaxTokens 把 GPT 系过小的 max_tokens 抬到该模型的下限。
//
// 背景：上游 GPT 系对过小 max_tokens 一律 400 code=11133 model_param_invalid
// （详见两个下限常量上的实测注释）。Claude Code 等客户端用 /model 切换时会先发
// max_tokens 极小的探针请求，全号轮转同样被拒——11133 在本仓库落 Classify 的
// ErrClient 兜底（只换号不避让），于是客户端拿到 503，模型永远切不过去。
// 账号与 body 其余部分无关，换号无用，只能在发送前修。
//
// 抬升只放宽输出上限、不改语义：上游返回 ≤max_tokens 个 token 都是合法答复，
// 客户端自行按需截断；而未修的话请求直接失败。
//
// **只钳正数**——与 translateMaxCompletionTokens 的 0/null/负数口径保持一致：
//   - 0 / null：语义是「未设置」，上游回落到默认输出上限（实测 32000）。钳到 16
//     等于把默认上限砍成 16，把能正常工作的请求改坏（比不修更糟）；
//   - 负数：非法值，上游应报 11101 参数错。伪造出一个合法值会把「客户端发了垃圾」
//     这个真实问题藏起来；
//   - 非数值（字符串等畸形）：同样原样透传，由上游报错，不编造。
//
// 调用点紧跟在 translateMaxCompletionTokens 之后：别名 max_completion_tokens=1
// 先被译成 max_tokens=1，此处才能一并抬升（顺序颠倒会漏掉别名形态）。
func clampMinMaxTokens(obj map[string]any) {
	model, _ := obj["model"].(string)
	floor := minMaxTokensFor(model)
	if floor <= 0 {
		return
	}
	var v int64
	switch n := obj["max_tokens"].(type) {
	case float64:
		v = int64(n)
	case int64:
		v = n
	case int:
		v = int64(n)
	default:
		return // 字段不存在 / null / 非数值：不钳（见上方口径）
	}
	if v > 0 && v < floor {
		obj["max_tokens"] = floor
		log.Printf("WARN: [upstream] max_tokens floored model=%s %d -> %d", model, v, floor)
	}
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("WARN: [upstream] reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("WARN: [upstream] reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化是「协议兼容」（补上游 role 白名单），不是「内容脱敏」，
// 因此有意与 SanitizeFingerprints / sanitize 参数解耦：即使 sanitize=false 也照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("[upstream] role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeImageURL 兼容 OpenAI chat 多模态内容的两种 image_url 写法。
//
// OpenAI Chat Completions 规范使用对象形态 {"url":"...","detail":"..."}，
// 部分客户端（以及 Responses -> Chat 转换器）会发送字符串形态 "data:..." 或
// "https://..."。WorkBuddy 上游只接受对象形态，字符串会返回 400 code=11101
// "cannot unmarshal string into ... ImageContent"。
//
// 这里只做形状转换：字符串转 {"url": 原值}；已有对象及其中 url/detail/mime_type
// 原样保留；空字符串、缺失值、对象内非法 url 一律不补默认值，让上游返回真实错误。
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				continue
			}
			part["image_url"] = map[string]any{"url": imageURL}
		}
	}
}

// ensureConsoleSystem global realm 兜底 system 注入（吸收 PR #45）。
//
// ⚠️ 当前**不在生产链路上**（prepareBody 已不再调用），且**禁止重新接回**：
// 它补的 "You are a helpful assistant." 在官方全量分发包（D:\PG\workbuddyAI 的
// main/server.js + cli/dist/codebuddy-lite-wb.mjs）里 grep 计数为 0，是「我们发了、
// 官方从没发过」的自造形态 —— 那是 11-128 渠道校验（displayTips：这次调用没通过
// 渠道校验）最直接的特征之一。
//
// 与现行兜底的区别：现在由 prompt.EnsureLeadingSystem（internal/prompt，在
// prepareBody 收口处调用）在「首条不是 system」时补一条官方 product.json 的
// base-agent-instructions 模板原文（"You are CodeBuddy Code.\n"）。上游对部分账号
// 灰度启用的形态校验要求 messages[0] 必须是 system（11-128「first message is not
// system prompt」），而官方客户端每次请求都带开头 system —— 补官方字节是补齐形态，
// 补自造句子才是加指纹。保留本函数体仅为记录历史修复与便于日后取证对照。
//
// 原始行为：首条消息非 system 时在 messages 最前补一条 fallback system。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不在这里二次错误化）。
func ensureConsoleSystem(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	first, ok := msgs[0].(map[string]any)
	if ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return body // 首条已是 system：不注入
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, msgs...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeToolPatterns 归一化 tools 子树里 pattern 的非标准转义 `\_`（→ `_`）。
//
// 上游对 tools[].function.parameters 做严格 JSON Schema/正则文法校验，pattern 含
// `\_`（转义的字面量下划线）会整体拒收：400 code=11129 invalid_function_call_
// parameters（displayMsg「工具定义不合规」）。`\_` 不是任何正则文法的合法转义，
// 但所有主流引擎（RE2/PCRE/JS Annex B）都宽容地视为 `_` 本身——上游校验器比它们
// 全部更严（对照 V8 严格文法 u 标志，唯一同样拒绝的实现）。实案：ZCode 的 exa 插件
// agent_run 工具 runId/previousRunId 带 `^agent\_run\_`，deepseek 系全家确定性 400
// → 网关侧归 ErrClient 只换号不罚但喂连败计数 → 轮转烧满 5 连败触发连败降权、
// 客户端 503（2026-09-29/30 两次实案）。schema 级拒绝换账号无用，只能在发送前修。
//
// 归一无损：`\_` 与 `_` 在所有引擎匹配语义相同（各引擎实测 + 上游对照探针：归一后
// 200），工具方功能不变。只动 tools 子树（pattern 值 + patternProperties 键）；
// 消息正文里的 `\_`（如 Windows 路径 C:\_x）不碰。其余非标转义（`\:` 等）未证实
// 触发，不扩面——有实案再议。独立于 sanitize 开关：这是「让请求通过」，不是脱敏。
func normalizeToolPatterns(obj map[string]any) {
	rawTools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI 形态 tools[].function.parameters；裸 tools[].parameters 兼容。
		if fn, ok := tool["function"].(map[string]any); ok {
			unescapePatternLiteralEscapes(fn["parameters"])
		}
		unescapePatternLiteralEscapes(tool["parameters"])
	}
}

// unescapePatternLiteralEscapes 递归改写 schema 树里 pattern 值与 patternProperties
// 键中的 `\_` → `_`（patternProperties 的键也是正则；map 键不可原地改，命中时重建
// 该层）。
func unescapePatternLiteralEscapes(node any) {
	switch n := node.(type) {
	case map[string]any:
		if p, ok := n["pattern"].(string); ok && strings.Contains(p, `\_`) {
			n["pattern"] = strings.ReplaceAll(p, `\_`, `_`)
		}
		if props, ok := n["patternProperties"].(map[string]any); ok {
			rebuilt := false
			fixed := make(map[string]any, len(props))
			for k, v := range props {
				if strings.Contains(k, `\_`) {
					k = strings.ReplaceAll(k, `\_`, `_`)
					rebuilt = true
				}
				fixed[k] = v
			}
			if rebuilt {
				n["patternProperties"] = fixed
			}
		}
		for _, v := range n {
			unescapePatternLiteralEscapes(v)
		}
	case []any:
		for _, v := range n {
			unescapePatternLiteralEscapes(v)
		}
	}
}

// normalizeToolEnumTypes 给「只有 enum、没有 type」的工具属性补上推断出的 type。
//
// 背景（上游实测 2026-10-24）：Kimi 系模型（k3.1-pro / k3-1 / k2.5）对
// tools[].function.parameters 做严格 schema 校验 —— 属性只写 {"enum":[...]} 而不写
// "type" 时整单 400：
//
//	{"code":11133,"msg":"Invalid request parameters",
//	 "extError":{"code":"400001","message":"the request parameters were rejected by the model provider"}}
//
// 同一份 schema 在 DeepSeek / GLM / MiniMax 上照常 200，所以这是「按模型严格程度
// 而异」的请求侧问题：不补 type，Kimi 用户等于该模型全废（每条请求都撞同一个 400）。
//
// 推断规则（只增补、不改 enum 取值，语义不变）：
//   - 全部字符串 → string
//   - 全部整数 → integer；整数与小数混合 → number
//   - 全部布尔 → boolean
//   - 混合标量/含 null → 不改写（无法无损推断），交由上游原样报错
//
// 作用域仅 tools/functions 的 parameters（与 normalizeToolPatterns 同口径），
// 不动 response_format 等其它位置的 schema。
func normalizeToolEnumTypes(obj map[string]any) {
	for _, key := range []string{"tools", "functions"} {
		list, ok := obj[key].([]any)
		if !ok {
			continue
		}
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// 现行形态把 parameters 包在 function 里；旧式形态直接挂在条目上。
			if fn, ok := m["function"].(map[string]any); ok {
				inferEnumTypes(fn["parameters"])
			}
			inferEnumTypes(m["parameters"])
		}
	}
}

// inferEnumTypes 递归处理 schema 树：补 type 并按常见关键字下钻。
func inferEnumTypes(node any) {
	switch n := node.(type) {
	case map[string]any:
		if enum, ok := n["enum"].([]any); ok {
			if _, hasType := n["type"]; !hasType {
				if t := enumScalarType(enum); t != "" {
					n["type"] = t
				}
			}
		}
		for k, v := range n {
			switch k {
			case "properties", "patternProperties", "definitions", "$defs":
				if props, ok := v.(map[string]any); ok {
					for _, pv := range props {
						inferEnumTypes(pv)
					}
				}
			case "items", "additionalProperties", "not", "contains":
				inferEnumTypes(v)
			case "anyOf", "oneOf", "allOf", "prefixItems":
				if list, ok := v.([]any); ok {
					for _, x := range list {
						inferEnumTypes(x)
					}
				}
			}
		}
	case []any:
		for _, v := range n {
			inferEnumTypes(v)
		}
	}
}

// enumScalarType 由 enum 取值推断 JSON Schema type；无法无损推断时返回空串。
func enumScalarType(vals []any) string {
	if len(vals) == 0 {
		return ""
	}
	typ := ""
	for _, v := range vals {
		var cur string
		switch x := v.(type) {
		case string:
			cur = "string"
		case bool:
			cur = "boolean"
		case float64:
			if math.Trunc(x) == x {
				cur = "integer"
			} else {
				cur = "number"
			}
		default:
			return ""
		}
		switch {
		case typ == "":
			typ = cur
		case typ == cur:
			// 同型，继续
		case (typ == "integer" && cur == "number") || (typ == "number" && cur == "integer"):
			typ = "number"
		default:
			return ""
		}
	}
	return typ
}
