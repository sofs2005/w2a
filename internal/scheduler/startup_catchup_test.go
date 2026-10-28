// startup_catchup_test.go 跨重启补跑（startupCatchUp）与调度状态落盘（persist.go）的测试。
//
// 这一组测试修的是线上实案的根因：容器 2026-10-27 12:49:38 CST 创建，而
// activity_hours=[10]（random_delay_minutes=26，实际槽位落在 10:00–10:26）——
// 槽位时刻早于进程启动时刻，nextWake 只挑 at.After(now) 的时点，于是当天永久丢失，
// docker logs 里 activity 相关行一条都没有。修复后靠落盘的 last_wake 反推出
// "停机期间过去了哪些槽位"，启动时补跑。
//
// 风格对齐 catchup_test.go：RandomDelayMinutes 固定 0（槽位落整点、可断言），
// 时区固定 cstZone；需要上游的地方用既有 stub（reportStub / fakeUpstream），
// 需要脚本的地方替换 newScriptCmd（不真跑 python3）。
package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// statePathIn 返回临时目录下的调度状态文件路径。
func statePathIn(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), StateFileName)
}

// writeStateRaw 直接写状态文件原文（模拟损坏 / 手工编辑 / 旧版本）。
func writeStateRaw(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写状态文件失败: %v", err)
	}
}

// readStateRaw 读回状态文件原文（断言"坏历史没被改写成别的东西"）。
func readStateRaw(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读状态文件失败: %v", err)
	}
	return string(raw)
}

// readStateLastWake 读回落盘的 last_wake（断言"推进到了哪个槽位"）。
func readStateLastWake(t *testing.T, path string) time.Time {
	t.Helper()
	at, ok := loadLastWake(path)
	if !ok {
		t.Fatalf("状态文件 %s 不可读或不存在", path)
	}
	return at
}

// tmpLeftovers 返回状态文件同目录下残留的临时文件（`<name>.tmp.<pid>`）。
//
// 按前缀匹配而不是写死名字：tmp 名带 pid（见 saveLastWake），写死一个名字的断言
// 会因为"名字对不上"而永远通过，等于没有断言。
func tmpLeftovers(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	prefix := filepath.Base(path) + ".tmp"
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// newActivityCatchUpScheduler 构造"只开活跃上报"的调度器（其余六类禁用），
// 用 reportStub 统计实际补跑了几轮上报。
func newActivityCatchUpScheduler(t *testing.T, statePath string, mutate func(*Config)) (*Scheduler, *reportStub) {
	t.Helper()
	fastActivity(t) // 关掉账号间/条间间隔，避免测试白等
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

// dispatchRecorder 记录各任务类实际执行的先后顺序（跨批次顺序断言用）。
// 批内多类是并行 goroutine，但本文件的多类用例每批只有一类，顺序确定。
type dispatchRecorder struct {
	mu  sync.Mutex
	seq []string
}

func (r *dispatchRecorder) add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq = append(r.seq, name)
}

func (r *dispatchRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.seq))
	copy(out, r.seq)
	return out
}

// recordingScript 替代 python3 子进程：只登记"哪类脚本被调起"，不真执行。
type recordingScript struct {
	rec   *dispatchRecorder
	label string
}

func (r *recordingScript) SetDir(string) {}

func (r *recordingScript) Run() error {
	r.rec.add(r.label)
	return nil
}

// scriptLabel 把一条脚本命令折成任务类名：三个脚本类任务共用 task_runner.py，
// 靠参数区分（见 school.go 的三个入口）——cat 是 `--only black_cat`，
// limited 是 `--limited`，其余即 growth 全量补跑。
func scriptLabel(program string, args []string) string {
	for _, a := range args {
		switch a {
		case "black_cat":
			return "cat"
		case "--limited":
			return "limited"
		}
	}
	if strings.Contains(program+strings.Join(args, " "), "task_runner.py") {
		return "growth"
	}
	return program
}

// installRecordingExec 把脚本执行器换成登记版（测试结束还原）。
func installRecordingExec(t *testing.T, rec *dispatchRecorder) {
	t.Helper()
	orig := newScriptCmd
	newScriptCmd = func(name string, args ...string) scriptRunner {
		return &recordingScript{rec: rec, label: scriptLabel(name, args)}
	}
	t.Cleanup(func() { newScriptCmd = orig })
}

// multiKindServer 同时模拟 billing（签到/余额/刷新）端点，并把"哪类任务真的打了上游"
// 记进顺序表。签到走 /daily-checkin、保活走 /token/refresh。
func multiKindServer(rec *dispatchRecorder) *httptest.Server {
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
			http.Error(w, "not found", 404)
		}
	}))
}

