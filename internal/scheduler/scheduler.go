// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子 / 成长任务补跑 七类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即七类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	// AccountDelayMinSeconds / AccountDelayMaxSeconds 定时遍历的账号间随机间隔
	// （秒）。0 区间 = 保持旧节奏；手动触发始终不受影响（见 random_schedule.go）。
	AccountDelayMinSeconds int
	AccountDelayMaxSeconds int
	// RandomDelayMinutes 每日时点在原小时内随机平移的窗口（分钟）：0 = 整点。
	// 1..60，偏移由 RandomDelaySeed + 日期 + 小时 + 任务类型哈希得出。
	RandomDelayMinutes int
	// RandomDelaySeed 部署私有的稳定种子（main 传 APIKey，空则 AuthDir）。
	// **绝不打印进日志** —— 知道 seed 就能反推出每天的实际触发时刻。
	RandomDelaySeed string
	Pool            *pool.Pool
	Upstream        *upstream.Client
	CheckinHours    []int // 默认 [9, 21]
	TravelHours     []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours   []int // 默认 [10]
	KeepaliveHours  []int // 默认 [22]
	CatHours        []int // 默认 [1]：夜猫子任务（迁移自系统 crontab）
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条同一 conversationId 内多轮上报把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int

	// ExpiringSoonWindow 快过期积分窗口：签到查余额时，把到期时间 <= now+window 的
	// 套餐余额标记为"快过期"（pool 据此优先消耗，见 entry.creditsExpiring）。
	// <=0 时禁用分桶（全部归长期，行为与引入前一致）。默认建议 7*24h。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// CatDisabled 显式关闭夜猫子任务排程（schedule.cat_enabled=false）。
	CatDisabled bool
	// GrowthHours 成长任务补跑时点：默认 [8]。task_runner.py 是幂等脚本（已
	// claimed/达标的账号自动跳过），每日扫一遍即天然覆盖新账号——存量账号的一次性
	// 成长任务（first_buddy/create_canvas/chat_5 等 16 项）此前无任何排程，只能手动
	// 跑脚本；新号入库后若没人记得跑，奖励就一直躺在"可点亮"状态。
	GrowthHours []int
	// GrowthDisabled 显式关闭成长任务补跑（schedule.growth_enabled=false）。
	GrowthDisabled bool
	// LimitedHours 限时活动任务时点：默认 [12]。这类任务带 valid_end，脚本侧
	// 过期即整体跳过，排程无需感知活动下线时刻。
	// 取代了原来的开学季排程（school_open_day_2026.py）：开学季本身也是一次性
	// 限时活动，活动结束后不该再占一个固定排程，改由本类统一承载。
	LimitedHours []int
	// LimitedDisabled 显式关闭限时活动任务排程（schedule.limited_enabled=false）。
	LimitedDisabled bool

	// BuddyBoxEnabled 能量开盲盒（/activity/growth/buddy/open）总开关。
	//
	// 与其它七类排程的开关命名相反（这里是「启用」而非「禁用」）：盲盒是**消耗性**
	// 操作，会把账号攒的能量花掉，新功能默认关、由用户显式打开更稳妥；零值 Config
	// 也因此不会意外开始烧能量。挂在活跃上报链尾（能量刚由连登奖励补进来时开）。
	BuddyBoxEnabled bool
	// BuddyBoxDailyCap 每账号每日开箱上限（个）。<=0 = 不限（仍受能量与上游额度约束）。
	// 上限的意义：能量攒着不会过期，一口气全烧掉就再也开不了；分散到每天开更稳。
	BuddyBoxDailyCap int
}

// Scheduler 调度器。
type Scheduler struct {
	// cfg 当前排程配置（atomic 快照，见 snapshot / Reload）。存 Config 值而非指针：
	// 快照一旦取出就不会被 Reload 改动，遍历账号期间读到的字段始终自洽。
	cfg atomic.Value // Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// rewardClaimed 连登奖励按自然日（CST）领取标记：uid → 当日日期。已领/已尝试的账号
	// 当日不再重打 redeem（领取类写操作按天幂等，避免对上游重复写请求）；自然日 00:00
	// CST 重置（上游增长体系按 CST 自然日刷新，见 travelDay/cstZone）。进程重启即清零
	// （服务端幂等兜底：重启后当日重复 redeem 会拿 409 正常态，无副作用）。
	rewardClaimed map[string]string

	// buddyBoxOpened 盲盒每日已开数量：uid → {日期, 已开个数}。自然日 CST 重置。
	// 进程重启即清零——上游没有「今日已开几次」的只读接口（quota 只给 affordable），
	// 所以重启后当日上限会重新计。这是刻意的：上限是省钱手段而非硬性业务规则，
	// 宁可重启后多开几个，也不要为了持久化引入一份状态文件。
	buddyBoxOpened map[string]buddyBoxCount

	// checkinMu 串行化签到：定时入口与手动触发互斥，避免同一时刻重复打上游签到接口。
	checkinMu sync.Mutex

	// reload 热重载信号：配置监听（cmd/server 的 startConfigWatcher）在排程段变化时
	// 关闭并替换通道，Run 主循环 select 到它即重算下一个唤醒时点。
	//
	// 为什么必须能打断等待：Run 原本一次 waitSlot 就睡到下一个整点（最长数小时），
	// 面板改完排程若不打断，改动要等到旧时点之后才生效——那与「热重载」的承诺不符。
	// 通道关闭语义（而非发值）：多次重载只需最后一次生效，close 天然合并重复信号。
	reloadMu sync.Mutex
	reload   chan struct{}
}

// Reload 用新配置替换排程段并立即唤醒主循环重算下一个时点（配置热重载）。
//
// Pool/Upstream 未填（零值）时沿用现值：调用方通常只关心排程段。但热重载确实要换
// 上游客户端（超时/代理/指纹变了），故非 nil 时如实替换——这里不做「只换排程」的
// 臆断，接线由装配层决定。
//
// 幂等且可并发调用：内部持锁替换，close 在持锁下完成，重复调用不会重复 close。
func (s *Scheduler) Reload(cfg Config) {
	cfg = normalizeConfig(cfg)
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	prev := s.cfg.Load().(Config)
	if cfg.Pool == nil {
		cfg.Pool = prev.Pool
	}
	if cfg.Upstream == nil {
		cfg.Upstream = prev.Upstream
	}
	s.cfg.Store(cfg)
	if s.reload != nil {
		close(s.reload)
		s.reload = make(chan struct{})
	}
}

// reloadSignal 返回当前的重载通道（调用方 select 它）。
func (s *Scheduler) reloadSignal() <-chan struct{} {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	return s.reload
}

// snapshot 返回当前排程配置的快照。
//
// 配置可被 Reload 在运行期整体替换，而任务 goroutine 会在遍历账号的过程中反复读
// 它——直接读字段就是数据竞争（-race 必报）。atomic.Value 存的是不可变 Config
// 值，每次取到的都是一份自洽快照。
func (s *Scheduler) snapshot() Config { return s.cfg.Load().(Config) }

// NextWakeForTest 返回 now 之后最近的一个唤醒时点（跨包测试观测热重载是否生效用；
// 生产代码不应依赖它——主循环自己调 nextWake）。
func (s *Scheduler) NextWakeForTest(now time.Time) time.Time {
	at, _ := s.nextWake(now)
	return at
}

