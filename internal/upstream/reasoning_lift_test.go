package upstream

import (
	"encoding/json"
	"testing"
)

// TestLiftReasoning 客户端回传的思考痕迹要抬成上游真正读取的 `reasoning` 字段。
//
// 依据：官方客户端读响应时优先 `reasoning`、回落 `reasoning_content`；社区实测
// （2026-10-27，www.workbuddy.ai）显示上游会丢弃 `reasoning_content`，只有
// `reasoning` 能进入模型的下一轮上下文。
func TestLiftReasoning(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		want     string // 期望的 reasoning 值；空串 = 不应写入
		wantKept bool   // reasoning_content 是否应保留
	}{
		{
			name:     "reasoning_content 抬升（非 deepseek 模型）",
			body:     `{"model":"glm-5.3","messages":[{"role":"assistant","content":"a","reasoning_content":"thought"}]}`,
			want:     "thought",
			wantKept: true,
		},
		{
			name: "reasoning_details 拼接",
			body: `{"model":"kimi-k3","messages":[{"role":"assistant","content":"a",` +
				`"reasoning_details":[{"text":"t1"},{"summary":"t2"}]}]}`,
			want: "t1t2",
		},
		{
			name: "已有非空 reasoning 不覆盖",
			body: `{"model":"glm-5.3","messages":[{"role":"assistant","content":"a",` +
				`"reasoning":"own","reasoning_content":"other"}]}`,
			want: "own",
		},
		{
			name: "用户消息不动",
			body: `{"model":"glm-5.3","messages":[{"role":"user","content":"hi","reasoning_content":"x"}]}`,
			want: "",
		},
		{
			name: "无痕迹不写字段",
			body: `{"model":"glm-5.3","messages":[{"role":"assistant","content":"a"}]}`,
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOpt([]byte(c.body), false)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal out: %v (out=%s)", err, out)
			}
			msgs, _ := obj["messages"].([]any)
			if len(msgs) == 0 {
				t.Fatalf("no messages in out=%s", out)
			}
			msg, _ := msgs[0].(map[string]any)
			got, _ := msg["reasoning"].(string)
			if got != c.want {
				t.Errorf("reasoning=%q want %q (out=%s)", got, c.want, out)
			}
			if _, has := msg["reasoning_content"]; c.wantKept && !has {
				t.Errorf("reasoning_content 应保留 (out=%s)", out)
			}
		})
	}
}
