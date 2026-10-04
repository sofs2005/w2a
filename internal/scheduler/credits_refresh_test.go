// credits_refresh_test.go 启动余额刷新（RefreshCreditsOnce）的门控与行为测试。
//
// 为什么单独测这个：它是「面板重启后显示旧积分」的修复本体，且与 CheckinAll 有
// 两处**刻意**的差异（不签到、不跳过 global），差异点正是回归风险所在——
// 误把 DailyCheckin 加进来会在每次重启打签到接口；误把 global 门控抄进来会让
// global 账号的 credits 继续终身冻结。
package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func fastCreditsRefresh(t *testing.T) {
	t.Helper()
	old := creditsRefreshDelay
	creditsRefreshDelay = 0
	t.Cleanup(func() { creditsRefreshDelay = old })
	// 上游瞬时错误重试（任务 #4）默认 2s+4s：失败账号每个要多耗 6s，本包多个
	// 用例的时限预算（如「失败不中断」的 10s）都是按「失败即时返回」定的，不压短
	// 就会假失败。压到毫秒级——本包测的是门控与互斥语义，不是重试节奏（后者由
	// upstream 包自己的用例钉住）。
	restore := upstream.SetBillingRetryDelayForTest(time.Millisecond)
	t.Cleanup(restore)
}

// balanceBody 构造 get-user-resource 的合法响应体（外层 {"code":0,"data":…} 信封
// 是既有 fake upstream 的统一形状，见 scheduler_test.go 的 fakeUpstream.server）。
// Cycle* 口径优先（packageRemainUsed 的判定）；size 恒 10000 且调用方传入的 remain
// 必须 <= size——取数会钳 [0,size]（上游脏数据防御），remain>size 会被静默钳到 size。
func balanceBody(remain int64) string {
	return `{"code":0,"data":{"Response":{"Data":{"TotalDosage":0,"Accounts":[
		{"PackageName":"p","CycleCapacitySize":10000,"CycleCapacityRemain":` +
		jsonI64(remain) + `,"CycleCapacityUsed":0}]}}}}`
}

// TestRefreshCreditsOnceUpdatesPoolBalances 核心行为：查到的余额写进池，
// 供 /status 与 state.json 使用（面板重启后读的就是它）。
func TestRefreshCreditsOnceUpdatesPoolBalances(t *testing.T) {
	fastCreditsRefresh(t)

	var balanceCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/get-user-resource") {
			balanceCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(balanceBody(4321)))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RefreshCreditsOnce(context.Background())

	if n := balanceCalls.Load(); n != 1 {
		t.Fatalf("余额查询调用数=%d want 1", n)
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 不在池中")
	}
	if st.Credits != 4321 {
		t.Errorf("credits=%d want 4321（启动刷新应写入权威余额）", st.Credits)
	}
}

// TestRefreshCreditsOnceDoesNotCheckin 本方法**只查余额，不签到**。
// 若误复用 CheckinAll，每次网关重启都会打一次 daily-checkin（有上游副作用）。
func TestRefreshCreditsOnceDoesNotCheckin(t *testing.T) {
	fastCreditsRefresh(t)

	var checkinCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(balanceBody(100)))
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			checkinCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RefreshCreditsOnce(context.Background())

	if n := checkinCalls.Load(); n != 0 {
		t.Errorf("签到调用数=%d want 0（启动刷新不得触发签到）", n)
	}
}