// CheckinHoursForTest 返回当前生效的签到时点（跨包测试观测排程热重载用）。
func (s *Scheduler) CheckinHoursForTest() []int { return s.snapshot().CheckinHours }

// mutate 在快照上就地改一处再发布（测试用：模拟 Reload 换排程，且与生产同一路径）。
func (s *Scheduler) mutate(fn func(*Config)) {
	cfg := s.snapshot()
	fn(&cfg)
	s.cfg.Store(cfg)
}

// buddyBoxCount 盲盒当日计数（自然日 + 已开个数）。
type buddyBoxCount struct {
	day   string
	count int
}

// New 构建。
func New(cfg Config) *Scheduler {
	s := &Scheduler{
		adoptTried:     make(map[string]string),
		rewardClaimed:  make(map[string]string),
		buddyBoxOpened: make(map[string]buddyBoxCount),
		reload:         make(chan struct{}),
	}
	s.cfg.Store(normalizeConfig(cfg))
	return s
}

// normalizeConfig 补齐 Config 的零值默认（New 与 Reload 共用，避免两处漂移）。
func normalizeConfig(cfg Config) Config {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.CatHours) == 0 {
		cfg.CatHours = []int{1}
	}
	if len(cfg.GrowthHours) == 0 {
		cfg.GrowthHours = []int{8}
	}
	if len(cfg.LimitedHours) == 0 {
		cfg.LimitedHours = []int{12}
	}
	// 0/缺省 = 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if cfg.ActivityReportCount <= 0 {
		cfg.ActivityReportCount = 1
	}
	return cfg
}

// checkinRefreshSkew 签到前判定"token 是否临近过期"的时间窗口（10 分钟）。
// 长时间停机/容器长期停跑后 access token 往往已过期，不先刷新则签到必然 401 白跑。
const checkinRefreshSkew = 10 * time.Minute

// CheckinStatus 单账号签到结果状态。
type CheckinStatus string

const (
	CheckinOK      CheckinStatus = "ok"      // 签到成功
	CheckinAlready CheckinStatus = "already" // 上游判定今天已签到（幂等重复，视为正常）
	CheckinFail    CheckinStatus = "fail"    // 刷新 token / 签到 / 余额查询失败
	CheckinSkipped CheckinStatus = "skipped" // 禁用账号或无有效凭证，未参与
)

// CheckinOutcome 单账号签到结果（供手动签到回执与日志汇总）。
type CheckinOutcome struct {
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname,omitempty"`
	Status   CheckinStatus `json:"status"`
	Credits  *int64        `json:"credits,omitempty"` // 签到后余额（余额查询成功才有值）
	Detail   string        `json:"detail,omitempty"`  // 失败/跳过原因（"已签到"不填）
}

// ErrBusy 已有一次签到正在执行（手动入口与定时撞车）。
var ErrBusy = errors.New("checkin already running")

// checkinAccountDelay 相邻账号之间的最小间隔（issue #256）。
//
// 12 个账号一轮里，3 个国内版的签到请求落在 3 秒窗口内，上游直接回
// `http 500 {"code":10001,"msg":"签到失败，请稍后重试"}`；同一批账号当天稍后
// 手动单独点击全部成功，因此不是账号问题而是短时并发被限流。串行 + 固定间隔
// 把一轮 12 个号摊到 30 秒以上，远低于上游限流窗口。
// 配置了 account_delay_min/max 的定时遍历会在此基础上再叠加随机抖动（accountGap）。
// 测试可置 0。
var checkinAccountDelay = 3 * time.Second

// checkinRetryDelays 签到遇到上游瞬时拒绝时的退避序列（issue #256）。
//
// 只对 HTTP 5xx 与业务码 10001 生效；"今天已签到"（同样是 10001）在判定前已按
// 幂等成功处理，不会走到这里。重试次数 = len(checkinRetryDelays)，全部失败才
// 记 fail_checkin。测试可置空。
var checkinRetryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 60 * time.Second}

// checkinRetryable 报告签到错误是否属于上游瞬时拒绝（值得退避重试）。
// 单号手动签到都能成功而批量偶发失败，说明 10001 在这里表达的是"稍后重试"，
// 而不是账号或参数问题——这类错误直接记 fail 只能等下一轮定时，白白错过当天。
//
// 注意 upstream.DailyCheckin 内部已对 5xx 做 2s/4s 有界重试（retryBillingTransient），
// 这里补的是「外层再等一轮」：批量限流窗口往往长于 4 秒，且 10001 业务码不被
// 那层重试识别（它只认 ErrServer）。
func checkinRetryable(err error) bool {
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		return false
	}
	if ue.Status >= 500 && ue.Status <= 599 {
		return true
	}
	return upstream.CodeOf(ue.Msg) == "10001"
}

// dailyCheckinWithRetry 执行签到，遇到上游瞬时失败（HTTP 5xx / code 10001）
// 按 checkinRetryDelays 退避重试；ctx 取消立即返回最后一次错误。
func (s *Scheduler) dailyCheckinWithRetry(ctx context.Context, a *auth.Auth, label string) error {
	err := s.snapshot().Upstream.DailyCheckin(a)
	for i, delay := range checkinRetryDelays {
		if err == nil || upstream.IsAlreadyCheckin(err) || !checkinRetryable(err) {
			return err
		}
		log.Printf("checkin %s: %v — %s 后重试(%d/%d)", label, err, delay, i+1, len(checkinRetryDelays))
		if !sleepCtx(ctx, delay) {
			return err
		}
		err = s.snapshot().Upstream.DailyCheckin(a)
	}
	return err
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskCat
	taskGrowth
	taskLimited
)

// String 返回任务类型的短名，供批次日志行按名字展示（缺省 %v 打出来是数字，
// "本轮唤醒了哪些任务"读不出来）。
func (k taskKind) String() string {
	switch k {
	case taskCheckin:
		return "checkin"
	case taskTravel:
		return "travel"
	case taskActivity:
		return "activity"
	case taskKeepalive:
		return "keepalive"
	case taskCat:
		return "cat"
	case taskGrowth:
		return "growth"
	case taskLimited:
		return "limited"
	}
	return "unknown"
}

// allTaskKinds 七类任务的全集（补跑扫描与测试共用，顺序即 taskKind 声明序）。
func allTaskKinds() []taskKind {
	return []taskKind{taskCheckin, taskTravel, taskActivity, taskKeepalive,
		taskCat, taskGrowth, taskLimited}
}

// taskHours 返回该类任务的时点表与禁用标记。
//
// nextWake 与 missedSlots 共用本方法：补跑是「重算一次漏掉了什么」，两处若各维护
// 一份七类清单，新增任务类时极易只改一处，补跑就会静默漏掉那一类。
func (c Config) taskHours(k taskKind) (hours []int, disabled bool) {
	switch k {
	case taskCheckin:
		return c.CheckinHours, c.CheckinDisabled
	case taskTravel:
		return c.TravelHours, c.TravelDisabled
	case taskActivity:
		return c.ActivityHours, c.ActivityDisabled
	case taskKeepalive:
		return c.KeepaliveHours, c.KeepaliveDisabled
	case taskCat:
		return c.CatHours, c.CatDisabled
	case taskGrowth:
		return c.GrowthHours, c.GrowthDisabled
	case taskLimited:
		return c.LimitedHours, c.LimitedDisabled
	}
	return nil, true
}

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	// 取一次快照：热重载可能在计算中途替换配置，若逐个字段重新 Load，本次排程会
	// 混用新旧两份配置（比如用新小时配旧开关），算出的时点不可解释。
	cfg := s.snapshot()
	var slots []slot
	for _, k := range allTaskKinds() {
		if hours, disabled := cfg.taskHours(k); !disabled {
			slots = append(slots, slot{s.nextScheduledFire(now, hours, k), k})
		}
	}
	return earliestSlot(slots)
}

