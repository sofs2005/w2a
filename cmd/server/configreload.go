// configreload.go config.json 热重载：面板保存后立即生效，不必重启容器。
//
// 为什么需要：网关此前只在启动时读一次 config.json（main.go 的 Load），面板「网关
// 配置」页保存后必须 `docker compose restart` 才生效。而合并成单进程后面板与网关
// 同进程，重启等于把自己掐断（见 panel/HOST-PATCHES.md 与 host.go 的说明），于是「改个
// 超时也要停一次服务」成了最扎手的运维摩擦。
//
// **按需触发，不做轮询**：面板与网关同进程，保存接口写完文件就直接调这里（见
// panel/internal/ops/config_reload.go 与 cmd/server/panel.go 的接线）。既然保存方
// 知道自己刚写过文件，再用文件指纹去「发现」改动就是多余的动作——代价是一整套
// 轮询周期、指纹比对、告警节流与和保存路径的基线同步。
//
// 代价与取舍：手工改 config.json（不经面板）不会自动生效，需重启进程。这是**刻意**
// 的：配置的写入方是面板，网关不该为一条旁路维护常驻监听。
//
// 生效范围：
//   - 可热生效（多数）：排程、池参数与冷却、上游超时/指纹/代理、提示词、api_key、
//     会话粘性 TTL、全局开关。做法是**重建一个 upstream.Client 再整体替换快照**
//     （upstream.Client 的参数散落在 Transport 与十几个字段上，逐字段热改既漏得掉
//     又是并发雷区），池与调度器则走各自的 Set*/Reload。
//   - 必须重启（少数）：listen、auth_dir、state_file、server、upstash —— 它们绑定的是
//     监听 socket、文件路径、已构造的 http.Server 与 Redis 连接，运行期换掉等于重启
//     进程本身。
//
// 失败语义：解析失败、校验不过、或只改动了「必须重启」的字段时**保持现状**并如实
// 返回/打日志，绝不把网关切到半截配置上。
package main

