package scheduler

// global_reward_test.go 国际版（realm=global）连登奖励链的行为与风控边界。
//
// 背景（2026-10-28 实测更正）：此前 claimGrowthRewards 里有一道「global 无条件 return」
// 的门，依据是「global 新账号 GET /activity/growth/streak 返回 500（sliverkiss 个例）」
// ——个例被当成了普遍规律。三个 www.workbuddy.ai 账号（foginsky / sofs2005 / jousleep）
// 实测 streak 全 200、foginsky 实领 7d/14d/28d 三档、积分 1044 → 1244。门因此换成
// 「探测失败才跳过」，本文件锁死换门之后的**风控边界**：
//
//	1. 达标就领（28d available → redeem 一次，tier=28d）
//	2. 探测失败 → 不领 + 每号每天一行日志 + 同日不再发任何请求（失败记忆）
//	3. 未达标 → 不试探（days=1 时绝不 POST 任何档位）
//	4. redeem 403 天数不足 = 正常态：静默、不记不可用、改天照样尝试
//	5. redeem 409 duplicate = 正常态：静默、按已领处理
//	6. 补签对 global 生效（昨日漏签 + 有卡才补）
//	7. 企业版仍跳过（回归）
//
// 全部走 fake 上游，不触真实接口。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// globalGrowthStub 模拟国际版 growth 域 + billing 域领取端点。
//
// 与 rewardStub/bonusChainStub 同构，但两处刻意不同：
//   - streakMode 可切（正常 / 500 / 404 / 坏 JSON）：实测里既有一切正常的号，也有
//     确实拿不到 state 的号，门控必须对两者都给出正确行为；
//   - 统计**全部**上游调用总量（totalCalls）：风控要求「当日探测失败后不再发任何请求」，
//     只看 redeem 次数是测不出来的——gift/comp/heatmap 都可能在偷偷继续发。
type globalGrowthStub struct {
	days   int    // streak.days
	tier7d string // redemption_status.tier_7d_status
	tier14 string
	tier28 string

	streakMode string // "" 正常 / "500" / "404" / "badjson"
	redeemMode string // "" 成功 / "dup" 409 / "noda" 403 / "500"
	// redeemModes 按调用序号覆盖 redeemMode（越界取最后一项）：用于「第一次 403、
	// 之后成功」这类同号多轮场景。必须在启服务前设好（handler 里只读，不并发写）。
	redeemModes []string
	chances     int    // lottery/chances balance
	drawMode    string // "" 成功 / "500"

	missed bool // 昨日 heatmap score==0（漏签）
	cards  int  // makeup_cards.balance

	streakCalls  atomic.Int32
	heatmapCalls atomic.Int32
	makeupCalls  atomic.Int32
	giftCalls    atomic.Int32
	compCalls    atomic.Int32
	redeemCalls  atomic.Int32
	chanceCalls  atomic.Int32
	drawCalls    atomic.Int32
	totalCalls   atomic.Int32

	mu         sync.Mutex
	redeemTier []string // 每次 redeem 请求的 tier（按序）
}

// newGlobalGrowthStub 默认：28 天连登、三档全 available、无漏签、无补签卡、
// 抽奖次数 0（不抽）、redeem 成功。想测别的分支就改字段。
func newGlobalGrowthStub() *globalGrowthStub {
	return &globalGrowthStub{
		days:    28,
		tier7d:  "available",
		tier14:  "available",
		tier28:  "available",
		chances: 0,
	}
}

// tiers 返回记录下来的 redeem 档位序列（拷贝，避免测试侧读时被写）。
func (s *globalGrowthStub) tiers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.redeemTier))
	copy(out, s.redeemTier)
	return out
}

