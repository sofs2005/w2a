// persist_interval_test.go 落盘周期（SetFlushInterval）与「结构性变更立即落盘」的测试。
//
// 背景：落盘周期从 5s 放宽到 30m 后，高频运行态（余额扣减/计数/冷却）攒批落盘，
// 但**运维意图类**变更必须绕过周期立即落盘，否则「停用后未及落盘即重启」会丢失意图。
// 这两个不变量是本文件要钉死的契约。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestManualDisabledPersistsWithoutExplicitFlush 手动停用**不依赖显式 Flush**
// 就应已落盘：这是「结构性变更绕过落盘周期」的核心断言。
//
// 为什么不能靠后台 flusher：默认周期 30m，测试若等它就得等半小时；真实场景里
// 「停用后 30 分钟内重启/强杀」会让运维意图丢失，账号自己回到选号池。
func TestManualDisabledPersistsWithoutExplicitFlush(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")

	p := New(state)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetManualDisabled("u1", true, "立即落盘")
	// 刻意不调 p.Flush() —— 落盘必须已由 SetManualDisabled 自身完成。

	// 直接读文件断言（不等后台 flusher，也不经过内存）：最能反映「此刻磁盘上有什么」。
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatalf("state.json 未落盘（手动停用应立即写盘）: %v", err)
	}
	var sf struct {
		Accounts map[string]struct {
			ManualDisabled bool   `json:"manual_disabled"`
			ManualReason   string `json:"manual_reason"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("解析 state.json: %v", err)
	}
	if got := sf.Accounts["u1"]; !got.ManualDisabled || got.ManualReason != "立即落盘" {
		t.Errorf("磁盘上 manual_disabled=%v reason=%q，want true/立即落盘",
			got.ManualDisabled, got.ManualReason)
	}

	// 模拟重启确认端到端（新池读同一文件）。
	p2 := New(state)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); !st.ManualDisabled {
		t.Fatal("重启后手动停用丢失")
	}
}

// TestManualEnablePersistsWithoutExplicitFlush 清除停用同样立即落盘——
// 否则「启用后重启」会复活停用位，运维以为恢复了的账号仍不可选。
func TestManualEnablePersistsWithoutExplicitFlush(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")

	p := New(state)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetManualDisabled("u1", true, "先停")
	p.SetManualDisabled("u1", false, "") // 刻意不 Flush

	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatalf("state.json 未落盘: %v", err)
	}
	var sf struct {
		Accounts map[string]struct {
			ManualDisabled bool `json:"manual_disabled"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("解析 state.json: %v", err)
	}
	if sf.Accounts["u1"].ManualDisabled {
		t.Error("磁盘上 manual_disabled 仍为 true，清除未立即落盘")
	}
}

// TestHighFrequencyWritesDoNotFlushImmediately 高频运行态写入**不应**立即落盘：
// 它们必须走周期，否则落盘降频就白做了（每次请求都整份重写 state.json）。
func TestHighFrequencyWritesDoNotFlushImmediately(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")

	p := New(state)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100) // 高频路径：只置 dirty
	p.NoteError("u1")
	p.NoteSuccess("u1")

	if _, err := os.Stat(state); err == nil {
		t.Error("高频运行态写入不应立即落盘（应攒到落盘周期）")
	}
	// 显式 Flush 后必须落盘（退出路径依赖它，不能因为降频而丢）。
	p.Flush()
	if _, err := os.Stat(state); err != nil {
		t.Errorf("Flush 后应已落盘: %v", err)
	}
}

// TestSetFlushInterval 注入的周期即时生效（经 ticker Reset），
// 且 0 表示关闭后台落盘（而非 Ticker.Reset(0) 的「持续触发」）。
func TestSetFlushInterval(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")

	p := New(state)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})

	// 改成一个很短的周期并确认后台 flusher 按新周期落盘（无需显式 Flush）。
	p.SetFlushInterval(10 * time.Millisecond)
	p.SetCredits("u1", 42)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(state); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SetFlushInterval 后后台 flusher 未按新周期落盘")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 0 = 关闭后台落盘：写入后不应再自动落盘（文件 mtime 不再推进）。
	p.SetFlushInterval(0)
	time.Sleep(50 * time.Millisecond) // 让可能已排队的 tick 走完
	before, err := os.Stat(state)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	p.SetCredits("u1", 999)
	time.Sleep(200 * time.Millisecond)
	after, err := os.Stat(state)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("SetFlushInterval(0) 后不应再有后台落盘")
	}

	// 关闭后显式 Flush 仍须可用（退出路径不受影响）。
	p.Flush()
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatalf("Flush 应仍可用: %v", err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if sf.Accounts["u1"].Credits != 999 {
		t.Errorf("credits=%d want 999（Flush 应写入最新值）", sf.Accounts["u1"].Credits)
	}
}

// TestSetFlushIntervalNegativeIgnored 负值非法，保留现值（与 SetSoftRateMax 同口径）。
func TestSetFlushIntervalNegativeIgnored(t *testing.T) {
	p := New("") // 无 stateFp：不起 flusher
	p.SetFlushInterval(5 * time.Minute)
	p.SetFlushInterval(-1)
	if p.flushInterval != 5*time.Minute {
		t.Errorf("flushInterval=%v want 5m（负值应被忽略）", p.flushInterval)
	}
}

// TestSetFlushIntervalBeforeFlusherStarted 在 flusher 启动前注入（stateFp 为空，
// 或 New 返回后立即调用）不应 panic，且字段被记下。
func TestSetFlushIntervalBeforeFlusherStarted(t *testing.T) {
	p := New("")
	p.SetFlushInterval(2 * time.Minute) // 无 ticker：只写字段
	if p.flushInterval != 2*time.Minute {
		t.Errorf("flushInterval=%v want 2m", p.flushInterval)
	}
	if p.flushTicker != nil {
		t.Error("stateFp 为空时不应有 ticker")
	}
}
