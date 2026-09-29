package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeScriptExec 记录命令构建参数并按需模拟执行失败，替代真实 exec 拉起 python3 子进程。
// running/maxRunning 用于断言 task_runner.py 的两个排程入口（cat / growth）不会并发，
// 用 delay 把 Run 拉长到足以让并发窗口出现（否则锁没锁都测不出来）。
type fakeScriptExec struct {
	lastName string
	lastArgs []string
	lastDir  string
	runN     int
	err      error
	delay    time.Duration

	mu         sync.Mutex
	running    int
	maxRunning int
}

func (f *fakeScriptExec) SetDir(dir string) { f.lastDir = dir }

func (f *fakeScriptExec) Run() error {
	f.mu.Lock()
	f.runN++
	f.running++
	if f.running > f.maxRunning {
		f.maxRunning = f.running
	}
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	f.running--
	f.mu.Unlock()
	return f.err
}

func (f *fakeScriptExec) peakConcurrency() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxRunning
}

// installFakeExec 替换 newScriptCmd，测试结束还原。
func installFakeExec(t *testing.T) *fakeScriptExec {
	t.Helper()
	f := &fakeScriptExec{}
	orig := newScriptCmd
	newScriptCmd = func(name string, args ...string) scriptRunner {
		f.lastName, f.lastArgs = name, args
		return f
	}
	t.Cleanup(func() { newScriptCmd = orig })
	return f
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNextWakeSchoolSlot 开学季任务在 school_hours（默认 12 点）处有独立时点。
func TestNextWakeSchoolSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 11, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 12, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school 12:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskSchool {
		t.Errorf("kinds=%v want [school]", kinds)
	}
}

// TestNextWakeCatSlot 夜猫子任务在 cat_hours（默认 1 点）处有独立时点。
func TestNextWakeCatSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CatHours:          []int{1},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 23, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 15, 1, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（cat 01:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCat {
		t.Errorf("kinds=%v want [cat]", kinds)
	}
}

// TestNextWakeSchoolCatDisabled 显式禁用 school/cat 后排程只剩签到时点（互不影响）。
func TestNextWakeSchoolCatDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{21},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
		CatHours:          []int{1},
		SchoolDisabled:    true,
		CatDisabled:       true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school/cat 禁用 → 只有签到 21:00）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeGrowthSlot 成长任务补跑在 growth_hours（默认 8 点）处有独立时点。
// 与 cat 的分工：cat 是时段敏感的单任务（black_cat），growth 是幂等全量补跑。
func TestNextWakeGrowthSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolDisabled:    true,
		CatDisabled:       true,
		GrowthHours:       []int{8},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 7, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 8, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（growth 08:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskGrowth {
		t.Errorf("kinds=%v want [growth]", kinds)
	}
}

// TestNextWakeGrowthDisabled 显式禁用成长任务补跑后排程只剩签到时点。
func TestNextWakeGrowthDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{21},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolDisabled:    true,
		CatDisabled:       true,
		GrowthHours:       []int{8},
		GrowthDisabled:    true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（growth 禁用 → 只有签到 21:00）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNewGrowthHoursDefaults 零值 Config（老调用方/老测试）下 GrowthHours 回落 [8]，
// 与其余六类同口径——否则 len==0 的 nextFire 返回零时点，任务静默永不触发。
func TestNewGrowthHoursDefaults(t *testing.T) {
	s := New(Config{})
	if len(s.cfg.GrowthHours) != 1 || s.cfg.GrowthHours[0] != 8 {
		t.Errorf("GrowthHours=%v want [8]", s.cfg.GrowthHours)
	}
}

// TestRunGrowthTasksNowBuildsCommand RunGrowthTasksNow 构造
// python3 scripts/task_runner.py ALL --yes（全量、不加 --only），工作目录为仓库根。
//
// 断言「不带 --only」是本测试的重点：带上 --only 就退化成 cat 排程，
// 16 项一次性成长任务（first_buddy / create_canvas / chat_5 …）永远补不上。
func TestRunGrowthTasksNowBuildsCommand(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.RunGrowthTasksNow()
	want := []string{"scripts/task_runner.py", "ALL", "--yes"}
	if f.lastName != "python3" || !equalArgs(f.lastArgs, want) {
		t.Errorf("cmd=%s %v want python3 %v", f.lastName, f.lastArgs, want)
	}
	if f.lastDir != repoRoot() {
		t.Errorf("dir=%q want repo root %q", f.lastDir, repoRoot())
	}
}

// TestDispatchGrowth dispatch 把 growth 分发给 task_runner.py 全量入口。
func TestDispatchGrowth(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.dispatch(context.Background(), taskGrowth)
	if f.runN != 1 || !equalArgs(f.lastArgs, []string{"scripts/task_runner.py", "ALL", "--yes"}) {
		t.Errorf("dispatch(growth) 未按全量入口执行: runN=%d last=%v", f.runN, f.lastArgs)
	}
}