func (s *globalGrowthStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.totalCalls.Add(1)
		// 昨日标签必须与生产侧同口径（CST 自然日）：判据是
		// upstream.GrowthYesterdayDate(time.Now())，容器时区恒 UTC，
		// 用 travelDay 现算可避开 CST 零点附近的错日窗口（同 growth_bonus_test）。
		yest := travelDay(time.Now().AddDate(0, 0, -1))
		switch r.URL.Path {
		case "/activity/growth/streak":
			s.streakCalls.Add(1)
			switch s.streakMode {
			case "500":
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
				return
			case "404":
				http.Error(w, "not found", 404)
				return
			case "badjson":
				w.Write([]byte(`{"code":0,"data":`)) // 截断：解析失败
				return
			}
			fmt.Fprintf(w, `{"code":0,"data":{"streak":{"days":%d},"makeup_cards":{"balance":%d,"max":5},`+
				`"redemption_status":{"tier_7d_status":"%s","tier_14d_status":"%s","tier_28d_status":"%s",`+
				`"remaining_days":%d,"tiers":[`+
				`{"tier":"7d","days":7,"credit":0,"energy":2,"cards":1,"chances":1},`+
				`{"tier":"14d","days":14,"credit":50,"energy":3,"cards":1,"chances":1},`+
				`{"tier":"28d","days":28,"credit":150,"energy":5,"cards":1,"chances":1}]}}}`,
				s.days, s.cards, s.tier7d, s.tier14, s.tier28, s.days)
		case "/activity/growth/heatmap":
			s.heatmapCalls.Add(1)
			score := 1
			if s.missed {
				score = 0
			}
			fmt.Fprintf(w, `{"code":0,"data":{"cells":[{"date":"%s","score":%d}]}}`, yest, score)
		case "/activity/growth/makeup-cards/use":
			s.makeupCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/billing/meter/claim-gift":
			s.giftCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"credit":0}}`))
		case "/billing/meter/claim-compensation":
			s.compCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"credit":0}}`))
		case "/activity/growth/redeem":
			n := s.redeemCalls.Add(1)
			var body struct {
				Tier string `json:"tier"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.redeemTier = append(s.redeemTier, body.Tier)
			s.mu.Unlock()
			mode := s.redeemMode
			if len(s.redeemModes) > 0 {
				if i := int(n) - 1; i < len(s.redeemModes) {
					mode = s.redeemModes[i]
				} else {
					mode = s.redeemModes[len(s.redeemModes)-1]
				}
			}
			switch mode {
			case "dup":
				w.WriteHeader(409)
				w.Write([]byte(`{"code":409,"msg":"duplicate: already claimed"}`))
			case "noda":
				w.WriteHeader(403)
				w.Write([]byte(`{"code":403,"msg":"连续登录天数不足，请继续打卡或使用补签卡"}`))
			case "500":
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
			default:
				w.Write([]byte(`{"code":0,"data":{"cards_granted":1,"credit_granted":150,"energy_granted":5,"chances_granted":1}}`))
			}
		case "/activity/growth/lottery/chances":
			s.chanceCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"data":{"balance":%d}}`, s.chances)
		case "/activity/growth/lottery/draw":
			s.drawCalls.Add(1)
			if s.drawMode == "500" {
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"prize_code":"积分_1","prize_name":"10 积分","prize_type":"credit","credit_amount":10}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

// globalRewardScheduler 构造「global 号 + global base 指向 fake 服务」的调度器。
//
// 关键点：ChatBaseGlobal/BillingBaseGlobal 必须一起指到 httptest 服务，否则
// chatBase() 会回落到缺省的 https://www.workbuddy.ai —— 测试就变成打真实上游了。
func globalRewardScheduler(t *testing.T, srv *httptest.Server, uid string) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{
		HTTP:              srv.Client(),
		ChatBaseCN:        srv.URL,
		BillingBaseCN:     srv.URL,
		ChatBaseGlobal:    srv.URL,
		BillingBaseGlobal: srv.URL,
		GlobalEnabled:     true,
	}
	return New(Config{Pool: p, Upstream: up, ActivityReportCount: 1}), p
}

