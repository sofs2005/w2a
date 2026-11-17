package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// sseOf 拼一段 SSE（每帧后加空行）。
func sseOf(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// TestFilterRefusalHeld 整段回复就是过滤器拒答 → 不写出任何字节并返回哨兵错误，
// 调用方据此换号。
func TestFilterRefusalHeld(t *testing.T) {
	body := sseOf(
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"content":"\nSorry, I can't respond to this question.\n"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
	)
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(body), nil, WithFilterRefusalHold())
	if !IsFilteredRefusal(err) {
		t.Fatalf("err=%v want filtered refusal", err)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("拒答不该写出任何字节，got %q", rec.Body.String())
	}
}

// TestFilterRefusalEmptyText content_filter 但没有任何正文 → 同样按拒答处理
// （空回复比换号更没有价值）。
func TestFilterRefusalEmptyText(t *testing.T) {
	body := sseOf(
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
	)
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(body), nil, WithFilterRefusalHold())
	if !IsFilteredRefusal(err) {
		t.Fatalf("err=%v want filtered refusal", err)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("不该写出字节，got %q", rec.Body.String())
	}
}

// TestFilterRefusalDiverges 回复以 "Sorry" 开头但继续说了别的话 → 全部原样透传，
// 延迟只到第一个不属于拒答句的字符为止。
func TestFilterRefusalDiverges(t *testing.T) {
	body := sseOf(
		`{"choices":[{"delta":{"content":"Sorry"}}]}`,
		`{"choices":[{"delta":{"content":", but here is the answer."}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	rec := httptest.NewRecorder()
	err := StreamHint(rec, strings.NewReader(body), nil, WithFilterRefusalHold())
	if err != nil {
		t.Fatalf("err=%v want nil", err)
	}
	out := rec.Body.String()
	for _, want := range []string{`"content":"Sorry"`, "here is the answer", `"finish_reason":"stop"`, "[DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q: %s", want, out)
		}
	}
}

// TestFilterRefusalNormalReply 普通回复不受影响（零扣留语义差异）。
func TestFilterRefusalNormalReply(t *testing.T) {
	body := sseOf(
		`{"choices":[{"delta":{"content":"Hello"}}]}`,
		`{"choices":[{"delta":{"content":" world"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(body), nil, WithFilterRefusalHold()); err != nil {
		t.Fatalf("err=%v", err)
	}
	for _, want := range []string{`"content":"Hello"`, `"content":" world"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("正文丢失 %q: %s", want, rec.Body.String())
		}
	}
}

// TestFilterRefusalToolCall 工具调用不是拒答，必须立即释放。
func TestFilterRefusalToolCall(t *testing.T) {
	body := sseOf(
		`{"choices":[{"delta":{"content":"Sorry"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(body), nil, WithFilterRefusalHold()); err != nil {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(rec.Body.String(), `"tool_calls"`) {
		t.Errorf("工具调用帧丢失: %s", rec.Body.String())
	}
}