import (
	"fmt"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// restartOnlyFields 只能靠重启生效的字段（改动它们时热重载只打提示，不切换配置）。
//
// 判据：绑定进程级资源（监听 socket / 文件路径 / Redis 连接），运行期替换无法安全
// 完成。其余字段一律热生效——漏掉的字段会退化成「改了没反应」，故这里的清单要短，
// 新增配置项默认应当是可热的。
var restartOnlyFields = []string{
	"listen",     // 监听地址：socket 已绑定
	"auth_dir",   // 凭证目录：池与监听器都已按旧路径接线
	"state_file", // 池状态文件：池已按旧路径加载/落盘
	// server 段目前只有 read_timeout，它只赋给启动时创建的 http.Server.ReadTimeout
	// （main.go 的 srv 构造），applyConfig 没有、也不该有更新它的手段 —— 改超时值
	// 必须重建 Server 才能生效。漏标它的后果很具体：面板会提示「已生效」，
	// 而实际请求仍在用旧的读超时。
	"server",
	"upstash", // Redis 连接：store 已建立，换连接等于重连
}

// configDiff 描述两次配置之间「有变化」的顶层字段。
//
// 用反射比较顶层字段而不是手写逐字段比对：新增配置项时不必记得来这里补一行
// （漏补的后果正是「改了不生效却没人知道」）。只比顶层键，报错信息足够定位。
//
// 跳过两类字段：
//   - 非导出字段（f.PkgPath != ""）；
//   - `json:"-"` 的派生字段（SoftRateDur / ServerReadTimeoutDur 这类解析后的
//     Duration）。它们由 JSON 键派生而来，源字段变了它们必然跟着变，重复计入
//     只会让清单出现两遍；更糟的是这些名字会原样进入面板的「需重启」提示。
func configDiff(old, new *Config) []string {
	if old == nil || new == nil {
		return nil
	}
	ov, nv := reflect.ValueOf(*old), reflect.ValueOf(*new)
	t := ov.Type()
	var changed []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // 非导出字段
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue // 派生字段：不对外暴露，也不该出现在变更清单里
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = f.Name
		}
		if !reflect.DeepEqual(ov.Field(i).Interface(), nv.Field(i).Interface()) {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	return changed
}

// splitHotAndCold 把变化字段拆成「可热生效」与「必须重启」两组。
func splitHotAndCold(changed []string) (hot, cold []string) {
	isCold := make(map[string]bool, len(restartOnlyFields))
	for _, f := range restartOnlyFields {
		isCold[f] = true
	}
	for _, f := range changed {
		if isCold[f] {
			cold = append(cold, f)
		} else {
			hot = append(hot, f)
		}
	}
	return hot, cold
}

// upstreamHolder 原子持有当前生效的出站客户端。
//
// 为什么不是普通指针：热重载要在运行期整体替换客户端，而请求 goroutine 正在并发
// 使用它——原地改写结构体字段是数据竞争（-race 必报）。atomic.Pointer 让「换」这
// 一动作原子可见，在途请求要么用旧客户端跑完、要么看到新的完整实例。
type upstreamHolder struct {
	p atomic.Pointer[upstream.Client]
}

func newUpstreamHolder(c *upstream.Client) *upstreamHolder {
	h := &upstreamHolder{}
	h.p.Store(c)
	return h
}

func (h *upstreamHolder) Load() *upstream.Client { return h.p.Load() }

func (h *upstreamHolder) Store(c *upstream.Client) { h.p.Store(c) }

// configTargets 热重载需要触碰的运行期对象（全部由 main 装配后注入）。
//
// 用结构体而不是一长串参数：这些对象的接线在 main 里一目了然，且新增目标时
// 只改这里与 main 的装配，不会把函数签名撑成一排省略号。
type configTargets struct {
	pool    *pool.Pool
	up      *upstreamHolder
	handler *server.Handler
	sched   *scheduler.Scheduler
	sess    *session.Router
	// stickyEnabled 会话粘性开关的原子位（handler 每次请求据此决定是否走粘性）。
	// router 本身常驻（见 main 的 newSessRouter），开关只切「用不用」，故能热切换。
	stickyEnabled *atomic.Bool
	// loaded 最近一次生效的配置（diff 基准）。由装配层传入同一份指针，
	// 每次成功重载后就地更新，下一轮据此算出「哪些字段变了」。
	loaded *Config
}

// configReloaderAdapter 把装配好的热重载能力暴露给面板（进程内直调）。
//
// 实现 panel/internal/ops.ConfigReloader：面板保存 config.json 后立刻调它，把
// 「保存」与「生效」压成同一步。
//
// mu 串行化并发保存：面板连点两次保存、或保存与重置备份的操作重叠时，重载会并发
// 进入——重载要重建上游客户端，并发跑两次既浪费又会在 loaded 基准上互相踩。
type configReloaderAdapter struct {
	mu     sync.Mutex
	path   string
	target configTargets
}

func newConfigReloaderAdapter(path string, t configTargets) *configReloaderAdapter {
	return &configReloaderAdapter{path: path, target: t}
}

// ReloadConfig 按磁盘上的 config.json 立即重载一次，并如实回报本次生效范围。
//
// 返回 error 仅当配置**读不出来或解析不过**——那是真的没生效，值得告知用户；
// 「只改了需重启字段」与「内容没有变化」都不算失败，但它们的 hot / cold 不同：
// 前者 hot 空、cold 非空，后者两者皆空。面板靠这两个清单决定提示语。
func (a *configReloaderAdapter) ReloadConfig() (hot, cold []string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	cfg, err := Load(a.path)
	if err != nil {
		// 保持现状：面板刚写的文件可能不合法（表单校验漏网、手改残留），
		// 此时绝不能让网关切到半截配置上。
		return nil, nil, fmt.Errorf("重新解析 %s 失败: %w", a.path, err)
	}

	changed := configDiff(a.target.loaded, cfg)
	if len(changed) == 0 {
		return nil, nil, nil // 解析结果一致（仅格式/空白变化）：无事可做
	}
	hot, cold = splitHotAndCold(changed)

	if len(hot) == 0 {
		// 只改了结构性字段：不切换（切换一半等于让网关处于两种配置之间的怪状态），
		// 但要明确告诉运维「已读到、只是需要重启」，否则会以为热重载坏了。
		log.Printf("[config] 检测到配置改动 %v —— 这些字段只能靠重启生效（docker compose restart），本次未应用", cold)
		return hot, cold, nil
	}

	applyConfig(cfg, a.target)
	a.target.setCurrent(cfg)
	if len(cold) > 0 {
		log.Printf("[config] 配置已热重载：%v 立即生效；%v 仍需重启才生效", hot, cold)
	} else {
		log.Printf("[config] 配置已热重载：%v 立即生效（无需重启）", hot)
	}
	return hot, cold, nil
}

// setCurrent 记录本次生效的配置（下一次 diff 的基准）。
func (t configTargets) setCurrent(cfg *Config) {
	if t.loaded != nil {
		*t.loaded = *cfg
	}
}

// applyConfig 把新配置应用到全部运行期对象（热生效部分）。
//
// 顺序有讲究：先建新上游客户端（它要接管旧客户端的探测缓存），再替换 handler 快照
// （此后新请求看到新客户端），最后才动池与调度器——池参数是「下一次选号/冷却」才
// 用到的，晚一步没有副作用，而提前换掉上游会让在途请求读到半新半旧的状态。
func applyConfig(cfg *Config, t configTargets) {
	// 1) 全局开关（进程级 atomic，读路径零锁）。
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	server.SetUnifiedRouting(cfg.Pool.UnifiedRouting)
	upstream.ConfigureReasoningFallback(cfg.ReasoningAsContent == nil || *cfg.ReasoningAsContent)
	upstream.ConfigureProxies(cfg.Proxies)
	upstream.SetModelCatalogPath(modelJSONPath(cfg.StateFile))

	// 2) 出站客户端整体重建：超时/代理/指纹/版本段/base 全在它身上。
	oldUp := t.up.Load()
	newUp := buildUpstream(cfg)
	newUp.AdoptCachesFrom(oldUp)
	t.up.Store(newUp)

	// 3) handler 快照：api_key / 冷却基数 / 提示词 / 上游客户端。
	hcfg := t.handler.Config()
	hcfg.Upstream = newUp
	hcfg.APIKey = cfg.APIKey
	hcfg.SoftCooldown = cfg.SoftRateDur
	hcfg.PromptMode = cfg.Prompt.Mode
	hcfg.PromptText = cfg.PromptText
	hcfg.GlobalEnabled = cfg.Global.Enabled
	t.handler.ApplyConfig(hcfg)

	// 4) 池参数与冷却（Set* 全部持锁、幂等）。
	p := t.pool
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)
	p.SetCreditExhausted(cfg.CreditExhaustedDur)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur)
	p.SetCreditFloor(cfg.Pool.CreditFloorCN, cfg.Pool.CreditFloorGlobal)
	p.SetFlushInterval(cfg.StateFlushDur)

	// 5) 排程段（含「立即重算下一个唤醒时点」，见 scheduler.Reload）。
	// Upstream 必须显式传新客户端：Reload 对 nil 的语义是「沿用旧值」，而这里恰恰
	// 要换掉它（超时/代理/指纹都变了）。
	if t.sched != nil {
		sc := schedulerConfig(cfg)
		sc.Upstream = newUp
		t.sched.Reload(sc)
	}

	// 6) 会话粘性：开关切原子位（handler 每请求读），TTL / GC 周期交给 router。
	// 关闭时停 GC、开启时重新拉起——router 本身常驻，绑定表保留（重开即恢复）。
	if t.stickyEnabled != nil {
		t.stickyEnabled.Store(cfg.SessionSticky.Enabled)
	}
	if t.sess != nil {
		t.sess.Reload(cfg.SessionTTL, cfg.SessionGCInterval)
		if cfg.SessionSticky.Enabled {
			t.sess.StartGC()
		} else {
			t.sess.StopGC()
		}
	}
}

