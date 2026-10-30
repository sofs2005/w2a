// upstream_timeout_logtail_test.go 钉住超时日志尾注随 cooled 二态化（M-1 修复）。
//
// 缺陷形态（QA 报的 Minor）：超时分支的日志尾注恒为
// "(rotation stopped, account not penalized)"，但 cooled=true 那一轮该号已被
// NoteTimeout 摘出池子（degradeUntil 生效 timeout_cooldown 时长）——同一行里
// cooled=true 与 "not penalized" 自相矛盾。这类「message 与 hint 互相矛盾」正是
// 本次故障的同类问题：读日志的人会被尾注带偏（以为号没被动过），而去错地方查。
//
// 修法：尾注二态化——未达阈保持原文案（该号确实没被动过），达阈明确写
// "slow-account threshold reached: account cooled"。本用例同时钉住「字段仍齐全」
// （i= / sticky= / cooled= 是排障必需列，不得因改文案被挤掉）。
package server

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

// captureLog 执行 fn 并返回其间写出的全部日志文本。
func captureLog(fn func()) string {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	fn()
	return buf.String()
}

// TestUpstreamTimeoutLogTailFollowsCooled 尾注二态化：两轮超时（第 1 轮未达阈、
// 第 2 轮达阈）分别校验尾注与 cooled 事实一致，且互不串味。
func TestUpstreamTimeoutLogTailFollowsCooled(t *testing.T) {
	const (
		tailNotPenalized = "(rotation stopped, account not penalized)"
		tailCooled       = "(rotation stopped, slow-account threshold reached: account cooled)"
	)
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"slow", "fast"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "slow", AccessToken: "at-slow", ExpiresAt: 9999999999},
		&auth.Auth{UID: "fast", AccessToken: "at-fast", ExpiresAt: 9999999999},
	)
	p.SetTimeout(2, time.Minute) // 阈 2：第 1 轮不出池、第 2 轮出池
	var callsSlow, callsFast int
	up := newSlowUpstream(t, "at-slow", &callsSlow, &callsFast)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, MaxRotate: 3})
	sess.Bind("conv-tail", "slow")

	// 第 1 轮：cooled=false → 尾注必须是原文案（号确实没被动过）。
	first := captureLog(func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, chatReqWithConv("conv-tail"))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("第 1 轮 code=%d want 503 body=%s", rec.Code, rec.Body)
		}
	})
	if !strings.Contains(first, "upstream timeout") {
		t.Fatalf("未捕获到超时日志行（日志格式变了？）: %q", first)
	}
	if !strings.Contains(first, "cooled=false") {
		t.Fatalf("第 1 轮日志应带 cooled=false: %s", first)
	}
	if !strings.Contains(first, tailNotPenalized) {
		t.Errorf("未达阈那一轮尾注应保持原文案 %q: %s", tailNotPenalized, first)
	}
	if strings.Contains(first, tailCooled) {
		t.Errorf("未达阈那一轮不得写「已出池」尾注: %s", first)
	}

	// 第 2 轮：cooled=true → 尾注必须写「已按慢号临时出池」，且不得再出现
	// "account not penalized"（那正是本 Minor 的矛盾点）。
	second := captureLog(func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, chatReqWithConv("conv-tail"))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("第 2 轮 code=%d want 503 body=%s", rec.Code, rec.Body)
		}
	})
	if !strings.Contains(second, "cooled=true") {
		t.Fatalf("达阈那一轮日志应带 cooled=true: %s", second)
	}
	if !strings.Contains(second, tailCooled) {
		t.Errorf("达阈那一轮尾注应说明号已出池（%q）: %s", tailCooled, second)
	}
	if strings.Contains(second, "account not penalized") {
		t.Errorf("cooled=true 与尾注「account not penalized」自相矛盾（M-1 回归）: %s", second)
	}
	// 排障必需列不得因改文案被挤掉。
	for _, want := range []string{"acct=", "model=", "i=", "sticky=", "cooled=", "err="} {
		if !strings.Contains(second, want) {
			t.Errorf("超时日志缺少字段 %q（排障必需）: %s", want, second)
		}
	}
	if !strings.Contains(second, "sticky=true") {
		t.Errorf("粘性命中那一轮应带 sticky=true: %s", second)
	}
	// 两轮都必须是同一条 WARN 行的形态（不是新开一行导致 grep 失配）。
	if strings.Count(second, "upstream timeout") != 1 {
		t.Errorf("达阈那一轮应恰好一条超时日志, got: %s", second)
	}
}
