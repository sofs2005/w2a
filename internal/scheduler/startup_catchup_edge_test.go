// startup_catchup_edge_test.go 跨重启补跑的边界与端到端用例（与
// startup_catchup_test.go 互补）。
//
// 为什么单独一份而不并进 startup_catchup_test.go：这里的 helper 全部自带 qa 前缀、
// 自带构造器与 fake server，**刻意不复用**那边的 newActivityCatchUpScheduler /
// dispatchRecorder / installRecordingExec。价值在于"换一套假设再跑一遍"——复用被测
// 方写的脚手架，会把它的隐含前提一起继承下来（例如"折叠一定生效"）。只复用仓库
// 既有、且本特性未触碰的公共 helper：cstZone（travel.go）、reportStub /
// fastActivity（activity_test.go）、captureLog（travel_test.go）。
//
// 覆盖：生产实景（槽位早于启动时刻）/ 幂等（同 now 二次启动不重复）/ 跨天多类 +
// 同类折叠 / 状态文件健壮性（空、半截 JSON、版本不符、零值年份）/ 未来时间戳 /
// 写入失败降级 / StatePath 为空零回归 / 相对路径 / Run 端到端接线 / 热重载不丢 StatePath。
package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// qaSeed 固定部署种子：scheduledSlot 是 (seed, 日期, 小时, 类型) 的纯函数，
// 固定后偏移可复现，断言才能写死"槽位落在 10:00–10:26"。
const qaSeed = "qa-acceptance-seed-2026-10"

// ────────────────────────── 通用 helper ──────────────────────────

func qaStatePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), StateFileName)
}

// qaPutState 预置状态文件（走生产同一条写入路径 saveLastWake）。
func qaPutState(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := saveLastWake(path, at); err != nil {
		t.Fatalf("预置状态文件失败: %v", err)
	}
}

// qaRawState 读状态文件原文（断言"文件里到底写了什么"，不经过任何内存结构）。
func qaRawState(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读状态文件 %s 失败: %v", path, err)
	}
	return string(b)
}

// qaStateDoc 把状态文件原文解析成落盘结构（连 version 一起断言）。
func qaStateDoc(t *testing.T, path string) schedulerState {
	t.Helper()
	var st schedulerState
	if err := json.Unmarshal([]byte(qaRawState(t, path)), &st); err != nil {
		t.Fatalf("状态文件不是合法 JSON: %v", err)
	}
	return st
}

// qaStateLastWake 从文件原文取 last_wake 时刻（解析失败即 Fatal）。
func qaStateLastWake(t *testing.T, path string) time.Time {
	t.Helper()
	st := qaStateDoc(t, path)
	at, err := time.Parse(time.RFC3339, st.LastWake)
	if err != nil {
		t.Fatalf("last_wake=%q 不是 RFC3339: %v", st.LastWake, err)
	}
	return at
}

// qaActivityScheduler 只开活跃上报（其余六类禁用）：reportStub 计 /v2/report 次数，
// 因此"派发了几轮活跃上报"== stub.calls，不会被其它任务族的调用污染。
func qaActivityScheduler(t *testing.T, statePath string, mutate func(*Config)) (*Scheduler, *reportStub) {
	t.Helper()
	fastActivity(t) // 关掉账号间 / 条间间隔，测试不白等
	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	cfg := Config{
		Pool:               p,
		Upstream:           up,
		ActivityHours:      []int{10},
		RandomDelayMinutes: 0,
		RandomDelaySeed:    qaSeed,
		StatePath:          statePath,
		CheckinDisabled:    true,
		TravelDisabled:     true,
		KeepaliveDisabled:  true,
		CatDisabled:        true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(cfg), stub
}

// qaChdir 把 CWD 切到 dir（相对 StatePath 用例需要），测试结束还原。
// 本包内没有 t.Parallel()，串行执行下 chdir 安全。
func qaChdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// qaRunAndWaitLog 起一个 goroutine 跑 Run，等日志出现 want 后取消 ctx 并等 Run 返回。
// 既证明"进程能起来进主循环"，也证明"ctx 取消后干净退出"（不 hang）。
func qaRunAndWaitLog(t *testing.T, s *Scheduler, logs func() string, want string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs(), want) {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("Run 未在 10s 内打出 %q，日志:\n%s", want, logs())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("ctx 取消后 Run 未退出（卡在补跑或等待里），日志:\n%s", logs())
	}
}

