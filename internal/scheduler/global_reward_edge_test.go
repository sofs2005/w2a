package scheduler

// global_reward_edge_test.go —— 放开 global 连登奖励链的边界用例（与 global_reward_test.go 互补）。
//
// 与工程师自测（global_reward_test.go）刻意不共用任何 helper：桩、构造器、日志捕获、
// 断言全部自建。理由很直接——若 helper 本身把「该发什么请求」写错了，共用它只会把
// 同一份错误抄两遍。
//
// 本文件在三点上比自测更硬：
//
//  1. 出站硬闸（hostGuard）：所有 http.Client 的 Transport 都被套上一层守卫，任何目标
//     host 不等于 httptest 地址的请求都会立刻 t.Errorf 并被拒绝出网。于是「测试有没有
//     打真实上游」不依赖「记得把四个 base 都指到 fake」这件事本身——即使有人漏设一个
//     base（正是 HEAD 上 gating_test.go 既存泄漏的成因），守卫也会当场抓住并让用例失败，
//     而不是安静地发出去。
//  2. 请求账本按 (method, path) 记账：可以直接断言「有没有任何 POST 落到
//     /activity/growth/」，而不是只数 redeem——gift/comp/heatmap 这些旁路同样可能在
//     偷偷发写请求。
//  3. 假数据取 2026-10-28 的真实抓包形态：days=1 的账号 tier_7d_status 就是
//     "available"（真实响应如此，见验收报告 D 项实测）。所以「不领未达标档」这一条
//     完全由 growthEligibleTier 的 `days >= sp.Days` 兜住，档位状态字段挡不住——
//     用例必须按这个形态造数据，否则等于没测。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// qaReq 一条出站请求的账目。
type qaReq struct {
	Method string
	Path   string
}

// qaGrowthStub 国际版 / CN 通用的 growth + billing + report 桩。
// 所有可变态必须在启服务前设好（handler 只读）。
type qaGrowthStub struct {
	days    int
	tier7d  string
	tier14d string
	tier28d string

	streakMode  string // "" 正常 / "500" / "404" / "badjson"
	heatmapMode string // "" 正常 / "500"
	redeemMode  string // "" 成功 / "noda" 403 / "dup" 409 / "500"
	chances     int    // lottery/chances 余额
	drawMode    string // "" 成功 / "500"

	missed bool // 昨日 heatmap score==0
	cards  int  // makeup_cards.balance

	mu       sync.Mutex
	reqs     []qaReq
	redeemAt []string // 每次 redeem 的 tier
}

func qaNewStub() *qaGrowthStub {
	return &qaGrowthStub{
		days:    28,
		tier7d:  "available",
		tier14d: "available",
		tier28d: "available",
	}
}

func (s *qaGrowthStub) record(method, path string) {
	s.mu.Lock()
	s.reqs = append(s.reqs, qaReq{Method: method, Path: path})
	s.mu.Unlock()
}

// snapshot 请求账本拷贝。
func (s *qaGrowthStub) snapshot() []qaReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]qaReq, len(s.reqs))
	copy(out, s.reqs)
	return out
}

// count 某 (method, path) 的请求次数。
func (s *qaGrowthStub) count(method, path string) int {
	n := 0
	for _, r := range s.snapshot() {
		if r.Method == method && r.Path == path {
			n++
		}
	}
	return n
}

// postsUnder 前缀下所有 POST 请求（用于「growth 域有没有写请求」这类断言）。
func (s *qaGrowthStub) postsUnder(prefix string) []qaReq {
	var out []qaReq
	for _, r := range s.snapshot() {
		if r.Method == http.MethodPost && strings.HasPrefix(r.Path, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func (s *qaGrowthStub) total() int { return len(s.snapshot()) }

func (s *qaGrowthStub) claimedTiers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.redeemAt))
	copy(out, s.redeemAt)
	return out
}

