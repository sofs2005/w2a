// timeout_cause_persist_test.go 钉住「临时出池成因跨重启保真」（M-2 修复）。
//
// 缺陷形态（QA 报的 Minor）：degradeCause 曾只存在于内存，degradeUntil 却持久化。
// 于是「超时出池 → 落盘 → 重启」之后，/status 的 reason 会从「连续上游超时」漂回
// "consecutive failures"——选号行为全对（degrade_until / cool_remaining 都在），
// 只有排查方向被指错（运维去查该号，而真正该查的是上游/网络是否挂起）。
//
// 本文件覆盖修法的两条边：
//  1. 成因随截止同分支落盘 + 恢复 → 重启前后 reason 一致；
//  2. 老 state.json（无 degrade_cause 键）零迁移：能正常加载，reason 回落连败文案，
//     且出池行为不变（向后兼容，不要求运维改写旧文件）。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestDegradeCauseSurvivesRestart 超时出池 → Flush → 重启 → reason 仍为超时文案。
func TestDegradeCauseSurvivesRestart(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetTimeout(1, 10*time.Minute) // 阈 1 + 10m：保证重启后仍未过期

	if cooled := p.NoteTimeout("u1"); !cooled {
		t.Fatal("阈 1 时一次超时即应出池")
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号缺失")
	}
	if st.Reason != timeoutDegradeReason {
		t.Fatalf("重启前 reason 应为超时专用文案, got %q", st.Reason)
	}
	p.Flush()
	p.Close()

	// 落盘侧：degrade_cause 必须与 degrade_until 同分支写出（同一份快照）。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("state.json 读取失败: %v", err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("state.json 解析失败: %v", err)
	}
	sa, ok := sf.Accounts["u1"]
	if !ok {
		t.Fatalf("state.json 缺 u1 条目: %s", raw)
	}
	if sa.DegradeUntil == nil {
		t.Fatalf("未过期的 degrade_until 应落盘: %s", raw)
	}
	if sa.DegradeCause != degradeCauseTimeout {
		t.Errorf("degrade_cause 应随 degrade_until 一起落盘（want %q）, got %q: %s",
			degradeCauseTimeout, sa.DegradeCause, raw)
	}

	// 恢复侧：重启后成因必须保真（这就是 M-2 的全部要求）。
	p2 := New(fp)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "u1"})
	st2, ok := p2.Status("u1")
	if !ok {
		t.Fatal("重启后账号缺失（degrade_until 的持久化被破坏？）")
	}
	if !st2.Cooling || st2.CoolKind != "degrade" {
		t.Fatalf("重启后出池状态应保留: cooling=%v kind=%q", st2.Cooling, st2.CoolKind)
	}
	if st2.Reason != timeoutDegradeReason {
		t.Errorf("重启后 reason 漂移：真实成因是超时出池，want %q got %q（degrade_cause 未恢复？）",
			timeoutDegradeReason, st2.Reason)
	}
	if _, _, cause, _ := p2.timeoutStateOf("u1"); cause != degradeCauseTimeout {
		t.Errorf("重启后 entry.degradeCause 应为 %q, got %q", degradeCauseTimeout, cause)
	}
	if uids := p2.AvailableUIDs(); len(uids) != 0 {
		t.Errorf("重启后出池号不得回到可用列表, got %v", uids)
	}
}

// TestDegradeCauseFailsAlsoSurvivesRestart 连败降权（cause=fails）同样跨重启保真：
// 修法不是「只给 timeout 打补丁」，而是成因整体持久化（两条成因走同一分支）。
func TestDegradeCauseFailsAlsoSurvivesRestart(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetDegrade(1, 10*time.Minute, 2*time.Hour) // 阈 1：一次 NoteFailures 即降权

	p.NoteFailures("u1")
	if _, _, cause, _ := p.timeoutStateOf("u1"); cause != degradeCauseFails {
		t.Fatalf("precondition: 成因应为 %q, got %q", degradeCauseFails, cause)
	}
	p.Flush()
	p.Close()

	p2 := New(fp)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "u1"})
	if _, _, cause, _ := p2.timeoutStateOf("u1"); cause != degradeCauseFails {
		t.Errorf("重启后连败成因应保真（want %q）, got %q", degradeCauseFails, cause)
	}
	st2, _ := p2.Status("u1")
	if st2.Reason != degradeReason {
		t.Errorf("连败降权的 reason 应仍是连败文案, got %q", st2.Reason)
	}
}

// TestLegacyStateWithoutDegradeCauseMigrates 老 state.json（无 degrade_cause 键）
// 零迁移：正常加载、出池行为不变，reason 回落连败文案（旧行为），不报错。
func TestLegacyStateWithoutDegradeCauseMigrates(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	until := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	// 只写 M-2 之前的字段集（没有 degrade_cause），模拟线上存量文件。
	legacy := `{"accounts":{"u1":{"credits":0,"disabled":false,"err_total":0,` +
		`"soft_streak":0,"session_dead_fails":0,"consecutive_fails":2,` +
		`"degrade_until":"` + until + `"}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("老 state.json 应能正常加载出账号")
	}
	// 出池行为必须保持（这是旧文件本来就该有的语义）。
	if !st.Cooling || st.CoolKind != "degrade" {
		t.Fatalf("老文件里的 degrade_until 应继续生效: cooling=%v kind=%q", st.Cooling, st.CoolKind)
	}
	// 成因未知 → 回落连败文案（零迁移的既定口径：老文件不要求运维改写）。
	if st.Reason != degradeReason {
		t.Errorf("成因未知时应回落连败文案 %q, got %q", degradeReason, st.Reason)
	}
	if _, _, cause, _ := p.timeoutStateOf("u1"); cause != "" {
		t.Errorf("老文件无 degrade_cause 时成因应为空, got %q", cause)
	}
	// 脏数据防御：手改出一个不认识的成因取值，不得让 /status 冒出第三种文案。
	if err := os.WriteFile(fp, []byte(strings.Replace(legacy,
		`"degrade_until"`, `"degrade_cause":"hand-edited-nonsense","degrade_until"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(fp)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "u1"})
	if _, _, cause, _ := p2.timeoutStateOf("u1"); cause != "" {
		t.Errorf("未知成因取值应被丢弃（want 空串）, got %q", cause)
	}
	st2, _ := p2.Status("u1")
	if st2.Reason != degradeReason {
		t.Errorf("未知成因取值不得改变 /status 文案（want %q）, got %q", degradeReason, st2.Reason)
	}
}
