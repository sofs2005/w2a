// config.go 加载 JSON 配置 + 环境变量覆盖。
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/config"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/upstream"
)

// Config 顶层配置。
type Config struct {
	Listen    string `json:"listen"`     // ":7863"
	APIKey    string `json:"api_key"`    // 空 = 不鉴权
	AuthDir   string `json:"auth_dir"`   // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	// Proxies 账号级出口线路表：线路名 → 代理地址（http / https / socks5 / socks5h）。
	//
	// 面板会给账号绑一条线路（auths/*.json 顶层 `proxy` 写的是**线路名**），这个名字
	// 在这里解析成地址；线路名查不到时上游**明确报错、不静默直连**（静默直连等于
	// 「以为隔离了其实没隔离」，见 upstream.TransportFor）。保留名 "__direct__"
	// 表示显式直连（忽略环境变量代理），不需要在表里配置。
	Proxies map[string]string `json:"proxies"`

	// Server 入站 HTTP 服务参数（issue #100）。
	//
	// 此段曾因 max_body_mb 移除而退役（请求体改由上游自然响应），现重新启用：
	// 旧配置里该段下的任何键都因 JSON 未知字段而自然忽略，所以从空结构体改回
	// 有字段不会让既有配置报错。
	Server struct {
		// ReadTimeout 入站请求读取（含 body 上传）总时长上限（issue #100）。
		// http.Server 的 ReadTimeout 覆盖整个请求读取：大上下文/文件块请求经
		// 反代链转发时上传可能超过旧固定值 60s，被掐后客户端拿到
		// 400 "read body: ... i/o timeout"——而请求本身完全合法。
		// 缺省 "300s"；"0" = 不限制（慢速 body 可无限占用连接，自担风险）；
		// 改动需重启进程。
		ReadTimeout string `json:"read_timeout"` // "300s"；"0" = 不限制
	} `json:"server"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown 三个历史键已退役：
		// 硬冷却固定为次日 04:00（CooldownUntilTomorrow4AM），连续错误语义并入熔断器。
		// 旧 config 中的这些键因 JSON 未知字段而自然忽略，不报错。
		SoftRate string `json:"soft_rate"` // "600s"，软限流冷却基数
		// SoftRateMax 软冷却指数退避的封顶，默认 "2h"。
		// 空值回落默认，非法值报错（处理风格同 soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule config.Schedule `json:"schedule"`

	// 注：旧版的 `admin` 段（admin.enabled，控制网关 /admin/accounts/* 端点）已删除。
	// 那组端点已下线，账号停用/启用/复活改由面板进程内直调账号池，没有开关可配。
	// 旧 config.json 里的 "admin": {...} 键作为未知字段被静默忽略，无需迁移。

	Global struct {
		// Enabled global realm 路由开关。缺省 true：Realm() 正常把 realm=global/
		// domain=workbuddy.ai 的账号判为 global 并路由 global base/路径。
		// 显式 "enabled": false 关闭（逃生门，纯 CN 锁定：即便 auth 写了 realm=global
		// 也不路由，auth.Realm() 双保险的第一道闸）。纯 CN 部署行为不变：CN 账号
		// 恒判 cn，global base 只在 realm=global 的账号上被使用。
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase 国际版上游 base 覆盖；空 = 回落内置默认
		// https://www.workbuddy.ai（D5，internal/upstream.defaultGlobalBase）。
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	// Proxy 出站代理（按域分档）。与 global 段形成对偶：global 段定义「去哪」，
	// 本段定义「怎么去」。
	//
	// 动机：网关常部署在国内服务器上，访问国外上游域（www.workbuddy.ai）需要代理，
	// 而国内域（copilot.tencent.com / codebuddy.cn）直连更快——一刀切的全局代理两头
	// 不讨好。此前只有 HTTP_PROXY/HTTPS_PROXY 环境变量可用，无法分域。
	//
	// 取值形如：
	//   "http://user:pass@proxy.corp:3128"
	//   "socks5://user:pass@10.0.0.1:1080"    （socks5h 同义，见 upstream/proxy.go）
	//
	// 语义：
	//   - 空 = 该域**不显式配置**，回落 HTTP_PROXY/HTTPS_PROXY/NO_PROXY 环境变量
	//     （再空则直连）。故「不配本段」的既有部署行为逐字节不变——零回归。
	//   - 非空 = 该域显式走该代理，此时**不看**环境变量，也**不套用** NO_PROXY
	//     （给了某域的代理就是「这域走这个代理」，再叠一层排除规则会让「到底走没走
	//     代理」变成不可预测的状态）。
	//   - 两域独立：只配 global 时 CN 仍走环境变量，不是「跟随 global」。
	//
	// 另：models.dev（国外站，/v1/models 的 context_length 兜底）在配了 global 时
	// 一并走 global 代理——它与账号无关，纯粹是「这个域名在国外」。
	//
	// 非法 URL（scheme 不在白名单 / 缺 host / userinfo 形态错）在 normalize 阶段
	// WARN 并置空回落，**不** fail-fast——一个错字段不该让网关起不来。改动需重启进程。
	Proxy struct {
		// CN 国内域（copilot.tencent.com / codebuddy.cn）出站代理；空 = 回落环境变量。
		CN string `json:"cn"`
		// Global 国外域（www.workbuddy.ai）+ models.dev 出站代理；空 = 回落环境变量。
		Global string `json:"global"`
	} `json:"proxy"`

	Upstream struct {
		// TimeoutSeconds 短 RPC（refresh/checkin/balance/FetchModels）总时长上限，默认 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 聊天 SSE 首字节前（响应头）上限；<=0 回落 TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）；<=0 回落默认 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
		// 全部出站请求生效：chat/refresh/checkin/balance/report/travel/FetchModels。
		// issue #42 深挖：官网「使用端」列基于出站请求 UA 的服务端归因，官方 WorkBuddy
		// 桌面 UA 为 `WorkBuddy/<version>`。默认值已对齐官方（A 段变更），用户仍可配完全
		// 自定义值改写。
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` 与白名单
		// 头组 X-IDE-Version 的取值）。空 = 按账号 realm 取内置默认（对齐官方分发包：
		// CN 5.8.3、global 5.8.4，两域版本不同步）；显式配置（如升级后的桌面包版本）
		// 则对两域统一随配置走。
		ClientVersion string `json:"client_version"`
		// CliVersion 出站 UA 中 `CLI/<ver>` 段的版本。空 = 内置默认（对齐官方内置 CLI
		// 2.161.0，两域同版）；显式配置则随配置走。
		CliVersion string `json:"cli_version"`
		// StainlessRuntimeVersion 出站 `X-Stainless-Runtime-Version`（官方 chat 走 OpenAI
		// Node SDK，SDK buildHeaders 会带 process.version）。空 = 内置默认
		// （defaultStainlessRuntimeVersion）。官方 CLI 跑在 Electron 内置 Node 上，具体
		// 版本无法从 Go 侧推断，故留此覆盖口，线上校准无需改码重编。
		StainlessRuntimeVersion string `json:"stainless_runtime_version"`
		// StainlessTimeoutSeconds 出站 `X-Stainless-Timeout`（秒）。空 = 内置默认 1200
		// （对应官方 buildOpenAIClientOptions 的 timeout: 1.2e6 ms）。
		StainlessTimeoutSeconds string `json:"stainless_timeout_seconds"`

		// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底。
		// 容器内无桌面端 Turing SDK，这是把外部生成的 token 注入的入口；空 = 不注入。
		// 每号覆盖优先级：auths 文件 device_token > 本全局值 > DeviceTokenFile（文件兜底）。
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile 宿主落盘的 device token 文件路径（可选，空 = 不读文件）。
		// 读取频率限 5 分钟一次缓存，>1KB 或读失败则忽略（优雅降级不注入）。
		DeviceTokenFile string `json:"device_token_file"`
		// ClientName 用量归属头 X-Product/X-IDE-Name/X-IDE-Type 的取值。
		// 空（缺省）= "WorkBuddy"：伪造官方桌面端指纹（X-IDE-* 四头 + X-Agent-Purpose，
		// 上游用量归因不再出现 client/agentPurpose 为空的网关特征）。
		// 显式配 "SaaS" 还原旧行为（仅 X-Product="SaaS"，不设 X-IDE-*）。
		ClientName string `json:"client_name"`
		// PassthroughIP 是否透传客户端 IP（X-Forwarded-For/X-Real-IP 首段）给上游。
		// 缺省 false（反代安全边界：不把内网/代理 IP 暴露给上游）；true 才透传。
		PassthroughIP bool `json:"passthrough_ip"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints 出站请求体黑名单指纹脱敏（默认 true；false 完全还原）。
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Prompt struct {
		// Mode passthrough（默认）= 透传客户端原始 system（降级重试仍会切到 Degraded）；
		// custom = 网关用自有系统提示词替换客户端 system/developer（显式配置仍可覆盖回替换）；
		// append = 两者并用：开头连续 system/developer 块后插网关 system，既有消息逐字不动（issue #129）。
		Mode string `json:"mode"` // "custom" / "append" / "passthrough"
		// File 提示词文件路径；空 = 内置默认 defaultprompt.md；
		// 路径非空但不可读 → 启动报错（fail fast，避免静默回落到内置默认）。
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText 解析后的系统提示词文本（custom/append 模式使用）。
	PromptText string `json:"-"`

	Upstash struct {
		URL   string `json:"url"`   // 空 = 纯内存模式；支持完整 rediss:// URL 或 https://xxx.upstash.io host
		Token string `json:"token"` // url 非完整连接串时用于组装 rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight        int    `json:"max_in_flight"`        // 单账号最大在途请求数，0 = 不限
		MaxInFlightGlobal  int    `json:"max_in_flight_global"` // global 域单账号在途上限（WAF 403 风控分档），0 = 回落 max_in_flight
		BreakerThreshold   int    `json:"breaker_threshold"`    // 连续失败次数触发熔断，默认 3
		BreakerCooldown    string `json:"breaker_cooldown"`     // 基础熔断时长，默认 "30m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // 指数退避封顶，默认 "6h"
		// 连败降权（issue #114「累计错误率高/连续失败 N 次的账号移出候选池一段时间」）：
		// ErrClient/传输层这类「不罚号」失败连续计数，达阈临时出池。与冷却/熔断
		// 并存取更长者不叠加。默认 5 次 / 10m。
		DegradeThreshold   int     `json:"degrade_threshold"`    // 连败次数触发降权，默认 5
		DegradeCooldown    string  `json:"degrade_cooldown"`     // 降权时长（固定，非指数退避），默认 "10m"
		DegradeCooldownMax string  `json:"degrade_cooldown_max"` // 降权时长的上限钳制，默认 "2h"（仅当 cooldown 超该值才钳制）
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"` // 闲置补偿：每小时未用 +0.5 权重
		IdleWeightMax      float64 `json:"idle_weight_max"`      // 闲置补偿封顶，默认 5.0
		// ExpiringSoon 快过期积分窗口（如 "168h"=7天）：签到查余额时，到期时间在此窗口内
		// 的积分被标记为"快过期"，选号优先消耗（issue:积分过期）。空/0 = 禁用分桶。
		ExpiringSoon string `json:"expiring_soon"`
		// UnifiedRouting 统一调度开关（缺省 true）：裸模型名（无 cn:/global: 前缀）
		// 是否进入**全池**调度（CN 与 global 账号共用同一套粘性/加权/分层/紧急逻辑）。
		//
		// 关闭（false）恢复旧语义：裸名只路由 CN 账号（老客户端零回归契约）。
		// 这是路由层逃生门，与 global.enabled（账号层纯 CN 锁定）正交；显式钉域
		// 仍可用 cn:/global: 前缀，与此开关无关。默认 true 是本次改造的目标形态。
		UnifiedRouting bool `json:"unified_routing"`
		// CostExploreInterval costTier 条件探索窗口（issue #136 方案 a′）：tier 0
		// 垄断层存在且 tier 1 有成员时，距上次探索 ≥ 窗口则本次 pick 生效层切
		// tier 1-only（探索=搭车改道，零新增上游请求；成功即毕业，失败走既有
		// 错误策略）。默认 "30m"（≤48 次/天/模型）；"0" 关停（完全回到现状行为）；
		// 空值回落默认。
		CostExploreInterval string `json:"cost_explore_interval"`
		// CreditFloorCN / CreditFloorGlobal 积分保底（按域分档）：账号余额低于本域
		// 保底值时，对收费模型不再参与选号——防止收费请求把余额打穿、连免费模型都
		// 402 冷却到次日签到（最坏约 11.5 小时不可用）。收费判据两级：本地实测台账
		// 优先，无观测时用上游目录倍率兜底（高价新模型全池无观测时不会绕过）。
		// 免费模型（实测 cost=0 或目录倍率 0）不受限；签到回血越过保底自动恢复。
		//
		// 分域理由：国内外积分购买力与免费额度差异大，同一阈值两头不讨好。
		// 默认 100 / 30（键缺席即取默认；显式写 0 = 关闭该域保底）；负值钳 0。
		// 紧急时改小或改 0 重启即放行（保留分本就是"应急可用"的余额）。
		CreditFloorCN     int64 `json:"credit_floor_cn"`
		CreditFloorGlobal int64 `json:"credit_floor_global"`
		// CreditFloor 已废弃：旧版单值保底（默认 0 = 关闭），被上面两个分域键取代。
		// 仅为兼容旧配置保留——非 0 时按同值迁移到两域（显式写新键仍优先），
		// 见 normalize。omitempty 使自动生成的配置只出现新键。
		CreditFloor int64 `json:"credit_floor,omitempty"`
		// StateFlush 池状态（state.json）后台落盘周期，如 "30m"。
		//
		// 余额扣减估算/成功错误计数/冷却截止这类**高频运行态**攒到周期末统一落盘；
		// **结构性变更**（手动停用、账号剔除）绕过本周期立即落盘——运维意图不等
		// 落盘窗口。默认 "30m"；空值回落默认；"0" = 关闭后台落盘（仅靠退出时 Flush，
		// 适合只读/调试部署）；负值钳 0。
		//
		// ⚠️ 本周期即「进程被强杀（SIGKILL / Windows taskkill /F / 容器 OOM）时的
		// 状态丢失窗口」。Docker 下 docker stop 走 SIGTERM 优雅停机，不受影响；
		// Windows 原生部署的 stop-workbuddy2api.cmd 用 taskkill /F，不触发信号处理。
		StateFlush string `json:"state_flush"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`     // 默认 true
		TTL        string `json:"ttl"`         // 会话绑定 TTL，默认 "30m"
		GCInterval string `json:"gc_interval"` // 会话 GC 周期，默认 "5m"
	} `json:"session_sticky"`

	// 解析后
	SoftRateDur         time.Duration `json:"-"`
	SoftRateMaxDur      time.Duration `json:"-"`
	BreakerCooldownDur  time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	DegradeCooldownDur  time.Duration `json:"-"`
	DegradeCooldownMaxD time.Duration `json:"-"`
	SessionTTL          time.Duration `json:"-"`
	SessionGCInterval   time.Duration `json:"-"`
	ExpiringSoonDur     time.Duration `json:"-"`
	// CostExploreIntervalDur 解析后的 costTier 探索窗口（issue #136）；0 = 关停。
	CostExploreIntervalDur time.Duration `json:"-"`
	// StateFlushDur 解析后的池状态落盘周期（pool.state_flush）；0 = 关闭后台落盘。
	StateFlushDur time.Duration `json:"-"`
	// ServerReadTimeoutDur 解析后的入站请求读取上限（server.read_timeout）；0 = 不限制。
	ServerReadTimeoutDur time.Duration `json:"-"`
}

