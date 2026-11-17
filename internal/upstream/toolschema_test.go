package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// objOf 解析 JSON 到 map，失败即 fatal。
func objOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("unmarshal %s: %v", s, err)
	}
	return m
}

// TestFitToolShapes 覆盖社区实测里各模型拒绝、但可以无损修好的形状。
func TestFitToolShapes(t *testing.T) {
	cases := []struct {
		name   string
		params string
		want   []string // 输出 JSON 必须包含的片段
		absent []string // 必须不出现的片段
	}{
		{
			name:   "根 anyOf 合并成 object",
			params: `{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]},{"type":"object","properties":{"b":{"type":"integer"}},"required":["b"]}]}`,
			want:   []string{`"type":"object"`, `"a"`, `"b"`},
			absent: []string{`"anyOf":[{"required":["a"]`},
		},
		{
			name:   "无 type 的 oneOf 全 object → type object",
			params: `{"oneOf":[{"type":"object","properties":{"x":{"type":"string"}}},{"type":"object","properties":{"y":{"type":"string"}}}]}`,
			want:   []string{`"type":"object"`},
		},
		{
			name:   "tuple items 数组 → anyOf",
			params: `{"type":"object","properties":{"p":{"type":"array","items":[{"type":"string"},{"type":"number"}],"additionalItems":false}}}`,
			want:   []string{`"items":{"anyOf"`},
			absent: []string{`"additionalItems"`},
		},
		{
			name:   "空 enum 删除",
			params: `{"type":"object","properties":{"e":{"enum":[]}}}`,
			absent: []string{`"enum":[]`},
		},
		{
			name:   "多类型 enum 拆 anyOf",
			params: `{"type":"object","properties":{"e":{"enum":["a",1]}}}`,
			want:   []string{`"anyOf"`, `"type":"string"`, `"type":"number"`},
			absent: []string{`"enum":["a",1]`},
		},
		{
			name:   "布尔 schema true → {}",
			params: `{"type":"object","properties":{"p":true}}`,
			absent: []string{`"p":true`},
		},
		{
			name:   "required 不是数组 → 删除",
			params: `{"type":"object","properties":{"p":{"type":"string"}},"required":"p"}`,
			absent: []string{`"required"`},
		},
		{
			name:   "type 大小写归一 + 未知 type 删除",
			params: `{"type":"OBJECT","properties":{"p":{"type":"String"}}}`,
			want:   []string{`"type":"object"`, `"type":"string"`},
		},
		{
			name:   "pattern 的 \\0 换成 \\u0000",
			params: `{"type":"object","properties":{"p":{"type":"string","pattern":"a\\0b"}}}`,
			want:   []string{`"pattern":"a\\u0000b"`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj := map[string]any{"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":       "t",
					"parameters": objOf(t, c.params),
				},
			}}}
			if !fitToolShapes(obj) {
				t.Fatal("fitToolShapes 报告无改动")
			}
			raw, _ := json.Marshal(obj)
			out := string(raw)
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("缺少 %q: %s", w, out)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(out, a) {
					t.Errorf("不应出现 %q: %s", a, out)
				}
			}
		})
	}
}

// TestSanitizeToolNames 非法工具名换 `_`，历史/tool_choice 同步，响应可换回。
func TestSanitizeToolNames(t *testing.T) {
	body := []byte(`{"model":"kimi-k3","tools":[{"type":"function","function":{"name":"server.tool","parameters":{"type":"object"}}},{"type":"function","function":{"name":"工具","parameters":{"type":"object"}}}],"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"server.tool","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","name":"server.tool","content":"ok"}],"tool_choice":{"type":"function","function":{"name":"server.tool"}}}`)
	out, names := SanitizeToolNames(body)
	if len(names) != 2 {
		t.Fatalf("names=%v want 2", names)
	}
	s := string(out)
	for _, w := range []string{`"name":"server_tool"`, `"name":"__"`} {
		if !strings.Contains(s, w) {
			t.Errorf("缺少改名 %q: %s", w, s)
		}
	}
	if strings.Contains(s, `"name":"server.tool"`) {
		t.Errorf("历史/tool_choice 未同步改名: %s", s)
	}
	// 回写：安全名 → 原名。
	chunk := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": float64(0), "function": map[string]any{"name": "server_tool"}}},
	}}}}
	RestoreToolNamesInObject(chunk, names)
	got := chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"]
	if got != "server.tool" {
		t.Errorf("回写名字=%q want server.tool", got)
	}

	// 合法名不动。
	if _, n := SanitizeToolNames([]byte(`{"tools":[{"type":"function","function":{"name":"ok_name-1"}}]}`)); n != nil {
		t.Errorf("合法名不该产生映射: %v", n)
	}
}