// slot 一个「时刻 + 该时刻要跑的任务类」候选。
type slot struct {
	at   time.Time
	kind taskKind
}

// earliestSlot 从候选里挑出最早时刻及与之同时刻的全部任务类。
func earliestSlot(slots []slot) (time.Time, []taskKind) {
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// missedSlot 一个「计划时刻已过、但没跑过」的槽位（补跑候选）。
type missedSlot struct {
	at   time.Time
	kind taskKind
}

// missedSlots 返回 (ranAt, now] 区间内**应当已触发却从未执行**的槽位，按时间升序。
//
// 起因：主循环是串行阻塞的——nextWake 只在批次跑完之后才重新计算，而 nextScheduledFire
// 只挑 at.After(now) 的时点。于是批次执行期间过去的槽位当天永久丢失，不会补跑。
// 配了账号间隔（220-600s）后签到批次要跑 22~60 分钟，比 random_delay_minutes 的 26
// 分钟窗口还长；签到与旅行都配在 9/21 点，签到批次把旅行槽位整个跨过去就成了常态
// （实测模拟：单个槽位旅行只有约 50% 概率能触发）。
//
// 判据用 ranAt（上次唤醒时刻）而不是「今天零点」：更早的槽位要么已执行、要么是
// 进程停机期间的（不该在启动瞬间补跑一整天的历史任务），ranAt 天然把它们挡在外面。
//
// 窗口与 nextScheduledFire 一致，只扫今天与明天：任意 hours 下，ranAt..now 之间的
// 槽位必定落在这两天内。
func (s *Scheduler) missedSlots(ranAt, now time.Time) []missedSlot {
	cfg := s.snapshot()
	var out []missedSlot
	// 从 ranAt 当天的零点起扫，保证「ranAt 当天的更早槽位」不会被漏扫；
	// 实际入选由 at.After(ranAt) 把关。
	day := time.Date(ranAt.Year(), ranAt.Month(), ranAt.Day(), 0, 0, 0, 0, ranAt.Location())
	for i := 0; i < 2; i++ {
		d := day.AddDate(0, 0, i)
		for _, k := range allTaskKinds() {
			hours, disabled := cfg.taskHours(k)
			if disabled {
				continue
			}
			for _, hour := range hours {
				at := s.scheduledSlot(d, hour, k)
				if at.After(ranAt) && !at.After(now) {
					out = append(out, missedSlot{at: at, kind: k})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// catchUpMissed 补跑 (ranAt, now] 内被批次跨过的槽位，按原定顺序执行。
//
// 同一时刻的多类任务合并成一批（与 nextWake 的「同刻多类一并执行」口径一致）；
// 不同时刻的槽位逐批串行——它们的原定间隔本就被跨过了，再并行只会让账号间隔
// 同时落在两批上，失去限速意义。
//
// 返回最后一个补跑槽位的计划时刻（零值 = 没有补跑任何东西）。调用方拿它当下一轮的
// ranAt 再查一次：补跑批次本身也可能跑得足够久、又跨过更后面的槽位（签到补跑 40
// 分钟足够跨过下一个时点），单次调用看不到这一点，必须由 Run 循环推进。
func (s *Scheduler) catchUpMissed(ctx context.Context, ranAt, now time.Time) time.Time {
	missed := s.missedSlots(ranAt, now)
	var last time.Time
	for i := 0; i < len(missed); {
		at := missed[i].at
		var kinds []taskKind
		for ; i < len(missed) && missed[i].at.Equal(at); i++ {
			kinds = append(kinds, missed[i].kind)
		}
		if ctx.Err() != nil {
			return last // 优雅停机：已派发的批次内部会自行收尾，剩余补跑放弃
		}
		log.Printf("[sched] catch-up: slot %s missed while previous batch ran, dispatch kinds=%v",
			at.Format("15:04"), kinds)
		s.runBatch(ctx, kinds)
		last = at
	}
	return last
}

// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限：Windows Modern Standby exit 后
// 网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置（分析报告裁定全套配置不成比例）。
// 测试可缩短（与 travelAccountDelay「测试可置 0」同口径）。
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGraceDelay 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限睡满，
// 本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// wallclockCheckStep 墙钟校验段长：等待槽位时单次 timer 的最大时长，每段醒来用
// 墙钟重判是否到点。值是「时点精度」与「空闲唤醒频率」的折中——60s 段内时点
// 偏差上限 60s，对签到/保活类任务足够（对齐同源 fork linguo2625469 的 97335bd）。
const wallclockCheckStep = time.Minute

// slotWake waitSlot 的结果。
type slotWake int

const (
	slotFired  slotWake = iota // 墙钟已到达计划时点：补跑本批
	slotCancel                 // ctx 取消：上层优雅退出
	slotReload                 // 排程已热重载：放弃本轮，用新排程重算时点
)

// waitSlot 分段等待到 next 的**墙钟**时刻（next 由 nextFire 用 time.Date 构造、
// 不携带单调读数，time.Until 对它是纯墙钟差）。
//
// 为什么不一把 time.NewTimer(time.Until(next)) 睡到底：timer 的等待基于单调时钟，
// macOS / Windows Modern Standby 睡眠会冻结它——睡眠时长不足整个等待周期时，fire
// 被顺延「睡眠时长」（墙钟已过点、timer 还要继续等），时点被错过且不会立即补跑；
// 睡眠时长超过整个等待周期时倒是无害的（唤醒瞬间 timer 到期，awaitWakeupGrace 补跑，
// 本地原有的 issue #152 处理覆盖的正是这一半）。
//
// 分段睡、每段醒来用墙钟重判，把冻结的影响限制在一段之内：睡眠结束后的第一段末尾
// 必然发现「墙钟已越过时点」并立即补跑，偏差上限 = step + 睡眠落段余量。
// ctx 取消在每段的 select 里随时返回，段长不影响响应性。
//
// reload 参数：排程热重载信号（见 Scheduler.Reload）。通道关闭即放弃本轮等待并返回
// slotReload——Run 据此回到顶部用新排程重算时点。没有它，面板改完排程要等旧时点
// 过去才生效（一次等待可能长达数小时），「热重载」名不副实。
func (s *Scheduler) waitSlot(ctx context.Context, next time.Time, step time.Duration, reload <-chan struct{}) slotWake {
	for {
		wallRemain := time.Until(next)
		if wallRemain <= 0 {
			return slotFired
		}
		d := wallRemain
		if d > step {
			d = step
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return slotCancel
		case <-reload:
			timer.Stop()
			return slotReload
		case <-timer.C:
			// 段末回到循环顶用墙钟重判：正常推进时若干段后到点；单调时钟被
			// 睡眠冻结时，墙钟大幅前进，至多一段之后即到点补跑。
		}
	}
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 七类任务全部禁用：不空转，只等退出信号（或配置热重载把它重新打开）。
			select {
			case <-ctx.Done():
				return
			case <-s.reloadSignal():
				continue // 排程已换：回到顶部重算时点（可能重新有任务可排）
			}
		}
		switch s.waitSlot(ctx, next, wallclockCheckStep, s.reloadSignal()) {
		case slotCancel:
			return
		case slotReload:
			// 配置已热重载：放弃本轮，回到顶部用新排程重算（旧时点作废）。
			log.Printf("[sched] 排程已热重载，重算下一个唤醒时点")
			continue
		}
		// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
		// 迟到唤醒（睡眠跨过槽位时刻）先等网络宽限：唤醒瞬间 DNS 未就绪，零宽限
		// 派发等于把唯一一次补跑机会打在注定失败的窗口里（issue #152）；准点触发
		// 零延迟不受影响。
		if !awaitWakeupGrace(ctx, next) {
			return // ctx 取消：放弃本批，优雅退出
		}
		// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
		// 54 号 × 5 条 ≈ 7-8 分钟睡眠）不再阻塞同槽其他任务族；返回前
		// 等全部任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠
		// 的风险与串行版相同——nextWake 只挑现在之后的时点）。
		// 批次级一行：各任务内部有明细，但"本轮唤醒了哪几类"没有汇总，
		// 排障时"签到到底跑没跑"要多跳几层才能判。
		log.Printf("[sched] wake up: dispatch kinds=%v", kinds)
		s.runBatch(ctx, kinds)
		// 批次跑完才重算时点，期间过去的槽位已被 nextWake 的 at.After(now) 判掉。
		// 补跑一轮，否则配了账号间隔后长批次（签到 22~60 分钟）会跨过同窗口的旅行等
		// 槽位，当天彻底丢失（见 missedSlots 注释）。
		//
		// 循环而非单次：补跑批次自己也可能跨过更后面的槽位，此时 ranAt 推进到刚补跑
		// 的槽位再查一次，直到没有新的漏跑为止。
		ranAt := next
		for ctx.Err() == nil {
			ranAt = s.catchUpMissed(ctx, ranAt, time.Now())
			if ranAt.IsZero() {
				break
			}
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// 供 Run 主循环与测试使用；ctx 取消时由各任务内部的 sleepCtx 快速收尾。
//
// 配了账号间隔时改为**串行**派发：否则同槽的两类任务各跑各的遍历，A 类正在
// 等「账号间隔」时 B 类可能同时对另一个账号发请求——间隔就形同虚设，而账号级
// 扰动正是为了不出现「几秒内多个号连续被访问」。串行时仍保持「单任务失败不
// 拖垮同批其余任务」：逐个 dispatch，panic 在单任务内兜住。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	if s.snapshot().AccountDelayMaxSeconds > 0 {
		for _, k := range kinds {
			if ctx.Err() != nil {
				return
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("WARN: [sched] task %v panicked: %v", k, r)
					}
				}()
				s.dispatch(ctx, k)
			}()
		}
		return
	}
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			s.dispatch(ctx, k)
		}(k)
	}
	wg.Wait()
}

// dispatch 按任务类型分发到对应执行函数。脚本类（cat/growth/limited）失败只记 WARN、
// 不影响其余任务继续执行（与现有各任务"单账号失败不阻断遍历"同口径）。
// ctx 传导给带账号间限速的遍历（取消时立即放弃剩余账号），纯脚本类任务不感知。
//
// ctx 在这里打上 scheduledAccountKey 标记：ac 间隔 / 脚本参数只对定时派发生效，
// 同一批执行函数被手动入口（RunXNow）调用时走 context.Background()，行为不变。
func (s *Scheduler) dispatch(ctx context.Context, k taskKind) {
	ctx = context.WithValue(ctx, scheduledAccountKey{}, true)
	switch k {
	case taskCheckin:
		s.runCheckinNow(ctx)
	case taskTravel:
		s.runTravel(ctx)
	case taskActivity:
		s.runActivity(ctx)
	case taskKeepalive:
		s.runKeepaliveNow(ctx)
	case taskCat:
		s.runCatNow(ctx)
	case taskGrowth:
		s.runGrowthTasksNow(ctx)
	case taskLimited:
		s.runLimitedTasksNow(ctx)
	}
}

// RunCheckinNow 定时触发的立即签到：逐账号结果由 CheckinAll 记日志，此处只兜住"撞车跳过"。
func (s *Scheduler) RunCheckinNow() { s.runCheckinNow(context.Background()) }

func (s *Scheduler) runCheckinNow(ctx context.Context) {
	if _, err := s.checkinAll(ctx); err != nil {
		log.Printf("scheduled checkin skipped: %v", err)
	}
}

// CheckinAll 全量签到：按需刷新 token → daily-checkin → 查余额 → 解冻冷却账号。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 同一时刻只允许一次签到在跑，重复调用返回 ErrBusy（防止手动触发与定时撞车重复打上游）。
//
// session dead 走 Pool.NoteSessionDead 的**连续计数**语义（与 keepalive 一致）：
// 一次刷新失败不再立即杀号，连续 sessionDeadThreshold 次才禁用，刷新成功清计数。
func (s *Scheduler) CheckinAll() ([]CheckinOutcome, error) {
	return s.checkinAll(context.Background())
}

func (s *Scheduler) checkinAll(ctx context.Context) ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	cfg := s.snapshot()
	statuses := cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failCheckinN, failResourceN, skipN int
	// 跳过原因分项计数：汇总行同时给出构成，避免 skipped=N 时无法判断
	// 「是禁用/无凭证」还是「global 门控」——后者是 D4 的正常语义，前者才要处理。
	var skipDisabledN, skipNoCredN, skipGlobalN int
	// pace 相邻账号之间的间隔：签到与只读余额查询都会打上游，global 账号也要限速。
	// 放在所有 skip 判定之后 —— 跳过号不产生上游请求，不该占用间隔（否则一批全禁用的
	// 号会白等 N×max 秒）。首号不睡；手动入口 accountGap 不叠加随机抖动。
	// 返回 false 表示 ctx 已取消。
	firstAccount := true
	pace := func() bool {
		if firstAccount {
			firstAccount = false
			return true
		}
		return sleepCtx(ctx, s.accountGap(ctx, checkinAccountDelay))
	}
	for _, st := range statuses {
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = CheckinSkipped, "disabled"
			skipN++
			skipDisabledN++
			log.Printf("checkin %s: skip (disabled)", logfmt.Label(st.UID, st.Nickname))
			out = append(out, oc)
			continue
		}
		a := cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			oc.Status, oc.Detail = CheckinSkipped, "no credentials"
			skipN++
			skipNoCredN++
			log.Printf("checkin %s: skip (no credentials)", logfmt.Label(st.UID, st.Nickname))
			out = append(out, oc)
			continue
		}
		// D4 门控：realm=global 账号无签到体系/任务中心，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是 D5 逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			oc.Status, oc.Detail = CheckinSkipped, "global"
			skipN++
			skipGlobalN++
			// 逐账号打点：汇总行的 skipped=N 只报数量，无法区分「禁用/无凭证/global 门控」，
			// 排查「签到是不是没跑」时只能靠账号数反推（travel.go 同口径补日志）。
			log.Printf("checkin %s: skip (global realm)", logfmt.Label(st.UID, st.Nickname))
			// 签到仍然跳过（global 没有签到体系），但**只读**余额必须刷新：
			// 否则 pool 里的 credits 恒 0，weightOf 的「余额 / 快过期」两个因子对
			// global 完全失效，选号退化成纯轮询（issue #228）。
			// UserResourceExpiry 只调 billing/meter 只读接口，不碰签到，风控语义
			// 与 D4 门控一致；顺带取逐包到期快照，让 72h 硬优先对 global 同样生效。
			if !pace() {
				return out, ctx.Err()
			}
			if remain, buckets, expiries, err := cfg.Upstream.UserResourceExpiry(a, cfg.ExpiringSoonWindow); err != nil {
				log.Printf("user-resource(global) %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			} else {
				cfg.Pool.ReenableIfCredits(st.UID, remain)
				cfg.Pool.SetCreditsExpiring(st.UID, remain, buckets.Expiring, toCreditBatches(expiries))
				oc.Credits = &remain
			}
			out = append(out, oc)
			continue
		}
		// 企业版门控：企业版无个人成长体系，daily-checkin 恒 400 code 10001
		// 「企业账号不支持该操作」（见 auth.IsEnterprise 实测注释）。与 IsGlobal 正交
		// （企业版 realm 可能 cn 也可能 global），必须单独判；不跳过则每天空发一次
		// 注定失败的调用并刷 WARN。
		if a.IsEnterprise() {
			oc.Status, oc.Detail = CheckinSkipped, "enterprise"
			skipN++
			log.Printf("checkin %s: skip (enterprise account)", logfmt.Label(st.UID, st.Nickname))
			out = append(out, oc)
			continue
		}
		// 账号间间隔由上方 pace() 承担（含 checkinAccountDelay 基数）。
		if !pace() {
			return out, ctx.Err()
		}
		// 停机跨过 token 有效期（关机过夜/容器长期停跑）时先补一次刷新，否则签到必然 401 白跑。
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("checkin %s refresh: %v", logfmt.Label(st.UID, st.Nickname), err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					if cfg.Pool.NoteSessionDead(st.UID) {
						log.Printf("WARN: checkin %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
					}
				}
				// 刷新只是"提前补票"：token 若仍有效，继续照常签到（否则刷新接口抖动
				// 会让本可成功的签到被白白跳过）；真正过期才判定失败。
				if a.NeedsRefresh(0) {
					oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
					failCheckinN++
					out = append(out, oc)
					continue
				}
			} else {
				a.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
				if err := a.SaveAtomic(); err != nil {
					// 刷新成功但落盘失败：重启会用旧 token，必须暴露。
					log.Printf("checkin %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
				}
			}
		}
		// 签到返回错误（含"今天已签到"）也继续查余额：余额恢复即可解冻账号。
		// 瞬时失败（HTTP 5xx / 10001）先在原地退避重试，避免错过当天签到。
		if err := s.dailyCheckinWithRetry(ctx, a, logfmt.Label(st.UID, st.Nickname)); err != nil {
			if upstream.IsAlreadyCheckin(err) {
				// "今天已签到"是幂等成功，不是错误：不填 detail，免得回执里
				// 出现一整段 400 报文、被误读成签到失败。
				oc.Status = CheckinAlready
				cfg.Pool.NoteCheckinDone(st.UID)
			} else {
				oc.Status = CheckinFail
				oc.Detail = err.Error()
				log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			}
		} else {
			oc.Status = CheckinOK
			// 首次签到成功此前静默——排查「签到到底跑没跑」时无迹可循（幂等行只在
			// 重复触发时出现），成功也落一行。
			cfg.Pool.NoteCheckinDone(st.UID)
			log.Printf("checkin %s: 签到成功", logfmt.Label(st.UID, st.Nickname))
		}
		// 分桶 + 逐包到期快照：快过期窗口内的积分单独标记（权重因子），逐包真实到期
		// 批次交给 pool 做 72 小时硬优先（issue:积分过期）。ExpiringSoonWindow<=0 时
		// 退化为纯总量（与引入前一致），但批次照常传递（硬优先只依赖批次，与窗口无关）。
		remain, buckets, expiries, err := cfg.Upstream.UserResourceExpiry(a, cfg.ExpiringSoonWindow)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			oc.Status = CheckinFail
			oc.Detail = joinDetail(oc.Detail, "resource: "+err.Error())
			failResourceN++
			out = append(out, oc)
			continue
		}
		cfg.Pool.ReenableIfCredits(st.UID, remain)
		cfg.Pool.SetCreditsExpiring(st.UID, remain, buckets.Expiring, toCreditBatches(expiries))
		oc.Credits = &remain
		switch oc.Status {
		case CheckinOK:
			okN++
		case CheckinAlready:
			alreadyN++
		default:
			failCheckinN++
		}
		out = append(out, oc)
	}
	// 汇总行保持原有前缀逐字不变（既有 grep / 日志检索习惯不受影响），
	// 仅在其后追加跳过原因构成：skipped=2 时能立刻看出是 global 门控还是别的。
	// fail 保留原有语义（签到 + 余额查询失败合计）供既有解析器/面板统计口径不变；
	// 末尾追加拆分字段，排障时能直接区分「上游限流」与「账号/余额异常」。
	log.Printf("checkin done: total=%d ok=%d already=%d fail=%d skipped=%d fail_checkin=%d fail_resource=%d",
		len(statuses), okN, alreadyN, failCheckinN+failResourceN, skipN, failCheckinN, failResourceN)
	if skipN > 0 {
		log.Printf("checkin skipped breakdown: disabled=%d no_credentials=%d global=%d",
			skipDisabledN, skipNoCredN, skipGlobalN)
	}
	return out, nil
}

