package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

// msg 测试用的消息形态（只覆盖搬迁需要看的两个字段）。
type msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// decodeMessages 解出 messages；解析失败即测试失败（MoveSystemToUser 成功时
// 必须产出合法 JSON）。
func decodeMessages(t *testing.T, body []byte) []msg {
	t.Helper()
	var req struct {
		Messages []msg `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	return req.Messages
}

// TestMoveSystemToUserMovesTextAndKeepsUserContent 正常搬迁：两条 system 合并、
// 首条换中性提示词、后续消息逐字不动。
func TestMoveSystemToUserMovesTextAndKeepsUserContent(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"PERSONA-A"},` +
		`{"role":"developer","content":"PERSONA-B"},` +
		`{"role":"user","content":"hello"},` +
		`{"role":"assistant","content":"hi"}]}`)
	out, ok := MoveSystemToUser(body)
	if !ok {
		t.Fatal("有 system 文本与 user 消息时应成功")
	}
	msgs := decodeMessages(t, out)
	if len(msgs) != 3 {
		t.Fatalf("两条 system 应合并成一条中性 system，got %d 条", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[0].Content != Degraded {
		t.Fatalf("首条应换成中性提示词，got %+v", msgs[0])
	}
	if msgs[1].Role != "user" {
		t.Fatalf("第二条应仍是 user，got %+v", msgs[1])
	}
	// 顺序：所有 system 文本按出现顺序 "\n\n" 连接 + "\n\n" + 原 user 文本。
	want := "PERSONA-A\n\nPERSONA-B\n\nhello"
	if msgs[1].Content != want {
		t.Fatalf("搬迁后 user 内容 = %q, want %q", msgs[1].Content, want)
	}
	if msgs[2].Role != "assistant" || msgs[2].Content != "hi" {
		t.Fatalf("后续消息应逐字不动，got %+v", msgs[2])
	}
}

// TestMoveSystemToUserKeepsRestVerbatim 首条 user 之后的其余消息（含中途 system、
// tool 调用）除了被收集为文本的那几条 system 外，逐字保留。
func TestMoveSystemToUserKeepsRestVerbatim(t *testing.T) {
	body := []byte(`{"model":"m","input":"keep-me","messages":[` +
		`{"role":"user","content":"u1","name":"n1"},` +
		`{"role":"system","content":"SYS"},` +
		`{"role":"assistant","content":"a1"},` +
		`{"role":"tool","content":"t1","tool_call_id":"c1"}]}`)
	out, ok := MoveSystemToUser(body)
	if !ok {
		t.Fatal("首条就是 user 也应能搬（system 在后面）")
	}
	msgs := decodeMessages(t, out)
	if len(msgs) != 4 { // system(Degraded) + user + assistant + tool
		t.Fatalf("消息条数 = %d, want 4（3 条原有非 system + 1 条中性 system）", len(msgs))
	}
	if msgs[0].Content != Degraded {
		t.Fatalf("首条应为中性提示词，got %q", msgs[0].Content)
	}
	if msgs[1].Content != "SYS\n\nu1" {
		t.Fatalf("首条 user 应拼上后续 system 文本，got %q", msgs[1].Content)
	}
	// 其余字段（name/tool_call_id）与顶层字段都不能丢。
	var raw struct {
		Input    string           `json:"input"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if raw.Input != "keep-me" {
		t.Errorf("顶层字段应原样保留，got %q", raw.Input)
	}
	if raw.Messages[1]["name"] != "n1" {
		t.Errorf("消息上的其他字段应逐字不动，got %+v", raw.Messages[1])
	}
	if raw.Messages[3]["tool_call_id"] != "c1" {
		t.Errorf("tool 消息应逐字不动，got %+v", raw.Messages[3])
	}
}

// TestMoveSystemToUserBails 五个保守返回 false 的场景：宁可让调用方按原错误
// 处理，也不改动用户请求的语义。
func TestMoveSystemToUserBails(t *testing.T) {
	cases := map[string]string{
		"非 JSON":     `not json`,
		"无 messages": `{"model":"m"}`,
		"没有 system":  `{"messages":[{"role":"user","content":"hi"}]}`,
		"没有 user":    `{"messages":[{"role":"system","content":"s"}]}`,
		"user 是数组":   `{"messages":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
	}
	for name, body := range cases {
		out, ok := MoveSystemToUser([]byte(body))
		if ok {
			t.Errorf("%s 应保守返回 false", name)
		}
		if out != nil {
			t.Errorf("%s 失败时应返回 nil，got %q", name, string(out))
		}
	}
	// system 内容为空串（只有 role 没有文本）同样算「无 system 文本」。
	if _, ok := MoveSystemToUser([]byte(`{"messages":[{"role":"system","content":""},{"role":"user","content":"hi"}]}`)); ok {
		t.Error("空 system 文本应保守返回 false")
	}
	// 空 body 也是保守返回（不 panic）。
	if _, ok := MoveSystemToUser(nil); ok {
		t.Error("空 body 应保守返回 false")
	}
}

// TestMoveSystemToUserDoesNotMutateInput 输入 body 被调用方复用（handler 的
// origBody），搬迁不能就地改写调用方的切片。
func TestMoveSystemToUserDoesNotMutateInput(t *testing.T) {
	const src = `{"messages":[{"role":"system","content":"S"},{"role":"user","content":"U"}]}`
	body := []byte(src)
	out, ok := MoveSystemToUser(body)
	if !ok {
		t.Fatal("应成功")
	}
	if string(body) != src {
		t.Fatalf("入参被就地改写: %q", string(body))
	}
	if strings.Contains(string(out), `"content":"S"`) {
		t.Fatalf("输出里不该再有原 system 文本独立承载: %s", string(out))
	}
}