// ────────────────────────── 脚本类 fake ──────────────────────────

// qaScriptRecorder 记录脚本类任务（cat / growth / limited）的调用顺序，不真跑 python3。
type qaScriptRecorder struct {
	mu  sync.Mutex
	seq []string
}

func (r *qaScriptRecorder) add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq = append(r.seq, name)
}

func (r *qaScriptRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.seq))
	copy(out, r.seq)
	return out
}

type qaFakeScript struct {
	rec   *qaScriptRecorder
	label string
}

func (f *qaFakeScript) SetDir(string) {}

func (f *qaFakeScript) Run() error {
	f.rec.add(f.label)
	return nil
}

// qaInstallScript 替换脚本执行器（测试结束还原）。label 按参数区分三类脚本入口：
// cat 带 --only black_cat，limited 带 --limited，其余 task_runner.py 即 growth。
func qaInstallScript(t *testing.T, rec *qaScriptRecorder) {
	t.Helper()
	orig := newScriptCmd
	newScriptCmd = func(name string, args ...string) scriptRunner {
		label := "growth"
		for _, a := range args {
			switch a {
			case "black_cat":
				label = "cat"
			case "--limited":
				label = "limited"
			}
		}
		return &qaFakeScript{rec: rec, label: label}
	}
	t.Cleanup(func() { newScriptCmd = orig })
}