// TestTaskRunnerEntriesSerialized cat 与 growth 是同一脚本、同一批 auth 文件，
// 配到同一小时时 runBatch 会并行派发两者——两个 python 进程同时读写同一份
// auth 目录，结果不确定。taskRunnerMu 必须把它们串起来。
func TestTaskRunnerEntriesSerialized(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	f.delay = 50 * time.Millisecond // 拉长子进程时长，让并发窗口真实存在
	s := New(Config{})

	// 两个任务族并行派发（复刻 runBatch 对同一槽位多类任务的处置）。
	s.runBatch(context.Background(), []taskKind{taskCat, taskGrowth})

	if f.runN != 2 {
		t.Fatalf("两个入口都应执行: runN=%d want 2", f.runN)
	}
	if peak := f.peakConcurrency(); peak != 1 {
		t.Errorf("task_runner.py 两个入口并发执行了（peak=%d want 1）——auth 目录会被同时读写", peak)
	}
}

// TestRunSchoolNowBuildsCommand RunSchoolNow 构造
// python3 scripts/school_open_day_2026.py ALL --run --yes，工作目录设为仓库根。
func TestRunSchoolNowBuildsCommand(t *testing.T) {
	// 防环境泄漏：WB2A_PYTHON 若在测试机已设置会改写 pythonCmd()，使默认值断言失败。
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.RunSchoolNow()
	if f.lastName != "python3" {
		t.Errorf("name=%q want python3", f.lastName)
	}
	want := []string{"scripts/school_open_day_2026.py", "ALL", "--run", "--yes"}
	if !equalArgs(f.lastArgs, want) {
		t.Errorf("args=%v want %v", f.lastArgs, want)
	}
	if f.lastDir != repoRoot() {
		t.Errorf("dir=%q want repo root %q", f.lastDir, repoRoot())
	}
	if _, err := os.Stat(filepath.Join(f.lastDir, "scripts", "school_open_day_2026.py")); err != nil {
		t.Errorf("仓库根 %q 内应有 scripts/school_open_day_2026.py: %v", f.lastDir, err)
	}
}

// TestRunCatNowBuildsCommand RunCatNow 构造
// python3 scripts/task_runner.py ALL --yes --only black_cat，工作目录为仓库根。
func TestRunCatNowBuildsCommand(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.RunCatNow()
	want := []string{"scripts/task_runner.py", "ALL", "--yes", "--only", "black_cat"}
	if f.lastName != "python3" || !equalArgs(f.lastArgs, want) {
		t.Errorf("cmd=%s %v want python3 %v", f.lastName, f.lastArgs, want)
	}
	if f.lastDir != repoRoot() {
		t.Errorf("dir=%q want repo root %q", f.lastDir, repoRoot())
	}
}

// TestDispatchSchoolCatAndFailureWarnsOnly dispatch 把 school/cat 分发给对应脚本；
// 脚本失败只记 WARN（不 panic/不向上抛），且不影响后续任务继续分发。
func TestDispatchSchoolCatAndFailureWarnsOnly(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	f.err = errors.New("boom boom")
	s := New(Config{})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	s.dispatch(context.Background(), taskSchool)
	if f.runN != 1 || f.lastArgs[0] != "scripts/school_open_day_2026.py" {
		t.Errorf("dispatch(school) 未执行: runN=%d last=%v", f.runN, f.lastArgs)
	}
	s.dispatch(context.Background(), taskCat)
	if f.runN != 2 || f.lastArgs[0] != "scripts/task_runner.py" {
		t.Errorf("dispatch(cat) 未执行: runN=%d last=%v", f.runN, f.lastArgs)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "scripts/school_open_day_2026.py") {
		t.Errorf("school 失败未按 WARN 记录:\n%s", out)
	}
	if !strings.Contains(out, "scripts/task_runner.py") {
		t.Errorf("cat 失败未按 WARN 记录:\n%s", out)
	}
}

// TestPythonCmd WB2A_PYTHON 覆盖解释器名：缺省/空白回落 "python3"（保持
// 容器与既有测试的行为不变），显式设置时取其值（Windows 等仅有 python 的环境）。
func TestPythonCmd(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	if got := pythonCmd(); got != "python3" {
		t.Errorf("default pythonCmd()=%q want python3", got)
	}

	t.Setenv("WB2A_PYTHON", "   ")
	if got := pythonCmd(); got != "python3" {
		t.Errorf("blank pythonCmd()=%q want python3", got)
	}

	t.Setenv("WB2A_PYTHON", "python")
	if got := pythonCmd(); got != "python" {
		t.Errorf("override pythonCmd()=%q want python", got)
	}

	t.Setenv("WB2A_PYTHON", "  /usr/bin/python3.10  ")
	if got := pythonCmd(); got != "/usr/bin/python3.10" {
		t.Errorf("trim pythonCmd()=%q want /usr/bin/python3.10", got)
	}
}
