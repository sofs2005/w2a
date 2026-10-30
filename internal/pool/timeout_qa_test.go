// timeout_qa_test.go QA 独立验证（不依赖工程师 timeout_test.go 的任何 helper/断言）：
// 「上游超时（慢号）」出池通道的边界、副作用与运维可见性。
//
// 与 timeout_test.go 的分工：那边钉 NoteTimeout 的正向语义（单次不罚/达阈出池/成功
// 清零/不缩短降权），这里专攻「实现可能写错但正向用例照样绿」的角度：
//   - 三处默认值口径（pool 常量 ⇄ config.example.json）是否漂移；
//   - 一次超时信号是否会误伤别的账号（池子被一次性打空）；
//   - 全池超时（全局上游变慢）时的最坏情况推演：会不会硬失败；
//   - 既有更长降权是否被「洗短」+ 出池成因是否跨重启保真（重启后不漂移）；
//   - /status 新增字段对既有面板解析器的兼容性。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// qaTimeoutParams 读出池参数（未导出字段，包内可读）。
func qaTimeoutParams(p *Pool) (streak int, cooldown time.Duration) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.timeoutStreak, p.timeoutCooldown
}

// qaTimeoutState 读出连续超时计数 / 出池截止 / 成因。
func qaTimeoutState(p *Pool, uid string) (streak int, until time.Time, cause string, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, exists := p.byUID[uid]
	if !exists {
		return 0, time.Time{}, "", false
	}
	return e.consecutiveTimeouts, e.degradeUntil, e.degradeCause, true
}

