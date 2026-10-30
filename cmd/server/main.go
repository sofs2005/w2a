// main.go W2A 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"workbuddy2api/internal/aliases"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// descOrDirect 代理描述的空值兜底（日志措辞用）：空串 = 未显式配置该域代理。
func descOrDirect(desc string) string {
	if desc == "" {
		return "(未配置，走环境变量)"
	}
	return desc
}

// modelJSONPath 由 state.json 路径推导 model.json 路径（同目录同名换缀）：
// 两者同为数据目录持久化物（Docker ./data volume），配套而非各自配置。
// state 路径为空（纯内存测试形态）→ 空 = 禁用 model.json 落盘（内存 + 种子仍可用）。
func modelJSONPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "model.json")
}

// aliasFilePath 由 state.json 路径推导别名文件路径（同目录固定名 model_aliases.json）。
//
// 为什么放数据目录而不是 config.json：网关只在启动时读 config.json，别名是高频调整的
// 运维参数，必须热生效（面板改完数秒内生效，见 reload.go 的 startAliasWatcher）。
// 为什么与 state.json 同目录：同属"运行期数据"（Docker ./data volume 持久化），
// 面板写它不需要额外配置项，部署形态与 state.json 完全一致。
// state 路径为空（纯内存测试形态）→ 空 = 别名功能关闭（无别名，一切透传）。
func aliasFilePath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "model_aliases.json")
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时：先自动落一份推荐配置（含随机 api_key），再加载。
		// 用 errors.Is 而非 os.IsNotExist——后者看不穿 Load 里 fmt.Errorf("%w") 的包装，
		// 会让这个分支永不命中、直接 log.Fatalf 退出（旧实现的实际行为）。
		if errors.Is(err, fs.ErrNotExist) {
			// 但「配置不存在」不等于「首次运行」：老部署的配置路径漂移时（实案：
			// ENTRYPOINT 从 /app/config.json 改到 /app/config/config.json，而宿主机
			// 单文件挂载仍钉旧路径），真配置在别处、只是读不到。此时自动生成会**静默
			// 换掉 api_key**（下游全 401），且旧配置里的调优从头到尾没生效过。
			// 用运行痕迹（state.json / 面板 data 侧产物）判别，拒绝这种「假装首启」；
			// 判据的取舍见 priorRunEvidence 的注释。
			//
			// 判据取自纯默认 + env：配置文件本就不存在，StateFile 只能来自默认值或
			// WB2A_* 覆盖——这正是「网关认为该去哪儿找」的权威口径。
			base, berr := Load("")
			if berr == nil {
				if ev := priorRunEvidence(base.StateFile); ev != "" {
					log.Fatalf("config %s 不存在，但检测到本部署的运行痕迹（%s）：\n"+
						"  这几乎总是**配置路径对不上**，而不是首次运行——真配置在另一个路径上，网关读不到。\n"+
						"  拒绝自动生成：那会换掉 api_key（下游客户端全部 401），并让既有调优静默失效。\n"+
						"  请核对容器的配置文件挂载路径与 -config 参数是否一致：\n"+
						"    docker inspect <容器名> --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{\"\\n\"}}{{end}}'\n"+
						"  注意 05b18ce 起 ENTRYPOINT 读的是 /app/config/config.json（挂**目录** ./config:/app/config），\n"+
						"  旧部署的单文件挂载 config.json:/app/config.json 已对不上。\n"+
						"  确属首次运行请清空上述痕迹，或显式用 -config 指向真实配置。",
						*cfgPath, ev)
				}
			}
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", *cfgPath, key)
				cfg, err = Load(*cfgPath)
			}
			if err != nil {
				// 生成失败（目录只读/单文件挂载不可建等）：退回纯默认 + env，不阻塞启动。
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", *cfgPath, err)
				cfg, err = Load("")
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	// 弱 api_key 启动告警（空 key / 示例占位符 / 过短）：只告警、不退出——空 key 是
	// w2a 有意支持的部署形态（纯内网 / 前置反代已鉴权），改成 fail-fast 会挡掉既有部署。
	// 判定与文案见 config.go 的 weakAPIKeyNotice。
	warnWeakAPIKey(cfg)

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// global realm 路由开关（config global.enabled，缺省 true）：注入 auth 包全局闸。
	// Realm()/IsGlobal() 先过此闸——显式 false 时恒 cn（逃生门：纯 CN 锁定的第一道闸）。
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	// 统一调度开关（config pool.unified_routing，缺省 true）：注入 server 包全局闸。
	// 裸模型名（无 cn:/global: 前缀）是否进全池调度（CN+global 共用粘性/加权/分层）。
	// 显式 false 退回旧语义（裸名只打 CN），是路由层逃生门；钉域前缀不受影响。
	// 纯 CN 部署（global.enabled=false）时本开关无实际作用：账号已被第一道闸锁成 cn。
	server.SetUnifiedRouting(cfg.Pool.UnifiedRouting)

	// model.json 本地缓存接线（context_length 四级查找链第 3 级）：数据目录与
	// state.json 同风格（Docker volume 持久化路径 ./data）。首次缺失/损坏自动回落
	// 仓库种子 embed；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(modelJSONPath(cfg.StateFile))

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Close() // 进程退出前停后台落盘 goroutine + 最后补一次落盘（FIX-4:goroutine 泄漏）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// auths 目录热加载：新增凭证文件自动进池，免去「加完账号手动重启网关」。
	// 启动时的 SyncToDir 已建立基线，监听只在后续目录内容变化时触发（见 pool/watch.go）。
	stopWatch := p.StartAuthDirWatch(cfg.AuthDir)
	defer stopWatch()

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	// 连败降权（issue #114）：ErrClient/传输层连败 N 次临时出池。
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	// 上游超时（慢号）出池：连续 N 次超时临时出池 + 解绑粘性（复用 degradeUntil 通道）。
	p.SetTimeout(cfg.Pool.TimeoutStreak, cfg.TimeoutCooldownDur)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域在途分档（WAF 403 修复 P1-1，默认 2）
	p.SetSoftRateMax(cfg.SoftRateMaxDur)               // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetCreditExhausted(cfg.CreditExhaustedDur)       // 积分耗尽后的冷却时长（credit_exhausted，0 = 旧行为：硬冷却到次日 04:00）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur)                 // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetCreditFloor(cfg.Pool.CreditFloorCN, cfg.Pool.CreditFloorGlobal) // 积分保底按域分档（默认 100/30；0 = 该域关闭）
	p.SetFlushInterval(cfg.StateFlushDur)                                // 池状态落盘周期（pool.state_flush，默认 30m；0 = 关闭后台落盘）

	// 模型别名映射表：统一对外名 → 各域真实上游名（见 internal/aliases）。
	// 启动即加载一次（缺失/空文件 = 空表，一切透传），之后由 startAliasWatcher 热加载。
	// 解析失败只打 WARN 并退回空表：别名是可选增强，绝不能因为它起不来。
	aliasPath := aliasFilePath(cfg.StateFile)
	aliasStore := aliases.NewStore()
	if tbl, err := aliases.Load(aliasPath); err != nil {
		log.Printf("WARN: [aliases] 别名文件加载失败（按无别名运行）: %v", err)
	} else {
		aliasStore.Set(tbl)
		if tbl.Len() > 0 {
			log.Printf("已加载 %d 条模型别名映射（%s）", tbl.Len(), aliasPath)
		}
	}

	// 会话粘性路由（可配关闭）。
	//
	// 总是构建一个 router：session_sticky.enabled 能热重载（面板勾/取消都很常见），
	// 而「启用/禁用」只是 handler 是否使用它——用 nil 表示关闭会导致开关一改就再也
	// 装不回去（main 只在启动时跑一次）。禁用时同样不启动后台 GC，与旧行为一致。
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	newSessRouter := func() *session.Router {
		r := session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被 6004 限额时重分配，
			// 而不是被钉在这个号上反复失败。
			// realm/别名感知闭包：经 ResolveRoute 得出对外名 + 候选域集合，按域过滤
			// 可用账号（跨 realm 不泄漏、单域别名不误选另一域，见 wiring.go）。
			AvailableForModel: realmAwareAvailableForModel(p, aliasStore),
			// 紧急到期优先候选集：仅新建/失效重绑时生效（见 session.Config.UrgentForModel）。
			UrgentForModel: realmAwareUrgentForModel(p, aliasStore),
			// 首绑按三因子权重分配（issue #265）：粘性命中即定，若首绑仍按哈希取模，
			// 「快过期积分优先消耗」这条池权重在默认配置下就永远轮不到——带会话标识的
			// 请求首轮绑定后滚动续期，压根走不到 Pool.pick。
			PickForModel: realmAwareWeightedPick(p, aliasStore),
		})
		r.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		r.StartGC()
		return r
	}
	sessRouter := newSessRouter()
	defer sessRouter.StopGC()
	// stickyEnabled 粘性开关的原子位：handler 每次请求据此判定，配置热重载负责翻转
	// 并同时启停后台 GC（router 常驻，绑定表在关闭期间保留，重开即恢复）。
	stickyEnabled := &atomic.Bool{}
	stickyEnabled.Store(cfg.SessionSticky.Enabled)
	if !stickyEnabled.Load() {
		sessRouter.StopGC() // 禁用时不跑后台 GC（启用后由热重载重新拉起）
	}
	sessCount := func() int { return sessRouter.Count() }

	// 出站客户端：按 config 装配（超时/代理/指纹/base 全在里面，与热重载共用同一份
	// 接线，见 configreload.go 的 buildUpstream）。
	//
	// 经 upHolder 间接持有：配置热重载要**整体替换**这个客户端（参数散落在 Transport
	// 与十几个字段上，逐字段热改既漏得掉又是并发雷区）。凡是需要跟随热重载的读取方
	// 都必须经 holder 取值（原地改写结构体字段是数据竞争）；持有一份固定 *Client 的
	// 地方由热重载层各自替换快照（handler → ApplyConfig，scheduler → Reload）。
	upHolder := newUpstreamHolder(buildUpstream(cfg))
	up := upHolder.Load()

	if cfg.Proxy.CN != "" || cfg.Proxy.Global != "" {
		log.Printf("INFO: [upstream] 出站代理已启用 cn=%q global=%q（改配置后数秒内热生效）",
			descOrDirect(up.ProxyCN), descOrDirect(up.ProxyGlobal))
	}

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。本地实测台账无观测
	// 时用它判收费——否则「没学过」恒等于「放行」，高价新模型会把触底号一笔打穿
	// （实案：全池无观测 → 保底全放行 → 两笔打穿并硬冷却到次日 04:00）。
	// 闭包每次调用经 holder 取实时客户端，热重载换掉客户端后自动跟随。
	p.SetModelRateOf(func(realm, model string) string { return upHolder.Load().ModelRate(realm, model) })

	// 思考通道兜底（issue #135）：默认开启；显式关掉时打一行日志，免得"为什么
	// 只看 content 的客户端又空了"变成悬案。
	if !upstream.ReasoningFallbackEnabled() {
		log.Printf("思考通道兜底已关闭：只含 reasoning_content 的回复不会再补成 content")
	}
	if names := upstream.RouteNames(); len(names) > 0 {
		log.Printf("INFO: [upstream] 出口线路已加载 %d 条（%s）—— 账号绑定的线路名按它解析",
			len(names), strings.Join(names, ", "))
	}

	// 调度器：排程段由 schedulerConfig 统一组装（启动与热重载共用，避免两处漂移）。
	schCfg := schedulerConfig(cfg)
	schCfg.Pool = p
	schCfg.Upstream = upHolder.Load()
	sch := scheduler.New(schCfg)
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	if !cfg.Schedule.CatEnabled {
		log.Printf("夜猫子任务已禁用（schedule.cat_enabled=false）")
	} else {
		log.Printf("夜猫子任务已启用：%v 点（task_runner.py ALL --yes --only black_cat）", cfg.Schedule.CatHours)
	}
	if !cfg.Schedule.GrowthEnabled {
		log.Printf("成长任务补跑已禁用（schedule.growth_enabled=false）")
	} else {
		log.Printf("成长任务补跑已启用：%v 点（task_runner.py ALL --yes，幂等：已领/已达标逐项跳过）", cfg.Schedule.GrowthHours)
	}
	if !cfg.Schedule.LimitedEnabled {
		log.Printf("限时活动任务已禁用（schedule.limited_enabled=false）")
	} else {
		log.Printf("限时活动任务已启用：%v 点（task_runner.py ALL --yes --limited，活动过期后脚本侧自动跳过）", cfg.Schedule.LimitedHours)
	}
	// 两类扰动的启动回执：只报"开了多大"，不报 seed。
	if cfg.Schedule.RandomDelayMinutes > 0 {
		log.Printf("排程随机平移已启用：每个时点在原小时内随机偏移 0-%d 分钟（按部署种子+日期+任务类型稳定取值）", cfg.Schedule.RandomDelayMinutes)
	}
	if cfg.Schedule.AccountDelayMaxSeconds > 0 {
		log.Printf("账号随机间隔已启用：定时遍历每两个账号之间额外等 %d-%d 秒（手动触发不受影响）",
			cfg.Schedule.AccountDelayMinSeconds, cfg.Schedule.AccountDelayMaxSeconds)
	}

	h := server.NewHandler(server.Config{
		Pool:          p,
		Upstream:      upHolder.Load(),
		APIKey:        cfg.APIKey,
		Session:       sessRouter,
		StickyEnabled: stickyEnabled.Load,
		StickyCount:   sessCount,
		RedisMode:     redisMode,
		SoftCooldown:  cfg.SoftRateDur,
		PromptMode:    cfg.Prompt.Mode,
		PromptText:    cfg.PromptText,
		// 网关侧网络错误（DNS/Dial/TLS）同号重试（upstream.connect_retry /
		// connect_retry_backoff）。**必须显式接线**：handler 侧的零值是「关闭」
		// （0 是合法语义），漏接线的表现是「配置写了却不重试」。热重载路径见
		// configreload.go 的 applyConfig（同一对字段）。
		ConnectRetry:        cfg.Upstream.ConnectRetry,
		ConnectRetryBackoff: cfg.ConnectRetryBackoffDur,
		// global realm 开关（handler 侧第三道闸：modelList 据此决定是否列 global 名单）。
		GlobalEnabled: cfg.Global.Enabled,
		// 模型别名表（热加载；nil 时 handler 按无别名运行）。
		Aliases: aliasStore,
	})

	// 配置热重载（见 configreload.go）：面板点保存 → 进程内立即重载，不做轮询。
	// loaded 是 diff 基准，随每次成功重载就地更新（同一指针，面板与重载器共用）。
	loaded := cfg
	configReloader := newConfigReloaderAdapter(*cfgPath, configTargets{
		pool:          p,
		up:            upHolder,
		handler:       h,
		sched:         sch,
		sess:          sessRouter,
		stickyEnabled: stickyEnabled,
		loaded:        loaded,
	})

	// 单端口对外：网关 handler 与面板 handler 合成一个 mux（见 panel.go）。
	// 装配失败不让网关跟着起不来 —— 面板是附属能力，降级为「仅网关」并明确告警。
	// p 一并传入：面板的停用/启用/复活需要进程内直达账号池（网关侧已无 /admin 端点）。
	// configReloader 让面板保存配置后直调热重载（同进程，保存即生效）。
	rootHandler, err := newRootHandler(cfg, *cfgPath, h, p, configReloader)
	if err != nil {
		log.Printf("WARN: [panel] 面板装配失败，本次仅提供网关接口: %v", err)
		rootHandler = h
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	// 池状态心跳：空闲期"服务还活着吗、池还健康吗"在 docker logs 里看不到任何迹象
	// （启动横幅之后，无请求/无非排程点时日志长期静默）。每 5 分钟一条低频汇总：
	// 频率取 5 分钟——不挂 pool 的 30 分钟落盘周期（太稀，空闲期仍看不出活性），
	// 也不挂 scheduler（Run 一睡可能数小时）。只打 ticker 触发的那条，不另打启动基线
	// （与首条 tick 重复，且启动横幅已证明日志通路活着）。ctx 取消即退出。
	go func() {
		const heartbeatInterval = 5 * time.Minute
		start := time.Now()
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				total, healthy, cooling, disabled, full := p.CountsDetailed()
				_, cnHealthy, _, _, _ := p.CountsDetailedForRealm("cn")
				_, globalHealthy, _, _, _ := p.CountsDetailedForRealm("global")
				log.Printf("[pool] heartbeat: accounts=%d healthy=%d (cn=%d global=%d) cooling=%d disabled=%d inflight_full=%d sticky=%d uptime=%s",
					total, healthy, cnHealthy, globalHealthy, cooling, disabled, full, sessCount(), time.Since(start).Round(time.Second))
			}
		}
	}()

	// 启动余额刷新：让 state.json 的 credits 从进程第一秒就是权威值，而非上次签到
	// 的陈旧快照（面板不点「积分」时读的就是它）。异步独立 goroutine：上游慢/不可达
	// 绝不能阻塞网关对外服务。用同一个 ctx，SIGTERM 时放弃剩余账号。
	//
	// 末尾显式 Flush 是必需的：SetCreditsDetailed 只置 dirty，而落盘周期现在是
	// 30 分钟（pool.state_flush）——不显式落盘就等于本功能没有生效。
	go func() {
		sch.RefreshCreditsOnce(ctx)
		p.Flush()
	}()

	// 账号目录热加载：替代「重启网关容器」加载新账号（见 reload.go）。
	// 面板扫码落盘后数秒自动进池，无需重启、零停机，也不必挂 docker.sock。
	stopAuthWatch := startAuthWatcher(ctx, cfg.AuthDir, p)
	defer stopAuthWatch()

	// 别名映射热加载：面板/手工改 data/model_aliases.json 数秒内生效（见 reload.go）。
	// 与账号目录热加载同模式；解析失败保持旧表（绝不清空别名）。
	stopAliasWatch := startAliasWatcher(ctx, aliasPath, aliasStore)
	defer stopAliasWatch()

	// 启动即预热模型积分倍率表：倍率只在 FetchModels/FetchGlobalModelInfos 成功时
	// 填充（两者均懒触发），重启后到首次 /v1/models 或面板模型页被访问之前，
	// ModelRate 恒返回空串——积分保底的目录兜底在这段空窗期内形同虚设，触底号
	// 会被当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费
	// 模型归零；倍率表当时尚未建立）。
	// 异步执行：不阻塞监听启动；失败仅记日志（下一轮懒触发仍可补上）。
	go warmModelRates(ctx, upHolder, p)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           rootHandler,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body 上传）：防慢速 body 拖死连接。
		// 请求体已无网关侧上限（max_body_mb 移除），此前硬编码 60s 会把大上下文/
		// 文件块经反代链的慢速上传掐掉，客户端拿到 400 "read body: ... i/o timeout"
		// ——而请求本身完全合法，重试同样注定超时（issue #100）。
		// 改由 server.read_timeout 配置：缺省 300s，"0" = 不限制。改动需重启进程。
		ReadTimeout: cfg.ServerReadTimeoutDur,
		// IdleTimeout keep-alive 空闲连接回收：配合 ctx 传播（FIX-2）防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		// Flush 已把最后一笔状态快照提交给 Redis（fire-and-forget）；store.Close
		// 等 Upstash 在途/排队写排空再关连接——最后一笔镜像必须写完才退出（发现 4）。
		// Noop 的 Close 是空操作；单写上限 5s × 上限 8，Close 内部另有超时兜底。
		if cErr := store.Close(); cErr != nil {
			log.Printf("WARN: [server] redisstore close: %v", cErr)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if cfg.Global.Enabled {
		log.Printf("global realm 已启用（chat_base=%q billing_base=%q，空=默认 workbuddy.ai）",
			cfg.Global.ChatBase, cfg.Global.BillingBase)
	} else {
		log.Printf("global realm 已禁用（config global.enabled=false，纯 CN）")
	}
	log.Printf("w2a listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// warmModelRates 启动预热各域模型积分倍率表（供积分保底的目录兜底判定）。
//
// 为什么需要：倍率表只在 FetchModels（CN）/ FetchGlobalModelInfos（global）成功时
// 填充，两者都是懒触发（被 /v1/models 或面板模型页访问才跑）。重启后到首次触发
// 之间的空窗期里 ModelRate 恒返回空串，保底的目录兜底判不出收费，触底号会被
// 当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费模型归零）。
//
// 失败处理：单域失败只记 WARN（不阻塞、不致命——后续懒触发仍会补上）；global 域
// 仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
func warmModelRates(ctx context.Context, holder *upstreamHolder, p *pool.Pool) {
	up := holder.Load()
	// 预热不得拖住进程退出：ctx 取消（SIGINT/SIGTERM）时立刻放弃剩余域。
	if ctx.Err() != nil {
		return
	}
	// CN：有可用 CN 账号才拉（与面板 models 同口径，避免无谓上游调用）。
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			if _, err := up.FetchModels(a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model rates: cn ok")
			}
		}
	}
	// global：独立目录端点（workbuddy.ai），倍率按 "global" 域键存储。
	if up.GlobalEnabled && ctx.Err() == nil {
		if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if a := p.AuthByUID(uids[0]); a != nil {
				// FetchGlobalModelInfos 无错误返回（内部负缓存自行节流），
				// 仅按结果条数判断是否拿到目录。
				if infos := up.FetchGlobalModelInfos(a); len(infos) == 0 {
					log.Printf("WARN: [upstream] warm model rates (global): empty model list")
				} else {
					log.Printf("[upstream] warm model rates: global ok (%d models)", len(infos))
				}
			}
		}
	}
}