// buildUpstream 按配置构造出站客户端（启动装配与热重载共用同一份接线）。
//
// 抽出来是为了让两条路径**逐字一致**：曾经只在 main 里手写一长串赋值，热重载再抄
// 一遍必然漂移（漏一个字段就是「改了没生效」）。新增出站参数时只改这里。
func buildUpstream(cfg *Config) *upstream.Client {
	up := upstream.New()

	// 出站代理必须在任何业务请求之前接上（替换 Transport 会让已建连接失效）。
	up.SetProxies(cfg.Proxy.CN, cfg.Proxy.Global)

	// 短 RPC 总时长上限；global 侧同款（两域超时口径一致）。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	up.HTTPGlobal.Timeout = up.HTTP.Timeout

	// 聊天 SSE 首字节前上限：CN / global 两侧 Transport 同步（两域参数逐字一致）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	if tr, ok := up.ChatHTTPGlobal.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 账号级出口线路的 Transport 是惰性构造的，构造时读取该值，故先设超时再注线路表。
	upstream.SetRouteHeaderTimeout(up.HeaderTimeout)
	upstream.ConfigureProxies(cfg.Proxies)

	// 聊天 SSE 流中空闲上限。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second

	// 出站指纹段：脱敏开关、UA、版本段、Stainless、设备风控、用量归属、IP 透传。
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.StainlessRuntimeVersion = cfg.Upstream.StainlessRuntimeVersion
	up.StainlessTimeoutSeconds = cfg.Upstream.StainlessTimeoutSeconds
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.ClientName = cfg.Upstream.ClientName
	up.PassthroughIP = cfg.Upstream.PassthroughIP

	// global realm 路由（base 空回落内置默认；开关与 auth 包双保险）。
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	up.GlobalEnabled = cfg.Global.Enabled
	return up
}