// pathList 账本的 "METHOD path" 形式（日志/对比用）。
func (s *qaGrowthStub) pathList() []string {
	out := make([]string, 0, s.total())
	for _, r := range s.snapshot() {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

func (s *qaGrowthStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r.Method, r.URL.Path)
		// 与生产同口径：昨日按 CST 现算，避开 CST 零点附近的错日窗口。
		yesterday := travelDay(time.Now().AddDate(0, 0, -1))
		switch r.URL.Path {
		case "/v2/report":
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case "/activity/growth/streak":
			switch s.streakMode {
			case "500":
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`boom`))
				return
			case "404":
				http.Error(w, "not found", http.StatusNotFound)
				return
			case "badjson":
				w.Write([]byte(`{"code":0,"data":`))
				return
			}
			fmt.Fprintf(w, `{"code":0,"data":{"streak":{"days":%d},"makeup_cards":{"balance":%d,"max":5},`+
				`"redemption_status":{"tier_7d_status":"%s","tier_14d_status":"%s","tier_28d_status":"%s",`+
				`"remaining_days":%d,"tiers":[`+
				`{"tier":"7d","days":7,"credit":0,"energy":2,"cards":1,"chances":1},`+
				`{"tier":"14d","days":14,"credit":50,"energy":3,"cards":1,"chances":1},`+
				`{"tier":"28d","days":28,"credit":150,"energy":5,"cards":1,"chances":1}]}}}`,
				s.days, s.cards, s.tier7d, s.tier14d, s.tier28d, s.days)
		case "/activity/growth/heatmap":
			if s.heatmapMode == "500" {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`boom`))
				return
			}
			score := 1
			if s.missed {
				score = 0
			}
			fmt.Fprintf(w, `{"code":0,"data":{"cells":[{"date":"%s","score":%d}]}}`, yesterday, score)
		case "/activity/growth/makeup-cards/use":
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/activity/growth/redeem":
			var body struct {
				Tier string `json:"tier"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.redeemAt = append(s.redeemAt, body.Tier)
			s.mu.Unlock()
			switch s.redeemMode {
			case "noda":
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"code":403,"msg":"连续登录天数不足，请继续打卡"}`))
			case "dup":
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"code":409,"msg":"duplicate: already claimed"}`))
			case "500":
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`boom`))
			default:
				w.Write([]byte(`{"code":0,"data":{"cards_granted":1,"credit_granted":150,"energy_granted":5,"chances_granted":1}}`))
			}
		case "/activity/growth/lottery/chances":
			fmt.Fprintf(w, `{"code":0,"data":{"balance":%d}}`, s.chances)
		case "/activity/growth/lottery/draw":
			if s.drawMode == "500" {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`boom`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"prize_name":"10 积分","prize_type":"credit","credit_amount":10}}`))
		case "/billing/meter/claim-gift", "/billing/meter/claim-compensation":
			w.Write([]byte(`{"code":0,"data":{"credit":0}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
}

// qaHostGuard 出站硬闸：只放行 httptest 的 host，其余一律拒绝并让用例失败。
//
// 这是本文件「绝不打真实上游」的核心保证：不靠测试作者记得设 base，而是从
// Transport 层强制。HEAD 上 gating_test.go 的既存泄漏（只设 BillingBaseGlobal、
// 漏 ChatBaseGlobal → 回落到 https://www.workbuddy.ai）若发生在套了本守卫的用例里，
// 会当场 t.Errorf 而不是静默出网。
type qaHostGuard struct {
	t           *testing.T
	allowedHost string
	mu          sync.Mutex
	hosts       map[string]int
}

func (g *qaHostGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	g.mu.Lock()
	g.hosts[req.URL.Host]++
	g.mu.Unlock()
	if req.URL.Host != g.allowedHost {
		g.t.Errorf("出站泄漏：%s %s 的目标 host=%q 不是 fake 上游 %q —— 该用例会打真实域名",
			req.Method, req.URL.Path, req.URL.Host, g.allowedHost)
		return nil, fmt.Errorf("qa host guard blocked real upstream: %s", req.URL)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// qaSeenHosts 守卫放行/拦截过的 host 集合。
func (g *qaHostGuard) qaSeenHosts() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]int, len(g.hosts))
	for k, v := range g.hosts {
		out[k] = v
	}
	return out
}

// qaHarness 一套「桩 + 守卫 + 调度器」。
type qaHarness struct {
	stub *qaGrowthStub
	s    *Scheduler
	p    *pool.Pool
	g    *qaHostGuard
	srv  *httptest.Server
}

// qaNewHarness 构造 harness。四路 base 全部显式指向 httptest，并在返回前自检，
// 少设一路会立刻 t.Fatalf（早于 guard 的运行时拦截，定位更直接）。
func qaNewHarness(t *testing.T, stub *qaGrowthStub, accounts ...*auth.Auth) *qaHarness {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse httptest url: %v", err)
	}
	g := &qaHostGuard{t: t, allowedHost: u.Host, hosts: map[string]int{}}
	hc := &http.Client{Transport: g, Timeout: 10 * time.Second}

	p := pool.New("")
	for _, a := range accounts {
		p.Add(a)
	}
	up := &upstream.Client{
		HTTP: hc, ChatHTTP: hc, HTTPGlobal: hc, ChatHTTPGlobal: hc,
		ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		ChatBaseGlobal: srv.URL, BillingBaseGlobal: srv.URL,
		GlobalEnabled: true,
	}
	// 自检：四路 base 必须逐字等于 fake 地址。
	for name, got := range map[string]string{
		"ChatBaseCN": up.ChatBaseCN, "BillingBaseCN": up.BillingBaseCN,
		"ChatBaseGlobal": up.ChatBaseGlobal, "BillingBaseGlobal": up.BillingBaseGlobal,
	} {
		if got != srv.URL {
			t.Fatalf("%s=%q want %q（fake 上游）——否则请求会落到真实域名", name, got, srv.URL)
		}
	}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})
	return &qaHarness{stub: stub, s: s, p: p, g: g, srv: srv}
}

// qaGlobalAuth 国际版个人号（domain 落在 workbuddy.ai 家族）。
func qaGlobalAuth(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
}

// qaCNAuth CN 个人号（无 domain = cn realm）。
func qaCNAuth(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
}

// qaLogCapture 自建日志捕获（不复用 travel_test.go 的 captureLog）。
func qaLogCapture(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&qaWriter{mu: &mu, buf: &buf})
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	})
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type qaWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *qaWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// qaLines 统计日志中包含 substr 的行数。
func qaLines(logs, substr string) int {
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// qaExpireDailyClaim 把「当日已领」标记改成昨日，等价于「过了一个自然日」。
// 直接改字段而不是等到明天：本用例只验语义，不验 travelDay 实现（那有独立用例）。
func qaExpireDailyClaim(t *testing.T, s *Scheduler, uid string) {
	t.Helper()
	s.mu.Lock()
	s.rewardClaimed[uid] = travelDay(time.Now().Add(-24 * time.Hour))
	s.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 1. 达标即领：global 号 days=28 → redeem 恰好一次，挑最高已达标档
// ---------------------------------------------------------------------------

func TestQAGlobalEligibleRedeemsExactlyOnce(t *testing.T) {
	stub := qaNewStub()
	stub.chances = 1 // 领奖送的 chances → 应抽一次
	h := qaNewHarness(t, stub, qaGlobalAuth("g1"))

	logs := qaLogCapture(t)
	h.s.claimGrowthRewards(h.p.AuthByUID("g1"))

	if got := stub.count(http.MethodPost, "/activity/growth/redeem"); got != 1 {
		t.Fatalf("global 达标档 redeem 次数=%d want 1", got)
	}
	if tiers := stub.claimedTiers(); len(tiers) != 1 || tiers[0] != "28d" {
		t.Errorf("redeem 档位序列=%v want [28d]（只领最高已达标档，不逐档试探）", tiers)
	}
	if got := stub.count(http.MethodPost, "/activity/growth/lottery/draw"); got != 1 {
		t.Errorf("抽奖次数=1 时应抽 1 次，实际=%d", got)
	}
	if !h.s.rewardClaimedToday("g1") {
		t.Error("领取成功后应标记当日已处理")
	}
	if h.s.growthUnavailableToday("g1") {
		t.Error("成功领取不得被记成「链不可用」")
	}
	if got := qaLines(logs(), "redeem tier=28d ok"); got != 1 {
		t.Errorf("成功日志行数=%d want 1；日志=%q", got, logs())
	}
	// 出站守卫：全程只碰过 fake 上游。
	for host := range h.g.qaSeenHosts() {
		if host != h.g.allowedHost {
			t.Errorf("出站 host=%q 不是 fake 上游", host)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. 探测失败（500）：不领、日志 1 行、同日第二次零请求
// ---------------------------------------------------------------------------

func TestQAGlobalStreak500NoClaimOneLogThenSilent(t *testing.T) {
	stub := qaNewStub()
	stub.streakMode = "500"
	h := qaNewHarness(t, stub, qaGlobalAuth("g1"))

	logs := qaLogCapture(t)
	h.s.claimGrowthRewards(h.p.AuthByUID("g1"))

	if got := stub.count(http.MethodPost, "/activity/growth/redeem"); got != 0 {
		t.Errorf("拿不到 state 却发了 redeem=%d 次（want 0）", got)
	}
	if !h.s.growthUnavailableToday("g1") {
		t.Error("探测 500 应记入当日「链不可用」")
	}
	if got := qaLines(logs(), "growth chain unavailable today"); got != 1 {
		t.Errorf("不可用日志行数=%d want 1；日志=%q", got, logs())
	}

	before := stub.total()
	h.s.claimGrowthRewards(h.p.AuthByUID("g1")) // 同日第二次
	if after := stub.total(); after != before {
		t.Errorf("同日第二次调用新增上游请求 %d 个（want 0）：%v", after-before, stub.pathList())
	}
	if got := qaLines(logs(), "growth chain unavailable today"); got != 1 {
		t.Errorf("重复触发后日志行数=%d want 1（每号每天一行，靠记忆而非 sync.Once）", got)
	}
}

// ---------------------------------------------------------------------------
// 3. days=1 未达标：growth 域零写请求（真实形态：档位状态就是 available）
// ---------------------------------------------------------------------------

func TestQAGlobalDaysOneNoGrowthWrites(t *testing.T) {
	stub := qaNewStub()
	// 2026-10-28 真实抓包形态：days=1 时 tier_7d_status 仍是 "available"。
	// 若判据只看 status，这里就会 POST 7d——正是风控要求禁止的动作。
	stub.days = 1
	stub.tier7d, stub.tier14d, stub.tier28d = "available", "available", "locked"
	h := qaNewHarness(t, stub, qaGlobalAuth("g1"))

	h.s.claimGrowthRewards(h.p.AuthByUID("g1"))

	if posts := stub.postsUnder("/activity/growth/"); len(posts) != 0 {
		t.Fatalf("days=1 却对 growth 域发了写请求：%v（未达标档位绝不试探）", posts)
	}
	if got := stub.count(http.MethodPost, "/activity/growth/redeem"); got != 0 {
		t.Errorf("redeem 次数=%d want 0", got)
	}
	if h.s.growthUnavailableToday("g1") {
		t.Error("「没到档」不是「链不可用」，不得标记 unavailable")
	}
	if h.s.rewardClaimedToday("g1") {
		t.Error("没发任何写请求就不该记「当日已领」")
	}
	// 已确认的旁路：礼包/补偿是 billing 域「有则领」幂等写，先于档位判定，
	// 与 CN 同口径。这里把它钉成显式断言，避免日后悄悄多出第三个 POST。
	var billingPosts []qaReq
	for _, r := range stub.snapshot() {
		if r.Method == http.MethodPost && strings.HasPrefix(r.Path, "/billing/") {
			billingPosts = append(billingPosts, r)
		}
	}
	if len(billingPosts) != 2 {
		t.Errorf("billing 域「有则领」POST 数=%d want 2（claim-gift + claim-compensation）：%v",
			len(billingPosts), billingPosts)
	}
}

// ---------------------------------------------------------------------------
// 4. redeem 403 天数不足：静默、不标 unavailable、后续仍会尝试
// ---------------------------------------------------------------------------

func TestQAGlobalRedeem403SilentAndRetryable(t *testing.T) {
	stub := qaNewStub()
	stub.days = 8 // 7d 达标，但服务端判天数不足
	stub.tier14d, stub.tier28d = "locked", "locked"
	stub.redeemMode = "noda"
	h := qaNewHarness(t, stub, qaGlobalAuth("g1"))

	logs := qaLogCapture(t)
	h.s.claimGrowthRewards(h.p.AuthByUID("g1"))

	if got := stub.count(http.MethodPost, "/activity/growth/redeem"); got != 1 {
		t.Fatalf("redeem 次数=%d want 1", got)
	}
	if h.s.growthUnavailableToday("g1") {
		t.Fatal("403 天数不足是正常业务态，标 unavailable 会白丢当天后续领取机会")
	}
	if got := qaLines(logs(), "WARN"); got != 0 {
		t.Errorf("403 天数不足不得刷 WARN，实际 %d 行；日志=%q", got, logs())
	}
	if got := qaLines(logs(), "skip (already claimed or days not enough)"); got != 1 {
		t.Errorf("403 应走「正常态静默」日志，实际行数=%d；日志=%q", got, logs())
	}

	// 等价于「次日」：仍会重新尝试 redeem（说明 403 没把该号打成当日不可用）。
	qaExpireDailyClaim(t, h.s, "g1")
	h.s.claimGrowthRewards(h.p.AuthByUID("g1"))
	if got := stub.count(http.MethodPost, "/activity/growth/redeem"); got != 2 {
		t.Errorf("403 之后的下一轮 redeem 次数=%d want 2（403 不得导致后续不再尝试）", got)
	}
	if h.s.growthUnavailableToday("g1") {
		t.Error("两次 403 都不该标记 unavailable")
	}
}

// ---------------------------------------------------------------------------
// 5. redeem 409 duplicate：静默、按已领处理
// ---------------------------------------------------------------------------

func TestQAGlobalRedeem409SilentMarkedClaimed(t *testing.T) {
	stub := qaNewStub()
	stub.redeemMode = "dup"
	h := qaNewHarness(t, stub, qaGlobalAuth("g1"))

	logs := qaLogCapture(t)
	h.s.claimGrowthRewards(h.p.AuthByUID("g1"))

	if got := stub.count(http.MethodPost, "/activity/growth/redeem"); got != 1 {
		t.Errorf("redeem 次数=%d want 1", got)
	}
	if !h.s.rewardClaimedToday("g1") {
		t.Error("409 duplicate = 本月已领 → 应记「当日已处理」")
	}
	if h.s.growthUnavailableToday("g1") {
		t.Error("409 duplicate 不得标记 unavailable")
	}
	if got := qaLines(logs(), "WARN"); got != 0 {
		t.Errorf("409 duplicate 不得刷 WARN，实际 %d 行；日志=%q", got, logs())
	}
}

// ---------------------------------------------------------------------------
// 6. 补签三态：有卡补 / 无卡不补 / heatmap 失败静默且不阻断领取
// ---------------------------------------------------------------------------

func TestQAGlobalMakeupCases(t *testing.T) {
	t.Run("有卡且昨日漏签_补一次", func(t *testing.T) {
		stub := qaNewStub()
		stub.missed, stub.cards = true, 2
		h := qaNewHarness(t, stub, qaGlobalAuth("g1"))

		h.s.claimGrowthRewards(h.p.AuthByUID("g1"))

		if got := stub.count(http.MethodPost, "/activity/growth/makeup-cards/use"); got != 1 {
			t.Errorf("makeup 次数=%d want 1（global 漏签+有卡应补签）", got)
		}
		// 探测 1 次 + 查卡 1 次 + 补签成功后重读 1 次 = 3 次 streak。
		if got := stub.count(http.MethodGet, "/activity/growth/streak"); got != 3 {
			t.Errorf("streak 次数=%d want 3（探测+查卡+补签后重读）", got)
		}
	})

	t.Run("无卡_不写", func(t *testing.T) {
		stub := qaNewStub()
		stub.missed, stub.cards = true, 0
		h := qaNewHarness(t, stub, qaGlobalAuth("g1"))

		h.s.claimGrowthRewards(h.p.AuthByUID("g1"))

		if got := stub.count(http.MethodPost, "/activity/growth/makeup-cards/use"); got != 0 {
			t.Errorf("makeup 次数=%d want 0（无卡不写）", got)
		}
	})

	t.Run("heatmap失败_静默且不阻断领取", func(t *testing.T) {
		stub := qaNewStub()
		stub.heatmapMode = "500"
		h := qaNewHarness(t, stub, qaGlobalAuth("g1"))

		logs := qaLogCapture(t)
		h.s.claimGrowthRewards(h.p.AuthByUID("g1"))

		if got := stub.count(http.MethodPost, "/activity/growth/makeup-cards/use"); got != 0 {
			t.Errorf("makeup 次数=%d want 0（判据失败不写）", got)
		}
		if h.s.growthUnavailableToday("g1") {
			t.Error("heatmap 失败只是判据失败，不代表链不可用")
		}
		if got := qaLines(logs(), "WARN"); got != 0 {
			t.Errorf("heatmap 失败应静默，实际 WARN %d 行；日志=%q", got, logs())
		}
		if got := stub.count(http.MethodPost, "/activity/growth/redeem"); got != 1 {
			t.Errorf("redeem 次数=%d want 1（heatmap 失败不阻断挑档领取）", got)
		}
	})
}

// ---------------------------------------------------------------------------
// 7. 企业版仍跳过（global + enterprise 双条件正交）
// ---------------------------------------------------------------------------

func TestQAGlobalEnterpriseStillSkipped(t *testing.T) {
	stub := qaNewStub()
	ent := qaGlobalAuth("ge1")
	ent.EnterpriseID = "ent-1"
	h := qaNewHarness(t, stub, ent)

	h.s.claimGrowthRewards(h.p.AuthByUID("ge1"))

	if got := stub.total(); got != 0 {
		t.Errorf("global+企业版上游请求=%d want 0（企业版无个人成长体系）：%v", got, stub.pathList())
	}
	if h.s.growthUnavailableToday("ge1") {
		t.Error("企业版是门控跳过，不是「链不可用」，不该写失败记忆")
	}
}

// ---------------------------------------------------------------------------
// 8. 探测解析失败（404 / 坏 JSON）同样记不可用 + 一行日志
// ---------------------------------------------------------------------------

func TestQAGlobalProbeFailuresMarkUnavailable(t *testing.T) {
	for _, mode := range []string{"404", "badjson"} {
		t.Run(mode, func(t *testing.T) {
			stub := qaNewStub()
			stub.streakMode = mode
			h := qaNewHarness(t, stub, qaGlobalAuth("g1"))

			logs := qaLogCapture(t)
			h.s.claimGrowthRewards(h.p.AuthByUID("g1"))

			if !h.s.growthUnavailableToday("g1") {
				t.Errorf("mode=%s 应记「链不可用」", mode)
			}
			if got := stub.count(http.MethodPost, "/activity/growth/redeem"); got != 0 {
				t.Errorf("mode=%s redeem 次数=%d want 0", mode, got)
			}
			if got := qaLines(logs(), "growth chain unavailable today"); got != 1 {
				t.Errorf("mode=%s 日志行数=%d want 1", mode, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 9. 失败记忆跨自然日自动恢复（不需要重启进程）
// ---------------------------------------------------------------------------

func TestQAGlobalUnavailableExpiresNextDay(t *testing.T) {
	stub := qaNewStub()
	h := qaNewHarness(t, stub, qaGlobalAuth("g1"))

	// 昨日失败记忆：今日必须重新探测并正常领取。
	h.s.mu.Lock()
	h.s.rewardUnavailable["g1"] = travelDay(time.Now().Add(-24 * time.Hour))
	h.s.mu.Unlock()

	h.s.claimGrowthRewards(h.p.AuthByUID("g1"))

	if got := stub.count(http.MethodGet, "/activity/growth/streak"); got == 0 {
		t.Error("跨日后应重新探测 streak（失败记忆按自然日重置）")
	}
	if got := stub.count(http.MethodPost, "/activity/growth/redeem"); got != 1 {
		t.Errorf("跨日恢复后 redeem 次数=%d want 1", got)
	}
}

// ---------------------------------------------------------------------------
// 10. 失败记忆按 uid 隔离：global 的失败不波及 CN
// ---------------------------------------------------------------------------

func TestQAUnavailableMemoryIsPerUID(t *testing.T) {
	stub := qaNewStub()
	h := qaNewHarness(t, stub, qaGlobalAuth("g1"), qaCNAuth("u1"))

	h.s.markGrowthUnavailable("g1")
	h.s.claimGrowthRewards(h.p.AuthByUID("u1"))

	if got := stub.count(http.MethodPost, "/activity/growth/redeem"); got != 1 {
		t.Errorf("CN 号 redeem 次数=%d want 1（global 的失败记忆不得波及 CN）", got)
	}
	if h.s.growthUnavailableToday("u1") {
		t.Error("CN 号不该被标成链不可用")
	}
}

// ---------------------------------------------------------------------------
// 11. CN 零回归：同一账号形态在 CN 上仍走完整链（成功路径逐请求对齐 HEAD）
// ---------------------------------------------------------------------------

func TestQACNChainUnchanged(t *testing.T) {
	stub := qaNewStub()
	stub.chances = 1
	h := qaNewHarness(t, stub, qaCNAuth("u1"))

	h.s.claimGrowthRewards(h.p.AuthByUID("u1"))

	// 逐请求账本（顺序即代码路径顺序）：礼包 → 补偿 → 探测 → heatmap → redeem → chances → draw。
	want := []string{
		"POST /billing/meter/claim-gift",
		"POST /billing/meter/claim-compensation",
		"GET /activity/growth/streak",
		"GET /activity/growth/heatmap",
		"POST /activity/growth/redeem",
		"GET /activity/growth/lottery/chances",
		"POST /activity/growth/lottery/draw",
	}
	got := stub.pathList()
	if len(got) != len(want) {
		t.Fatalf("CN 请求序列=%v\nwant=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("CN 第 %d 个请求=%q want %q（成功路径零回归）\n全部=%v", i+1, got[i], want[i], got)
		}
	}
	if tiers := stub.claimedTiers(); len(tiers) != 1 || tiers[0] != "28d" {
		t.Errorf("CN redeem 档位=%v want [28d]", tiers)
	}
	if h.s.growthUnavailableToday("u1") {
		t.Error("CN 成功路径不得标记 unavailable")
	}
}

// ---------------------------------------------------------------------------
// 12. 全链集成：RunActivityNow 对 global 号 = report + streak 自检 + 领奖链
//     （顺带证明没有任何请求离开 fake 上游）
// ---------------------------------------------------------------------------

func TestQARunActivityGlobalFullChainNoRealEgress(t *testing.T) {
	oldDelay, oldGap := activityAccountDelay, activityReportGap
	activityAccountDelay, activityReportGap = 0, 0
	t.Cleanup(func() { activityAccountDelay, activityReportGap = oldDelay, oldGap })

	stub := qaNewStub()
	stub.chances = 1
	h := qaNewHarness(t, stub, qaGlobalAuth("g1"))

	h.s.RunActivityNow()

	if got := stub.count(http.MethodPost, "/v2/report"); got != 1 {
		t.Errorf("global 上报次数=%d want 1（国际版上报早已放开）", got)
	}
	// report → streak 自检 → 领奖链（gift/comp/state/heatmap/redeem/chances/draw）。
	if got := stub.count(http.MethodGet, "/activity/growth/streak"); got < 2 {
		t.Errorf("streak 次数=%d want >=2（自检 1 + 领奖链探测 1）", got)
	}
	if got := stub.count(http.MethodPost, "/activity/growth/redeem"); got != 1 {
		t.Errorf("redeem 次数=%d want 1", got)
	}
	// 硬闸复核：所有出站只碰过 fake host。
	seen := h.g.qaSeenHosts()
	if len(seen) != 1 {
		t.Errorf("出站 host 集合=%v want 仅 fake 上游一个", seen)
	}
	for host := range seen {
		if host != h.g.allowedHost {
			t.Errorf("出站 host=%q 是真实域名（测试泄漏）", host)
		}
	}
}

// ---------------------------------------------------------------------------
// 13. 无档可领时不得顺手抽奖（lottery 只在 redeem 之后，chances 不为 0 也不抽）
// ---------------------------------------------------------------------------

func TestQAGlobalNoEligibleTierSkipsLottery(t *testing.T) {
	stub := qaNewStub()
	stub.days = 28
	stub.tier7d, stub.tier14d, stub.tier28d = "claimed", "claimed", "claimed" // 三档全领完
	stub.chances = 3                                                          // 实测 foginsky 现状：3 次机会
	h := qaNewHarness(t, stub, qaGlobalAuth("g1"))

	h.s.claimGrowthRewards(h.p.AuthByUID("g1"))

	if got := stub.count(http.MethodPost, "/activity/growth/redeem"); got != 0 {
		t.Errorf("三档已领完仍发 redeem=%d 次（want 0）", got)
	}
	if got := stub.count(http.MethodPost, "/activity/growth/lottery/draw"); got != 0 {
		t.Errorf("无档可领时不该进抽奖段，draw=%d 次（want 0）", got)
	}
	if got := stub.postsUnder("/activity/growth/"); len(got) != 0 {
		t.Errorf("无档可领时 growth 域写请求=%v want 0", got)
	}
	if h.s.growthUnavailableToday("g1") {
		t.Error("三档已领完是正常态，不是「链不可用」")
	}
}
