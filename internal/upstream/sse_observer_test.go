package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamHintErrorFrameObserver 上游 error 帧必须旁路通知观察者（handler 的账号
// 处置挂载点），且**透传字节不变**——error 帧原文（code/msg/requestId）照常到达
// 客户端（error-passthrough 语义）。普通数据帧不得触发观察者（否则正常流也会被
// 当成失败处置，把健康号冷却掉）。
func TestStreamHintErrorFrameObserver(t *testing.T) {
	const errPayload = `{"error":{"code":6004,"message":"模型限流，将在 2026-10-14 01:00:00 重置"}}`
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: " + errPayload + "\n\n" +
		"data: [DONE]\n\n"

	var observed []string
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), nil, WithErrorFrameObserver(func(payload string) {
		observed = append(observed, payload)
	})); err != nil {
		t.Fatal(err)
	}

	if len(observed) != 1 || observed[0] != errPayload {
		t.Fatalf("观察者回调 = %v want [%s]", observed, errPayload)
	}
	// 透传字节不变：error 帧原文必须仍在响应里。
	if !strings.Contains(rec.Body.String(), "6004") {
		t.Fatalf("error 帧原文未透传: %s", rec.Body.String())
	}

	// 正常流（无 error 帧）：观察者零回调。
	var normalObserved int
	raw2 := "data: {\"id\":\"x2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: [DONE]\n\n"
	rec2 := httptest.NewRecorder()
	if err := StreamHint(rec2, strings.NewReader(raw2), nil, WithErrorFrameObserver(func(payload string) {
		normalObserved++
	})); err != nil {
		t.Fatal(err)
	}
	if normalObserved != 0 {
		t.Errorf("正常数据帧不得触发观察者，got %d 次", normalObserved)
	}
}

// TestStreamHintObserverNilEqualsStream 无观察者时与既有 StreamHint 逐字节一致
// （新增可选参数不得改变透传行为——零回归锚）。
func TestStreamHintObserverNilEqualsStream(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"error\":{\"code\":6004,\"message\":\"限流\"}}\n\n" +
		"data: [DONE]\n\n"
	a := httptest.NewRecorder()
	if err := StreamHint(a, strings.NewReader(raw), nil); err != nil {
		t.Fatal(err)
	}
	b := httptest.NewRecorder()
	if err := StreamHint(b, strings.NewReader(raw), nil, WithErrorFrameObserver(func(string) {})); err != nil {
		t.Fatal(err)
	}
	if a.Body.String() != b.Body.String() {
		t.Errorf("观察者是旁路，不得改写字节:\n无观察者=%q\n有观察者=%q", a.Body.String(), b.Body.String())
	}
}