// checkinActivityServer 同时模拟签到（/daily-checkin）与活跃上报（/v2/report），
// 其余路径一律回 200 空数据（活跃上报的 streak 自检等辅助调用失败也不影响主流程，
// 这里直接给成功响应，让断言只盯着"哪类任务被派发过"）。
//
// 为什么需要它：验证"启动补跑批次跨过的槽位"必须让一次启动里同时有签到与活跃两类，
// 且能分别计数（multiKindServer 只覆盖签到/保活/余额，没有 /v2/report）。
func checkinActivityServer(rec *dispatchRecorder) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			rec.add("checkin")
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case r.URL.Path == "/v2/report":
			rec.add("activity")
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":` +
				`[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		default:
			w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
}

// TestStartupCatchUpNoStateFileIsSilent 首次部署（无状态文件）不补跑、不写文件、不 panic。
//
// 首次部署没有可信的历史，无从判断"哪些槽位该补"；猜一个保守值继续补只会在新装
// 环境里把当天的槽位提前跑一遍。宁可不补：最坏漏一次，明天照常。
func TestStartupCatchUpNoStateFileIsSilent(t *testing.T) {
	path := statePathIn(t)
	s, stub := newActivityCatchUpScheduler(t, path, nil)

	s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 12, 49, 38, 0, cstZone))

	if n := stub.calls.Load(); n != 0 {
		t.Errorf("上报调用=%d want 0（首次部署不该补跑）", n)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("首次部署不该写状态文件（err=%v）", err)
	}
}

// TestStartupCatchUpCorruptStateFileIsSilent 状态文件损坏的三种形态都不补跑、不 panic。
//
// 损坏时的共同处理是"按首次部署"：读到半截 JSON 时任何猜测都可能把补跑区间算错，
// 而算错的方向是"多打上游"——比不补更糟（会被上游当异常行为）。
func TestStartupCatchUpCorruptStateFileIsSilent(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"截断 JSON", `{"version":1,"last_wake":"2026-10-26T22:1`},
		{"空文件", ""},
		{"last_wake 非 RFC3339", `{"version":1,"last_wake":"2026-10-26 22:14:40"}`},
		{"版本不支持", `{"version":99,"last_wake":"2026-10-26T22:14:40+08:00"}`},
		{"last_wake 缺失", `{"version":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := statePathIn(t)
			writeStateRaw(t, path, c.content)
			s, stub := newActivityCatchUpScheduler(t, path, nil)

			// 不 panic 即为通过的一半；另一半是"没有因此多打上游"。
			s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 12, 49, 38, 0, cstZone))

			if n := stub.calls.Load(); n != 0 {
				t.Errorf("上报调用=%d want 0（状态不可信时不补跑）", n)
			}
		})
	}
}

// TestStartupCatchUpRejectsZeroAndAbsurdYearLastWake 零值/异常年份的 last_wake
// 按"损坏"处理：不补跑、不派发。
//
// 为什么单独立一条用例：`0001-01-01T00:00:00Z` 是 time.Time 的零值编码，也是
// **合法 RFC3339**——光靠 time.Parse 判不出来，于是它会被当成"很久以前处理过"，
// 补跑窗口被 lookback 夹到最近 12 小时 → 每次启动都把窗口内的槽位补一遍。这与
// persist.go 开头的声明（手工编辑过 → 按没有历史处理 → 不补跑）正好相反。
// `1900-01-01T00:00:00+08:00` 覆盖"带时区的早期年份"：项目 2026 年才上线，
// 任何真实槽位都不可能早于 2000 年，出现即是写坏了。
func TestStartupCatchUpRejectsZeroAndAbsurdYearLastWake(t *testing.T) {
	cases := []struct {
		name     string
		lastWake string
	}{
		{"零值时间戳", "0001-01-01T00:00:00Z"},
		{"1900 年带时区", "1900-01-01T00:00:00+08:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := statePathIn(t)
			writeStateRaw(t, path, `{"version":1,"last_wake":"`+c.lastWake+`"}`)

			// ① 读取层：直接判为不可信，调用方因此拿不到"历史"。
			if at, ok := loadLastWake(path); ok {
				t.Fatalf("loadLastWake(%q)=%s,ok want 不可信（零值/异常年份不该被当成可信历史）",
					c.lastWake, at.Format(time.RFC3339))
			}

			// ② 补跑层：即使窗口内确实有槽位（10:00 已过），也一个都不派发。
			s, stub := newActivityCatchUpScheduler(t, path, nil)
			got := s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 12, 49, 38, 0, cstZone))
			if !got.IsZero() {
				t.Errorf("startupCatchUpAt=%s want 零值（不可信的历史不产生补跑）", got.Format(time.RFC3339))
			}
			if n := stub.calls.Load(); n != 0 {
				t.Errorf("上报调用=%d want 0（零值/异常年份不补跑）", n)
			}
			// ③ 也不该把这份"坏历史"改写成别的东西：运维手改错时还能再改回来。
			if raw := readStateRaw(t, path); raw != `{"version":1,"last_wake":"`+c.lastWake+`"}` {
				t.Errorf("状态文件被改写: %q", raw)
			}
		})
	}
}

// TestPersistLastWakeSkippedWhenCtxCancelled 优雅停机时不把半截批次标记为完成。
//
// 场景：runBatch 在 ctx 取消时提前返回（批次只跑了一部分），若照常落盘，这个槽位
// 就被标记成"已完成"，剩下没跑的账号要等下一个自然日才被处理——方向与"宁可重复
// 一次，不可漏掉一天"正好相反。所以 ctx 已取消时必须停在**上一个**真正跑完的槽位。
//
// 走 persistLastWakeIfRunning（Run 主循环调用的就是它）：Run 里的落盘点在 waitSlot
// 之后，测试不等待真实槽位就走不到那里，抽出来的这个小函数就是为了让这条行为可测
// （不给 Run 加时钟注入参数）。
func TestPersistLastWakeSkippedWhenCtxCancelled(t *testing.T) {
	path := statePathIn(t)
	s := New(Config{StatePath: path})

	done := time.Date(2026, 10, 27, 10, 0, 0, 0, cstZone) // 上一个真正跑完的槽位
	half := time.Date(2026, 10, 27, 11, 0, 0, 0, cstZone) // 半截批次所在的槽位
	s.persistLastWake(done)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 停机信号：runBatch 已提前返回
	s.persistLastWakeIfRunning(ctx, half)

	if got := readStateLastWake(t, path); !got.Equal(done) {
		t.Errorf("last_wake=%s want %s（ctx 已取消时不该推进：半截批次必须留给下次启动补跑）",
			got.Format(time.RFC3339), done.Format(time.RFC3339))
	}

	// 反向对照：ctx 未取消时同一调用必须推进。没有这一半，上面的断言会因为
	// "这个方法永远不落盘"而假绿。
	s.persistLastWakeIfRunning(context.Background(), half)
	if got := readStateLastWake(t, path); !got.Equal(half) {
		t.Errorf("last_wake=%s want %s（ctx 正常时批次跑完就该推进）",
			got.Format(time.RFC3339), half.Format(time.RFC3339))
	}
}

// TestStartupCatchUpReplaysActivitySlotMissedByDowntime 线上实景复现。
//
// 容器 2026-10-27 12:49 启动，activity_hours=[10] → 当天 10:00 的槽位早于启动时刻，
// 修复前它永久丢失（docker logs 里 activity 相关行 0 条）。状态文件里
// last_wake = 前一天 22:14（上一个容器最后一次保活），于是补跑区间是
// (10-26 22:14, 10-27 12:49]，其中只有 10-27 10:00 一个 activity 槽位。
func TestStartupCatchUpReplaysActivitySlotMissedByDowntime(t *testing.T) {
	path := statePathIn(t)
	if err := saveLastWake(path, time.Date(2026, 10, 26, 22, 14, 40, 0, cstZone)); err != nil {
		t.Fatalf("预置状态文件失败: %v", err)
	}
	s, stub := newActivityCatchUpScheduler(t, path, nil)

	s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 12, 49, 38, 0, cstZone))

	if n := stub.calls.Load(); n != 1 {
		t.Fatalf("上报调用=%d want 1（10:00 的活跃槽位应被补跑一次）", n)
	}
	// last_wake 必须推进到刚补的那个槽位，而不是"启动时刻"——下次启动的补跑区间
	// 从槽位算起，才不会把同一个槽位再补一遍。
	want := time.Date(2026, 10, 27, 10, 0, 0, 0, cstZone)
	if got := readStateLastWake(t, path); !got.Equal(want) {
		t.Errorf("last_wake=%s want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestStartupCatchUpDispatchesMultipleKindsInSlotOrder 停机跨多类多槽位：
// 昨天 21:00 签到、22:00 保活、今天 01:00 夜猫，08:00 启动 → 三类各补一次，
// 顺序按槽位时刻升序（签到 → 保活 → 夜猫）。
func TestStartupCatchUpDispatchesMultipleKindsInSlotOrder(t *testing.T) {
	rec := &dispatchRecorder{}
	installRecordingExec(t, rec) // cat 走脚本，登记而不真跑 python3
	srv := multiKindServer(rec)
	defer srv.Close()

	path := statePathIn(t)
	if err := saveLastWake(path, time.Date(2026, 10, 26, 20, 0, 0, 0, cstZone)); err != nil {
		t.Fatalf("预置状态文件失败: %v", err)
	}

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool:               p,
		Upstream:           up,
		CheckinHours:       []int{21},
		KeepaliveHours:     []int{22},
		CatHours:           []int{1},
		RandomDelayMinutes: 0,
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
		t.Fatalf("dispatch=%v want %v（三类各补一次）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dispatch=%v want %v（必须按槽位时刻升序）", got, want)
		}
	}
	// 最后一批是 01:00 的夜猫：last_wake 推进到它。
	last := time.Date(2026, 10, 27, 1, 0, 0, 0, cstZone)
	if v := readStateLastWake(t, path); !v.Equal(last) {
		t.Errorf("last_wake=%s want %s", v.Format(time.RFC3339), last.Format(time.RFC3339))
	}
}

// TestLatestPerKindKeepsLatestSlotPerKind 折叠是纯函数层面的行为：同类多个槽位
// 只留最新一个，跨类顺序保持升序，同刻按 taskKind 声明序（让日志与断言稳定）。
func TestLatestPerKindKeepsLatestSlotPerKind(t *testing.T) {
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, cstZone)
	in := []missedSlot{
		{at: day.Add(9 * time.Hour), kind: taskCheckin},
		{at: day.Add(22 * time.Hour), kind: taskKeepalive},
		{at: day.Add(21 * time.Hour), kind: taskCheckin}, // 乱序输入：函数不能依赖入参已排序
		{at: day.Add(22 * time.Hour), kind: taskCat},     // 同刻不同类：都要留
	}

	got := latestPerKind(in)
	want := []missedSlot{
		{at: day.Add(21 * time.Hour), kind: taskCheckin},
		{at: day.Add(22 * time.Hour), kind: taskKeepalive},
		{at: day.Add(22 * time.Hour), kind: taskCat},
	}
	if len(got) != len(want) {
		t.Fatalf("latestPerKind=%v want %v", got, want)
	}
	for i := range want {
		if got[i].kind != want[i].kind || !got[i].at.Equal(want[i].at) {
			t.Fatalf("latestPerKind[%d]=%v@%s want %v@%s",
				i, got[i].kind, got[i].at.Format("15:04"), want[i].kind, want[i].at.Format("15:04"))
		}
	}
}

// TestStartupCatchUpFoldsSameKindSlots 端到端折叠：签到 09:00 与 20:00 都已过点，
// 20:30 启动只补 20:00 那个（昨天/更早的同类槽位已被更近的取代，补它纯浪费）。
//
// 时段取 09/20 而不是默认的 09/21：21:00 与 09:00 正好相距 12 小时 = lookback 上限，
// 两者不可能同时落在同一个补跑窗口里（这里要测的是折叠，不是 lookback 边界）。
func TestStartupCatchUpFoldsSameKindSlots(t *testing.T) {
	path := statePathIn(t)
	if err := saveLastWake(path, time.Date(2026, 10, 26, 22, 0, 0, 0, cstZone)); err != nil {
		t.Fatalf("预置状态文件失败: %v", err)
	}
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool:               p,
		Upstream:           up,
		CheckinHours:       []int{9, 20},
		RandomDelayMinutes: 0,
		StatePath:          path,
		TravelDisabled:     true,
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		CatDisabled:        true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	})

	s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 20, 30, 0, 0, cstZone))

	if n := f.checkinCalls.Load(); n != 1 {
		t.Fatalf("签到调用=%d want 1（09:00 与 20:00 折叠成一次）", n)
	}
	last := time.Date(2026, 10, 27, 20, 0, 0, 0, cstZone)
	if v := readStateLastWake(t, path); !v.Equal(last) {
		t.Errorf("last_wake=%s want %s（推进到被保留的 20:00 槽位）", v.Format(time.RFC3339), last.Format(time.RFC3339))
	}
}

// TestStartupCatchUpIgnoresFutureLastWake last_wake 在未来（时钟回拨 / 时区变更 /
// 手改文件）→ 补跑区间倒挂，直接放弃，等主循环正常排程。
func TestStartupCatchUpIgnoresFutureLastWake(t *testing.T) {
	path := statePathIn(t)
	now := time.Date(2026, 10, 27, 12, 49, 0, 0, cstZone)
	if err := saveLastWake(path, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("预置状态文件失败: %v", err)
	}
	s, stub := newActivityCatchUpScheduler(t, path, nil)

	s.startupCatchUpAt(context.Background(), now)

	if n := stub.calls.Load(); n != 0 {
		t.Errorf("上报调用=%d want 0（last_wake 在未来，不补跑）", n)
	}
}

// TestStartupCatchUpIgnoresSlotsOlderThanLookback last_wake 远早于 lookback 时，
// 补跑区间被夹到 now-12h：更早的槽位一律不补（停机三天不该补一整天历史任务）。
func TestStartupCatchUpIgnoresSlotsOlderThanLookback(t *testing.T) {
	path := statePathIn(t)
	// 30 小时前 = 10-25 02:00；启动时刻 10-27 08:00 → 窗口夹到 10-26 20:00，
	// 而 activity 槽位只有 10-26 10:00（早于窗口）与 10-27 10:00（还没到）。
	old := time.Date(2026, 10, 25, 2, 0, 0, 0, cstZone)
	if err := saveLastWake(path, old); err != nil {
		t.Fatalf("预置状态文件失败: %v", err)
	}
	s, stub := newActivityCatchUpScheduler(t, path, nil)

	s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 8, 0, 0, 0, cstZone))

	if n := stub.calls.Load(); n != 0 {
		t.Errorf("上报调用=%d want 0（超出 lookback 的槽位不补）", n)
	}
	// 没补跑就不该推进 last_wake：否则下次启动会以为"这段已经处理过"。
	if v := readStateLastWake(t, path); !v.Equal(old) {
		t.Errorf("last_wake=%s want 保持 %s", v.Format(time.RFC3339), old.Format(time.RFC3339))
	}
}

// TestStartupCatchUpNoRepeatWhenStateIsCurrent last_wake 已等于最近的槽位 →
// 重启不重复补跑（幂等的最后一道保险：正常运行时每批都会推进 last_wake）。
func TestStartupCatchUpNoRepeatWhenStateIsCurrent(t *testing.T) {
	path := statePathIn(t)
	if err := saveLastWake(path, time.Date(2026, 10, 27, 10, 0, 0, 0, cstZone)); err != nil {
		t.Fatalf("预置状态文件失败: %v", err)
	}
	s, stub := newActivityCatchUpScheduler(t, path, nil)

	s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 12, 49, 0, 0, cstZone))

	if n := stub.calls.Load(); n != 0 {
		t.Errorf("上报调用=%d want 0（该槽位已处理过，不重复补跑）", n)
	}
}

// TestStartupCatchUpSkipsDisabledKinds 已禁用的任务类不补跑（与 nextWake / missedSlots
// 同口径）：面板关掉活跃上报后，重启不该因为它而多打一轮上游。
func TestStartupCatchUpSkipsDisabledKinds(t *testing.T) {
	path := statePathIn(t)
	if err := saveLastWake(path, time.Date(2026, 10, 26, 22, 14, 0, 0, cstZone)); err != nil {
		t.Fatalf("预置状态文件失败: %v", err)
	}
	s, stub := newActivityCatchUpScheduler(t, path, func(c *Config) { c.ActivityDisabled = true })

	s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 12, 49, 0, 0, cstZone))

	if n := stub.calls.Load(); n != 0 {
		t.Errorf("上报调用=%d want 0（活跃上报已禁用）", n)
	}
}

// TestStartupCatchUpNoopWithoutStatePath StatePath 为空 = 本特性关闭：
// 不读文件、不补跑、不写文件，行为与引入持久化之前完全一致（零回归）。
func TestStartupCatchUpNoopWithoutStatePath(t *testing.T) {
	s, stub := newActivityCatchUpScheduler(t, "", nil)

	// 即使 last_wake 窗口内确实有槽位（10:00 已过），也不该派发任何任务。
	s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 12, 49, 0, 0, cstZone))

	if n := stub.calls.Load(); n != 0 {
		t.Errorf("上报调用=%d want 0（StatePath 为空 = 特性关闭）", n)
	}
	if p := s.snapshot().StatePath; p != "" {
		t.Errorf("StatePath=%q want 空", p)
	}
}

// TestPersistLastWakeRoundTrip 落盘可被原样读回（RFC3339 带时区，秒级精度）。
func TestPersistLastWakeRoundTrip(t *testing.T) {
	path := statePathIn(t)
	s := New(Config{StatePath: path})

	at := time.Date(2026, 10, 27, 22, 14, 40, 0, cstZone)
	s.persistLastWake(at)

	got := readStateLastWake(t, path)
	if !got.Equal(at) {
		t.Errorf("last_wake=%s want %s", got.Format(time.RFC3339), at.Format(time.RFC3339))
	}
	// 原子替换的中间产物不该残留（否则 data 目录里会攒一堆 .tmp）。
	// tmp 名带 pid（见 saveLastWake），所以不能只 stat 固定名：按前缀扫一遍目录，
	// 否则这条断言会因为"名字对不上"而永远真——假绿比没有断言更糟。
	if leftover := tmpLeftovers(t, path); len(leftover) != 0 {
		t.Errorf("残留 tmp 文件 %v", leftover)
	}
	// 0600：槽位时刻能反推出部署的随机偏移规律。
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("权限=%o want 600", perm)
	}
}

// TestPersistLastWakeSilentWithoutStatePath StatePath 为空时落盘是空操作。
func TestPersistLastWakeSilentWithoutStatePath(t *testing.T) {
	dir := t.TempDir()
	s := New(Config{})
	s.persistLastWake(time.Date(2026, 10, 27, 10, 0, 0, 0, cstZone))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("不该写任何文件，实际 %v", entries)
	}
}

// TestPersistLastWakeWarnsOnceOnFailure 落盘失败只报一次 WARN、不 panic、不阻塞调度。
//
// 每批任务完成都会落盘，磁盘满/只读挂载时若逐次告警，日志会被这条淹掉，真正的
// 任务日志反而看不见；一次提示足够让运维去查挂载。
func TestPersistLastWakeWarnsOnceOnFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备阻塞文件失败: %v", err)
	}
	// 父目录是一个普通文件 → MkdirAll 必失败（ENOTDIR）。
	s := New(Config{StatePath: filepath.Join(blocker, "data", StateFileName)})

	logs := captureLog(t)
	s.persistLastWake(time.Date(2026, 10, 27, 10, 0, 0, 0, cstZone))
	s.persistLastWake(time.Date(2026, 10, 27, 11, 0, 0, 0, cstZone))

	if n := strings.Count(logs(), "调度状态落盘失败"); n != 1 {
		t.Errorf("WARN 次数=%d want 1（只报一次），日志:\n%s", n, logs())
	}
}

// TestStartupCatchUpCoversSlotsCrossedByItsOwnBatch 启动补跑自己派发的批次也会跑很久，
// 它跨过的槽位必须在**同一次启动**里补掉，不能等下一次重启。
//
// 线上形态：checkin_hours=[9,21]、activity_hours=[10]、account_delay 220~600s、7 个账号
// → 一轮签到 22~60 分钟，09:00 的签到补跑必然跨过 10:00 的活跃上报。Run 的
// catchUpMissed 循环原先只接"主循环批次跑过头"，启动补跑批次跑过头这一半没人管，
// 被跨过的槽位要等下一次重启才补——生产上等于丢一天。
//
// 不注入"批次耗时"（那要改 runBatch 签名），而是把墙钟人为推进后直接调
// catchUpMissed：① 的返回值就是 Run 里接进循环的那个 ranAt，② 的两次调用就是循环体
// 本身（ranAt = catchUpMissed(ranAt, time.Now()) 直到返回零值）。
func TestStartupCatchUpCoversSlotsCrossedByItsOwnBatch(t *testing.T) {
	rec := &dispatchRecorder{}
	srv := checkinActivityServer(rec)
	defer srv.Close()
	fastActivity(t) // 关掉账号间/条间间隔，避免测试白等

	path := statePathIn(t)
	// 停机前最后一次保活 10-26 22:14；签到槽位 09:00、活跃槽位 10:00（整点，无随机偏移）。
	if err := saveLastWake(path, time.Date(2026, 10, 26, 22, 14, 0, 0, cstZone)); err != nil {
		t.Fatalf("预置状态文件失败: %v", err)
	}

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool:               p,
		Upstream:           up,
		CheckinHours:       []int{9},
		ActivityHours:      []int{10},
		RandomDelayMinutes: 0,
		StatePath:          path,
		TravelDisabled:     true,
		KeepaliveDisabled:  true,
		CatDisabled:        true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	})

	// ① 启动补跑：09:40 启动 → 窗口 (10-26 22:14, 10-27 09:40] 内只有 09:00 的签到。
	ranAt := s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 9, 40, 0, 0, cstZone))
	wantSlot := time.Date(2026, 10, 27, 9, 0, 0, 0, cstZone)
	if !ranAt.Equal(wantSlot) {
		t.Fatalf("startupCatchUpAt=%s want %s（应返回最后处理的签到槽位）",
			ranAt.Format(time.RFC3339), wantSlot.Format(time.RFC3339))
	}
	if got := rec.list(); len(got) != 1 || got[0] != "checkin" {
		t.Fatalf("启动补跑 dispatch=%v want [checkin]（10:00 的活跃此时还没到点）", got)
	}

	// ② 签到批次在生产上要跑 22~60 分钟，墙钟已越过 10:00 —— Run 的 catch-up 循环体。
	next := s.catchUpMissed(context.Background(), ranAt, time.Date(2026, 10, 27, 10, 50, 0, 0, cstZone))
	wantActivity := time.Date(2026, 10, 27, 10, 0, 0, 0, cstZone)
	if !next.Equal(wantActivity) {
		t.Fatalf("catchUpMissed=%s want %s（10:00 的活跃槽位被启动批次跨过，应被补跑）",
			next.Format(time.RFC3339), wantActivity.Format(time.RFC3339))
	}
	got := rec.list()
	if len(got) != 2 || got[0] != "checkin" || got[1] != "activity" {
		t.Fatalf("dispatch=%v want [checkin activity]（activity 必须在同一次启动里补跑，不能等重启）", got)
	}
	// 循环体里紧跟的落盘：推进到 10:00，下次重启从这里算起，不会把这一轮再补一遍。
	s.persistLastWake(next)
	if v := readStateLastWake(t, path); !v.Equal(next) {
		t.Errorf("last_wake=%s want %s", v.Format(time.RFC3339), next.Format(time.RFC3339))
	}

	// ③ 循环下一轮：同一时刻再查一次没有新槽位 → 零值，循环退出，且不重复派发。
	if again := s.catchUpMissed(context.Background(), next, time.Date(2026, 10, 27, 10, 50, 0, 0, cstZone)); !again.IsZero() {
		t.Errorf("catchUpMissed=%s want 零值（没有新槽位）", again.Format(time.RFC3339))
	}
	if final := rec.list(); len(final) != 2 {
		t.Errorf("重复派发：dispatch=%v want [checkin activity]", final)
	}
}

// TestStartupCatchUpReturnsLastHandledSlot 返回值语义：Run 靠它把启动补跑接进
// catch-up 循环，返回错了（零值 / 中间某个槽位）会让后续查漏从错误的起点算起。
func TestStartupCatchUpReturnsLastHandledSlot(t *testing.T) {
	t.Run("返回最后一个被补跑的槽位", func(t *testing.T) {
		path := statePathIn(t)
		if err := saveLastWake(path, time.Date(2026, 10, 26, 22, 14, 0, 0, cstZone)); err != nil {
			t.Fatalf("预置状态文件失败: %v", err)
		}
		s, stub := newActivityCatchUpScheduler(t, path, nil)

		got := s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 12, 49, 0, 0, cstZone))

		want := time.Date(2026, 10, 27, 10, 0, 0, 0, cstZone)
		if !got.Equal(want) {
			t.Errorf("startupCatchUpAt=%s want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		if n := stub.calls.Load(); n != 1 {
			t.Errorf("上报调用=%d want 1（返回值必须对应真实派发过的槽位）", n)
		}
	})

	t.Run("无可补时返回零值", func(t *testing.T) {
		path := statePathIn(t)
		// last_wake 已经是最近的槽位：窗口内没有可补的，Run 的循环因此不会进。
		if err := saveLastWake(path, time.Date(2026, 10, 27, 10, 0, 0, 0, cstZone)); err != nil {
			t.Fatalf("预置状态文件失败: %v", err)
		}
		s, stub := newActivityCatchUpScheduler(t, path, nil)

		if got := s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 12, 49, 0, 0, cstZone)); !got.IsZero() {
			t.Errorf("startupCatchUpAt=%s want 零值（没有补跑任何东西）", got.Format(time.RFC3339))
		}
		if n := stub.calls.Load(); n != 0 {
			t.Errorf("上报调用=%d want 0", n)
		}
	})

	t.Run("ctx 已取消时返回零值", func(t *testing.T) {
		path := statePathIn(t)
		if err := saveLastWake(path, time.Date(2026, 10, 26, 22, 14, 0, 0, cstZone)); err != nil {
			t.Fatalf("预置状态文件失败: %v", err)
		}
		s, stub := newActivityCatchUpScheduler(t, path, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // 进循环前就取消：一个槽位都没跑

		if got := s.startupCatchUpAt(ctx, time.Date(2026, 10, 27, 12, 49, 0, 0, cstZone)); !got.IsZero() {
			t.Errorf("startupCatchUpAt=%s want 零值（ctx 取消，未补跑任何槽位）", got.Format(time.RFC3339))
		}
		if n := stub.calls.Load(); n != 0 {
			t.Errorf("上报调用=%d want 0（ctx 已取消不该派发）", n)
		}
	})

	t.Run("StatePath 为空时返回零值", func(t *testing.T) {
		s, _ := newActivityCatchUpScheduler(t, "", nil)
		if got := s.startupCatchUpAt(context.Background(), time.Date(2026, 10, 27, 12, 49, 0, 0, cstZone)); !got.IsZero() {
			t.Errorf("startupCatchUpAt=%s want 零值（特性关闭）", got.Format(time.RFC3339))
		}
	})
}

// blockingCheckinServer 让**第一次** /daily-checkin 卡在 handler 里不返回，用来构造
// "批次执行到一半、ctx 被取消"的中间态：此刻第一个账号的签到正在途，第二个账号还没轮到。
//
// 为什么不让"取消 ctx 自动中断请求"来造这个场景：签到走的是 upstream.Client 的无 ctx
// 签名（DailyCheckin(a)），取消调度 ctx 不会掐断在途 HTTP 请求——这恰好是生产实况
// （优雅停机等批次自己收尾，而不是砍掉在途连接）。所以必须由 handler 自己停住，测试
// 才拿得到"批次确实只跑了一部分"这个状态。
type blockingCheckinServer struct {
	calls   atomic.Int32  // /daily-checkin 累计调用数（"半截"与"重补"都靠它断言）
	started chan struct{} // 第一次进入阻塞时关闭：批次已开工、尚未跑完
	release chan struct{} // 关闭后阻塞中的请求才返回
	first   atomic.Bool
}

// newBlockingCheckinServer 起一个带阻塞语义的签到端点（其余路径一律回成功空数据，
// 余额查询等辅助调用失败与否不影响本用例的断言）。
func newBlockingCheckinServer() (*blockingCheckinServer, *httptest.Server) {
	b := &blockingCheckinServer{started: make(chan struct{}), release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			b.calls.Add(1)
			if b.first.CompareAndSwap(false, true) {
				close(b.started) // 先告诉测试"批次开工了"，再卡住不返回
				<-b.release
			}
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":` +
				`[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		default:
			w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	return b, srv
}

// TestCatchUpDoesNotPersistHalfDoneBatchOnShutdown 停机时"半截批次"不得被标记成已完成，
// 且该槽位必须在下一次启动被补上——这是整条落盘/补跑链的价值所在。
//
// 三处落盘点（startupCatchUpAt 的批次循环、Run 的启动补跑 catch-up 循环体、Run 主循环）
// 都必须走 persistLastWakeIfRunning：只要有一处无条件落盘，被 ctx 取消的那一批就会被
// 当成"已完成"，剩下没跑的账号要等下一个自然日才被处理（方向与"宁可重复一次，不可
// 漏掉一天"正好相反）。
//
// 本用例端到端走 startupCatchUpAt 那条路径（真实 runBatch → 真实签到 → 真实 HTTP →
// 真实落盘）：另外两处落盘点在 waitSlot 之后，测试不等待真实槽位就走不到那里
// （见 persistLastWakeIfRunning 的注释）。
func TestCatchUpDoesNotPersistHalfDoneBatchOnShutdown(t *testing.T) {
	b, srv := newBlockingCheckinServer()
	defer srv.Close()

	path := statePathIn(t)
	// 停机前最后一次保活 10-26 22:14；签到槽位 10-27 09:00（整点，无随机偏移）。
	prev := time.Date(2026, 10, 26, 22, 14, 0, 0, cstZone)
	if err := saveLastWake(path, prev); err != nil {
		t.Fatalf("预置状态文件失败: %v", err)
	}
	rawBefore := readStateRaw(t, path)

	// 两个账号：第一个跑起来后阻塞，第二个永远轮不到 → 半截批次。
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2", RefreshToken: "rt2", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool:               p,
		Upstream:           up,
		CheckinHours:       []int{9},
		RandomDelayMinutes: 0,
		StatePath:          path,
		TravelDisabled:     true,
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		CatDisabled:        true,
		GrowthDisabled:     true,
		LimitedDisabled:    true,
	})
	now := time.Date(2026, 10, 27, 12, 49, 38, 0, cstZone)
	slot := time.Date(2026, 10, 27, 9, 0, 0, 0, cstZone)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan time.Time, 1)
	go func() { done <- s.startupCatchUpAt(ctx, now) }()

	select {
	case <-b.started:
	case <-time.After(10 * time.Second):
		t.Fatal("签到批次 10s 内没有开工")
	}
	// 此刻第一个账号的 daily-checkin 正卡在 handler 里、第二个账号还没轮到。
	cancel()         // 优雅停机信号
	close(b.release) // 放行在途请求，让批次能真的收尾（不掐断 HTTP，与生产一致）

	var handled time.Time
	select {
	case handled = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx 取消后批次 10s 内没有收尾")
	}

	// 场景自检：确实只跑了一个账号（半截），否则后面的断言毫无意义。
	if n := b.calls.Load(); n != 1 {
		t.Fatalf("签到调用=%d want 1（ctx 取消后第二个账号不该被派发，否则不是半截批次）", n)
	}
	// 返回值语义：已跑完的槽位照常返回（Run 靠它查漏）；ctx 已取消时 Run 的 catch-up
	// 循环守卫（ctx.Err() == nil）不会进，所以这个返回值不会引出任何落盘。
	if !handled.Equal(slot) {
		t.Errorf("startupCatchUpAt=%s want %s", handled.Format(time.RFC3339), slot.Format(time.RFC3339))
	}

	// ① 该槽位没有被写进 scheduler_state.json。
	if raw := readStateRaw(t, path); strings.Contains(raw, slot.Format(time.RFC3339)) {
		t.Errorf("状态文件里出现了半截批次所在的槽位 %s：\n%s", slot.Format(time.RFC3339), raw)
	}
	// ② 状态文件仍是原值（原文逐字节比对，连缩进/换行都不许变）。
	if raw := readStateRaw(t, path); raw != rawBefore {
		t.Errorf("状态文件被改写:\n got=%q\nwant=%q", raw, rawBefore)
	}
	if got := readStateLastWake(t, path); !got.Equal(prev) {
		t.Errorf("last_wake=%s want %s（半截批次不得推进）", got.Format(time.RFC3339), prev.Format(time.RFC3339))
	}
	// 落盘是 tmp+rename：不落盘就不该留下任何临时文件。
	if left := tmpLeftovers(t, path); len(left) != 0 {
		t.Errorf("残留临时文件=%v want 空（ctx 取消时压根不该写盘）", left)
	}

	// ③ 下一次启动必须把这个槽位补上 —— "没推进"确实等于"下次会补"，整条链的价值在此。
	before := b.calls.Load()
	if got := s.startupCatchUpAt(context.Background(), now); !got.Equal(slot) {
		t.Errorf("二次启动 startupCatchUpAt=%s want %s（上次没落盘的槽位必须重新补跑）",
			got.Format(time.RFC3339), slot.Format(time.RFC3339))
	}
	if n := b.calls.Load() - before; n != 2 {
		t.Errorf("二次启动签到调用=%d want 2（两个账号各补一次）", n)
	}
	if got := readStateLastWake(t, path); !got.Equal(slot) {
		t.Errorf("last_wake=%s want %s（这一轮真跑完了，才允许推进）",
			got.Format(time.RFC3339), slot.Format(time.RFC3339))
	}
}