// qaMultiKindServer 同时模拟签到（/daily-checkin + 余额）与保活（/token/refresh），
// 并把"真的打了上游的那一类"按到达顺序记进 recorder。
func qaMultiKindServer(rec *qaScriptRecorder) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			rec.add("checkin")
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":` +
				`[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			rec.add("keepalive")
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
}

// ─────────────── B1：生产实景（activity 槽位早于进程启动时刻） ───────────────

// TestQAProdActivitySlotReplayedAfterRestart 复刻线上故障：
// 容器 2026-10-27 12:49:38 CST 启动，activity_hours=[10] + random_delay_minutes=26
// → 槽位落在 10:00–10:26，早于启动时刻；last_wake = 前一天 22:14:40（保活）。
// 断言：活跃上报被派发**恰好一次**，且状态文件的 last_wake 推进到该槽位（读文件原文）。
func TestQAProdActivitySlotReplayedAfterRestart(t *testing.T) {
	path := qaStatePath(t)
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, cstZone)
	qaPutState(t, path, time.Date(2026, 10, 26, 22, 14, 40, 0, cstZone))

	s, stub := qaActivityScheduler(t, path, func(c *Config) {
		c.RandomDelayMinutes = 26 // 与生产 config.json 一致
	})
	slot := s.scheduledSlot(day, 10, taskActivity)
	now := time.Date(2026, 10, 27, 12, 49, 38, 0, cstZone)

	// 前提校验：槽位必须真的落在 10:00–10:26 且早于启动时刻，否则这个用例
	// 复刻的不是线上实景（比如 seed 变了导致偏移出窗口）。
	if slot.Hour() != 10 || slot.Minute() > 26 {
		t.Fatalf("前提不成立：10 点槽位=%s 不在 10:00–10:26 窗口内", slot.Format("15:04:05"))
	}
	if !slot.Before(now) {
		t.Fatalf("前提不成立：槽位 %s 不早于启动时刻 %s", slot.Format(time.RFC3339), now.Format(time.RFC3339))
	}

	s.startupCatchUpAt(context.Background(), now)

	if n := stub.calls.Load(); n != 1 {
		t.Fatalf("活跃上报调用=%d want 恰好 1（%s 的槽位应被补跑一次，这正是线上丢的那一次）",
			n, slot.Format("15:04:05"))
	}
	st := qaStateDoc(t, path)
	if st.Version != schedulerStateVersion {
		t.Errorf("version=%d want %d", st.Version, schedulerStateVersion)
	}
	if got := qaStateLastWake(t, path); !got.Equal(slot) {
		t.Errorf("状态文件 last_wake=%s want %s（必须推进到刚补的槽位，而不是启动时刻）",
			got.Format(time.RFC3339), slot.Format(time.RFC3339))
	}
}

// ────────────────────── B2：幂等 / 不重复补跑 ──────────────────────

// TestQAStartupCatchUpIsIdempotent 同一个 now 连续补跑两次：第二次零派发，
// 且状态文件一个字节都不许变（不能被回退，也不能被改写）。
func TestQAStartupCatchUpIsIdempotent(t *testing.T) {
	path := qaStatePath(t)
	qaPutState(t, path, time.Date(2026, 10, 26, 22, 14, 40, 0, cstZone))
	s, stub := qaActivityScheduler(t, path, nil)
	now := time.Date(2026, 10, 27, 12, 49, 38, 0, cstZone)

	if got := s.startupCatchUpAt(context.Background(), now); got.IsZero() {
		t.Fatal("第一次补跑报告没有补任何槽位（前提不成立）")
	}
	firstCalls := stub.calls.Load()
	before := qaRawState(t, path)

	if got := s.startupCatchUpAt(context.Background(), now); !got.IsZero() {
		t.Errorf("第二次补跑返回 %s want 零值（同一 now 下不该再补）", got.Format(time.RFC3339))
	}
	if n := stub.calls.Load(); n != firstCalls {
		t.Errorf("第二次补跑派发了 %d 次（累计 %d），want 0（累计 %d）", n-firstCalls, n, firstCalls)
	}
	if after := qaRawState(t, path); after != before {
		t.Errorf("状态文件被改写：\nbefore=%s\nafter =%s", before, after)
	}
}

// ─────────── B3：跨天多类（升序）+ 同类折叠（昨天的签到不被补） ───────────

// TestQAStartupCatchUpCrossDayMultipleKinds 停机一夜后启动：
// checkin_hours=[9,21]、keepalive_hours=[22]、cat_hours=[1]，
// last_wake=昨天 20:00 → 今天 08:00 启动 → 三类各一次，且按槽位时刻升序。
// 昨天 09:00 的签到在 last_wake 之前，不得补跑（否则等于把昨天整天的任务重放一遍）。
func TestQAStartupCatchUpCrossDayMultipleKinds(t *testing.T) {
	rec := &qaScriptRecorder{}
	qaInstallScript(t, rec) // cat 走脚本：登记而不真跑 python3
	srv := qaMultiKindServer(rec)
	t.Cleanup(srv.Close)

	path := qaStatePath(t)
	qaPutState(t, path, time.Date(2026, 10, 26, 20, 0, 0, 0, cstZone))

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool:               p,
		Upstream:           up,
		CheckinHours:       []int{9, 21},
		KeepaliveHours:     []int{22},
		CatHours:           []int{1},
		RandomDelayMinutes: 0,
		RandomDelaySeed:    qaSeed,
		StatePath:          path,
		TravelDisabled:     true,
		ActivityDisabled:   true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	})

	s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 8, 0, 0, 0, cstZone))

	want := []string{"checkin", "keepalive", "cat"}
	got := rec.list()
	if len(got) != len(want) {
		t.Fatalf("派发顺序=%v want %v（三类各一次，按槽位时刻升序）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("派发顺序=%v want %v（必须 21:00 签到 → 22:00 保活 → 01:00 夜猫）", got, want)
		}
	}
	// 签到只跑一次 = 昨天 09:00 的签到没有被单独补跑（被 21:00 折叠/被窗口挡掉）。
	if n := countStr(got, "checkin"); n != 1 {
		t.Errorf("签到派发 %d 次 want 1", n)
	}
	// 最后一批是今天 01:00 的夜猫：last_wake 必须停在它上面。
	if v := qaStateLastWake(t, path); !v.Equal(time.Date(2026, 10, 27, 1, 0, 0, 0, cstZone)) {
		t.Errorf("last_wake=%s want 2026-10-27T01:00:00+08:00", v.Format(time.RFC3339))
	}
}

// TestQALatestPerKindFoldsYesterdayMorningSlot 折叠语义本身：
// 窗口里同时有昨天 09:00 与 21:00 的签到（跨 24h 窗口，直接喂 missedSlots 绕开
// 12h lookback 上限），latestPerKind 必须只留 21:00，丢掉 09:00。
func TestQALatestPerKindFoldsYesterdayMorningSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:       []int{9, 21},
		RandomDelayMinutes: 0,
		RandomDelaySeed:    qaSeed,
		TravelDisabled:     true,
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		CatDisabled:        true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	})
	from := time.Date(2026, 10, 26, 8, 0, 0, 0, cstZone)
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, cstZone)

	raw := s.missedSlots(from, now)
	if n := countKind(raw, taskCheckin); n != 2 {
		t.Fatalf("原始候选里签到 %d 个 want 2（昨天 09:00 与 21:00），missed=%v", n, kindsOf(raw))
	}
	folded := latestPerKind(raw)
	if n := countKind(folded, taskCheckin); n != 1 {
		t.Fatalf("折叠后签到 %d 个 want 1", n)
	}
	for _, m := range folded {
		if m.kind == taskCheckin && !m.at.Equal(time.Date(2026, 10, 26, 21, 0, 0, 0, cstZone)) {
			t.Errorf("保留下来的签到槽位=%s want 昨天 21:00（09:00 应被折叠掉）", m.at.Format("01-02 15:04"))
		}
	}
}

// ────────────────────── B4：状态文件健壮性 ──────────────────────

// TestQACorruptStateFileNoReplayAndRunStarts 损坏/缺字段/版本不支持的状态文件：
// 不补跑、不 panic，且 **Run 能起来进主循环并在 ctx 取消后干净退出**。
func TestQACorruptStateFileNoReplayAndRunStarts(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"空文件", ""},
		{"半截 JSON", `{"version":1,"last_wake":"2026-10-26T22:1`},
		{"version=99", `{"version":99,"last_wake":"2026-10-26T22:14:40+08:00"}`},
		{"字段缺失", `{"version":1}`},
		{"非 RFC3339", `{"version":1,"last_wake":"2026-10-26 22:14:40"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := qaStatePath(t)
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatalf("写状态文件失败: %v", err)
			}
			s, stub := qaActivityScheduler(t, path, nil)
			before := qaRawState(t, path)

			// ① 注入固定 now：损坏状态下不得补跑（宁可漏一次，不可乱打上游）。
			s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 12, 49, 38, 0, cstZone))
			if n := stub.calls.Load(); n != 0 {
				t.Errorf("活跃上报调用=%d want 0（状态不可信时不补跑）", n)
			}
			if after := qaRawState(t, path); after != before {
				t.Errorf("没补跑却改写了状态文件:\nbefore=%q\nafter =%q", before, after)
			}

			// ② 真的把 Run 跑起来：不 panic、进主循环、ctx 取消后退出。
			logs := captureLog(t)
			qaRunAndWaitLog(t, s, logs, "[sched] next wake")
			if n := stub.calls.Load(); n != 0 {
				t.Errorf("Run 起来后仍派发了 %d 次上报 want 0", n)
			}
		})
	}
}

// TestQAZeroTimeLastWakeRejectedAsCorrupt 零值 / 异常年份的 last_wake 一律按
// 「损坏」处理：不补跑，状态文件一个字节都不许动。
//
// 背景（QA 建议 #1，工程师已采纳）：`0001-01-01T00:00:00Z` 是**合法 RFC3339**
// （time.Time 的零值就编码成这样，运维手改文件也会写成这样），光靠 Parse 判不出来。
// 若采信它 → 被当成"很久以前处理过" → 补跑窗口被 lookback 夹到最近 12 小时 →
// **每次启动**都把窗口内的槽位重打一遍上游，与 persist.go 开头声明的"读坏就按
// 没有历史处理、不补跑"正好相反。Year() < 2000 同理：本项目 2026 年才上线，
// 真实部署的槽位时刻不可能早于 2000 年。
//
// 本用例上轮锁定的是相反行为（旧实现接受零值并补跑），本轮随实现反转。断言三条，
// 缺一不可：
//  1. loadLastWake 返回 ok=false（判为损坏，而非"陈旧历史"）；
//  2. 零派发（不可信的历史不得驱动补跑）；
//  3. 状态文件原文逐字节不变（损坏内容原样留给运维排查，不许被"自愈"覆盖）。
func TestQAZeroTimeLastWakeRejectedAsCorrupt(t *testing.T) {
	cases := []struct{ name, content string }{
		{"time.Time 零值", `{"version":1,"last_wake":"0001-01-01T00:00:00Z"}`},
		{"异常年份 1999", `{"version":1,"last_wake":"1999-12-31T23:59:59+08:00"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := qaStatePath(t)
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatalf("写状态文件失败: %v", err)
			}
			before := qaRawState(t, path)

			// ① 判为损坏。
			if at, ok := loadLastWake(path); ok {
				t.Fatalf("loadLastWake=%s ok=true want ok=false（零值/异常年份应按损坏处理）",
					at.Format(time.RFC3339))
			}

			// ② 零派发：即便区间内确实有槽位（10:00 活跃上报），也不许补。
			s, stub := qaActivityScheduler(t, path, nil)
			logs := captureLog(t)
			got := s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 12, 49, 38, 0, cstZone))
			if !got.IsZero() {
				t.Errorf("startupCatchUpAt=%s want 零值（不可信的历史不补跑）", got.Format(time.RFC3339))
			}
			if n := stub.calls.Load(); n != 0 {
				t.Errorf("活跃上报调用=%d want 0（零值/异常年份不得驱动补跑）", n)
			}

			// ③ 状态文件字节不变。
			if after := qaRawState(t, path); after != before {
				t.Errorf("状态文件被改写（损坏内容应原样保留）:\nbefore=%q\nafter =%q", before, after)
			}

			// 留一行可解释的 WARN：否则运维只会看到"没补跑"而查不出原因。
			if !strings.Contains(logs(), "零值/异常年份") {
				t.Errorf("应留一行说明零值/异常年份的 WARN，日志:\n%s", logs())
			}
		})
	}
}