// TestRefreshCreditsOnceIncludesGlobal 与 CheckinAll 的 D4 门控相反：本方法
// **包含 global 账号**。global 不参与签到体系，不刷新则其 credits 终身冻结
// （见 pool/state.go 注释「global 账号不签到，credits 曾是终身冻结」）。
func TestRefreshCreditsOnceIncludesGlobal(t *testing.T) {
	fastCreditsRefresh(t)

	var globalCalls, cnCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls := &cnCalls
		if !strings.Contains(r.URL.Path, "/v2/") {
			calls = &globalCalls // global 打无 /v2 前缀的路径（billingMeterPaths 首选）
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if calls == &globalCalls {
			_, _ = w.Write([]byte(balanceBody(777)))
			return
		}
		_, _ = w.Write([]byte(balanceBody(555)))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"}) // realm=global
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.cn"}) // realm=cn
	// global 走 BillingBaseGlobal（globalBillingBase 只用它，不看 BillingBaseCN）；
	// 两域共用同一个 srv，靠路径是否含 /v2/ 区分——这正是被测的 realm 路由行为。
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up})

	s.RefreshCreditsOnce(context.Background())

	if n := globalCalls.Load(); n != 1 {
		t.Errorf("global 账号余额查询调用数=%d want 1（不应跳过 global）", n)
	}
	if n := cnCalls.Load(); n != 1 {
		t.Errorf("cn 账号余额查询调用数=%d want 1", n)
	}
	gst, _ := p.Status("g1")
	if gst.Credits != 777 {
		t.Errorf("global credits=%d want 777", gst.Credits)
	}
}

// TestRefreshCreditsOnceSkipsDisabledAndCredentialless 门控：禁用账号与无凭证
// 账号一律不发起上游调用。
func TestRefreshCreditsOnceSkipsDisabledAndCredentialless(t *testing.T) {
	fastCreditsRefresh(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "disabled", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("disabled", "test")
	p.Add(&auth.Auth{UID: "norefresh", AccessToken: "at", ExpiresAt: 9999999999}) // 无 refreshToken
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RefreshCreditsOnce(context.Background())

	if n := calls.Load(); n != 0 {
		t.Errorf("上游调用数=%d want 0（禁用/无凭证应跳过）", n)
	}
}

// TestRefreshCreditsOnceFailureDoesNotStopOthers 单账号失败不中断遍历——
// 一个坏号不该让其余账号的余额全部停在陈旧值。
func TestRefreshCreditsOnceFailureDoesNotStopOthers(t *testing.T) {
	fastCreditsRefresh(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "a", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "b", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	done := make(chan struct{})
	go func() {
		s.RefreshCreditsOnce(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("全部账号失败时不应卡住")
	}
	// 失败时余额保持原值（0），不写入垃圾数据。
	st, _ := p.Status("b")
	if st.Credits != 0 {
		t.Errorf("失败账号 credits=%d want 0（不得写入垃圾值）", st.Credits)
	}
}

// TestRefreshCreditsOnceWaitsForRunningCheckin 与签到互斥：签到持锁期间启动刷新
// 应等待（而非跳过或并发打上游）。用 **Lock 而非 TryLock** 的原因——本方法跑在
// 启动期后台 goroutine，等待无代价，而跳过会让只有本方法覆盖的 global 账号
// 错过刷新（CheckinAll 跳过 global），退回本方法要修的问题。
func TestRefreshCreditsOnceWaitsForRunningCheckin(t *testing.T) {
	fastCreditsRefresh(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(balanceBody(100)))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.checkinMu.Lock() // 模拟签到正在执行
	done := make(chan struct{})
	go func() {
		s.RefreshCreditsOnce(context.Background())
		close(done)
	}()

	// 持锁期间不得有任何上游调用（并发打上游是本次要避免的）。
	select {
	case <-done:
		t.Fatal("签到持锁期间不应完成（应等待锁）")
	case <-time.After(100 * time.Millisecond):
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("持锁期间上游调用数=%d want 0（不得与签到并发打上游）", n)
	}

	s.checkinMu.Unlock() // 放锁后应继续执行
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("放锁后未继续执行")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("放锁后上游调用数=%d want 1", n)
	}
	st, _ := p.Status("u1")
	if st.Credits != 100 {
		t.Errorf("credits=%d want 100（放锁后应完成刷新）", st.Credits)
	}
}

// TestRefreshCreditsOnceCancelStops 卡在节流等待时 ctx 取消应立即返回
// （优雅停机不必等遍历完所有账号）。
func TestRefreshCreditsOnceCancelStops(t *testing.T) {
	old := creditsRefreshDelay
	creditsRefreshDelay = 10 * time.Second // 足够长，确保卡在等待中
	t.Cleanup(func() { creditsRefreshDelay = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(balanceBody(1)))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "a", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "b", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RefreshCreditsOnce(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // 让第一个账号查完、进入第二个账号前的 sleep
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后未及时返回（应放弃剩余账号）")
	}
}