// TestQATimeoutDefaultsMatchExampleConfig 默认值三处口径一致性（第 8 条的一半）：
// pool 包常量 ⇄ config.example.json 的 pool.timeout_streak / timeout_cooldown。
//
// cmd/server 那一半在 cmd/server/timeout_config_qa_test.go（跨包无法互引未导出常量，
// 两半合起来才是完整的三处比对）。漂移的表现是「示例配置抄下去的行为 ≠ 缺键时的行为」，
// 这类不一致没有任何运行期报错，只能靠测试拦。
func TestQATimeoutDefaultsMatchExampleConfig(t *testing.T) {
	// 1) pool 包常量 + New 之后的生效值。
	if defaultTimeoutStreak != 2 {
		t.Errorf("defaultTimeoutStreak=%d want 2（文档口径：连续 2 次超时才出池）", defaultTimeoutStreak)
	}
	if defaultTimeoutCooldown != 60*time.Second {
		t.Errorf("defaultTimeoutCooldown=%v want 60s", defaultTimeoutCooldown)
	}
	p := New("")
	defer p.Close()
	if s, d := qaTimeoutParams(p); s != defaultTimeoutStreak || d != defaultTimeoutCooldown {
		t.Fatalf("New 未注入默认出池参数: streak=%d cooldown=%v", s, d)
	}

	// 2) 示例配置必须与之一致（运维首装抄的就是它）。
	const examplePath = "../../config.example.json"
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Skipf("示例配置不在（%v），跳过三处口径比对", err)
	}
	var doc struct {
		Pool struct {
			TimeoutStreak   *int   `json:"timeout_streak"`
			TimeoutCooldown string `json:"timeout_cooldown"`
		} `json:"pool"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config.example.json 解析失败: %v", err)
	}
	if doc.Pool.TimeoutStreak == nil {
		t.Fatal("config.example.json 缺少 pool.timeout_streak（运维抄不到这个旋钮）")
	}
	if *doc.Pool.TimeoutStreak != defaultTimeoutStreak {
		t.Errorf("config.example.json timeout_streak=%d 与 pool 默认 %d 漂移",
			*doc.Pool.TimeoutStreak, defaultTimeoutStreak)
	}
	d, err := time.ParseDuration(doc.Pool.TimeoutCooldown)
	if err != nil {
		t.Fatalf("config.example.json timeout_cooldown=%q 不可解析: %v", doc.Pool.TimeoutCooldown, err)
	}
	if d != defaultTimeoutCooldown {
		t.Errorf("config.example.json timeout_cooldown=%v 与 pool 默认 %v 漂移", d, defaultTimeoutCooldown)
	}
}

// TestQATimeoutStreakOneCoolsOnlyHitAccount 第 4 条：timeout_streak=1 的边界语义。
//
// 用户显式配 1 = 明确要「单次超时就出池」（放弃默认的「单次不罚」保护），这是允许的；
// 但必须验证「一次信号只摘一个号」——若 NoteTimeout 在达阈时顺手把别的号也塞进
// degradeUntil（或调用方在单次请求里对所有候选都喂了一次），streak=1 就会变成
// 「一个请求打空整池」的放大器。这里用 3 个号逐次喂信号，每次只允许少 1 个可用号。
func TestQATimeoutStreakOneCoolsOnlyHitAccount(t *testing.T) {
	p := New("")
	defer p.Close()
	for _, uid := range []string{"a1", "a2", "a3"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetTimeout(1, time.Minute)

	// 一次信号 → 只摘 a1，其余两个必须仍可选。
	if cooled := p.NoteTimeout("a1"); !cooled {
		t.Fatal("streak=1：单次超时即应出池（用户显式配置的语义）")
	}
	if uids := p.AvailableUIDs(); len(uids) != 2 {
		t.Fatalf("一次超时信号只能摘掉被命中的那一个号, 可用=%v", uids)
	}
	for _, uid := range []string{"a2", "a3"} {
		if st, _ := p.Status(uid); st.Cooling || !st.DegradeUntil.IsZero() {
			t.Fatalf("未命中的号不得被连坐出池: %s cooling=%v until=%v", uid, st.Cooling, st.DegradeUntil)
		}
	}

	// 逐次喂满：每次只少一个，第 3 次后整池为空（这是配置语义，不是缺陷）。
	p.NoteTimeout("a2")
	if uids := p.AvailableUIDs(); len(uids) != 1 || uids[0] != "a3" {
		t.Fatalf("第二个信号后应只剩 a3, got %v", uids)
	}
	p.NoteTimeout("a3")
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("三个信号后整池出池（streak=1 的预期语义）, got %v", uids)
	}
}

// TestQATimeoutAllCooledStillPicksFallback 第 5 条：全局上游变慢的雪崩推演。
//
// 最坏情况：所有号都在连续超时（上游整体挂起）→ 每个号各自被摘 60s → normal 选号
// 候选集为空 → 走 pickEarliestExpiryLocked 兜底（degradeUntil 计入 expiry）→ **仍然
// 返回一个号**。行为结论：
//   - 不会硬失败成「池里没号」（no_healthy_account 那种更糟的语义）；
//   - 兜底返回的正是「最早到期」的号，即最先恢复的那个，上游一好就能被选中；
//   - 单次请求只撞一个号（超时分支 break），所以雪崩速度被 MaxRotate=1 摊平，
//     不是「一个请求把所有号打成出池」。
func TestQATimeoutAllCooledStillPicksFallback(t *testing.T) {
	p := New("")
	defer p.Close()
	for _, uid := range []string{"a1", "a2", "a3"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.SetTimeout(2, time.Minute)

	for _, uid := range []string{"a1", "a2", "a3"} {
		p.NoteTimeout(uid)
		p.NoteTimeout(uid) // 达阈 → 出池
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("precondition: 全池应已出池, 可用=%v", uids)
	}

	// 兜底必须仍能给出一个号（不硬失败），且反复调用恒有返回值。
	for i := 0; i < 5; i++ {
		got := p.PickExcludingForRealms(nil, "", nil)
		if got == nil {
			t.Fatalf("全池超时出池时选号不得返回 nil（第 %d 次）——那会退化成 no_healthy_account 硬失败", i+1)
		}
		if got.UID == "" {
			t.Fatalf("兜底返回的账号 UID 为空（第 %d 次）", i+1)
		}
	}
	// 兜底选中必须来自真实池成员（不得凭空造号）。
	if got := p.PickExcludingForRealms(nil, "", nil); got == nil ||
		(got.UID != "a1" && got.UID != "a2" && got.UID != "a3") {
		t.Fatalf("兜底返回了池外账号: %+v", got)
	}
}

// TestQATimeoutRollingExtensionStaysBounded 第 5 条的补充推演：出池期内再次达阈会
// **顺延**截止（now+cooldown 比原截止更远），不会缩短、也不会指数放大。
//
// 为什么这值得钉：全局上游变慢时，兜底选号会反复选中已出池的号，于是「出池 → 再达阈
// → 顺延」可能滚动发生。必须确认它是**有界线性**的（每次只加一个 cooldown），而不是
// 指数退避或永久出池——上游一恢复，一次 NoteSuccess 就能立刻撤销出池（见
// TestQATimeoutSuccessWhileCooledReturnsToPoolImmediately）。
func TestQATimeoutRollingExtensionStaysBounded(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "a1"})
	p.SetTimeout(2, 90*time.Second)

	p.NoteTimeout("a1")
	p.NoteTimeout("a1") // 达阈 → 出池 90s
	_, first, _, _ := qaTimeoutState(p, "a1")
	if first.IsZero() {
		t.Fatal("达阈应出池")
	}
	// 出池期内再来两个信号（模拟兜底反复选中同一个慢号）。
	p.NoteTimeout("a1")
	p.NoteTimeout("a1")
	_, second, cause, _ := qaTimeoutState(p, "a1")
	if second.Before(first) {
		t.Fatalf("顺延不得缩短截止: first=%v second=%v", first, second)
	}
	if cause != "timeout" {
		t.Fatalf("顺延后成因仍应是 timeout, got %q", cause)
	}
	if d := second.Sub(time.Now()); d > 2*90*time.Second {
		t.Fatalf("顺延必须是线性的（每次 ≤ 一个 cooldown），got 剩余 %v", d)
	}
	// 上游恢复：一次成功立刻回池，不留下任何「累计惩罚」。
	p.NoteSuccess("a1")
	if _, until, _, _ := qaTimeoutState(p, "a1"); !until.IsZero() {
		t.Fatalf("成功后出池应彻底撤销（无累计惩罚）, until=%v", until)
	}
}

// TestQATimeoutDoesNotShortenLongerFailsDegrade 第 6 条（独立于工程师用例的第二条路径）：
// 先造 10m 连败降权，再用 **streak=1** 让一次超时直接达阈——此时 degradeUntil 只能
// 保持原值（更远者胜），且成因文案必须留在 "fails"（不得被改写成 timeout，否则
// /status 会指向错误的排查方向）。
func TestQATimeoutDoesNotShortenLongerFailsDegrade(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "a1"})
	p.SetDegrade(1, 10*time.Minute, 2*time.Hour) // 阈 1：一次 NoteFailures 即 10m 降权
	p.SetTimeout(1, 30*time.Second)              // 阈 1 + 30s：一次超时即达阈，但更短

	p.NoteFailures("a1")
	_, before, cause, _ := qaTimeoutState(p, "a1")
	if before.IsZero() || cause != "fails" {
		t.Fatalf("precondition: 应已连败降权（cause=fails）, until=%v cause=%q", before, cause)
	}

	if cooled := p.NoteTimeout("a1"); !cooled {
		t.Fatal("streak=1：达阈必须返回 true（该号此刻确实在池外，调用方需解绑粘性）")
	}
	_, after, cause, _ := qaTimeoutState(p, "a1")
	if !after.Equal(before) {
		t.Fatalf("既有更长降权必须原样保留（不得被 30s 洗短）: before=%v after=%v", before, after)
	}
	if cause != "fails" {
		t.Fatalf("保留既有降权时成因不得被改写成 timeout（/status 会误导）: cause=%q", cause)
	}
	st, _ := p.Status("a1")
	if st.Reason != degradeReason {
		t.Fatalf("/status reason 应仍是连败文案, got %q", st.Reason)
	}
	if st.CoolRemaining < 500 {
		t.Fatalf("冷却剩余应约 10m（未被洗短）, got %ds", st.CoolRemaining)
	}
}

// TestQATimeoutReasonSurvivesRestart 第 3 条（硬断言）：超时出池的成因必须跨重启保真。
//
// 缺陷形态（曾报 Minor）：degradeCause 只在内存、degradeUntil 却持久化（persist.go 只落
// 未过期条目）。于是「超时出池 → 落盘 → 重启」之后 /status 的 reason 从「连续上游超时」
// 漂回 "consecutive failures"——选号行为全对，只有排查方向被指错（运维去查该号，而真正
// 该查的是上游是否挂起）。
//
// 修法：成因随截止同分支落盘/恢复（stateAccount.degrade_cause）。
//
// 本用例是**回归护栏**，所以断言必须是硬的：修复前那版写成「两态接受」（漂移也算过，
// 只 t.Logf 一句），成因一旦再退回内存态（degrade_cause 被删/漏接、恢复侧 switch 漏了
// timeout），用例照样绿——那就等于没有护栏。这里任何漂移都直接红。
func TestQATimeoutReasonSurvivesRestart(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "a1"})
	p.SetTimeout(2, 5*time.Minute) // 出池时长取长一些，保证重启后仍未过期
	p.NoteTimeout("a1")
	p.NoteTimeout("a1") // 达阈 → 出池（cause=timeout）

	st, _ := p.Status("a1")
	if st.Reason != timeoutDegradeReason {
		t.Fatalf("重启前 reason 应为超时专用文案, got %q", st.Reason)
	}
	p.Flush()
	p.Close()

	p2 := New(fp)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "a1"})
	p2.SetTimeout(2, 5*time.Minute)

	st2, ok := p2.Status("a1")
	if !ok {
		t.Fatal("重启后账号缺失")
	}
	// 选号行为必须保持正确：仍在池外、剩余时间仍是 5m 量级。
	if !st2.Cooling || st2.CoolKind != "degrade" {
		t.Fatalf("重启后出池状态应保留: cooling=%v kind=%q", st2.Cooling, st2.CoolKind)
	}
	if st2.CoolRemaining < 240 || st2.CoolRemaining > 301 {
		t.Fatalf("重启后冷却剩余应约为 5m（degradeUntil 已持久化）, got %d", st2.CoolRemaining)
	}
	if uids := p2.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("重启后出池号不得回到可用列表, got %v", uids)
	}
	// 成因文案：重启后必须仍是超时专用文案（硬断言，不接受「漂回连败文案」）。
	// 这两种失败形态的根因不同，分开报：
	//   - 退回 degradeReason → 成因没被持久化/没被恢复（M-2 回归）；
	//   - 第三种文案 → 有人改了 statusOf 的分流却没同步这里（文案漂移）。
	if st2.Reason != timeoutDegradeReason {
		if st2.Reason == degradeReason {
			t.Fatalf("重启后成因漂移：真实成因是超时出池，want %q got %q（degrade_cause 未落盘/未恢复？）",
				timeoutDegradeReason, st2.Reason)
		}
		t.Fatalf("重启后 reason 既非超时文案也非连败文案（新引入的第三种文案）: %q", st2.Reason)
	}
	// 文案对齐还不够：内存里的成因字段也必须保真（否则下一次落盘又会把它写丢，
	// 「重启两次后漂移」这类间歇回归只有在这里才拦得住）。
	if _, _, cause, ok := qaTimeoutState(p2, "a1"); !ok || cause != degradeCauseTimeout {
		t.Fatalf("重启后 entry.degradeCause 应为 %q, got %q ok=%v", degradeCauseTimeout, cause, ok)
	}
}

// TestQATimeoutStatusJSONCompatibleWithLegacyParsers 第 7 条：新增
// consecutive_timeouts 字段对既有 /status 消费方的兼容性。
//
// 面板（panel/internal/gateway/client.go 的 AccountStatus）用 encoding/json 的默认
// 解码——未知字段被静默忽略，不会报错；也**没有** DisallowUnknownFields。这里直接
// 验证两件事：新字段确实下发了（运维可见）、旧解析器（只声明既有字段）不会报错。
func TestQATimeoutStatusJSONCompatibleWithLegacyParsers(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "a1"})
	p.SetTimeout(5, time.Minute)
	p.NoteTimeout("a1")

	st, ok := p.Status("a1")
	if !ok {
		t.Fatal("账号缺失")
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("Status 不可序列化: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("Status JSON 非法: %v", err)
	}
	if _, ok := probe["consecutive_timeouts"]; !ok {
		t.Errorf("/status 未下发 consecutive_timeouts（运维看不到慢号信号）: %s", raw)
	}
	// 零值也必须透出（与 consecutive_fails 同口径：缺失会被误读成「没记录」）。
	p2 := New("")
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "a2"})
	st2, _ := p2.Status("a2")
	raw2, _ := json.Marshal(st2)
	var probe2 map[string]json.RawMessage
	if err := json.Unmarshal(raw2, &probe2); err != nil {
		t.Fatalf("Status JSON 非法: %v", err)
	}
	if v, ok := probe2["consecutive_timeouts"]; !ok || string(v) != "0" {
		t.Errorf("零值也应显式下发 consecutive_timeouts=0, got %s", raw2)
	}

	// 旧解析器（只声明既有字段，模拟面板未升级）必须能无错解码。
	var legacy struct {
		UID               string    `json:"uid"`
		Cooling           bool      `json:"cooling"`
		CoolKind          string    `json:"cool_kind"`
		CoolRemaining     int64     `json:"cool_remaining_sec"`
		Reason            string    `json:"reason"`
		ConsecutiveFails  int       `json:"consecutive_fails"`
		DegradeUntil      time.Time `json:"degrade_until"`
		InFlight          int       `json:"in_flight"`
		BreakerFails      int       `json:"breaker_fails"`
		BreakerUntil      time.Time `json:"breaker_until"`
		RateLimitedModels []any     `json:"rate_limited_models"`
		ModelCosts        []any     `json:"model_costs"`
		SoftStreak        int       `json:"soft_streak"`
		SuccessCount      int64     `json:"success_count"`
		ErrTotal          int64     `json:"err_total"`
		LastSuccessTime   time.Time `json:"last_success"`
		LastErrTime       time.Time `json:"last_err"`
		CheckinDone       bool      `json:"checkin_done"`
		Until             time.Time `json:"until"`
		ManualDisabled    bool      `json:"manual_disabled"`
		ManualReason      string    `json:"manual_reason"`
		Disabled          bool      `json:"disabled"`
		DisabledReason    string    `json:"disabled_reason"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("旧解析器解码新 /status 失败（兼容性破坏）: %v", err)
	}
	if legacy.UID != "a1" || legacy.ConsecutiveFails != 0 {
		t.Fatalf("旧字段解码结果不对: %+v", legacy)
	}
}