// ────────────────────── B5：未来时间戳 ──────────────────────

// TestQAFutureLastWakeSkippedAndFileIntact last_wake 在未来（时钟回拨 / TZ 变更 /
// 手改文件）：不补跑，且状态文件**一个字节都不许被写坏**（这是唯一能自愈的线索，
// 运维手改错时还能再改回来）。
func TestQAFutureLastWakeSkippedAndFileIntact(t *testing.T) {
	path := qaStatePath(t)
	now := time.Date(2026, 10, 27, 12, 49, 0, 0, cstZone)
	qaPutState(t, path, now.Add(2*time.Hour))
	before := qaRawState(t, path)

	s, stub := qaActivityScheduler(t, path, nil)
	logs := captureLog(t)
	if got := s.startupCatchUpAt(context.Background(), now); !got.IsZero() {
		t.Errorf("startupCatchUpAt=%s want 零值（last_wake 在未来）", got.Format(time.RFC3339))
	}
	if n := stub.calls.Load(); n != 0 {
		t.Errorf("活跃上报调用=%d want 0", n)
	}
	if after := qaRawState(t, path); after != before {
		t.Errorf("状态文件被改写:\nbefore=%s\nafter =%s", before, after)
	}
	if !strings.Contains(logs(), "不早于当前时刻") {
		t.Errorf("应留一行可解释的 WARN，日志:\n%s", logs())
	}
}