// 积分保底（pool.credit_floor_cn / credit_floor_global）的默认值。提为常量是因为
// 旧键迁移要用它们判「该域仍是默认值、可以铺旧键」，见 normalize。
const (
	defaultCreditFloorCN     int64 = 100
	defaultCreditFloorGlobal int64 = 30
)

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:    ":7863",
		APIKey:    "",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Server.ReadTimeout = "300s"
	// 排程段默认值由 internal/config 集中维护（cmd/server 与 cmd/activity 共用，
	// 消除 issue #49 的默认值漂移）。
	c.Schedule = config.DefaultSchedule()
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds 默认 0（未设置态），回落见 normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// Global.Enabled 缺省 true（纯 CN 行为不变：CN 账号恒判 cn，global base 不被使用）；
	// ChatBase/BillingBase 缺省空（回落内置默认）。
	c.Global.Enabled = true
	// 出站指纹默认伪造官方 WorkBuddy 桌面端：UA 三段式 + X-IDE-* 头组
	// （upstream.Client 的 attributionClientName 空值也回落 WorkBuddy，双保险）；
	// 显式 client_name="SaaS" 还原旧行为。
	c.Upstream.ClientName = "WorkBuddy"
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "passthrough" // 缺省 passthrough：默认透传客户端原始 system；显式配置 custom 仍可覆盖回替换
	c.Pool.MaxInFlight = 3
	// MaxInFlightGlobal 缺省 2：global 域 WAF 风控更紧，压低单号并发（WAF 403
	// 修复 P1-1）；0/负数 normalize 回落。显式 0 需配 -1 之外的方式关闭分档
	// ——这里约定 0 = 未设置回落默认 2（与 max_in_flight 的 0=不限语义不同，
	// 分档键的 0 没有合理语义，回退分档默认最稳）。
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.ExpiringSoon = "168h" // 快过期窗口默认 7 天：官方活动奖励积分多在两周内过期
	// UnifiedRouting 缺省 true：裸名全池调度（本改造目标形态）。显式 false 退回
	// 「裸名只打 CN」旧语义（路由层逃生门，见字段注释）。
	c.Pool.UnifiedRouting = true
	// costTier 探索默认 30m（issue #136：垄断破除 + 搭车改道零新增请求）；"0" 关停。
	c.Pool.CostExploreInterval = "30m"
	// 积分保底按域分档默认：国内 100 / 国外 30（键缺席即取此默认，见 Load 的
	// Default→Unmarshal 顺序；显式写 0 才关闭）。国外账号的免费额度与积分购买力
	// 都低于国内，留 100 会把号压得太死。
	c.Pool.CreditFloorCN = defaultCreditFloorCN
	c.Pool.CreditFloorGlobal = defaultCreditFloorGlobal
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	// 池状态落盘周期默认 30m：高频运行态攒批落盘，结构性变更绕过本周期立即落盘。
	c.Pool.StateFlush = "30m"
	return c
}

