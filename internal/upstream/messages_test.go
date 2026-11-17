package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestBuildMessagesBody OpenAI chat 请求 → Anthropic Messages 请求的关键映射。
func TestBuildMessagesBody(t *testing.T) {
	openAI := []byte(`{"model":"claude-opus-5.5","max_tokens":2048,"reasoning_effort":"max",` +
		`"messages":[` +
		`{"role":"system","content":"sys A"},` +
		`{"role":"developer","content":"sys B"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"calling","tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},` +
		`{"role":"tool","tool_call_id":"t1","content":"result"}] ,` +
		`"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"integer"}}}}}],` +
		`"tool_choice":"required"}`)
	out, err := buildMessagesBody(openAI)
	if err != nil {
		t.Fatalf("buildMessagesBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("out not json: %v", err)
	}
	if m["system"] != "sys A\n\nsys B" {
		t.Errorf("system=%v", m["system"])
	}
	if m["stream"] != true || m["max_tokens"] != float64(2048) {
		t.Errorf("stream/max_tokens=%v/%v", m["stream"], m["max_tokens"])
	}
	// 档位：thinking adaptive + output_config.effort=max
	if oc, ok := m["output_config"].(map[string]any); !ok || oc["effort"] != "max" {
		t.Errorf("output_config=%v", m["output_config"])
	}
	if th, ok := m["thinking"].(map[string]any); !ok || th["type"] != "adaptive" {
		t.Errorf("thinking=%v", m["thinking"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len=%d want 3 (%s)", len(msgs), out)
	}
	// assistant 的 tool_calls → tool_use 块
	asst := msgs[1].(map[string]any)
	blocks := asst["content"].([]any)
	foundUse := false
	for _, b := range blocks {
		bm := b.(map[string]any)
		if bm["type"] == "tool_use" {
			foundUse = true
			if bm["id"] != "t1" || bm["name"] != "f" {
				t.Errorf("tool_use=%v", bm)
			}
			if inp, _ := bm["input"].(map[string]any); inp["a"] != float64(1) {
				t.Errorf("input=%v", bm["input"])
			}
		}
	}
	if !foundUse {
		t.Error("缺少 tool_use 块")
	}
	// tool 消息 → user + tool_result
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "user" {
		t.Errorf("tool 消息角色=%v", toolMsg["role"])
	}
	cb := toolMsg["content"].([]any)[0].(map[string]any)
	if cb["type"] != "tool_result" || cb["tool_use_id"] != "t1" || cb["content"] != "result" {
		t.Errorf("tool_result=%v", cb)
	}
	// tools → input_schema
	tools := m["tools"].([]any)
	tf := tools[0].(map[string]any)
	if tf["name"] != "f" || tf["input_schema"] == nil {
		t.Errorf("tool=%v", tf)
	}
	// tool_choice required → any
	if tc := m["tool_choice"].(map[string]any); tc["type"] != "any" {
		t.Errorf("tool_choice=%v", tc)
	}
	// no effort → 不带 thinking/output_config
	plain, _ := buildMessagesBody([]byte(`{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`))
	if strings.Contains(string(plain), "output_config") || strings.Contains(string(plain), `"thinking"`) {
		t.Errorf("无档位时不该带 thinking/output_config: %s", plain)
	}
}

// TestMessagesConverter Anthropic SSE → OpenAI SSE 的完整事件映射。
func TestMessagesConverter(t *testing.T) {
	sse := "" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-opus-5.5\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"think\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"cache_read_input_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	rc := newMessagesToChatReader(strings.NewReader(sse), "claude-opus-5.5")
	outb, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(outb)
	for _, want := range []string{
		`"role":"assistant"`,
		`"reasoning_content":"think"`,
		`"content":"Hello"`,
		`"id":"toolu_1"`,
		`"name":"f"`,
		`"arguments":"{\"a\":"`,
		`"finish_reason":"tool_calls"`,
		`"prompt_tokens":10`,
		`"completion_tokens":5`,
		`"cached_tokens":3`,
		`[DONE]`,
		`"id":"msg_1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q:\n%s", want, out)
		}
	}
}

// TestMessagesMergeToolResults 并行工具调用在 OpenAI 里是多条相邻 tool 消息，
// Anthropic 要求 user/assistant 交替：必须合并成一条 user 的多个 tool_result，
// 且紧随其后的用户发言也并进同一条（顺序保持 tool_result 在前）。
func TestMessagesMergeToolResults(t *testing.T) {
	body := `{"model":"claude-x","messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":"","tool_calls":[` +
		`{"id":"t1","type":"function","function":{"name":"a","arguments":"{}"}},` +
		`{"id":"t2","type":"function","function":{"name":"b","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"t1","content":"r1"},` +
		`{"role":"tool","tool_call_id":"t2","content":"r2"},` +
		`{"role":"user","content":"next"}]}`
	out, err := buildMessagesBody([]byte(body))
	if err != nil {
		t.Fatalf("buildMessagesBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("out not json: %v (%s)", err, out)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len=%d want 3 (%s)", len(msgs), out)
	}
	for i, want := range []string{"user", "assistant", "user"} {
		if got := msgs[i].(map[string]any)["role"]; got != want {
			t.Fatalf("msg[%d].role=%v want %v", i, got, want)
		}
	}
	blocks := msgs[2].(map[string]any)["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("last user blocks=%d want 2 tool_result + 1 text (%v)", len(blocks), blocks)
	}
	for i, id := range []string{"t1", "t2"} {
		b := blocks[i].(map[string]any)
		if b["type"] != "tool_result" || b["tool_use_id"] != id {
			t.Errorf("block[%d]=%v", i, b)
		}
	}
	if last := blocks[2].(map[string]any); last["type"] != "text" || last["text"] != "next" {
		t.Errorf("trailing text=%v", last)
	}
}

// TestMessagesNotApplicable 转换不了（坏 body / 没有 user 消息）时必须报
// errMessagesNotApplicable，让调用方回落 chat，而不是当成上游 4xx。
func TestMessagesNotApplicable(t *testing.T) {
	if _, err := buildMessagesBody([]byte(`not json`)); !errors.Is(err, errMessagesNotApplicable) {
		t.Errorf("坏 body err=%v", err)
	}
	onlyAsst := `{"model":"claude-x","messages":[{"role":"assistant","content":"hi"}]}`
	if _, err := buildMessagesBody([]byte(onlyAsst)); !errors.Is(err, errMessagesNotApplicable) {
		t.Errorf("无 user 消息 err=%v", err)
	}
	// 开头的 assistant 段应被丢弃，但后面的 user 保留。
	out, err := buildMessagesBody([]byte(`{"model":"claude-x","messages":[` +
		`{"role":"assistant","content":"hi"},{"role":"user","content":"yo"}]}`))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("leading assistant not dropped: %s", out)
	}
}

// TestMessagesToolIndex 文本块占用 Anthropic 的 index，工具调用的 OpenAI index
// 必须从 0 单独计数，否则「先说话再调工具」会产出 index=1 的孤儿调用。
func TestMessagesToolIndex(t *testing.T) {
	sse := "" +
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu\",\"name\":\"f\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out, err := io.ReadAll(newMessagesToChatReader(strings.NewReader(sse), "claude-x"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	seen := 0
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("chunk not json: %v", err)
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			if idx := asFloat(call["index"]); idx != 0 {
				t.Errorf("tool index=%v want 0 (Anthropic block index leaked): %s", idx, line)
			}
			seen++
		}
	}
	if seen != 2 {
		t.Errorf("tool_calls chunks=%d want 2\n%s", seen, out)
	}
}

// TestMessagesNative 只有 CN 域的 claude-* 走 Messages。
func TestMessagesNative(t *testing.T) {
	cn := &fakeAuth{global: false}
	global := &fakeAuth{global: true}
	if !messagesNative(cn, "claude-opus-5.5") {
		t.Error("CN claude 应走 Messages")
	}
	if messagesNative(cn, "glm-5.3") {
		t.Error("非 claude 模型不该走 Messages")
	}
	if messagesNative(global, "claude-opus-5.5") {
		t.Error("global 域没有 Messages 路由")
	}
}

type fakeAuth struct{ global bool }

func (f *fakeAuth) IsGlobal() bool { return f.global }