// TestQATimeoutSuccessWhileCooledReturnsToPoolImmediately 成功即撤销出池（契约
// 「成功（NoteSuccess）立即清零超时计数与出池」的另一半：不只是计数清零，而是
// degradeUntil 被清空、账号立刻回池——上游一恢复就该立刻可用，不必等 60s）。
func TestQATimeoutSuccessWhileCooledReturnsToPoolImmediately(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "a1"})
	p.SetTimeout(1, time.Hour) // 出池一小时：只能靠 NoteSuccess 提前回池

	if cooled := p.NoteTimeout("a1"); !cooled {
		t.Fatal("precondition: streak=1 应出池")
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("precondition: 出池期可用列表应为空, got %v", uids)
	}
	p.NoteSuccess("a1")

	streak, until, cause, _ := qaTimeoutState(p, "a1")
	if streak != 0 || !until.IsZero() || cause != "" {
		t.Fatalf("成功后应立即清零计数/出池/成因: streak=%d until=%v cause=%q", streak, until, cause)
	}
	if uids := p.AvailableUIDs(); len(uids) != 1 || uids[0] != "a1" {
		t.Fatalf("成功后账号应立刻回池, got %v", uids)
	}
	if st, _ := p.Status("a1"); st.Cooling {
		t.Fatal("成功后不得仍显示 Cooling")
	}
}

