// toolnames.go 工具名清洗与回写。
//
// 上游只收 `[A-Za-z0-9_-]` 组成的工具名（社区实测：`.` `:` `/` 空格、CJK 都整单拒），
// 而 MCP/插件生态里 `server.tool`、`ns:tool`、`工具名` 很常见。出站前把非法字符换成
// `_` 并记录 安全名→原名，响应里的 tool_calls 再换回原名，客户端看到的仍是自己的名字。
//
// 历史与 tool_choice 里的名字也要同步改：上游按工具名关联历史调用，改名只改定义
// 会让历史里的旧名字对不上。
package upstream

import (
	"encoding/json"
	"regexp"
	"strconv"
)

// toolNameOK 上游接受的工具名字符集。
var toolNameOK = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// toolNameIllegal 工具名里出现即需替换的字符。
var toolNameIllegal = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// SanitizeToolNames 把非法工具名换成合法名，返回新 body 与
// 「安全名 → 客户端原名」的映射（无改动时 body 原样、map 为 nil）。
func SanitizeToolNames(body []byte) ([]byte, map[string]string) {
	if len(body) == 0 {
		return body, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, nil
	}
	list, ok := obj["tools"].([]any)
	if !ok || len(list) == 0 {
		return body, nil
	}
	// 已合法（无需改）的名字先占位，避免生成的重名撞上它们。
	taken := map[string]bool{}
	for _, raw := range list {
		t := asSchemaMap(raw)
		if t == nil {
			continue
		}
		if fn := functionOf(t); fn != nil {
			if n, ok := fn["name"].(string); ok && toolNameOK.MatchString(n) {
				taken[n] = true
			}
		}
	}
	names := map[string]string{}      // 安全名 → 原名
	byOriginal := map[string]string{} // 原名 → 安全名
	for _, raw := range list {
		t := asSchemaMap(raw)
		if t == nil {
			continue
		}
		fn := functionOf(t)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" || toolNameOK.MatchString(name) {
			continue
		}
		safe := safeToolName(name, taken)
		fn["name"] = safe
		names[safe] = name
		byOriginal[name] = safe
	}
	if len(names) == 0 {
		return body, nil
	}
	// 历史调用与 tool_choice 同步。
	if msgs, ok := obj["messages"].([]any); ok {
		for _, raw := range msgs {
			m := asSchemaMap(raw)
			if m == nil {
				continue
			}
			if calls, ok := m["tool_calls"].([]any); ok {
				for _, c := range calls {
					call := asSchemaMap(c)
					if call == nil {
						continue
					}
					if fn := asSchemaMap(call["function"]); fn != nil {
						if n, ok := fn["name"].(string); ok {
							if safe, hit := byOriginal[n]; hit {
								fn["name"] = safe
							}
						}
					}
				}
			}
			if role, _ := m["role"].(string); role == "tool" {
				if n, ok := m["name"].(string); ok {
					if safe, hit := byOriginal[n]; hit {
						m["name"] = safe
					}
				}
			}
		}
	}
	switch tc := obj["tool_choice"].(type) {
	case map[string]any:
		// 对象形态：{"type":"function","function":{"name":...}} 或 {"name":...}
		if fn := asSchemaMap(tc["function"]); fn != nil {
			if n, ok := fn["name"].(string); ok {
				if safe, hit := byOriginal[n]; hit {
					fn["name"] = safe
				}
			}
		}
		if n, ok := tc["name"].(string); ok {
			if safe, hit := byOriginal[n]; hit {
				tc["name"] = safe
			}
		}
	case string:
		// 字符串形态即工具名（normalizeToolChoice 之后的出站形态）。
		if safe, hit := byOriginal[tc]; hit {
			obj["tool_choice"] = safe
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body, nil
	}
	return out, names
}

// functionOf 取工具的 function 对象（兼容 tools[].function 与裸 tools[]）。
func functionOf(t map[string]any) map[string]any {
	if fn := asSchemaMap(t["function"]); fn != nil {
		return fn
	}
	if _, ok := t["name"]; ok {
		return t
	}
	return nil
}

// safeToolName 生成合法名：非法字符逐个换成 `_`，空名回落 "tool"，
// 与已有名冲突时追加 _2、_3…
func safeToolName(name string, taken map[string]bool) string {
	base := toolNameIllegal.ReplaceAllString(name, "_")
	if base == "" {
		base = "tool"
	}
	n := base
	for i := 2; taken[n]; i++ {
		n = base + "_" + strconv.Itoa(i)
	}
	taken[n] = true
	return n
}

// RestoreToolName 把响应里的安全名换回客户端原名（未命中返回原值）。
func RestoreToolName(name string, names map[string]string) string {
	if len(names) == 0 {
		return name
	}
	if orig, ok := names[name]; ok {
		return orig
	}
	return name
}

// RestoreToolNamesInObject 回写非流式聚合结果里的 tool_calls 名。
func RestoreToolNamesInObject(obj map[string]any, names map[string]string) {
	if len(names) == 0 || obj == nil {
		return
	}
	choices, _ := obj["choices"].([]any)
	for _, raw := range choices {
		c := asSchemaMap(raw)
		if c == nil {
			continue
		}
		if msg := asSchemaMap(c["message"]); msg != nil {
			restoreCalls(msg, names)
		}
		if delta := asSchemaMap(c["delta"]); delta != nil {
			restoreCalls(delta, names)
		}
	}
}

func restoreCalls(holder map[string]any, names map[string]string) {
	calls, _ := holder["tool_calls"].([]any)
	for _, c := range calls {
		call := asSchemaMap(c)
		if call == nil {
			continue
		}
		if fn := asSchemaMap(call["function"]); fn != nil {
			if n, ok := fn["name"].(string); ok {
				fn["name"] = RestoreToolName(n, names)
			}
		}
	}
}