// ────────────────────── B6：写入失败降级 ──────────────────────

// TestQAPersistFailureWarnsOnceAndSchedulingContinues 状态文件写不进去
// （父路径是普通文件 → MkdirAll ENOTDIR）：只 WARN 一次、不 panic、调度继续。
func TestQAPersistFailureWarnsOnceAndSchedulingContinues(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备阻塞文件失败: %v", err)
	}
	badPath := filepath.Join(blocker, "data", StateFileName) // 父路径是普通文件

	s, _ := qaActivityScheduler(t, badPath, nil)
	logs := captureLog(t)
	for i := 0; i < 5; i++ {
		s.persistLastWake(time.Date(2026, 10, 27, 10+i, 0, 0, 0, cstZone))
	}
	if n := strings.Count(logs(), "调度状态落盘失败"); n != 1 {
		t.Errorf("落盘失败 WARN=%d 次 want 1（否则日志会被刷屏），日志:\n%s", n, logs())
	}
	// 父路径是普通文件 → stat 返回 ENOTDIR（不是 IsNotExist），两者都算"没有文件"。
	if _, err := os.Stat(badPath); err == nil {
		t.Errorf("不该有文件被创建：%s", badPath)
	}

	// 调度继续：Run 仍能进主循环并干净退出。
	qaRunAndWaitLog(t, s, logs, "[sched] next wake")
}

