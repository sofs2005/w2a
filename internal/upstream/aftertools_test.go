package upstream

import (
	"encoding/json"
	"testing"
)

// TestReplyAfterTools 11153 修复：工具结果与尾部 user 之间补一条空 assistant。
func TestReplyAfterTools(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		wantOK bool
		roles  []string // 期望的角色序列（wantOK 时校验）
	}{
		{
			name:   "tool 后跟 user → 插入空 assistant",
			body:   `{"messages":[{"role":"system","content":"s"},{"role":"user","content":"u1"},{"role":"assistant","content":null},{"role":"tool","content":"r"},{"role":"user","content":"reminder"}]}`,
			wantOK: true,
			roles:  []string{"system", "user", "assistant", "tool", "assistant", "user"},
		},
		{
			name:   "多条尾部 user 只插一条",
			body:   `{"messages":[{"role":"tool","content":"r"},{"role":"user","content":"a"},{"role":"user","content":"b"}]}`,
			wantOK: true,
			roles:  []string{"tool", "assistant", "user", "user"},
		},
		{
			name:   "assistant 结尾（prefill）不碰",
			body:   `{"messages":[{"role":"tool","content":"r"},{"role":"assistant","content":"x"}]}`,
			wantOK: false,
		},
		{
			name:   "全是 user 不碰",
			body:   `{"messages":[{"role":"user","content":"hi"}]}`,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, ok := ReplyAfterTools([]byte(c.body))
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			msgs := obj["messages"].([]any)
			if len(msgs) != len(c.roles) {
				t.Fatalf("roles len=%d want %d (%s)", len(msgs), len(c.roles), out)
			}
			for i, want := range c.roles {
				m := msgs[i].(map[string]any)
				if got := m["role"]; got != want {
					t.Errorf("msgs[%d].role=%v want %s", i, got, want)
				}
			}
			// 插入点 = 最后一条 tool 之后。
			lastTool := -1
			for i, want := range c.roles {
				if want == "tool" {
					lastTool = i
				}
			}
			if lastTool >= 0 {
				if ins := msgs[lastTool+1].(map[string]any); ins["content"] != "" {
					t.Errorf("插入的 assistant 内容=%v want 空串", ins["content"])
				}
			}
		})
	}
}

// TestIsAfterToolsRefused 业务码与文案两种表述都认，且不误伤更长数字。
func TestIsAfterToolsRefused(t *testing.T) {
	yes := []string{
		`{"code":11153,"msg":"the conversation must end with a user message"}`,
		`{"code":"11153"}`,
		`{"msg":"The Conversation Must End With A User Message"}`,
	}
	for _, s := range yes {
		if !IsAfterToolsRefused(s) {
			t.Errorf("应命中: %s", s)
		}
	}
	if IsAfterToolsRefused(`{"code":111530}`) {
		t.Error("111530 不该命中")
	}
}

// TestAfterToolsMemory 记忆后直接修复；形态不符时原样返回。
func TestAfterToolsMemory(t *testing.T) {
	ResetAfterToolsMemory()
	body := []byte(`{"messages":[{"role":"tool","content":"r"},{"role":"user","content":"x"}]}`)
	if string(ReplyAfterToolsIfKnown("m", body)) != string(body) {
		t.Error("未记忆时不该改动")
	}
	RememberAfterTools("m")
	if !AfterToolsRemembered("m") {
		t.Fatal("记忆未生效")
	}
	out := ReplyAfterToolsIfKnown("m", body)
	if string(out) == string(body) {
		t.Error("已记忆时应修复")
	}
	ResetAfterToolsMemory()
}
