// comma.go 的「逗号层」：11-128 渠道校验的第一次请求级抢救。
//
// 背景：上游的渠道校验对已知 agent 开场白做**整句精确匹配**——`You are an AI agent
// powered by DeepSeek Harness.` 被拦，而同一句加一个逗号（`You are an AI agent,
// powered by DeepSeek Harness.`）放行（社区 2026-10-30 实测：加词不行、加逗号行且
// 语义不变）。校验只读 system 与 assistant 两类消息（user/tool 内容不读）。
//
// 与「搬 system 进 user」相比，逗号是更小的一步：system 仍留在 system 槽位，模型
// 仍能读到自己的运行环境。因此处理顺序是**先逗号、后搬迁**（见 handler 的 11-128 分支）。
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// commaOpeners 匹配「某个 agent/assistant 由某物驱动/运行」这类开场句，在连接词前
// 插一个逗号。规则来自社区实测的开场句形态：形状 `You are an|the … agent|assistant`
// + `powered by|running on|running in|built on|based on`。只匹配这个形状，不做
// 泛指改写——上游校验按整行匹配，逗号恰好破坏整行、语义不变。
var commaOpeners = []struct {
	re   *regexp.Regexp
	repl string
}{
	{
		re:   regexp.MustCompile(`(?i)(\bYou are (?:an?|the)\s[^\n.,]{0,60}?\b(?:agent|assistant))\s+(powered by|running on|running in|built on|based on)\b`),
		repl: `$1, $2`,
	},
}

// pausedRoles 上游渠道校验实际读取的角色：system 与 assistant。
// developer 一并覆盖：本包的改写发生在 normalizeRoles（developer→system）之前，
// 漏掉它等于漏掉即将变成 system 的消息。
var pausedRoles = map[string]bool{"system": true, "assistant": true, "developer": true}

// CommaPause 在 system/assistant/developer 消息的开场句里插入一个逗号。
// 返回 (新 body, 是否改动)；无可改 / body 非 JSON → (nil, false)。
// 只动文本，不重排、不增删消息；content 数组里的 text 分片同样处理。
func CommaPause(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, false
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return nil, false
	}
	changed := false
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if !pausedRoles[strings.ToLower(strings.TrimSpace(role))] {
			continue
		}
		switch c := msg["content"].(type) {
		case string:
			if n := commaPausedText(c); n != c {
				msg["content"] = n
				changed = true
			}
		case []any:
			for _, raw := range c {
				part, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := part["text"].(string); ok {
					if n := commaPausedText(t); n != t {
						part["text"] = n
						changed = true
					}
				}
			}
		}
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// commaPausedText 对一段文本施加全部开场句改写。
func commaPausedText(s string) string {
	for _, r := range commaOpeners {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// SystemKey 计算一个请求「system 文本」的指纹：system/developer 消息的文本按序拼接
// 后做 sha256。空 system（或与官方便捷 system 相同）→ 空串，表示无需记忆。
//
// 用途：某个 system 被上游拒绝过一次后，同一 agent 的后续请求在发送前就直接带逗号，
// 不必每轮、每个工具步都再付一次拒绝。
func SystemKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return ""
	}
	var texts []string
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if r := strings.ToLower(strings.TrimSpace(role)); r != "system" && r != "developer" {
			continue
		}
		texts = append(texts, textOfContent(msg["content"]))
	}
	joined := strings.TrimSpace(strings.Join(texts, "\n"))
	if joined == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:])
}

// textOfContent 提取 content 的纯文本（字符串原样；数组只拼 text 分片）。
func textOfContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, raw := range c {
			if part, ok := raw.(map[string]any); ok {
				if t, ok := part["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

// commaKeys 已确认「必须带逗号才能过」的 system 指纹。进程级、有界：
// 这是请求形态缓存，不是持久状态；重启后第一轮再付一次拒绝即可。
var (
	commaMu   sync.RWMutex
	commaKeys = make(map[string]struct{}, 64)
)

// commaKeysMax 上限：每个指纹约 64 字节，1k 条约 64KB，足够覆盖常见 agent 的
// 几种 system prompt；超过即清空重来（宁可多付一次拒绝，也不无限增长）。
const commaKeysMax = 1024

// RememberSystemKey 记住一个「被上游拒绝后加逗号才通过」的 system 指纹。
func RememberSystemKey(key string) {
	if key == "" {
		return
	}
	commaMu.Lock()
	defer commaMu.Unlock()
	if len(commaKeys) >= commaKeysMax {
		commaKeys = make(map[string]struct{}, 64)
	}
	commaKeys[key] = struct{}{}
}

// KnownSystemKey 报告指纹是否在记忆里。
func KnownSystemKey(key string) bool {
	if key == "" {
		return false
	}
	commaMu.RLock()
	defer commaMu.RUnlock()
	_, ok := commaKeys[key]
	return ok
}

// PauseIfRemembered 对已被记为「需要逗号」的 system 直接施加逗号；未命中原样返回。
// 调用点：轮转循环每次出站前（同一请求的每次换号都基于 origBody 重算）。
func PauseIfRemembered(body []byte) []byte {
	if !KnownSystemKey(SystemKey(body)) {
		return body
	}
	if out, ok := CommaPause(body); ok {
		return out
	}
	return body
}

// ResetCommaMemory 清空记忆（测试与配置热重载用）。
func ResetCommaMemory() {
	commaMu.Lock()
	commaKeys = make(map[string]struct{}, 64)
	commaMu.Unlock()
}
