package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// boxStub 模拟盲盒端点：quota + open。
// affordable 决定还能开几个；每次 open 递减，直到耗尽（模拟能量被扣）。
type boxStub struct {
	affordable int
	balance    int
	cost       int
	openFail   bool // open 返回 500（模拟风控/网络）
	emptyResp  bool // open 回 200 但 results 为空

	quotaCalls atomic.Int32
	openCalls  atomic.Int32
	opened     atomic.Int32
}

func (s *boxStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/buddy/quota":
			s.quotaCalls.Add(1)
			left := s.affordable - int(s.opened.Load())
			if left < 0 {
				left = 0
			}
			fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"affordable":%d,"balance":%d,"cost_per_open":%d,"max_open_count":5}}`,
				left, s.balance-int(s.opened.Load())*s.cost, s.cost)
		case "/activity/growth/buddy/open":
			s.openCalls.Add(1)
			if s.openFail {
				w.WriteHeader(500)
				w.Write([]byte(`{"code":500,"msg":"system busy"}`))
				return
			}
			var body struct {
				Count int `json:"count"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if s.emptyResp {
				w.Write([]byte(`{"code":0,"msg":"OK","data":{"count":0,"results":[]}}`))
				return
			}
			n := body.Count
			if n < 1 {
				n = 1
			}
			s.opened.Add(int32(n))
			results := ""
			for i := 0; i < n; i++ {
				if i > 0 {
					results += ","
				}
				results += fmt.Sprintf(`{"instance":{"id":%d,"template_id":12,"is_shiny":0,"soul_desc":"价值投资官","source":"gacha"},`+
					`"template":{"id":12,"name":"金算喵","description":"价值投资官"}}`, 25053427+i)
			}
			fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"count":%d,"results":[%s]}}`, n, results)
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func newBoxScheduler(t *testing.T, srv *httptest.Server, cfg Config, uids ...string) *Scheduler {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	cfg.Pool = p
	cfg.Upstream = up
	return New(cfg)
}

func boxAuth(s *Scheduler, uid string) *auth.Auth { return s.snapshot().Pool.AuthByUID(uid) }

// TestOpenBuddyBoxesDisabledByDefault 总开关关闭时一个请求都不发。
func TestOpenBuddyBoxesDisabledByDefault(t *testing.T) {
	stub := &boxStub{affordable: 3, balance: 30, cost: 10}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s := newBoxScheduler(t, srv, Config{BuddyBoxEnabled: false, BuddyBoxDailyCap: 5}, "u1")
	s.openBuddyBoxes(boxAuth(s, "u1"))

	if n := stub.quotaCalls.Load() + stub.openCalls.Load(); n != 0 {
		t.Errorf("上游调用=%d want 0（默认关闭）", n)
	}
}

// TestOpenBuddyBoxesOpensUntilExhausted 能量够开几个就开几个，逐个发（count=1）。
func TestOpenBuddyBoxesOpensUntilExhausted(t *testing.T) {
	stub := &boxStub{affordable: 3, balance: 30, cost: 10}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s := newBoxScheduler(t, srv, Config{BuddyBoxEnabled: true, BuddyBoxDailyCap: 0}, "u1")
	s.openBuddyBoxes(boxAuth(s, "u1"))

	if n := stub.openCalls.Load(); n != 3 {
		t.Errorf("open 调用=%d want 3（affordable=3）", n)
	}
	if n := stub.opened.Load(); n != 3 {
		t.Errorf("opened=%d want 3", n)
	}
}

// TestOpenBuddyBoxesRespectsDailyCap 每日上限比能量更紧时按上限停。
func TestOpenBuddyBoxesRespectsDailyCap(t *testing.T) {
	stub := &boxStub{affordable: 10, balance: 100, cost: 10}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s := newBoxScheduler(t, srv, Config{BuddyBoxEnabled: true, BuddyBoxDailyCap: 2}, "u1")
	s.openBuddyBoxes(boxAuth(s, "u1"))

	if n := stub.openCalls.Load(); n != 2 {
		t.Errorf("open 调用=%d want 2（每日上限 2）", n)
	}
}

// TestOpenBuddyBoxesDailyCapAccumulates 同日第二次调用不重复开（上限跨轮累计）。
func TestOpenBuddyBoxesDailyCapAccumulates(t *testing.T) {
	stub := &boxStub{affordable: 10, balance: 100, cost: 10}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s := newBoxScheduler(t, srv, Config{BuddyBoxEnabled: true, BuddyBoxDailyCap: 3}, "u1")
	a := boxAuth(s, "u1")
	s.openBuddyBoxes(a) // 开 3 个，达上限
	first := stub.openCalls.Load()
	s.openBuddyBoxes(a) // 同日再调：不应再开
	if got := stub.openCalls.Load(); got != first {
		t.Errorf("第二轮又多开了 %d 个（应受每日上限 3 约束）", got-first)
	}
}

// TestOpenBuddyBoxesNoEnergy 能量不够（affordable=0）时不发 open 请求。
func TestOpenBuddyBoxesNoEnergy(t *testing.T) {
	stub := &boxStub{affordable: 0, balance: 5, cost: 10}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s := newBoxScheduler(t, srv, Config{BuddyBoxEnabled: true, BuddyBoxDailyCap: 5}, "u1")
	s.openBuddyBoxes(boxAuth(s, "u1"))

	if n := stub.openCalls.Load(); n != 0 {
		t.Errorf("open 调用=%d want 0（能量不足）", n)
	}
}

// TestOpenBuddyBoxesStopsOnError 中途失败立即停手，不继续发后续请求（避免连环扣费）。
func TestOpenBuddyBoxesStopsOnError(t *testing.T) {
	stub := &boxStub{affordable: 5, balance: 50, cost: 10, openFail: true}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s := newBoxScheduler(t, srv, Config{BuddyBoxEnabled: true, BuddyBoxDailyCap: 0}, "u1")
	s.openBuddyBoxes(boxAuth(s, "u1"))

	if n := stub.openCalls.Load(); n != 1 {
		t.Errorf("open 调用=%d want 1（首次失败即停）", n)
	}
}

// TestOpenBuddyBoxesEmptyResultsStops 上游回 200 但无结果：按开 1 个计数并停手，
// 不再继续开（否则可能连环扣费却拿不到任何记录）。
func TestOpenBuddyBoxesEmptyResultsStops(t *testing.T) {
	stub := &boxStub{affordable: 5, balance: 50, cost: 10, emptyResp: true}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	s := newBoxScheduler(t, srv, Config{BuddyBoxEnabled: true, BuddyBoxDailyCap: 0}, "u1")
	s.openBuddyBoxes(boxAuth(s, "u1"))

	if n := stub.openCalls.Load(); n != 1 {
		t.Errorf("open 调用=%d want 1（空结果即停）", n)
	}
}

// TestOpenBuddyBoxesGlobalAndEnterpriseGated global / 企业版不发任何请求。
func TestOpenBuddyBoxesGlobalAndEnterpriseGated(t *testing.T) {
	stub := &boxStub{affordable: 5, balance: 50, cost: 10}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	ga := &auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	ga.SetRealm("global")
	p.Add(ga)
	p.Add(&auth.Auth{UID: "e1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, EnterpriseID: "ent-1"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, BuddyBoxEnabled: true, BuddyBoxDailyCap: 5})

	for _, uid := range []string{"g1", "e1"} {
		s.openBuddyBoxes(p.AuthByUID(uid))
	}
	if n := stub.quotaCalls.Load() + stub.openCalls.Load(); n != 0 {
		t.Errorf("上游调用=%d want 0（global/企业版门控）", n)
	}
}

// TestBuddyBoxRemainingResetsAcrossDays 每日上限按 CST 自然日重置。
func TestBuddyBoxRemainingResetsAcrossDays(t *testing.T) {
	s := New(Config{BuddyBoxEnabled: true, BuddyBoxDailyCap: 3})
	s.markBuddyBoxOpened("u1", 3)
	if got := s.buddyBoxRemaining("u1", 3); got != 0 {
		t.Errorf("remaining=%d want 0（今日已开满）", got)
	}
	// 把计数改成昨天，模拟跨日
	s.mu.Lock()
	s.buddyBoxOpened["u1"] = buddyBoxCount{day: "2000-01-01", count: 3}
	s.mu.Unlock()
	if got := s.buddyBoxRemaining("u1", 3); got != 3 {
		t.Errorf("remaining=%d want 3（跨日应重置）", got)
	}
}

// TestBuddyBoxCapZeroMeansUnlimited 上限 0 = 不限（只由能量约束）。
func TestBuddyBoxCapZeroMeansUnlimited(t *testing.T) {
	s := New(Config{BuddyBoxEnabled: true, BuddyBoxDailyCap: 0})
	s.markBuddyBoxOpened("u1", 100)
	if got := s.buddyBoxRemaining("u1", 0); got <= 100 {
		t.Errorf("remaining=%d want 不限（远大于 100）", got)
	}
}