// Load 从文件读，再用 WB2A_* env 覆盖。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// priorRunEvidence 判定「这台部署曾经跑过」，返回检测到的痕迹描述；空串 = 干净首次运行。
//
// 为什么需要它：配置缺失有两种成因，处理方式截然相反——
//
//  1. 真正的首次运行（全新部署，什么都没跑过）：自动生成一份随机 api_key 的推荐配置
//     是贴心且安全的；
//  2. **老部署的配置路径漂移**（实案：05b18ce 把 ENTRYPOINT 从 /app/config.json 改成
//     /app/config/config.json，而宿主机的单文件挂载仍钉在旧路径）：此时配置缺失只是
//     假象——真配置在另一个路径上，网关读不到。自动生成会**静默换掉 api_key**，下游
//     客户端全部 401；同时旧 config.json 里的 listen / upstash / pool 调优从头到尾
//     没被读过。症状极难归因：每次 down+up 都换一把新 key，像「随机失效」。
//
// 判据只取**网关/面板自己产出的**文件，不取 auths/：README 记载的首装流程是
// 「先 login.sh 登录（写 auths/）→ 再 docker compose up -d」，故 auths/ 里有凭证
// 完全可能是尚未首启的正常状态，拿它当「跑过」的证据会把首装直接打死。
// 同理不能拿「目录是否存在」当判据——Dockerfile 里 `mkdir -p /app/auths /app/data`
// 让这两个目录在全新容器里就已存在，只有里面的**内容**才是运行痕迹。
//
// state.json 是网关的池状态落盘（internal/pool/persist.go），登录脚本绝不产生它；
// 面板的 data 侧产物（model_aliases.json / backups / gui-credentials.json）同理。
func priorRunEvidence(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	dir := filepath.Dir(stateFile)
	candidates := []struct{ path, desc string }{
		{stateFile, "池状态文件"},
		{filepath.Join(dir, "model_aliases.json"), "模型别名文件"},
		{filepath.Join(dir, "gui-credentials.json"), "面板凭据文件"},
		{filepath.Join(dir, "backups"), "面板配置备份目录"},
	}
	for _, c := range candidates {
		if _, err := os.Stat(c.path); err == nil {
			return c.desc + " " + c.path
		}
	}
	return ""
}