// TestQAReloadKeepsStatePathAndWarnOnce 面板保存配置（热重载）不得把 StatePath
// 弄丢、也不得重置 stateWarnOnce 导致 WARN 刷屏。
func TestQAReloadKeepsStatePathAndWarnOnce(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备阻塞文件失败: %v", err)
	}
	badPath := filepath.Join(blocker, "data", StateFileName)

	s, _ := qaActivityScheduler(t, badPath, nil)
	logs := captureLog(t)
	s.persistLastWake(time.Date(2026, 10, 27, 10, 0, 0, 0, cstZone))

	// 模拟一次面板保存：装配层重新组装 Config 后 Reload（与 configreload.go 同一路径）。
	s.Reload(Config{
		ActivityHours:      []int{11},
		RandomDelayMinutes: 26,
		RandomDelaySeed:    "changed-seed",
		StatePath:          badPath,
		CheckinDisabled:    true,
		TravelDisabled:     true,
		KeepaliveDisabled:  true,
		CatDisabled:        true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	})

	if got := s.snapshot().StatePath; got != badPath {
		t.Errorf("热重载后 StatePath=%q want %q（丢了就等于特性被静默关闭）", got, badPath)
	}
	s.persistLastWake(time.Date(2026, 10, 27, 11, 0, 0, 0, cstZone))
	if n := strings.Count(logs(), "调度状态落盘失败"); n != 1 {
		t.Errorf("热重载后落盘失败 WARN=%d 次 want 1（stateWarnOnce 不该被重置）", n)
	}
}

// ────────────────── B7：StatePath 为空 = 零回归 ──────────────────

// TestQAEmptyStatePathLeavesNoTrace StatePath="" 时 Run 不创建任何文件、
// 不补跑，行为与引入持久化之前一致（在空目录里跑，断言目录仍为空）。
func TestQAEmptyStatePathLeavesNoTrace(t *testing.T) {
	dir := t.TempDir()
	qaChdir(t, dir) // 任何相对路径落盘都会落在 dir 里，躲不掉

	s, stub := qaActivityScheduler(t, "", nil)
	logs := captureLog(t)
	qaRunAndWaitLog(t, s, logs, "[sched] next wake")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("StatePath 为空时不该落任何文件，实际 %v", names)
	}
	if n := stub.calls.Load(); n != 0 {
		t.Errorf("活跃上报调用=%d want 0（特性关闭，不做启动补跑）", n)
	}
	if strings.Contains(logs(), "startup catch-up") {
		t.Errorf("特性关闭时不该打 startup catch-up 日志:\n%s", logs())
	}
}

// ────────────── B8：StatePath 相对路径（生产就是这样派生的） ──────────────

