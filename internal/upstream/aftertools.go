// aftertools.go 11153「对话必须以 user 消息结尾」的请求级修复。
//
// 背景：WorkBuddy AI 的 gemini 系模型对「工具结果之后直接跟用户消息」的历史返回
// 400 code=11153（社区 2026-10-14 实测）。这类历史很常见：客户端在工具执行后补一条
// 用户提醒、工具图片，或用户在任务中途插话。上游其实接受同一段历史——只要工具结果
// 与用户消息之间隔一条空的 assistant 消息（看起来像「模型这一轮没说话」）。
//
// 因此这里在工具结果与尾部 user 消息之间插一条 {role:assistant, content:""}。
// 只对「结尾是 tool 结果 + 若干 user 消息」的形态生效；以 assistant 结尾（prefill）
// 的请求不碰（上游照旧拒绝，那是客户端自己的形态）。
package upstream

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// afterToolsRefused 匹配上游 11153 的两种表述：文案或业务码。
// 用 \b 收口，避免 111530 这类更长数字误命中（RE2 无环视）。
var afterToolsRefused = regexp.MustCompile(`(?i)must end with a user message|["']code["']\s*:\s*["']?11153\b`)

// IsAfterToolsRefused 报告响应体是否是 11153。
func IsAfterToolsRefused(body string) bool {
	return afterToolsRefused.MatchString(body)
}

// ReplyAfterTools 在工具结果与尾部 user 消息之间插入空 assistant。
// 返回 (新 body, 是否改动)；形态不符 / 非 JSON → (nil, false)。
func ReplyAfterTools(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return nil, false
	}
	i := len(msgs) - 1
	for i >= 0 {
		m, _ := msgs[i].(map[string]any)
		if role, _ := m["role"].(string); !strings.EqualFold(strings.TrimSpace(role), "user") {
			break
		}
		i--
	}
	if i == len(msgs)-1 || i < 0 {
		return nil, false // 结尾不是 user / 全是 user：不属于该形态
	}
	last, _ := msgs[i].(map[string]any)
	if role, _ := last["role"].(string); !strings.EqualFold(strings.TrimSpace(role), "tool") {
		return nil, false // 工具结果之后不是直接跟 user
	}
	insert := map[string]any{"role": "assistant", "content": ""}
	out := make([]any, 0, len(msgs)+1)
	out = append(out, msgs[:i+1]...)
	out = append(out, insert)
	out = append(out, msgs[i+1:]...)
	obj["messages"] = out
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// afterToolsModels 已确认需要空 assistant 的模型（进程级，重启即清）。
// 命中后后续请求直接按同一形态修好，不必每条都先吃一次 400。
var afterToolsModels sync.Map

// RememberAfterTools 记住模型需要该修复。
func RememberAfterTools(model string) {
	if model = strings.TrimSpace(model); model != "" {
		afterToolsModels.Store(model, struct{}{})
	}
}

// AfterToolsRemembered 报告模型是否已记住。
func AfterToolsRemembered(model string) bool {
	_, ok := afterToolsModels.Load(strings.TrimSpace(model))
	return ok
}

// ReplyAfterToolsIfKnown 对已记住的模型直接施加修复（形态不符时原样返回）。
func ReplyAfterToolsIfKnown(model string, body []byte) []byte {
	if !AfterToolsRemembered(model) {
		return body
	}
	if out, ok := ReplyAfterTools(body); ok {
		return out
	}
	return body
}

// ResetAfterToolsMemory 清空记忆（测试用）。
func ResetAfterToolsMemory() {
	afterToolsModels.Range(func(k, _ any) bool {
		afterToolsModels.Delete(k)
		return true
	})
}