// countLogLines 统计日志里包含 substr 的行数（观测「每号每天一行」这类承诺）。
func countLogLines(logs, substr string) int {
	n := 0
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// TestGlobalClaimsEligibleTier 风控第 1 条：global 号 days=28、28d available →
// redeem 恰好一次且 tier=28d（挑最高已达标档），领完照常抽奖。
func TestGlobalClaimsEligibleTier(t *testing.T) {
	fastActivity(t)
	stub := newGlobalGrowthStub() // days=28，三档 available
	stub.chances = 1              // 领奖送的 chances → 应抽一次
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := globalRewardScheduler(t, srv, "g1")

	s.claimGrowthRewards(p.AuthByUID("g1"))

	if n := stub.redeemCalls.Load(); n != 1 {
		t.Fatalf("redeem calls=%d want 1（global 达标档应领取）", n)
	}
	if tiers := stub.tiers(); len(tiers) != 1 || tiers[0] != "28d" {
		t.Errorf("redeem tiers=%v want [28d]（只领最高已达标档）", tiers)
	}
	if n := stub.drawCalls.Load(); n != 1 {
		t.Errorf("draw calls=%d want 1（领奖送的 chances 应抽掉）", n)
	}
	if !s.rewardClaimedToday("g1") {
		t.Error("领取成功应标记当日已处理（当日不再重复写）")
	}
	if s.growthUnavailableToday("g1") {
		t.Error("成功领取不得标记「链不可用」")
	}
}

// TestGlobalStreakFailureMarksUnavailableOnce 风控第 2 条：探测失败（500）→
// 不领、只打一行日志、同日第二次调用不再发任何请求。
func TestGlobalStreakFailureMarksUnavailableOnce(t *testing.T) {
	fastActivity(t)
	stub := newGlobalGrowthStub()
	stub.streakMode = "500"
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := globalRewardScheduler(t, srv, "g1")

	logs := captureLog(t)
	s.claimGrowthRewards(p.AuthByUID("g1"))

	if n := stub.redeemCalls.Load(); n != 0 {
		t.Errorf("redeem calls=%d want 0（拿不到 state 就不该动写接口）", n)
	}
	if !s.growthUnavailableToday("g1") {
		t.Error("探测失败应记入当日「链不可用」记忆")
	}
	got := logs()
	if n := countLogLines(got, "growth chain unavailable today"); n != 1 {
		t.Errorf("unavailable 日志行数=%d want 1；日志=%q", n, got)
	}
	if strings.Contains(got, "redeem") {
		t.Errorf("失败路径不应有 redeem 日志；日志=%q", got)
	}

	// 同日第二次调用：任何端点都不再打（失败记忆生效，靠记忆而不是靠 sync.Once）。
	before := stub.totalCalls.Load()
	s.claimGrowthRewards(p.AuthByUID("g1"))
	if after := stub.totalCalls.Load(); after != before {
		t.Errorf("同日第二次调用上游请求数 %d → %d（want 0 新增）", before, after)
	}
	if n := countLogLines(logs(), "growth chain unavailable today"); n != 1 {
		t.Errorf("重复触发后日志行数=%d want 1（每号每天一行）", n)
	}
}

// TestGlobalStreakParseFailureMarksUnavailable 解析失败同样算「链不可用」
// （风控第 7 条的语义分类：404/5xx/网络/解析 一律不可用）。
func TestGlobalStreakParseFailureMarksUnavailable(t *testing.T) {
	fastActivity(t)
	for _, mode := range []string{"404", "badjson"} {
		t.Run(mode, func(t *testing.T) {
			stub := newGlobalGrowthStub()
			stub.streakMode = mode
			srv := httptest.NewServer(stub.handler())
			defer srv.Close()
			s, p := globalRewardScheduler(t, srv, "g1")

			logs := captureLog(t)
			s.claimGrowthRewards(p.AuthByUID("g1"))

			if n := stub.redeemCalls.Load(); n != 0 {
				t.Errorf("redeem calls=%d want 0", n)
			}
			if !s.growthUnavailableToday("g1") {
				t.Errorf("mode=%s 应记入「链不可用」", mode)
			}
			if n := countLogLines(logs(), "growth chain unavailable today"); n != 1 {
				t.Errorf("mode=%s 日志行数=%d want 1", mode, n)
			}
		})
	}
}

// TestGlobalUnavailableExpiresNextDay 失败记忆按「号 + 自然日（CST）」：昨日标记
// 不影响今日探测（自动恢复，不需要重启进程）。
func TestGlobalUnavailableExpiresNextDay(t *testing.T) {
	fastActivity(t)
	stub := newGlobalGrowthStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := globalRewardScheduler(t, srv, "g1")

	s.rewardUnavailable["g1"] = travelDay(time.Now().Add(-24 * time.Hour)) // 昨日不可用
	s.claimGrowthRewards(p.AuthByUID("g1"))

	if n := stub.streakCalls.Load(); n != 1 {
		t.Errorf("streak calls=%d want 1（跨日应重新探测）", n)
	}
	if n := stub.redeemCalls.Load(); n != 1 {
		t.Errorf("redeem calls=%d want 1（跨日恢复后正常领取）", n)
	}
}

// TestGlobalDaysNotEnoughNoProbe 风控第 3 条（核心）：days=1、28d locked →
// 一个写接口都不碰。绝不试探未达标档位。
func TestGlobalDaysNotEnoughNoProbe(t *testing.T) {
	fastActivity(t)
	stub := newGlobalGrowthStub()
	stub.days = 1
	stub.tier7d, stub.tier14, stub.tier28 = "locked", "locked", "locked"
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := globalRewardScheduler(t, srv, "g1")

	s.claimGrowthRewards(p.AuthByUID("g1"))

	if n := stub.redeemCalls.Load(); n != 0 {
		t.Errorf("redeem calls=%d want 0（未达标绝不试探）", n)
	}
	if n := stub.chanceCalls.Load(); n != 0 {
		t.Errorf("chances calls=%d want 0（没领奖就不该进抽奖段）", n)
	}
	if s.rewardClaimedToday("g1") {
		t.Error("无档可领不应标记当日已领（未发任何写请求）")
	}
	if s.growthUnavailableToday("g1") {
		t.Error("「没到档」不等于「链不可用」，不得标记 unavailable")
	}
}

// TestGlobalStreakZeroDaysIsNotUnavailable 风控第 7 条第 3 项：streak 200 但 days==0
// （上游静默丢弃）按「无档可领」走完流程，不标记 unavailable——否则一次静默丢弃
// 会让该号当天彻底不领奖。
func TestGlobalStreakZeroDaysIsNotUnavailable(t *testing.T) {
	fastActivity(t)
	stub := newGlobalGrowthStub()
	stub.days = 0
	stub.tier7d, stub.tier14, stub.tier28 = "locked", "locked", "locked"
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := globalRewardScheduler(t, srv, "g1")

	s.claimGrowthRewards(p.AuthByUID("g1"))

	if n := stub.redeemCalls.Load(); n != 0 {
		t.Errorf("redeem calls=%d want 0", n)
	}
	if s.growthUnavailableToday("g1") {
		t.Error("days==0 是「无档可领」不是「链不可用」")
	}
}

// TestGlobalRedeemNotEnoughDaysSilent 风控第 4 条：redeem 403 天数不足 = 正常业务态。
// 静默（无 WARN）、不标记 unavailable——把已领标记模拟成昨日后再调一次，仍会尝试领取。
func TestGlobalRedeemNotEnoughDaysSilent(t *testing.T) {
	fastActivity(t)
	stub := newGlobalGrowthStub()
	stub.days = 8 // 有 7d 档可挑，但服务端判天数不足
	stub.tier14, stub.tier28 = "locked", "locked"
	stub.redeemModes = []string{"noda", ""} // 第一次 403 天数不足，第二次正常
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := globalRewardScheduler(t, srv, "g1")

	logs := captureLog(t)
	s.claimGrowthRewards(p.AuthByUID("g1"))

	if n := stub.redeemCalls.Load(); n != 1 {
		t.Fatalf("redeem calls=%d want 1（7d 达标应尝试）", n)
	}
	if s.growthUnavailableToday("g1") {
		t.Fatal("403 天数不足是正常态，不得标记 unavailable（否则当天白丢一次领取机会）")
	}
	if got := logs(); strings.Contains(got, "WARN") {
		t.Errorf("403 天数不足不得刷 WARN；日志=%q", got)
	}

	// 把「当日已领」标记模拟成昨日（等价于次日）：仍会尝试领取，证明 403 没有把该号
	// 打成「今日不可用」（若误记 unavailable，第二次调用会在入口被挡，redeem 仍为 1）。
	s.rewardClaimed["g1"] = travelDay(time.Now().Add(-24 * time.Hour))
	s.claimGrowthRewards(p.AuthByUID("g1"))

	if n := stub.redeemCalls.Load(); n != 2 {
		t.Errorf("redeem calls=%d want 2（403 不该导致后续不再尝试）", n)
	}
	if tiers := stub.tiers(); len(tiers) != 2 || tiers[0] != "7d" || tiers[1] != "7d" {
		t.Errorf("redeem tiers=%v want [7d 7d]", tiers)
	}
	if s.growthUnavailableToday("g1") {
		t.Error("第二次成功后更不该标记 unavailable")
	}
}

// TestGlobalRedeemDuplicateSilent 风控第 5 条：409 duplicate = 本月已领，静默、按已领处理。
func TestGlobalRedeemDuplicateSilent(t *testing.T) {
	fastActivity(t)
	stub := newGlobalGrowthStub()
	stub.redeemMode = "dup"
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := globalRewardScheduler(t, srv, "g1")

	logs := captureLog(t)
	s.claimGrowthRewards(p.AuthByUID("g1"))

	if n := stub.redeemCalls.Load(); n != 1 {
		t.Errorf("redeem calls=%d want 1", n)
	}
	if !s.rewardClaimedToday("g1") {
		t.Error("duplicate 按已领处理 → 应标记当日已处理")
	}
	if s.growthUnavailableToday("g1") {
		t.Error("409 duplicate 不得标记 unavailable")
	}
	if got := logs(); strings.Contains(got, "WARN") {
		t.Errorf("409 duplicate 不得刷 WARN；日志=%q", got)
	}
}

// TestGlobalRedeemServerErrorMarksUnavailable 风控第 7 条第 2 项：redeem 5xx（非 403/409）
// 视为链不可用 → 记 unavailable（当日不再重打；此处不新增日志行，redeem 那行已足够）。
func TestGlobalRedeemServerErrorMarksUnavailable(t *testing.T) {
	fastActivity(t)
	stub := newGlobalGrowthStub()
	stub.redeemMode = "500"
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := globalRewardScheduler(t, srv, "g1")

	logs := captureLog(t)
	s.claimGrowthRewards(p.AuthByUID("g1"))

	if n := stub.redeemCalls.Load(); n != 1 {
		t.Errorf("redeem calls=%d want 1", n)
	}
	if !s.growthUnavailableToday("g1") {
		t.Error("5xx 视为链不可用，应记 unavailable")
	}
	if n := countLogLines(logs(), "redeem tier=28d"); n != 1 {
		t.Errorf("redeem 失败日志行数=%d want 1（每号每天一行）", n)
	}
}

// TestGlobalMakeupMissedWithCards 风控第 6 条：global 昨日漏签 + 有卡 → 补签。
func TestGlobalMakeupMissedWithCards(t *testing.T) {
	fastActivity(t)
	stub := newGlobalGrowthStub()
	stub.missed = true
	stub.cards = 2
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := globalRewardScheduler(t, srv, "g1")

	s.claimGrowthRewards(p.AuthByUID("g1"))

	if n := stub.makeupCalls.Load(); n != 1 {
		t.Errorf("makeup calls=%d want 1（global 漏签+有卡应补签）", n)
	}
	// reward-state + 查卡余额 + 补签成功后重读 = 3 次 streak。
	if n := stub.streakCalls.Load(); n != 3 {
		t.Errorf("streak calls=%d want 3（探测+查卡+补签后重读）", n)
	}
}

// TestGlobalMakeupNoCardsNoWrite 无卡 → 不补签（判据不变：heatmap score==0 且有卡）。
func TestGlobalMakeupNoCardsNoWrite(t *testing.T) {
	fastActivity(t)
	stub := newGlobalGrowthStub()
	stub.missed = true
	stub.cards = 0
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := globalRewardScheduler(t, srv, "g1")

	s.claimGrowthRewards(p.AuthByUID("g1"))

	if n := stub.makeupCalls.Load(); n != 0 {
		t.Errorf("makeup calls=%d want 0（无卡不写）", n)
	}
}

// TestGlobalMakeupHeatmapFailureSilent heatmap 查询失败 → 静默（只读判据，不升级成 WARN，
// 也不记 unavailable——补签判据失败不代表链不可用）。
func TestGlobalMakeupHeatmapFailureSilent(t *testing.T) {
	fastActivity(t)
	stub := newGlobalGrowthStub()
	base := stub.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/activity/growth/heatmap" {
			stub.totalCalls.Add(1)
			w.WriteHeader(500)
			w.Write([]byte(`boom`))
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer srv.Close()
	s, p := globalRewardScheduler(t, srv, "g1")

	logs := captureLog(t)
	s.claimGrowthRewards(p.AuthByUID("g1"))

	if n := stub.makeupCalls.Load(); n != 0 {
		t.Errorf("makeup calls=%d want 0（判据失败不写）", n)
	}
	if s.growthUnavailableToday("g1") {
		t.Error("heatmap 失败只是判据失败，不得标记链不可用")
	}
	if got := logs(); strings.Contains(got, "heatmap") {
		t.Errorf("heatmap 失败应静默；日志=%q", got)
	}
	// 链继续走：heatmap 失败不影响挑档领取。
	if n := stub.redeemCalls.Load(); n != 1 {
		t.Errorf("redeem calls=%d want 1（heatmap 失败不阻断领取）", n)
	}
}

// TestGlobalEnterpriseStillSkipped 风控回归：global + 企业版仍然零上游调用
// （企业版确实没有成长体系，与 realm 正交）。
func TestGlobalEnterpriseStillSkipped(t *testing.T) {
	fastActivity(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected for enterprise", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "ge1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai", EnterpriseID: "ent-1"})
	up := &upstream.Client{
		HTTP:              srv.Client(),
		ChatBaseCN:        srv.URL,
		BillingBaseCN:     srv.URL,
		ChatBaseGlobal:    srv.URL,
		BillingBaseGlobal: srv.URL,
		GlobalEnabled:     true,
	}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.claimGrowthRewards(p.AuthByUID("ge1"))

	if n := calls.Load(); n != 0 {
		t.Errorf("global+enterprise upstream calls=%d want 0（企业版门控优先）", n)
	}
	if s.growthUnavailableToday("ge1") {
		t.Error("企业版是门控跳过，不是「链不可用」，不该写失败记忆")
	}
}

// TestGlobalAndCNShareNoState global 与 CN 号的失败记忆互不影响（按 uid 隔离）。
func TestGlobalAndCNShareNoState(t *testing.T) {
	fastActivity(t)
	stub := newGlobalGrowthStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:              srv.Client(),
		ChatBaseCN:        srv.URL,
		BillingBaseCN:     srv.URL,
		ChatBaseGlobal:    srv.URL,
		BillingBaseGlobal: srv.URL,
		GlobalEnabled:     true,
	}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.markGrowthUnavailable("g1")
	s.claimGrowthRewards(p.AuthByUID("u1"))

	if n := stub.redeemCalls.Load(); n != 1 {
		t.Errorf("CN 号 redeem calls=%d want 1（global 的失败记忆不得波及 CN）", n)
	}
	if s.growthUnavailableToday("u1") {
		t.Error("CN 号不该被标成链不可用")
	}
}