// WriteDefault 在 path 落一份推荐配置（首次运行自动生成，免去「手工复制 config.example.json
// 再改 key」这一步）。api_key 用 crypto/rand 随机生成，**不留示例占位符**：
// listen 默认绑 :7863（0.0.0.0），空 key 或 "test_key" 等于把网关连同全部账号裸暴露。
// 已存在时经 O_EXCL 原子拒绝，绝不改写用户配置。返回生成的 key 供启动日志透出。
func WriteDefault(path string) (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	key := "sk-" + base64.RawURLEncoding.EncodeToString(raw)
	c := Default()
	c.APIKey = key
	if err := c.normalize(); err != nil {
		// Default() 全合法，normalize 只为补齐 header/idle 超时的展示值；
		// 真出错说明默认值被改坏了，落盘前就暴露，不要写一份带病配置。
		return "", fmt.Errorf("normalize default config: %w", err)
	}
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	// O_EXCL 原子拒绝覆盖：即使调用方漏判「不存在」，也绝不悄悄改写用户已有配置。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	return key, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_PROXY_CN"); v != "" {
		c.Proxy.CN = v
	}
	if v := os.Getenv("WB2A_PROXY_GLOBAL"); v != "" {
		c.Proxy.Global = v
	}
	// WB2A_PROXIES：账号级线路表（JSON 对象，如 {"route-a":"socks5h://u:p@h:1080"}）。
	// 解析失败静默忽略是刻意的（与其它环境变量同口径）；normalize 会再校验地址。
	if v := strings.TrimSpace(os.Getenv("WB2A_PROXIES")); v != "" {
		var table map[string]string
		if err := json.Unmarshal([]byte(v), &table); err == nil {
			c.Proxies = table
		}
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_STAINLESS_RUNTIME_VERSION"); v != "" {
		c.Upstream.StainlessRuntimeVersion = v
	}
	if v := os.Getenv("WB2A_STAINLESS_TIMEOUT_SECONDS"); v != "" {
		c.Upstream.StainlessTimeoutSeconds = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := os.Getenv("WB2A_STATE_FLUSH"); v != "" {
		c.Pool.StateFlush = v
	}
	// 注：WB2A_ADMIN_ENABLED 随 admin 段一并删除，设置了也不再被读取（管理面已下线）。
}