// TestQARelativeStatePath data/scheduler_state.json（相对 CWD）：
// data/ 已存在时能正常读写；data/ 不存在时 MkdirAll 能建出来。
func TestQARelativeStatePath(t *testing.T) {
	at := time.Date(2026, 10, 27, 10, 0, 0, 0, cstZone)

	t.Run("data 已存在", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		qaChdir(t, dir)

		s := New(Config{StatePath: filepath.Join("data", StateFileName)})
		s.persistLastWake(at)

		got, ok := loadLastWake(filepath.Join("data", StateFileName))
		if !ok || !got.Equal(at) {
			t.Fatalf("loadLastWake=%s ok=%v want %s", got.Format(time.RFC3339), ok, at.Format(time.RFC3339))
		}
	})

	t.Run("data 不存在时自动创建", func(t *testing.T) {
		dir := t.TempDir()
		qaChdir(t, dir)

		s := New(Config{StatePath: filepath.Join("data", StateFileName)})
		s.persistLastWake(at)

		if st, err := os.Stat(filepath.Join("data", StateFileName)); err != nil {
			t.Fatalf("MkdirAll 未把 data/ 建出来: %v", err)
		} else if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("状态文件权限=%o want 600", perm)
		}
		got, ok := loadLastWake(filepath.Join("data", StateFileName))
		if !ok || !got.Equal(at) {
			t.Fatalf("loadLastWake=%s ok=%v want %s", got.Format(time.RFC3339), ok, at.Format(time.RFC3339))
		}
	})
}

// ────────────── Run 端到端接线（工程师的用例没覆盖这一层） ──────────────

// TestQARunDispatchesStartupCatchUp 真的调 Run（而不是 startupCatchUpAt）：
// 启动前已经过去的槽位必须由 Run 自己补跑一次，落盘推进，然后打 next wake 进主循环。
// 这条覆盖"接线"，防止 persist/startupCatchUp 写对了但 Run 里忘了调。
func TestQARunDispatchesStartupCatchUp(t *testing.T) {
	path := qaStatePath(t)
	now := time.Now()
	slot := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())
	if slot.After(now) {
		t.Fatalf("前提不成立：整点槽位 %s 晚于现在 %s", slot, now)
	}
	// last_wake 落在"本小时槽位"之前 10 分钟 → 窗口内正好有这个槽位。
	qaPutState(t, path, slot.Add(-10*time.Minute))

	s, stub := qaActivityScheduler(t, path, func(c *Config) {
		c.ActivityHours = []int{now.Hour()} // 只用当前小时，槽位必在最近一小时内
	})
	logs := captureLog(t)
	qaRunAndWaitLog(t, s, logs, "[sched] next wake")

	if n := stub.calls.Load(); n != 1 {
		t.Fatalf("活跃上报调用=%d want 1（Run 内部必须补跑启动前过去的槽位，且不重复）", n)
	}
	if got := qaStateLastWake(t, path); !got.Equal(slot) {
		t.Errorf("状态文件 last_wake=%s want %s", got.Format(time.RFC3339), slot.Format(time.RFC3339))
	}
	// 可观测性：每次重算都要有一行 next wake，且带 kinds。
	if !strings.Contains(logs(), "kinds=[activity]") {
		t.Errorf("缺少 `[sched] next wake ... kinds=[activity]` 日志，实际:\n%s", logs())
	}
	// 启动补跑本身要留痕（排障时"到底补了没补"必须能从日志判断）。
	if !strings.Contains(logs(), "startup catch-up") {
		t.Errorf("缺少 startup catch-up 日志，实际:\n%s", logs())
	}
}

// ────────────────────────── 小工具 ──────────────────────────

func countStr(xs []string, want string) int {
	n := 0
	for _, x := range xs {
		if x == want {
			n++
		}
	}
	return n
}

func countKind(ms []missedSlot, want taskKind) int {
	n := 0
	for _, m := range ms {
		if m.kind == want {
			n++
		}
	}
	return n
}

// 编译期确认：qaFakeScript 满足 scriptRunner（新增方法时立刻暴露）。
var _ scriptRunner = (*qaFakeScript)(nil)