// ---- M-2 成因持久化的独立复验（不复用工程师 timeout_cause_persist_test.go 的任何
// helper/断言）：两种成因、老文件、脏数据、Redis 快照四条边各自验一遍。 ----

// TestQAFailsCauseAlsoSurvivesRestart M-2 的另一半：修法必须对**两种成因**同时成立。
//
// 「只给 timeout 打补丁」是这条修复最容易走偏的形态（成因仍写死在 statusOf 里、
// 或恢复侧只认 "timeout"），表现为：超时成因保真、连败成因却退回默认文案。这里用
// 连败降权走一遍重启，要求成因与文案都保真。
func TestQAFailsCauseAlsoSurvivesRestart(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "a1"})
	p.SetDegrade(1, 10*time.Minute, 2*time.Hour) // 阈 1：一次连败即 10m 降权
	p.NoteFailures("a1")
	if _, _, cause, _ := qaTimeoutState(p, "a1"); cause != degradeCauseFails {
		t.Fatalf("precondition: 成因应为 %q, got %q", degradeCauseFails, cause)
	}
	p.Flush()
	p.Close()

	p2 := New(fp)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "a1"})
	if _, until, cause, _ := qaTimeoutState(p2, "a1"); until.IsZero() || cause != degradeCauseFails {
		t.Fatalf("连败成因未跨重启保真: until=%v cause=%q（want 非零 / %q）", until, cause, degradeCauseFails)
	}
	st2, _ := p2.Status("a1")
	if st2.Reason != degradeReason {
		t.Fatalf("连败降权的 reason 应仍是连败文案 %q, got %q", degradeReason, st2.Reason)
	}
}

