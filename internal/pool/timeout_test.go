// timeout_test.go 钉住「上游超时（慢号）」出池通道（NoteTimeout）的语义边界。
//
// 背景（线上故障）：handler 的超时分支是**止损**设计（150af36：不换号、不罚号），
// 但超时信号完全不进池子 + 超时不解绑粘性，叠加成「同一个慢号被反复抽中、每次白烧
// 一个 header_timeout」的死循环。修法只补「同一个号**连续**超时」这一种强信号：
// 单次超时保持原设计（不罚），连续达阈才复用 degradeUntil 临时出池。
//
// 这些测试覆盖四条不可退让的边界：
//  1. 单次超时不罚号（账号保持可选、degradeUntil 仍为零）；
//  2. 连续达阈才出池（Pick/AvailableUIDs 选不到），且计数清零供下一轮累计；
//  3. 成功即清零（「超时一次 → 成功 → 再超时一次」不得凑满阈值）；
//  4. 不缩短既有降权（惩罚只能加重不能减轻，否则客户端重试可以当减刑器用）。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// timeoutStateOf 曝露 entry 的「连续超时」运行态（包内私有 helper）。
// 返回计数 / 降权截止 / 降权成因（"" / "fails" / "timeout"）。
func (p *Pool) timeoutStateOf(uid string) (streak int, degradeUntil time.Time, cause string, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, exists := p.byUID[uid]
	if !exists {
		return 0, time.Time{}, "", false
	}
	return e.consecutiveTimeouts, e.degradeUntil, e.degradeCause, true
}

// TestNoteTimeoutSingleNoPenalty 单次超时**不罚号**：只计数、不出池、账号照常可选。
// 这是 150af36 原设计的锚点——一次超时多半是上游抖动，罚号的代价（header_timeout
// 600s 的部署里长时间缺号）远大于收益。
func TestNoteTimeoutSingleNoPenalty(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetTimeout(2, time.Minute)

	if cooled := p.NoteTimeout("u1"); cooled {
		t.Fatal("单次超时不得触发冷却（未达阈值 2）")
	}
	streak, until, cause, ok := p.timeoutStateOf("u1")
	if !ok || streak != 1 {
		t.Fatalf("单次超时应只计数: streak=%d ok=%v", streak, ok)
	}
	if !until.IsZero() {
		t.Fatalf("单次超时不得写 degradeUntil, got %v", until)
	}
	if cause != "" {
		t.Fatalf("未出池不该有成因文案, got %q", cause)
	}
	// 账号必须保持可选（选号与可用列表两条路径都验）。
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatal("单次超时后账号应保持可选")
	}
	if uids := p.AvailableUIDs(); len(uids) != 1 || uids[0] != "u1" {
		t.Fatalf("单次超时后账号应在可用列表, got %v", uids)
	}
	// 连败计数不得被超时污染（两者是独立的成因通道）。
	st, _ := p.Status("u1")
	if st.ConsecutiveFails != 0 {
		t.Fatalf("超时不得喂连败计数, got %d", st.ConsecutiveFails)
	}
	if st.ConsecutiveTimeouts != 1 {
		t.Fatalf("Status 应透出连续超时计数 1, got %d", st.ConsecutiveTimeouts)
	}
	if st.Cooling {
		t.Fatal("单次超时不得让账号进入非健康态")
	}
}

// TestNoteTimeoutStreakCoolsAndResets 连续达阈才出池：第二次超时 → degradeUntil
// 生效（复用连败降权通道）、计数清零、/status 成因文案指向「连续超时」。
func TestNoteTimeoutStreakCoolsAndResets(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetTimeout(2, time.Minute)

	if cooled := p.NoteTimeout("u1"); cooled {
		t.Fatal("第 1 次超时不该出池")
	}
	if cooled := p.NoteTimeout("u1"); !cooled {
		t.Fatal("第 2 次连续超时应出池")
	}
	streak, until, cause, _ := p.timeoutStateOf("u1")
	if streak != 0 {
		t.Fatalf("达阈后计数应清零供下一轮累计, got %d", streak)
	}
	if until.IsZero() || !time.Now().Before(until) {
		t.Fatalf("达阈应写 degradeUntil, got %v", until)
	}
	if cause != "timeout" {
		t.Fatalf("成因应为 timeout（/status 文案据此区分）, got %q", cause)
	}
	// 出池：normal 选号选不到它，可用列表也不含它（另一号仍在）。
	if uids := p.AvailableUIDs(); len(uids) != 1 || uids[0] != "u2" {
		t.Fatalf("超时出池的账号不得出现在可用列表, got %v", uids)
	}
	// 出池：normal 选号只会落到健康号（u2）。注意不能用「tried 排掉 u2 再选」
	// 来断言——那会触发既有的「全冷却兜底」（pickEarliestExpiryLocked 会返回最早
	// 到期的号，包括出池中的号），兜底是刻意语义，不是本通道要改的东西。
	if got := p.PickExcludingForRealm(nil, "", ""); got == nil || got.UID != "u2" {
		t.Fatalf("超时出池的账号不得被选号选中（应落到 u2）, got %v", got)
	}
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "degrade" {
		t.Fatalf("出池期应呈 degrade 形态: cooling=%v kind=%q", st.Cooling, st.CoolKind)
	}
	if st.Reason != timeoutDegradeReason {
		t.Fatalf("reason 应为超时专用文案（与连败降权区分）, got %q", st.Reason)
	}
	if st.CoolRemaining <= 0 || st.CoolRemaining > 61 {
		t.Fatalf("冷却剩余秒数应约为 60s, got %d", st.CoolRemaining)
	}
}

