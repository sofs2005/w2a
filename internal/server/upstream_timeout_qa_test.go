// upstream_timeout_qa_test.go QA 独立验证（端到端）：「连续上游超时 → 出池 + 解绑
// 粘性」这条链路的**副作用范围**与**运维可见性**。
//
// 与 upstream_timeout_streak_test.go 的分工：那边钉主路径（单次不罚 → 连续达阈 →
// 第 3 个请求落到别的号），这里专攻实现容易漏的横向影响：
//   - 解绑是否只动本会话键（不误伤共用同一个慢号的其它会话）、是否幂等；
//   - timeout_streak=1 时一次请求是否只摘一个号（不会把整池打空）；
//   - 全池出池（全局上游变慢）时是否仍会去上游，而不是硬失败成 no_healthy_account；
//   - 超时日志的字段是否齐全、尾注是否随 cooled 二态且与池子事实一致。
package server

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
)

// qaNewStickySession 搭一个带绑定镜像 store 的会话路由（Available 闭包声明两个号）。
func qaNewStickySession(t *testing.T, st *bindStore, uids ...string) *session.Router {
	t.Helper()
	return session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return uids },
	})
}

// TestQATimeoutUnbindsOnlyTheTimedOutSession 第 1 条：解绑的作用域与幂等。
//
// 两个会话（conv-a / conv-b）都钉在同一个慢号 slow 上：
//   - conv-a 连续两次超时达阈 → 只解绑 conv-a 的键，conv-b 的绑定必须原样保留
//     （否则一个慢号会把所有共用它的会话一起踢掉，属于越权副作用）；
//   - Unbind 只被调用一次（第二次超时那一轮），即解绑是幂等的、不会重复删；
//   - 单次超时（第 1 轮）不得产生任何 Unbind 调用。
func TestQATimeoutUnbindsOnlyTheTimedOutSession(t *testing.T) {
	st := newBindStore()
	sess := qaNewStickySession(t, st, "slow", "fast")
	p := testPoolWith(
		&auth.Auth{UID: "slow", AccessToken: "at-slow", ExpiresAt: 9999999999},
		&auth.Auth{UID: "fast", AccessToken: "at-fast", ExpiresAt: 9999999999},
	)
	p.SetTimeout(2, time.Minute)
	var callsSlow, callsFast int
	up := newSlowUpstream(t, "at-slow", &callsSlow, &callsFast)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, MaxRotate: 3})

	sess.Bind("conv-a", "slow")
	sess.Bind("conv-b", "slow")

	// 第 1 轮（conv-a）：单次超时，粘性保留、零 Unbind。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-a"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("第 1 轮 code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if st.delCnt != 0 {
		t.Fatalf("单次超时不得产生 Unbind 调用, delCnt=%d", st.delCnt)
	}
	if uid, ok := st.lastUID("conv-a"); !ok || uid != "slow" {
		t.Fatalf("单次超时后 conv-a 应仍绑定 slow, got %s ok=%v", uid, ok)
	}

	// 第 2 轮（conv-a）：达阈 → 只解绑 conv-a。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-a"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("第 2 轮 code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if _, ok := st.lastUID("conv-a"); ok {
		t.Error("达阈后 conv-a 的绑定必须解绑")
	}
	if uid, ok := st.lastUID("conv-b"); !ok || uid != "slow" {
		t.Fatalf("解绑不得误伤其它会话：conv-b 应仍绑定 slow, got %s ok=%v", uid, ok)
	}
	if st.delCnt != 1 {
		t.Fatalf("解绑应恰好一次（幂等，不重复删）, delCnt=%d", st.delCnt)
	}
}

// qaTailRe 抓日志行末尾的括号段（尾注）：`(...)` 一直延伸到行尾。
var qaTailRe = regexp.MustCompile(`\(([^()]*)\)\s*$`)

// qaRunChatCapturingTimeoutLog 跑一轮 chat 请求，只回传其间写出的**超时日志行**。
//
// 自己抓日志（不读源码里的文案常量、不复用工程师的用例）：尾注是运维唯一能一眼看出
// 「号被怎么处置了」的地方，这里要验的是**真实输出**。log.SetOutput 用 defer 还原，
// 保证断言失败时不会把后续用例的日志吞掉。
func qaRunChatCapturingTimeoutLog(t *testing.T, h *Handler, convID string) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv(convID))

	var lines []string
	for _, ln := range strings.Split(buf.String(), "\n") {
		if strings.Contains(ln, "upstream timeout") {
			lines = append(lines, ln)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("一轮请求应恰好写出一条超时日志（实际 %d 条）: %q", len(lines), buf.String())
	}
	return lines[0], rec.Code
}