// joinDetail 拼接多段原因，避免后一段覆盖前一段的失败信息。
func joinDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// toCreditBatches 把上游的逐包到期快照转成 pool 的批次类型（跨包类型转换：
// upstream.CreditExpiry → pool.CreditBatch，字段同名同义，仅避免 upstream 依赖 pool）。
// expiries 为空（无未到期积分包）→ nil，pool 侧清空快照、账号失去临期优先资格。
func toCreditBatches(expiries []upstream.CreditExpiry) []pool.CreditBatch {
	if len(expiries) == 0 {
		return nil
	}
	out := make([]pool.CreditBatch, 0, len(expiries))
	for _, e := range expiries {
		out = append(out, pool.CreditBatch{ExpiresAt: e.ExpiresAt, Remain: e.Remain})
	}
	return out
}

// RunActivityNow 立即对池内所有可用账号执行对话活跃上报。
// 禁用账号跳过；无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// CN 与 global 账号**都上报**（PR #45 实测国际版 /v2/report 可用）；单账号失败
// 只记 WARN 不影响遍历。
//
// 每号上报 N 条（ActivityReportCount，默认 5）：N 条共用同一 conversationId
// （wb2api-<ms>），模拟同一会话内 N 轮对话——这是领养猫（buddy/first）对话量
// 门槛的实测刷法（chat_5 前置需 5 次对话）。requestId 各条独立（同会话多轮）。
// 账号内 N 条之间间隔 activityReportGap（1.5s）避免秒发触发风控。
//
// 0/缺省 ActivityReportCount = 1 条，兼容旧行为（仅点亮连登 + 解锁 first_buddy）。
//
// 上报成功后：① streak 自检（回读连登，发现「200 但静默丢弃」）；
// ② 无猫账号立即重试领养（travelAdoptForce）——对话量刚补满的新状态，不算重试，
// 豁免 adoptTriedToday 当日防抖（旅行排程 09 点已领养过且 skip，10 点上报补满后
// 不能依赖下一轮旅行领养，就地闭环）。
// RunActivityNow 立即对池内所有可用账号执行对话活跃上报（无 ctx 的外部入口：
// cmd/activity 一次性触发、测试）。内部走 runActivity，取背景 ctx（不可取消，
// 语义与引入前 time.Sleep 版一致）。
func (s *Scheduler) RunActivityNow() {
	s.runActivity(context.Background())
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
func (s *Scheduler) runActivity(ctx context.Context) {
	cfg := s.snapshot()
	count := cfg.ActivityReportCount
	first := true
	for _, st := range cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.snapshot().Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		// global 账号同样上报（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK，
		// 点亮连登）；realmBase 路由/头由 upstream.billingJSON/BillingHeaders 按 realm 切。
		// 单账号失败只记 WARN 不影响遍历（下方 report err → break 该号 → continue 下号）。
		if !first {
			if !sleepCtx(ctx, s.accountGap(ctx, activityAccountDelay)) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		// N 条共用同一 conversationId（同会话），requestId 各自独立（每条一个）。
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		ok := 0
		for i := 1; i <= count; i++ {
			rid := fmt.Sprintf("%s-r%d", cid, i)
			if err := s.snapshot().Upstream.ReportChatActivity(a, cid, rid); err != nil {
				log.Printf("activity %s: report %d/%d: %v", logfmt.Label(a.UID, a.Nickname), i, count, err)
				break // 本号上报失败：不再续发，streak 自检无意义
			}
			log.Printf("activity %s: report %d/%d ok", logfmt.Label(a.UID, a.Nickname), i, count)
			ok++
			if i < count {
				// 账号内 5 条之间间隔，避免秒发风控；取消时立即放弃本号剩余条数。
				if !sleepCtx(ctx, activityReportGap) {
					return
				}
			}
		}
		if ok < count {
			continue // N 条未发满：streak 自检与领养均无意义，下个账号
		}
		s.checkActivityStreak(a) // N 条全发满 → 回读 streak 自检（只留结论行）
		s.travelAdoptForce(a)    // 无猫账号对话量刚补满 → 立即重试领养（豁免防抖）
		s.claimGrowthRewards(a)  // 连登奖励 + 抽奖：点亮连登后按天领取（finally 语义：失败不拖累上报）
		s.openBuddyBoxes(a)      // 能量开盲盒：放在领奖之后，吃刚补进来的能量
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.snapshot().Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("WARN: activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.Nickname), err)
		return true
	}
	if days == 0 {
		log.Printf("WARN: activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.Nickname))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.Nickname), days)
	return false
}

// claimGrowthRewards 领取连登奖励（里程碑兑换）+ 执行连登抽奖。
// 在 runActivity 上报成功 + streak 自检之后调用：连登达标（days>=某档）才能领奖，
// 领奖送的 chances 才是抽奖次数来源，故先领奖后抽奖。
//
// 全链（panel 连登管家吸收版）：礼包/补偿领取 → 读 reward-state → 补签保连登
// （补签成功重读 state 吃恢复后的天数）→ 挑档 redeem → chances → draw。
//
// 幂等/风控语义（与现有 travel/travel 同口径：单号失败只该号 WARN，不影响其他账号）：
//   - 按天幂等：每日每号最多领一轮（rewardClaimedToday 闸；自然日 CST 重置，进程重启清零——
//     重启后当日重复 redeem 由上游 409 duplicate 正常态兜底，不刷 WARN）。
//   - 服务端正常态识别为静默跳过：redeem 409 duplicate/403 天数不足；lottery 400 无次数/未开启——
//     这些不是失败，不刷 WARN（见 upstream.IsRedeemAlreadyClaimed 等）。
//   - 领奖只领「本次新达标」的档位：状态非 claimed 且 days>=档位天数。跨档连领（14d 未领而
//     days 已到 28）是官方正常态（spa 按 byTier 逐档可兑），但每日一轮限一档，避免同日多写。
//   - 礼包/补偿是「有则领」的幂等写，业务错误静默；补签只在「昨日漏签且有卡」时触发，
//     无卡/无漏签不写。
//
// 日志每号一行可 grep：`activity %s: redeem tier=%s ...` / `activity %s: lottery ...`。
func (s *Scheduler) claimGrowthRewards(a *auth.Auth) {
	if a == nil || a.AccessTokenValue() == "" {
		return
	}
	// global 门控：连登奖励/抽奖链只服务 CN。国际版 /activity/growth/* 端点虽同构存在
	// （/tmp/analysis-global-credit.md §1.1：lottery/streak/redeem 在国际版上线），但真实
	// global 新账号 GET /activity/growth/streak 返回 500（实测 sliverkiss）——链上第一步就
	// 拿不到 days，无法挑档；且 streak 500 会每趟刷 WARN 污染日志。结论：证据不足，跳过
	// global（不发起任何领取类调用）。CN 账号无此问题（CN streak 200 days=N）。
	if a.IsGlobal() {
		return
	}
	// 企业版门控：连登奖励链（redeem / lottery / claim-gift）同样是个人成长体系，
	// 企业版一律 403「growth system is only available for personal users」或
	// 400 code 10001「企业账号不支持该操作」（见 auth.IsEnterprise 实测注释）。
	// 与 IsGlobal 正交，须单独判——否则每趟都白跑一轮注定失败的领取链。
	if a.IsEnterprise() {
		return
	}
	if s.rewardClaimedToday(a.UID) {
		return // 当日已领过一轮，跳过（按天幂等）
	}
	// 0. 礼包/补偿领取（panel 连登管家口径）：每号一次 / 有则领的幂等写，
	// 业务错误静默跳过（不刷 WARN），先于 redeem——到账积分不依赖连登状态。
	s.claimGrowthBonus(a)
	state, err := s.snapshot().Upstream.GrowthRewardState(a)
	if err != nil {
		log.Printf("WARN: activity %s: reward-state: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	// 0.5 补签保连登（panel makeupYesterday 口径）：昨日漏签（heatmap score==0）且
	// 有补签卡 → 补昨日。放在 reward-state 之后：若补签把 streak 恢复到新档位，
	// 重读 state 让本日 redeem 直接吃到恢复后的天数（补签是保 7d/14d/28d 里程碑的关键）。
	if s.makeupYesterday(a) {
		if st2, err2 := s.snapshot().Upstream.GrowthRewardState(a); err2 == nil {
			state = st2 // 补签成功 → 用恢复后的天数挑档
		}
	}
	days := state.Days()
	tier := growthEligibleTier(days, &state.Redemption)
	if tier == "" {
		// 无新达标档位：不动写接口（不刷 WARN，这是正常态——很多天没到 7d）。
		return
	}
	res, err := s.snapshot().Upstream.GrowthRedeem(a, tier, "")
	switch {
	case err == nil:
		log.Printf("activity %s: redeem tier=%s ok (+%d credit, +%d energy, +%d chances)",
			logfmt.Label(a.UID, a.Nickname), tier, res.CreditGranted, res.EnergyGranted, res.ChancesGranted)
	case upstream.IsRedeemAlreadyClaimed(err) || upstream.IsRedeemNotEnoughDays(err):
		log.Printf("activity %s: redeem tier=%s skip (already claimed or days not enough)", logfmt.Label(a.UID, a.Nickname), tier)
	default:
		log.Printf("activity %s: redeem tier=%s: %v", logfmt.Label(a.UID, a.Nickname), tier, err)
	}
	// 标记当日已处理（无论 redeem 是否成功都记一次：领取类各状态当日不再重试，
	// 避免对上游重复写；成功→无需再领，失败→当日不轰炸，次日自然日重置/上游幂等兜底）。
	s.markRewardClaimed(a.UID)
	s.claimGrowthLottery(a)
}

// growthEligibleTier 按当前连登天数挑选「尚未领取且达标」的最高档位。
// 返回 "" 表示无可领档（未达标或全部已领），调用方据此跳过 redeem（正常态）。
func growthEligibleTier(days int, rs *upstream.GrowthRedemptionStatus) string {
	if rs == nil {
		return ""
	}
	// 档位按 days 升序，从高到低挑最高的已达标未领档（一次领一份，每日一轮）。
	for i := len(rs.Tiers) - 1; i >= 0; i-- {
		sp := rs.Tiers[i]
		if days >= sp.Days && !rs.Claimed(sp.Tier) {
			return sp.Tier
		}
	}
	return ""
}

// claimGrowthBonus 新手礼包 + 活动补偿领取（panel 连登管家口径）。
// 两者都是幂等写：礼包每号一次（已领业务错误静默）、补偿有则领（无则业务错误静默）。
// 只在成功到账时打日志（每号一生一次的事件，不值得每日刷行）；失败静默——
// 业务错误是常态（绝大多数号早已领过），无法与真错误可靠区分，不刷 WARN。
func (s *Scheduler) claimGrowthBonus(a *auth.Auth) {
	if credit, err := s.snapshot().Upstream.ClaimGift(a); err == nil && credit > 0 {
		log.Printf("activity %s: gift ok (+%d credit)", logfmt.Label(a.UID, a.Nickname), credit)
	}
	if credit, err := s.snapshot().Upstream.ClaimCompensation(a); err == nil && credit > 0 {
		log.Printf("activity %s: compensation ok (+%d credit)", logfmt.Label(a.UID, a.Nickname), credit)
	}
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数，panel 口径）。
// 连续天数一断就要重攒 7 天，一张卡代价远小——有漏签 + 有卡即补。
// 判据链：heatmap 昨日格 score==0（漏签）→ streak.makeup_cards.balance>0（有卡）
// → POST makeup-cards/use {"target_date":昨日}。
// 无卡 / 无漏签 / 无该日格 / 查询失败均静默返回 false（不影响主流程）；
// 补签成功打一行日志并返回 true（调用方重读 streak 天数挑档）。
func (s *Scheduler) makeupYesterday(a *auth.Auth) bool {
	cells, err := s.snapshot().Upstream.GrowthHeatmap(a)
	if err != nil {
		return false // 只读判据失败：静默（每日重试，无写风险）
	}
	yesterday := upstream.GrowthYesterdayDate(time.Now())
	score, ok := upstream.HeatmapDayScore(cells, yesterday)
	if !ok || score != 0 {
		return false // 昨日有分或无判据：无需补签
	}
	// 有漏签 → 查补签卡余额（streak 端点同一响应体）。
	st, err := s.snapshot().Upstream.GrowthStreakWithCards(a)
	if err != nil || st.MakeupCards.Balance <= 0 {
		return false // 无卡或查询失败：静默（次日再判）
	}
	if err := s.snapshot().Upstream.UseMakeupCard(a, yesterday); err != nil {
		log.Printf("activity %s: makeup %s: %v", logfmt.Label(a.UID, a.Nickname), yesterday, err)
		return false
	}
	log.Printf("activity %s: makeup ok %s (+streak kept)", logfmt.Label(a.UID, a.Nickname), yesterday)
	return true
}

// claimGrowthLottery 消耗连登奖励赠与的抽奖次数。仅抽 balance>0 的次数；无次数跳过
// （400 insufficient 正常态静默）；抽奖未开启（400 lottery disabled）静默。
// client_token 每次 draw 必须新键（security-relevant，见 upstream.GrowthLotteryDraw）。
func (s *Scheduler) claimGrowthLottery(a *auth.Auth) {
	chances, err := s.snapshot().Upstream.GrowthLotteryChances(a)
	if err != nil {
		log.Printf("WARN: activity %s: lottery-chances: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	if chances <= 0 {
		log.Printf("activity %s: lottery skip (no chances)", logfmt.Label(a.UID, a.Nickname))
		return
	}
	res, err := s.snapshot().Upstream.GrowthLotteryDraw(a, "") // 每次自动新 client_token
	switch {
	case err == nil:
		log.Printf("activity %s: lottery drawn prize=%s (%s)", logfmt.Label(a.UID, a.Nickname), res.PrizeName, res.PrizeType)
	case upstream.IsLotteryNoChance(err) || upstream.IsLotteryDisabled(err):
		log.Printf("activity %s: lottery skip (no chances or disabled)", logfmt.Label(a.UID, a.Nickname))
	default:
		log.Printf("activity %s: lottery draw: %v", logfmt.Label(a.UID, a.Nickname), err)
	}
}

// rewardClaimedToday 该账号当日是否已处理过连登奖励领取（自然日 CST）。
func (s *Scheduler) rewardClaimedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rewardClaimed[uid] == travelDay(time.Now())
}

// markRewardClaimed 记录该账号当日已处理连登奖励领取。
func (s *Scheduler) markRewardClaimed(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rewardClaimed[uid] = travelDay(time.Now())
}

// buddyBoxRemaining 该账号今日还能开几个（受每日上限约束；上限 <=0 表示不限）。
func (s *Scheduler) buddyBoxRemaining(uid string, cap int) int {
	if cap <= 0 {
		return math.MaxInt32 // 不限：只由能量与上游额度决定
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.buddyBoxOpened[uid]
	if !ok || c.day != travelDay(time.Now()) {
		return cap
	}
	if c.count >= cap {
		return 0
	}
	return cap - c.count
}

// markBuddyBoxOpened 累加该账号今日已开箱数。
func (s *Scheduler) markBuddyBoxOpened(uid string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := travelDay(time.Now())
	c := s.buddyBoxOpened[uid]
	if c.day != day {
		c = buddyBoxCount{day: day} // 跨日重置
	}
	c.count += n
	s.buddyBoxOpened[uid] = c
}

// openBuddyBoxes 用能量开盲盒（逐个开，开到能量不够或触达每日上限为止）。
//
// 逆向来源：CN 成长中心 SPA 实测（2026-10-27，/profile/growth-center 页面「开启盲盒」
// 按钮，位于「派猫猫旅行」左侧）。能量每开一次扣 cost_per_open（实测 10）。
//
// 为什么逐个开而不是一次 count=N：
//   - 中途失败（能量不足/风控）时能立刻停在已扣费的那个，不会把剩下的请求一起打出去；
//   - 日志逐条可读（开出哪只、什么稀有度），一次开 5 个只留一行汇总就没法对账。
//
// 闸口顺序：总开关 → global/企业版门控 → 每日上限 → 上游 quota.affordable。
// affordable 是权威闸（上游自己算的「还能开几个」），比本地 balance/10 可靠。
//
// 幂等/失败语义：能量已扣、盲盒已开是不可逆的，因此**任何一次开箱成功都立即计数**，
// 失败则停止本号本轮（不重试）——重试可能重复扣能量。上游若因风控拒绝，下一轮排程再说。
func (s *Scheduler) openBuddyBoxes(a *auth.Auth) {
	cfg := s.snapshot()
	if !cfg.BuddyBoxEnabled {
		return
	}
	// global 门控：国际版任务中心没有 buddy 盲盒（与 travel/claimGrowthRewards 同口径）。
	if a.IsGlobal() {
		return
	}
	// 企业版门控：成长体系端点对个人版之外一律拒绝（见 auth.IsEnterprise 实测注释）。
	if a.IsEnterprise() {
		return
	}
	label := logfmt.Label(a.UID, a.Nickname)

	quota, err := cfg.Upstream.BuddyQuotaQuery(a)
	if err != nil {
		// 只读判据失败：静默跳过本轮（每轮重试，无扣费风险）。
		log.Printf("buddy-box %s: quota: %v", label, err)
		return
	}
	if quota.Affordable <= 0 {
		return // 能量不够：正常态，不刷日志（绝大多数号每天都是这个状态）
	}

	limit := quota.Affordable
	if daily := s.buddyBoxRemaining(a.UID, cfg.BuddyBoxDailyCap); daily < limit {
		limit = daily
	}
	if limit <= 0 {
		return // 今日已达上限（正常态）
	}

	opened := 0
	for i := 0; i < limit; i++ {
		resp, err := cfg.Upstream.BuddyOpen(a, 1)
		if err != nil {
			// 已开出的那些已在上一轮循环里计过数；这里停手不重试（避免重复扣能量）。
			log.Printf("buddy-box %s: open %d/%d: %v", label, i+1, limit, err)
			break
		}
		n := len(resp.Results)
		if n == 0 {
			// 上游回 200 但没给结果：能量大概率已扣，按「开了 1 个」计数并停止，
			// 不再继续开——否则可能连环扣费却拿不到任何记录。
			log.Printf("buddy-box %s: open %d/%d: empty results (stop)", label, i+1, limit)
			s.markBuddyBoxOpened(a.UID, 1)
			opened++
			break
		}
		for _, r := range resp.Results {
			name := r.Template.Name
			if name == "" {
				name = r.Instance.SoulDesc
			}
			log.Printf("buddy-box %s: open ok %d/%d name=%q template=%d shiny=%d",
				label, i+1, limit, name, r.Template.ID, r.Instance.IsShiny)
		}
		s.markBuddyBoxOpened(a.UID, n)
		opened += n
	}

	if opened > 0 {
		// 收尾一行：本轮开了几个 + 剩余能量，方便对账（逐条日志在循环里）。
		left := quota.Balance - opened*quota.CostPerOpen
		if left < 0 {
			left = 0
		}
		log.Printf("buddy-box %s: done opened=%d energy_left≈%d", label, opened, left)
	}
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() { s.runKeepaliveNow(context.Background()) }

func (s *Scheduler) runKeepaliveNow(ctx context.Context) {
	cfg := s.snapshot()
	firstAccount := true
	for _, st := range cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.snapshot().Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// 账号间随机间隔（仅定时派发）：放在 skip 判定之后，跳过号不占间隔。
		if !firstAccount {
			if !sleepCtx(ctx, s.accountGap(ctx, 0)) {
				return
			}
		}
		firstAccount = false
		if err := s.snapshot().Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.snapshot().Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.snapshot().Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		a.BackfillRealm()                          // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	}
}

// creditsRefreshDelay 启动余额刷新的账号间限速：避免对上游造成瞬时压力
// （与 checkinAccountDelay 同口径；54 个账号约 11 秒）。测试可置 0。
var creditsRefreshDelay = 200 * time.Millisecond

// RefreshCreditsOnce 启动时刷新一遍全量账号余额（**只查余额，不签到**）。
//
// 为什么需要：池的 credits 只在签到时刷新（默认 9/21 点），网关启动时不查余额，
// 于是重启后面板回落到 /status 的 credits 时，看到的是「上次签到值 − 之后的消耗
// 估算」——一个不知道多久以前的数字（credits 字段没有时间戳）。本方法在启动时补
// 一次权威查询，配合调用方的显式落盘，让 state.json 里的 credits 从进程第一秒
// 就是新鲜的。
//
// 与 CheckinAll 的差异：
//   - **不调 DailyCheckin**：那是签到动作，有上游副作用，重启不该触发；本方法只做
//     「按需刷新 token → 查余额」这一读路径。这是本方法不能复用 CheckinAll 的原因。
//   - **不跳过 global 账号**：global 域不参与签到体系（CheckinAll 的 D4 门控），
//     其 credits 否则终身冻结在 state.json 里。余额查询是只读 billing 调用，
//     面板手动查积分时已在对 global 做同样的事；仓库内也有同样的取舍先例——
//     runActivity 有意包含 global 账号（其注释记录了 PR #45 的实测依据）。
//     realm 路由由 upstream 的 billingMeterPaths 按账号自动切分，此处无需分支。
//
// 门控（照 CheckinAll）：禁用账号跳过、无凭证跳过、token 真过期（刷新失败且
// NeedsRefresh(0)）跳过。单账号失败只记日志，不中断遍历；ctx 取消立即放弃剩余账号。
//
// 与签到互斥（checkinMu）：若进程恰在整点签到时刻前启动，两轮会同时对同一批账号
// 打上游（双倍调用）。这里用 **Lock 而非 TryLock**——本方法跑在启动期的后台
// goroutine 里，等待没有代价（不阻塞网关对外服务），而 TryLock 撞车时会整轮跳过，
// 让**只有本方法覆盖的 global 账号**错过刷新（CheckinAll 跳过 global），
// 恰好退回本方法要修的那个问题。等待期间 ctx 取消由 Lock 后的 sleepCtx 承接。
func (s *Scheduler) RefreshCreditsOnce(ctx context.Context) {
	s.checkinMu.Lock()
	defer s.checkinMu.Unlock()
	cfg := s.snapshot()
	statuses := cfg.Pool.List()
	var okN, failN, skipN int
	for i, st := range statuses {
		if i > 0 && !sleepCtx(ctx, creditsRefreshDelay) {
			log.Printf("启动余额刷新被取消（已完成 %d/%d）", i, len(statuses))
			return
		}
		if st.Disabled {
			skipN++
			continue
		}
		a := s.snapshot().Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			skipN++
			continue
		}
		// 停机跨过 token 有效期（关机过夜/容器长期停跑）时先补一次刷新，否则查余额
		// 必然 401 白跑。刷新失败不致命：token 若仍有效，继续照常查询。
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := s.snapshot().Upstream.RefreshToken(a); err != nil {
				log.Printf("startup credits %s refresh: %v", logfmt.Label(st.UID, st.Nickname), err)
				if a.NeedsRefresh(0) {
					failN++
					continue
				}
			} else {
				a.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
				if err := a.SaveAtomic(); err != nil {
					// 刷新成功但落盘失败：重启会用旧 token，必须暴露。
					log.Printf("startup credits %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
				}
			}
		}
		remain, buckets, expiries, err := s.snapshot().Upstream.UserResourceExpiry(a, s.snapshot().ExpiringSoonWindow)
		if err != nil {
			log.Printf("startup credits %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			failN++
			continue
		}
		// 与签到同口径：SetCreditsExpiring 一并更新快过架子集（权重因子）与逐包到期
		// 快照（72 小时硬优先）。creditsExpiring 会被钳到 [0, credits]，批次经
		// normalizeBatches 过滤（过期/零值剔除、升序、累计钳制）。
		s.snapshot().Pool.SetCreditsExpiring(st.UID, remain, buckets.Expiring, toCreditBatches(expiries))
		okN++
	}
	log.Printf("启动余额刷新完成：成功 %d，失败 %d，跳过 %d", okN, failN, skipN)
}
