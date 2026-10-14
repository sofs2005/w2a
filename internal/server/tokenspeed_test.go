package server

import (
	"strings"
	"testing"
	"time"
)

// TestGenerationSeconds TTFB 扣减口径：分母是「生成耗时」= 端到端 - 首 token 等待。
//
// 这条测试的价值在于把「没有观测就不扣」钉住：非流式回复没有首个 data 帧，若实现
// 改成「没测到就按某个默认 TTFB 扣」，速率会虚高，而单看代码很难发现。
func TestGenerationSeconds(t *testing.T) {
	cases := []struct {
		name  string
		total float64 // ms
		ttfb  time.Duration
		want  float64 // 秒
	}{
		{"无 TTFB 观测（非流式）按端到端算", 2000, 0, 2},
		{"扣掉 TTFB：1200ms 里等了 200ms", 1200, 200 * time.Millisecond, 1},
		{"TTFB 占总耗时一半", 1000, 500 * time.Millisecond, 0.5},
		{"TTFB 超过总耗时 → 退回端到端，不得负/零分母", 100, 500 * time.Millisecond, 0.1},
		{"TTFB 恰好等于总耗时 → 退回端到端", 100, 100 * time.Millisecond, 0.1},
		{"零耗时", 0, 0, 0},
		{"负耗时（时钟回拨防御）", -5, 0, 0},
	}
	for _, c := range cases {
		got := generationSeconds(c.total, c.ttfb)
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: generationSeconds(%v, %v)=%v want %v", c.name, c.total, c.ttfb, got, c.want)
		}
	}
}

// TestLogChatRowRateSubtractsTTFB 控制台流水行的 tok/s 必须扣掉 TTFB——与面板
// TokensPerSec（accumulateMetric 的 genSecSum）同口径。此前流水行自己除 total，
// 漏扣首 token 等待，两处数字对不上。
func TestLogChatRowRateSubtractsTTFB(t *testing.T) {
	withChatLog(t)
	// 2000ms 里等了 1000ms 才出首字 → 生成 1s，100 tok → 100.0tok/s。
	// 漏扣口径会报 100/2 = 50.0tok/s。
	out := captureStdout(t, func() {
		logChatRow(1, time.Second, 2*time.Second, "glm-5.2", "stream", "00e26541abcdef", "sample", 200, 100)
	})
	if !strings.Contains(out, "100.0tok/s") {
		t.Errorf("速率应扣掉 TTFB（生成 1s → 100.0tok/s），got:\n%s", out)
	}
	if strings.Contains(out, "50.0tok/s") {
		t.Errorf("速率漏扣 TTFB（按端到端 2s 算成 50.0tok/s）:\n%s", out)
	}
}

// TestLogChatRowRateNoTTFBUsesTotal 非流式（无 TTFB 观测）不得凭空扣减：
// 分母就是端到端耗时。
func TestLogChatRowRateNoTTFBUsesTotal(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(1, 0, 2*time.Second, "glm-5.2", "sync", "00e26541abcdef", "sample", 200, 100)
	})
	if !strings.Contains(out, "50.0tok/s") {
		t.Errorf("无 TTFB 观测应按端到端算（100/2s → 50.0tok/s），got:\n%s", out)
	}
}