// qaTimeoutLogTail 取出超时日志行的尾注（行末括号段的内容）。
func qaTimeoutLogTail(t *testing.T, line string) string {
	t.Helper()
	m := qaTailRe.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
	if m == nil {
		t.Fatalf("超时日志行没有可识别的尾注（行末括号段）: %q", line)
	}
	return m[1]
}

// TestQATimeoutLogTailMatchesCooled 第 2 条（硬断言）：超时日志字段完整性 + 尾注随
// cooled 二态且**与事实一致**（M-1 修复后的回归护栏）。
//
// 契约：日志带 i= / sticky= / cooled=，尾注说明「轮转停了、号被怎么处置」：
//   - cooled=false（未达阈，号确实没被动过）→ 尾注说「未罚号」是对的；
//   - cooled=true（号已被摘出池一个 timeout_cooldown）→ 尾注**不得**再说
//     "account not penalized"（同一行里 cooled=true 与「未罚号」自相矛盾，运维会
//     以为号还好着），且必须明确表达「已出池」。
//
// 断言是硬的：修复前那版对两种形态都只 t.Logf（漂移/矛盾也算过），成因一回归就
// 拦不住。这里还额外用池子状态反查——尾注说的必须是真的，不能只是文案改漂亮了。
func TestQATimeoutLogTailMatchesCooled(t *testing.T) {
	st := newBindStore()
	sess := qaNewStickySession(t, st, "slow", "fast")
	p := testPoolWith(
		&auth.Auth{UID: "slow", AccessToken: "at-slow", ExpiresAt: 9999999999},
		&auth.Auth{UID: "fast", AccessToken: "at-fast", ExpiresAt: 9999999999},
	)
	p.SetTimeout(2, time.Minute) // 阈 2：第 1 轮 cooled=false，第 2 轮 cooled=true
	var callsSlow, callsFast int
	up := newSlowUpstream(t, "at-slow", &callsSlow, &callsFast)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, MaxRotate: 3})
	sess.Bind("conv-log", "slow")

	// 第 1 轮：未达阈 → cooled=false。
	line1, code1 := qaRunChatCapturingTimeoutLog(t, h, "conv-log")
	if code1 != http.StatusServiceUnavailable {
		t.Fatalf("第 1 轮 code=%d want 503", code1)
	}
	// 第 2 轮：达阈 → cooled=true（号被摘出池）。
	line2, code2 := qaRunChatCapturingTimeoutLog(t, h, "conv-log")
	if code2 != http.StatusServiceUnavailable {
		t.Fatalf("第 2 轮 code=%d want 503", code2)
	}

	// 字段完整性（两轮都要）：i= / sticky= / cooled= 是排障必需的三列。
	for i, line := range []string{line1, line2} {
		for _, want := range []string{"i=", "sticky=", "cooled="} {
			if !strings.Contains(line, want) {
				t.Errorf("第 %d 轮超时日志缺少字段 %q（排障必需）: %s", i+1, want, line)
			}
		}
		if !strings.Contains(line, "sticky=true") {
			t.Errorf("第 %d 轮命中了粘性号，日志应带 sticky=true: %s", i+1, line)
		}
	}
	if !strings.Contains(line1, "cooled=false") {
		t.Errorf("第 1 轮（未达阈）应带 cooled=false: %s", line1)
	}
	if !strings.Contains(line2, "cooled=true") {
		t.Fatalf("第 2 轮（达阈）应带 cooled=true: %s", line2)
	}

	// 尾注一：cooled=false 时必须明确「轮转停了、号未被罚」（这是超时止损的原设计）。
	tail1 := qaTimeoutLogTail(t, line1)
	t.Logf("cooled=false 日志行: %s", line1)
	if !strings.Contains(tail1, "rotation stopped") {
		t.Errorf("cooled=false 的尾注应说明轮转已停: %q", tail1)
	}
	if !strings.Contains(tail1, "not penalized") {
		t.Errorf("cooled=false 时号确实没被动过，尾注应说明未罚号: %q", tail1)
	}

	// 尾注二：cooled=true 时**不得**再说 not penalized，且必须表达已出池。
	tail2 := qaTimeoutLogTail(t, line2)
	t.Logf("cooled=true 日志行: %s", line2)
	if strings.Contains(tail2, "not penalized") {
		t.Fatalf("cooled=true（号已出池）时尾注不得再说未罚号（自相矛盾）: %q", tail2)
	}
	if !strings.Contains(tail2, "rotation stopped") {
		t.Errorf("cooled=true 的尾注仍应说明轮转已停（不换号是止损的一部分）: %q", tail2)
	}
	saysOutOfPool := false
	for _, marker := range []string{"out of pool", "out-of-pool", "cooled", "cooling", "taken out"} {
		if strings.Contains(tail2, marker) {
			saysOutOfPool = true
			break
		}
	}
	if !saysOutOfPool {
		t.Errorf("cooled=true 的尾注应明确表达「该号已出池」, got %q", tail2)
	}
	if tail1 == tail2 {
		t.Fatalf("尾注没有随 cooled 二态化（两轮相同）: %q", tail1)
	}

	// 尾注说的必须是真的：第 2 轮后号确实在池外（不只是文案改漂亮了）。
	stSlow, _ := p.Status("slow")
	if !stSlow.Cooling {
		t.Fatal("cooled=true 的日志声称已出池，但池子里该号并未 Cooling（日志撒谎）")
	}
	if uids := p.AvailableUIDs(); len(uids) != 1 || uids[0] != "fast" {
		t.Fatalf("达阈后只剩 fast 可用, got %v", uids)
	}
}

