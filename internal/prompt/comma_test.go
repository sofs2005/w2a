package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCommaPauseOpener 开场句加逗号：形状匹配、语义不变、其余消息不动。
func TestCommaPauseOpener(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"system","content":"You are an AI agent powered by DeepSeek Harness.\nBe brief."},` +
		`{"role":"user","content":"You are an AI agent powered by DeepSeek Harness."},` +
		`{"role":"assistant","content":[{"type":"text","text":"You are the assistant powered by Foo."}]}]}`)
	out, ok := CommaPause(body)
	if !ok {
		t.Fatal("expect comma inserted")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := obj["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "You are an AI agent, powered by DeepSeek Harness.") {
		t.Errorf("system 未加逗号: %q", sys)
	}
	// 用户消息不动（上游不读 user）。
	if got := msgs[1].(map[string]any)["content"].(string); got != "You are an AI agent powered by DeepSeek Harness." {
		t.Errorf("user 消息被改写: %q", got)
	}
	// assistant 的 content 分片也要加。
	part := msgs[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if !strings.Contains(part["text"].(string), "You are the assistant, powered by Foo.") {
		t.Errorf("assistant 分片未加逗号: %q", part["text"])
	}

	// 没有可改的形状 → 原样（ok=false）。
	if _, ok := CommaPause([]byte(`{"messages":[{"role":"system","content":"You are helpful."}]}`)); ok {
		t.Error("无开场句形状不该改动")
	}
	// 已经带逗号 → 不再重复加。
	if _, ok := CommaPause([]byte(`{"messages":[{"role":"system","content":"You are an AI agent, powered by X."}]}`)); ok {
		t.Error("已有逗号不该再改")
	}
}

// TestCommaMemory 记住指纹后，同类 system 的后续请求直接带逗号。
func TestCommaMemory(t *testing.T) {
	ResetCommaMemory()
	body := []byte(`{"messages":[` +
		`{"role":"system","content":"You are an AI agent powered by DeepSeek Harness."},` +
		`{"role":"user","content":"hi"}]}`)
	key := SystemKey(body)
	if key == "" {
		t.Fatal("system key 不该为空")
	}
	if got := PauseIfRemembered(body); string(got) != string(body) {
		t.Error("未记忆时不该改动")
	}
	RememberSystemKey(key)
	out := PauseIfRemembered(body)
	if string(out) == string(body) {
		t.Fatal("已记忆时应直接加逗号")
	}
	if !strings.Contains(string(out), "You are an AI agent, powered by DeepSeek Harness.") {
		t.Errorf("记忆命中未加逗号: %s", out)
	}
	// 空 system / 无 system → 指纹为空，永不记忆。
	if SystemKey([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)) != "" {
		t.Error("无 system 时 key 应为空")
	}
	ResetCommaMemory()
}