// schedulerConfig 由 Config 组装调度器配置（启动装配与热重载共用）。
//
// 排程种子沿用 APIKey（空则 AuthDir）：它与「哪个部署」绑定，热重载换 api_key 时
// 随机偏移会跟着变一天——可接受（api_key 是运维显式动作，且偏移本就在同一小时内）。
func schedulerConfig(cfg *Config) scheduler.Config {
	seed := cfg.APIKey
	if seed == "" {
		seed = cfg.AuthDir
	}
	return scheduler.Config{
		AccountDelayMinSeconds: cfg.Schedule.AccountDelayMinSeconds,
		AccountDelayMaxSeconds: cfg.Schedule.AccountDelayMaxSeconds,
		RandomDelayMinutes:     cfg.Schedule.RandomDelayMinutes,
		RandomDelaySeed:        seed,
		CheckinHours:           cfg.Schedule.CheckinHours,
		TravelHours:            cfg.Schedule.TravelHours,
		ActivityHours:          cfg.Schedule.ActivityHours,
		KeepaliveHours:         cfg.Schedule.KeepaliveHours,
		CatHours:               cfg.Schedule.CatHours,
		GrowthHours:            cfg.Schedule.GrowthHours,
		LimitedHours:           cfg.Schedule.LimitedHours,
		ActivityReportCount:    cfg.Schedule.ActivityReportCount,
		ExpiringSoonWindow:     cfg.ExpiringSoonDur,
		CheckinDisabled:        !cfg.Schedule.CheckinEnabled,
		TravelDisabled:         !cfg.Schedule.TravelEnabled,
		ActivityDisabled:       !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:      !cfg.Schedule.KeepaliveEnabled,
		CatDisabled:            !cfg.Schedule.CatEnabled,
		GrowthDisabled:         !cfg.Schedule.GrowthEnabled,
		LimitedDisabled:        !cfg.Schedule.LimitedEnabled,
		BuddyBoxEnabled:        cfg.Schedule.BuddyBoxEnabled,
		BuddyBoxDailyCap:       cfg.Schedule.BuddyBoxDailyCap,
	}
}