// TestQATimeoutStreakOneCoolsOnlyHitAccount 第 4 条（端到端）：timeout_streak=1 时
// 一次请求只能摘掉它真正打到的那个号，其余号必须仍可选。
func TestQATimeoutStreakOneCoolsOnlyHitAccount(t *testing.T) {
	st := newBindStore()
	sess := qaNewStickySession(t, st, "slow", "fast1", "fast2")
	p := testPoolWith(
		&auth.Auth{UID: "slow", AccessToken: "at-slow", ExpiresAt: 9999999999},
		&auth.Auth{UID: "fast1", AccessToken: "at-fast1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "fast2", AccessToken: "at-fast2", ExpiresAt: 9999999999},
	)
	p.SetTimeout(1, time.Minute) // 用户显式配置「单次即出池」
	var callsSlow, callsFast int
	up := newSlowUpstream(t, "at-slow", &callsSlow, &callsFast)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, MaxRotate: 3})
	sess.Bind("conv-1", "slow")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if callsSlow != 1 {
		t.Fatalf("超时仍须止损（只打一次上游）, callsSlow=%d", callsSlow)
	}
	if stSlow, _ := p.Status("slow"); !stSlow.Cooling {
		t.Fatal("streak=1：单次超时应出池（用户显式配置的语义）")
	}
	uids := p.AvailableUIDs()
	if len(uids) != 2 {
		t.Fatalf("一次超时只能摘掉被命中的号，其余两个必须仍可用, got %v", uids)
	}
	for _, uid := range uids {
		if uid == "slow" {
			t.Fatalf("slow 已出池却仍在可用列表: %v", uids)
		}
	}
}

// TestQATimeoutAllCooledStillHitsUpstream 第 5 条（端到端）：全池都因连续超时出池时，
// 网关必须仍走兜底选号去上游，而不是把「出池」升级成「池里没号」的硬失败。
//
// 断言口径：响应 code 必须是 upstream_timeout（本地哨兵），**不得**是
// no_healthy_account；且上游确实被打了一次（有号可用）。这是「雪崩不会变成硬失败」
// 的最终验收点。
func TestQATimeoutAllCooledStillHitsUpstream(t *testing.T) {
	st := newBindStore()
	sess := qaNewStickySession(t, st, "a1", "a2")
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-a2", ExpiresAt: 9999999999},
	)
	p.SetTimeout(2, 5*time.Minute)
	// 两个号都已连续超时出池（模拟全局上游变慢）。
	for _, uid := range []string{"a1", "a2"} {
		p.NoteTimeout(uid)
		p.NoteTimeout(uid)
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("precondition: 全池应已出池, got %v", uids)
	}

	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, timeoutErr{true}
	})}
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, MaxRotate: 3})
	sess.Bind("conv-1", "a1")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReqWithConv("conv-1"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("全池出池时仍应有号可用（兜底选号打一次上游）, calls=%d", calls)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "upstream_timeout") {
		t.Fatalf("全池出池 + 上游超时仍应报 upstream_timeout（不得退化成 no_healthy_account）: %s", rec.Body)
	}
}