// TestNoteTimeoutSuccessResetsStreak 成功即清零：超时一次 → 成功 → 再超时一次，
// 仍不得出池（否则「超时→成功→超时」能凭空凑满阈值，把偶发抖动升级成缺号）。
func TestNoteTimeoutSuccessResetsStreak(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetTimeout(2, time.Minute)

	p.NoteTimeout("u1")
	p.NoteSuccess("u1")
	if streak, _, cause, _ := p.timeoutStateOf("u1"); streak != 0 || cause != "" {
		t.Fatalf("成功应清零连续超时计数与成因: streak=%d cause=%q", streak, cause)
	}
	if cooled := p.NoteTimeout("u1"); cooled {
		t.Fatal("成功清零后的首次超时不得出池")
	}
	if _, until, _, _ := p.timeoutStateOf("u1"); !until.IsZero() {
		t.Fatalf("超时 1 次（成功后）不得写 degradeUntil, got %v", until)
	}
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatal("账号应保持可选")
	}
}

// TestNoteTimeoutDoesNotShortenDegrade 不缩短既有降权：先 NoteFailures 触发 10m
// 降权，再连续超时达阈，degradeUntil 只能更远、不能变小（惩罚只能加重）。
// 否则「打几个超时」就能把 10m 的连败降权洗成 60s，等于给客户端一个减刑器。
func TestNoteTimeoutDoesNotShortenDegrade(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetDegrade(1, 10*time.Minute, 2*time.Hour) // 阈 1：一次 NoteFailures 即降权
	p.SetTimeout(2, time.Minute)

	p.NoteFailures("u1")
	_, before, cause, _ := p.timeoutStateOf("u1")
	if before.IsZero() || cause != "fails" {
		t.Fatalf("precondition: 应已连败降权（cause=fails）, until=%v cause=%q", before, cause)
	}
	if cooled := p.NoteTimeout("u1"); cooled {
		t.Fatal("第 1 次超时未达阈，不该返回 cooled")
	}
	if cooled := p.NoteTimeout("u1"); !cooled {
		// 达阈但既有降权更远：该号确实在池外，调用方必须据此解绑粘性 → 仍返回 true。
		t.Fatal("达阈时应返回 true（该号此刻确实在池外，调用方需解绑粘性）")
	}
	_, after, cause, _ := p.timeoutStateOf("u1")
	if after.Before(before) {
		t.Fatalf("超时达阈不得缩短既有降权: before=%v after=%v", before, after)
	}
	if !after.Equal(before) {
		t.Fatalf("既有降权更远时应原样保留: before=%v after=%v", before, after)
	}
	if cause != "fails" {
		t.Fatalf("保留既有降权时应保留原成因（文案不漂移）, got %q", cause)
	}
}

// TestNoteTimeoutDefaultParams 默认参数（未注入 SetTimeout）：连续 2 次 / 出池 60s。
// 默认值是文档口径的一部分，钉住它避免有人「顺手调成 1 次」把单次抖动也罚掉。
func TestNoteTimeoutDefaultParams(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	start := time.Now()
	if cooled := p.NoteTimeout("u1"); cooled {
		t.Fatal("默认阈值 2：第 1 次超时不得出池")
	}
	if cooled := p.NoteTimeout("u1"); !cooled {
		t.Fatal("默认阈值 2：第 2 次超时应出池")
	}
	_, until, _, _ := p.timeoutStateOf("u1")
	// 60s ± 5s 的容差：只验量级，不做脆弱的精确时间断言。
	if d := until.Sub(start); d < 55*time.Second || d > 65*time.Second {
		t.Fatalf("默认出池时长应约为 60s, got %v", d)
	}
}

// TestNoteTimeoutCooldownExpires 到期自动回池（无需显式复位）：复用 degradeUntil
// 通道的直接收益——到期判定与连败降权共用 entry.healthy，不新增复位路径。
func TestNoteTimeoutCooldownExpires(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetTimeout(1, 30*time.Millisecond) // 阈 1、30ms：一次即出池，快速到期

	if cooled := p.NoteTimeout("u1"); !cooled {
		t.Fatal("阈 1 时一次超时即应出池")
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("出池期可用列表应为空, got %v", uids)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if uids := p.AvailableUIDs(); len(uids) == 1 && uids[0] == "u1" {
			return // 到期回池
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("超时出池到期后账号应自动回池（2s 内）")
}

// TestNoteTimeoutUnknownUID 未知 UID 是空操作（池里没有的号不得被凭空创建/计数）。
func TestNoteTimeoutUnknownUID(t *testing.T) {
	p := New("")
	if cooled := p.NoteTimeout("nobody"); cooled {
		t.Fatal("未知 UID 不得返回 cooled")
	}
	if _, ok := p.Status("nobody"); ok {
		t.Fatal("未知 UID 不得被凭空加入池")
	}
}