// TestQALegacyStateCauseUnknownFallsBack 老 state.json（**没有** degrade_cause 键）
// 必须零迁移可用：加载成功、出池行为不变、reason 回落旧文案（连败）。
//
// 这条不是「顺手测一下向后兼容」——线上存量的 state.json 全是这个形态，恢复侧若
// 把「缺键」当成「非法值」而丢掉 degrade_until，重启就会让一批号提前回池（行为回归，
// 比文案问题严重得多）。故三件事一起验：能加载 / 仍出池 / 文案回落。
func TestQALegacyStateCauseUnknownFallsBack(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	// 手写「修复前」的字段集：有 degrade_until、没有 degrade_cause。
	until := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	legacy := `{"accounts":{"a1":{"credits":7,"consecutive_fails":3,` +
		`"degrade_until":"` + until + `"}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "a1"})
	st, ok := p.Status("a1")
	if !ok {
		t.Fatal("老 state.json 应能正常加载出账号")
	}
	if !st.Cooling || st.CoolKind != "degrade" {
		t.Fatalf("老文件里的 degrade_until 应继续生效: cooling=%v kind=%q", st.Cooling, st.CoolKind)
	}
	if st.CoolRemaining < 500 || st.CoolRemaining > 601 {
		t.Fatalf("老文件的出池剩余时间应约为 10m, got %d", st.CoolRemaining)
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("老文件里的出池号不得出现在可用列表, got %v", uids)
	}
	if _, _, cause, _ := qaTimeoutState(p, "a1"); cause != "" {
		t.Fatalf("缺键时成因应为空（未知）, got %q", cause)
	}
	if st.Reason != degradeReason {
		t.Fatalf("缺键时 reason 应回落旧文案 %q, got %q", degradeReason, st.Reason)
	}
	if st.ConsecutiveFails != 3 {
		t.Errorf("既有字段（consecutive_fails）不得因新增键而丢失: %d", st.ConsecutiveFails)
	}

	// 脏数据防御：手改成不认识的取值（"weird"）必须被丢弃，且不得冒出第三种文案。
	dirty := strings.Replace(legacy, `"degrade_until"`, `"degrade_cause":"weird","degrade_until"`, 1)
	if err := os.WriteFile(fp, []byte(dirty), 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(fp)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "a1"})
	if _, _, cause, _ := qaTimeoutState(p2, "a1"); cause != "" {
		t.Fatalf("未知成因取值必须被丢弃（want 空串）, got %q", cause)
	}
	st2, _ := p2.Status("a1")
	if st2.Reason != degradeReason {
		t.Fatalf("未知成因取值不得改变 /status 文案（want %q）, got %q", degradeReason, st2.Reason)
	}
	if !st2.Cooling {
		t.Fatal("丢弃脏成因不得连累出池状态（degrade_until 仍应生效）")
	}
}

// qaSnapStore 最小的 StoreSnapshotter 假实现（本文件自持，不复用其它测试文件的 fake）。
type qaSnapStore struct {
	mu   sync.Mutex
	data []byte
	ok   bool
}

func (s *qaSnapStore) SaveState(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = append([]byte(nil), data...)
	s.ok = true
}

func (s *qaSnapStore) LoadState() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ok {
		return nil, false
	}
	return append([]byte(nil), s.data...), true
}

// TestQACauseSurvivesRedisSnapshotRestore M-2 的漏接风险点：Redis 快照路径。
//
// 快照 = stateFile 的镜像（snapshot 内嵌 stateFile），恢复走 adoptSnapshot →
// applyAccountsLocked，与本地 load 共用同一段恢复代码。理论上「自动带上成因」，
// 但这条路径**没有任何既有用例覆盖**——一旦有人给快照单独写一份序列化/恢复逻辑
// （例如只挑部分字段镜像），成因就会在新卷/新节点首启时静默丢失，而本地重启的
// 用例照样绿。故这里端到端走一遍：落盘镜像 → 本地缺失 → 采用快照 → 成因保真。
func TestQACauseSurvivesRedisSnapshotRestore(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	store := &qaSnapStore{}

	p := New(fp)
	p.SetStore(store)
	p.Add(&auth.Auth{UID: "a1"})
	p.SetTimeout(1, 10*time.Minute) // 阈 1：一次超时即出池
	if cooled := p.NoteTimeout("a1"); !cooled {
		t.Fatal("precondition: 阈 1 时一次超时即应出池")
	}
	p.Flush() // 落盘 + 镜像一份快照到 store（带 saved_at）
	p.Close()

	// 新节点首启：本地 state.json 不存在，只能靠快照恢复（真实场景：新卷/新副本）。
	fp2 := filepath.Join(t.TempDir(), "state.json")
	p2 := New(fp2)
	defer p2.Close()
	p2.SetStore(store)
	p2.RestoreFromSnapshot()
	p2.Add(&auth.Auth{UID: "a1"})

	if _, until, cause, ok := qaTimeoutState(p2, "a1"); !ok || until.IsZero() || cause != degradeCauseTimeout {
		t.Fatalf("快照恢复后出池成因丢失: until=%v cause=%q ok=%v（want 非零 / %q）",
			until, cause, ok, degradeCauseTimeout)
	}
	st, ok := p2.Status("a1")
	if !ok {
		t.Fatal("快照恢复后账号缺失")
	}
	if st.Reason != timeoutDegradeReason {
		t.Fatalf("快照恢复后 reason 应仍是超时文案, got %q", st.Reason)
	}
	if uids := p2.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("快照恢复后出池号不得回到可用列表, got %v", uids)
	}
}

// TestQADegradeCooldownMaxNoLongerGovernsTimeout M-3 的回归风险点：degrade_cooldown_max
// 的职责已收窄为「只管 degrade_cooldown」，不得再钳 timeout_cooldown。
//
// 收窄前的实现拿 degradeCooldownMax 兜 timeout 的上界，于是「degrade 上限配小
// （2m）」会顺手把超时出池也压到 2m——运维调一个键却改了两个行为，且没有任何提示。
// 现在上限只由配置层的 timeout_cooldown_max 决定（默认 10m），池子只消费已归一值。
// 这里两个方向都钉：degrade 上限变小**不影响**超时出池；超时出池时长也不受
// degrade_cooldown 影响。
func TestQADegradeCooldownMaxNoLongerGovernsTimeout(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "a1"})
	// degrade 上限压到 2m（远小于超时出池的 10m），阈 1 便于一次触发。
	p.SetDegrade(1, 10*time.Minute, 2*time.Minute)
	p.SetTimeout(1, 10*time.Minute)

	if cooled := p.NoteTimeout("a1"); !cooled {
		t.Fatal("precondition: 阈 1 时一次超时即应出池")
	}
	st, _ := p.Status("a1")
	if st.CoolRemaining < 590 || st.CoolRemaining > 601 {
		t.Fatalf("超时出池时长不得被 degrade_cooldown_max（2m）钳制, got %ds（want ≈600）", st.CoolRemaining)
	}
	if st.Reason != timeoutDegradeReason {
		t.Fatalf("超时出池的 reason 应为超时文案, got %q", st.Reason)
	}

	// 反向：degrade 路径仍受自己的上限钳制（收窄不等于失效）。
	p2 := New("")
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "b1"})
	p2.SetDegrade(1, 10*time.Minute, 2*time.Minute)
	p2.NoteFailures("b1")
	st2, _ := p2.Status("b1")
	if st2.CoolRemaining < 110 || st2.CoolRemaining > 121 {
		t.Fatalf("连败降权仍应被 degrade_cooldown_max 钳到 2m, got %ds（want ≈120）", st2.CoolRemaining)
	}
}

// TestQADegradeDefaultsUnchangedByTimeoutWork 回归护栏：新增超时通道不得动到既有
// 连败降权的默认口径（5 次 / 10m，上限 2h）与 NoteFailures 的行为。
//
// 收窄/新增参数时最容易踩的坑是「顺手改了共享常量」——degrade 的默认值被拖去
// 对齐 timeout（60s）或反之。这里从 New() 的出厂默认值开始，走真实 NoteFailures 路径。
func TestQADegradeDefaultsUnchangedByTimeoutWork(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "a1"})

	// 出厂默认 5 次：前 4 次只计数、不出池。
	for i := 1; i <= 4; i++ {
		p.NoteFailures("a1")
		if _, until, _, _ := qaTimeoutState(p, "a1"); !until.IsZero() {
			t.Fatalf("第 %d 次连败（默认阈 %d）不得出池, until=%v", i, defaultDegradeThreshold, until)
		}
	}
	p.NoteFailures("a1") // 第 5 次达阈
	st, _ := p.Status("a1")
	if !st.Cooling || st.CoolKind != "degrade" {
		t.Fatalf("默认阈 %d 的第 5 次连败应出池: cooling=%v kind=%q", defaultDegradeThreshold, st.Cooling, st.CoolKind)
	}
	if st.CoolRemaining < 590 || st.CoolRemaining > 601 {
		t.Fatalf("默认降权时长应为 10m（不得被超时通道的 60s 拖走）, got %ds", st.CoolRemaining)
	}
	if st.Reason != degradeReason {
		t.Fatalf("连败降权的 reason 应为 %q, got %q", degradeReason, st.Reason)
	}
	if _, _, cause, _ := qaTimeoutState(p, "a1"); cause != degradeCauseFails {
		t.Fatalf("连败成因应为 %q, got %q", degradeCauseFails, cause)
	}

	// 上限 2h：配 3h 仍钳到 2h（degrade_cooldown_max 收窄后依然生效）。
	p2 := New("")
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "b1"})
	p2.SetDegrade(1, 3*time.Hour, 2*time.Hour)
	p2.NoteFailures("b1")
	if st2, _ := p2.Status("b1"); st2.CoolRemaining < 7190 || st2.CoolRemaining > 7201 {
		t.Fatalf("degrade 上限钳制应仍为 2h, got %ds", st2.CoolRemaining)
	}
}