func (c *Config) normalize() error {
	var err error
	// 入站读取上限（issue #100）：空值回落默认 300s；"0" 合法（不限制）；
	// 负值 fail fast —— 静默钳 0 会把保护悄悄关掉（配置本意是「限制多少」，
	// 写负数几乎必然是笔误，而不是「请关掉保护」）。
	if c.Server.ReadTimeout == "" {
		c.Server.ReadTimeout = "300s"
	}
	if c.ServerReadTimeoutDur, err = time.ParseDuration(c.Server.ReadTimeout); err != nil {
		return fmt.Errorf("server.read_timeout: %w", err)
	}
	if c.ServerReadTimeoutDur < 0 {
		return fmt.Errorf("server.read_timeout: 负时长 %q 无意义（关掉限制请写 \"0\"）", c.Server.ReadTimeout)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// 空值回落默认 2h（Default() 已置值；此兜底覆盖显式 "" 与 Default() 被绕过的场景）。
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	// 连败降权参数缺省归一（非法/未设置回落默认，与 breaker_threshold 同风格）。
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	// global 在途分档：0/负数视为未设置回落默认 2（WAF 403 修复 P1-1）。
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	// 快过期窗口：空值回落默认 168h（Default 已置；此兜底覆盖显式 ""）；显式 "0"/负值 = 禁用分桶。
	if c.Pool.ExpiringSoon == "" {
		c.Pool.ExpiringSoon = "168h"
	}
	if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
		return fmt.Errorf("pool.expiring_soon: %w", err)
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0 // 负值视为禁用，避免 upstream 判定窗口反转
	}
	// costTier 探索窗口（issue #136）：空值回落默认 30m（Default 已置；此兜底覆盖
	// 显式 ""）；"0" 是合法值（关停，完全回到现状行为），不回落；负值钳 0 同关停
	// （"−5m" 无合理语义）。
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = "30m"
	}
	if c.CostExploreIntervalDur, err = time.ParseDuration(c.Pool.CostExploreInterval); err != nil {
		return fmt.Errorf("pool.cost_explore_interval: %w", err)
	}
	if c.CostExploreIntervalDur < 0 {
		c.CostExploreIntervalDur = 0
	}
	// 积分保底（按域分档）：默认 100/30 由 Default() 预置——Load 的顺序是
	// Default → Unmarshal → applyEnv → normalize，故 JSON 里**键缺席**时字段保留
	// 默认值、**显式写 0** 才落到 0（= 关闭该域）。这里不做空值回落（0 是合法值）。
	//
	// 旧键迁移：credit_floor 是已废弃的单值保底（旧默认 0 = 关闭）。仅在"显式配了
	// 非 0"时把它当回退值铺过去，且**只铺仍是默认值的那个域**——显式写了新键
	// （尤其显式写 0 关闭某域）的配置不被旧键顶掉。老部署升级后语义不突变。
	if c.Pool.CreditFloor > 0 {
		if c.Pool.CreditFloorCN == defaultCreditFloorCN {
			c.Pool.CreditFloorCN = c.Pool.CreditFloor
		}
		if c.Pool.CreditFloorGlobal == defaultCreditFloorGlobal {
			c.Pool.CreditFloorGlobal = c.Pool.CreditFloor
		}
	}
	c.Pool.CreditFloor = 0 // 归一为关闭态：此后只有两个分域字段是事实来源
	// 负值钳 0（= 关闭该域；负数阈值无意义，且会让 credits < floor 恒假而静默失效）。
	if c.Pool.CreditFloorCN < 0 {
		c.Pool.CreditFloorCN = 0
	}
	if c.Pool.CreditFloorGlobal < 0 {
		c.Pool.CreditFloorGlobal = 0
	}
	// 池状态落盘周期：空值回落默认 30m（Default 已置；此兜底覆盖显式 ""）；
	// "0" 是合法值（关闭后台落盘，仅靠退出时 Flush），不回落；负值钳 0 同关闭。
	if c.Pool.StateFlush == "" {
		c.Pool.StateFlush = "30m"
	}
	if c.StateFlushDur, err = time.ParseDuration(c.Pool.StateFlush); err != nil {
		return fmt.Errorf("pool.state_flush: %w", err)
	}
	if c.StateFlushDur < 0 {
		c.StateFlushDur = 0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header 缺省回落 timeout（保"首字节前换号"既有语义）；idle 缺省走内置大值。
	// 任务书约定：0 一律视为"未设置"走默认，真正的"禁用"留待后续（避免歧义）。
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	// 出站代理（proxy.cn / proxy.global）：空串合法（= 回落环境变量）。非空则校验，
	// 非法**不 fail-fast** 而是 WARN + 置空回落——理由见 Proxy 字段注释：代理配错
	// 最坏是「走直连」（与不配等效），而 fail-fast 会让一个笔误把整个网关挡住。
	// 校验口径与运行期共用 upstream.ParseProxyURL（避免两处漂移）。
	if c.Proxy.CN != "" {
		if _, err := upstream.ParseProxyURL(c.Proxy.CN); err != nil {
			log.Printf("WARN: [config] proxy.cn 非法，按未配置处理（回落环境变量代理）: %v", err)
			c.Proxy.CN = ""
		}
	}
	if c.Proxy.Global != "" {
		if _, err := upstream.ParseProxyURL(c.Proxy.Global); err != nil {
			log.Printf("WARN: [config] proxy.global 非法，按未配置处理（回落环境变量代理）: %v", err)
			c.Proxy.Global = ""
		}
	}
	// 出口线路表（proxies）：名字与地址都去空白；scheme 只认 http/https/socks5/socks5h。
	// 与上面域级 proxy 的「WARN 回落」不同，这里**启动即报错**：线路是账号级强绑定，
	// 配错后静默回退等于「以为隔离了其实没隔离」——这是本功能唯一要避免的结果。
	// 线路名查不到（账号引用了不存在的名字）则在用时明确报错，不静默直连。
	if len(c.Proxies) > 0 {
		clean := make(map[string]string, len(c.Proxies))
		for name, addr := range c.Proxies {
			n := strings.TrimSpace(name)
			v := strings.TrimSpace(addr)
			if n == "" || v == "" {
				continue
			}
			u, err := url.Parse(v)
			if err != nil {
				return fmt.Errorf("proxies[%s]: %w", n, err)
			}
			switch strings.ToLower(u.Scheme) {
			case "http", "https", "socks5", "socks5h":
			default:
				return fmt.Errorf("proxies[%s]: 不支持的协议 %q（只能是 http/https/socks5/socks5h）", n, u.Scheme)
			}
			if u.Host == "" {
				return fmt.Errorf("proxies[%s]: 地址缺少主机名（%q）", n, v)
			}
			clean[n] = v
		}
		c.Proxies = clean
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// 排程段归一（空数组回落默认、ActivityReportCount 归一、小时范围校验）
	// 由 internal/config 统一实现，cmd/server 与 cmd/activity 共用同一份语义。
	if err := c.Schedule.Normalize(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizePrompt 校验 prompt.mode 并按 file 加载提示词文本（custom/append 模式）。
//
// mode 非法（非 custom/append/passthrough）启动报错，避免静默回落到某一分支；
// custom/append 模式下 file 非空但不可读 → 报错（fail fast），file 空 → 用内置默认
// （两模式共用同一加载路径，PromptText 均非空）。
// passthrough 模式不加载文本（透传客户端原始 system，文本在降级时用 prompt.Degraded）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough" // 缺省 passthrough：默认透传客户端原始 system
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（custom / append / passthrough）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" || c.Prompt.Mode == "append" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}
