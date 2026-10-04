// types.ts 与 Go 后端 JSON 结构一一对应的类型定义。

/** 账号合并视图（磁盘凭证 + 网关运行态 + 积分缓存）。 */
export interface Account {
  uid: string
  nickname: string
  enterprise_id: string
  domain: string

  has_file: boolean
  file_name: string
  expires_at: number
  expired: boolean
  needs_refresh: boolean
  has_refresh_token: boolean

  in_gateway: boolean
  /** healthy | cooling | disabled | token_expired | gateway_unreachable | missing_credential | unknown */
  status: string
  /** cn | global。统一调度后裸名跨域选号，靠它分辨请求落在哪一域的号上。 */
  realm?: string

  cooling: boolean
  cool_kind?: string
  cool_remaining_sec?: number
  disabled: boolean
  reason?: string
  in_flight: number
  breaker_fails: number
  breaker_until?: string
  soft_streak?: number
  success_count: number
  err_total: number
  last_success?: string
  last_err?: string

  /**
   * 该账号当前仍在限额的模型（6004 模型级冷却，未到期条目）。
   * 网关侧到期即消失，故这里恒为「此刻仍受限」的集合；缺省 = 无受限模型。
   */
  rate_limited_models?: RateLimitedModel[]
  /**
   * 该账号每模型的实测成本台账。
   *
   * 这是「按号分账」在面板上唯一的可见处：/v1/stats 只按模型字符串聚合、
   * 不区分账号，所以「裸名的请求落在国内号还是国际号、各自花了多少」
   * 只能从这里或容器日志看。
   */
  model_costs?: ModelCostStatus[]

  credits: number
  live_credits?: number
  credits_at?: string
  /** 积分到期：与 live_credits 同源同时刻。 */
  credits_expire_at?: string
  credits_expiring?: number
}

/** 单个被限流模型的台账行（账号详情用）。 */
export interface RateLimitedModel {
  model: string
  /** 该模型独立冷却的截止（可能已被 soft_rate_max 截断）。 */
  until?: string
  /** 上游「将在 … 重置」的原始墙钟；未截断时与 until 同值。 */
  reset_at?: string
  reason?: string
}

/**
 * 单个 (账号, 模型) 的成本台账行。
 *
 * tier 不单独下发，按 cost_per_1k 推出（与网关同一口径，避免两处表示漂移）：
 * ≤0 = 实测免费（tier 0，选号优先）；>0 = 按单价排序（tier 2）。
 */
export interface ModelCostStatus {
  model: string
  cost_per_1k: number
  last_seen: string
  samples?: number
}

/** 单个积分包的到期明细。 */
export interface CreditPack {
  name?: string
  /**
   * 真正的到期时间，上游原文 "2006-01-02 15:04:05"；空 = 无到期。
   *
   * 后端取的是上游 `CycleEndTime`（本周期边界，积分到期即作废）——**不是**
   * `DeductionEndTime`：按周期发量的包（如「个人体验版」）后者是 2034 年，
   * 与注册日同月日、恰隔 10 年，是账户级的登记上限而非作废时刻。
   */
  end_time?: string
  /**
   * 上游 CycleEndTime 原文。仅当它解析失败、end_time 回退到 DeductionEndTime 时
   * 才与 end_time 不同，此时一并给出便于排查。
   */
  cycle_end_time?: string
  remain: number
  size: number
}

export interface Credits {
  remain: number
  used: number
  size: number
  packages: number
  /** 最近一次积分到期时间（上游原文，空 = 无到期）。 */
  expires_at?: string
  /** 与 expires_at 同时刻到期的那批剩余积分。 */
  expiring_remain?: number
  details?: CreditPack[] | null
}

export interface CreditsTotal {
  remain: number
  used: number
  size: number
  accounts: number
  ok: number
  failed: number
}

export interface Health {
  healthy: number
  total: number
  service: string
}

export interface Overview {
  gateway_url: string
  gateway_ok: boolean
  gateway_error?: string
  health?: Health

  total: number
  healthy: number
  cooling: number
  disabled: number
  in_flight_full: number
  in_flight: number
  sticky_sessions: number
  redis_mode: string

  file_count: number
  expired: number
  expiring: number
  warnings?: string[]
  file_issues?: string[]

  credits: CreditsTotal
  read_only: boolean
  dangerous_ops: boolean
  server_time: string
}

export interface GatewayStatus {
  total: number
  healthy: number
  cooling: number
  disabled: number
  in_flight_full: number
  sticky_sessions: number
  redis_mode: string
}

export interface AccountsResponse {
  accounts: Account[]
  file_issues?: string[]
  gateway_ok: boolean
  gateway_error?: string
  summary?: GatewayStatus
}

export interface OpResult {
  uid: string
  action: string
  ok: boolean
  message: string
  reward?: number
  data?: Record<string, unknown>
}

export interface TaskItem {
  uid: string
  nickname: string
  action: string
  ok: boolean
  message: string
  reward?: number
  started_at: string
  ended_at: string
}

export interface TaskView {
  id: string
  kind: string
  title: string
  running: boolean
  error?: string
  started_at: string
  finished_at?: string
  total: number
  done: number
  ok: number
  failed: number
  items?: TaskItem[]
}

export interface TaskListResponse {
  tasks: TaskView[] | null
  running: TaskView[] | null
}

export type LoginState = 'pending' | 'success' | 'error' | 'expired' | 'cancelled'

export interface LoginSession {
  id: string
  region: 'cn' | 'global'
  auth_url: string
  status: LoginState
  message?: string
  created_at: string
  updated_at: string
  uid?: string
  nickname?: string
  saved: boolean
  file?: string
  restart?: string
}

export interface Model {
  id: string
  object: string
  created: number
  owned_by: string
  context_length: number
  max_output_tokens?: number

  /** 以下为网关 /v1/models 透出的上游元信息（模型目录页用）。 */
  /** 上游显示名，如 "Deepseek-V4.1-Flash"。 */
  name?: string
  description?: string
  /**
   * 积分倍率原文，形如 "x0.03" 或 "x0.59 credits"；缺省 = 上游未标倍率。
   *
   * 两域都有该模型时这是**并集去重留下的单个值**（CN 优先），只代表国内版价；
   * 按域分栏展示请用 credits_cn / credits_global。
   */
  credits?: string
  /**
   * 该模型在国内版 / 国际版的积分倍率原文（各域自己的价）。
   *
   * 为什么按域拆：两域倍率确实会不同，且促销（限时免费 / 夜间折扣）也是分域下发的
   * ——只留合并后的 credits 会让国际版 tab 显示国内价。
   * 缺省（老网关未透出）= undefined，此时回退用 credits。
   */
  credits_cn?: string
  credits_global?: string
  vendor?: string
  tags?: string[]
  is_default?: boolean
  only_reasoning?: boolean
  /**
   * 上游声明的图片能力。
   *
   * ⚠️ **不要拿它上屏**：2026-09-23 实测国际版 hy* 系（hy3 / hy4-preview-f / hy4-preview）
   * 全标 true 但根本认不出图（二选一 8/20 = 瞎猜水平；同账号 glm-5v-turbo 4/4、
   * 国内版同族模型各 8/8）。这个字段只说明「接口收得下图片」，不代表看得懂。
   * 要判断图片能力只能实测，故模型页不展示这一列。
   */
  supports_images?: boolean
  supports_reasoning?: boolean
  supports_tool_call?: boolean
  max_allowed_size?: number
  reasoning_effort?: string
  reasoning_summary?: string

  /**
   * 命中的上游促销（限时免费 / 折扣），按域各一份，各按优先级降序。
   *
   * 促销是分域下发的（同一个模型两域挂的活动可能完全不同），故不能合并成一份
   * ——否则总会有一个 tab 显示错域的优惠。两者都缺省 = 该域无活动（或老网关未拆）。
   */
  promotions_cn?: ModelPromotion[] | null
  promotions_global?: ModelPromotion[] | null

  /**
   * 该模型可用的账号域（裸名目录专用）。
   *
   * 统一调度后 /v1/models 只输出裸名，同一模型若两域都有就只出现一次，靠这个字段
   * 区分「仅国内」「仅国际」「两域都有」。缺省（undefined）= 老网关未透出该字段。
   */
  realms?: string[]
}

/** 促销的每日时段（如夜间折扣）。 */
export interface PromoWindow {
  /** "HH:MM" */
  start: string
  /** "HH:MM"；早于 start 表示跨零点（如 23:00→08:00） */
  end: string
}

/** 上游 /v3/config 下发的模型促销条目。 */
export interface ModelPromotion {
  id: string
  /** 生效的裸模型名（无域前缀），与 Model.id 去掉 realm 前缀后对齐。 */
  model_ids: string[] | null
  enabled: boolean
  /** 促销类型原文，如 "limited_free" / "off_peak"。 */
  kind?: string
  priority?: number
  badge_label?: string
  badge_color?: string
  /** 上游是否下发了 discount 块。false = 只挂徽标的条目（factor 无意义，别当免费）。 */
  has_discount?: boolean
  /** 折扣系数：0 = 免费，0.5 = 五折，1 = 无折扣。
   *  仅在 has_discount 为真时下发 —— 没有 discount 块的条目这个字段是**缺席**的，
   *  不是 0（后端用指针 + omitempty 保证），所以别用 `factor ?? 0`。 */
  factor?: number
  discounted_credits?: string
  /** 日期区间型促销的起止（上游原文，形如 "2026-09-25 23:59:59"）。 */
  valid_from?: string
  valid_until?: string
  /** 每日时段型促销的窗口。 */
  daily?: PromoWindow[] | null
  timezone?: string
  /** 上游 hover 文案（中文）。 */
  text?: string
}

export interface ModelsResponse {
  data: Model[] | null
  count: number
  /** 按域记录促销拉取失败原因（best-effort，失败不影响模型列表）。 */
  promo_errors?: Record<string, string> | null
}

export interface SessionInfo {
  authenticated: boolean
  username: string
  read_only: boolean
  dangerous_ops: boolean
  using_default_password: boolean
  gateway_url: string
  /** 服务端是否支持网页改密码（配置了 credentials_file）。 */
  password_changeable?: boolean
}

export interface ConfigMeta {
  path: string
  exists: boolean
  size: number
  mod_time?: string
  backup_path?: string
  backup_at?: string
  parse_error?: string
  is_valid_json: boolean
  restart_note: string
}

export interface ConfigResponse {
  config: Record<string, unknown>
  meta: ConfigMeta
}

/** 单条模型别名：对外名 → 各域真实上游模型名（某一域可为空 = 该域无此模型）。 */
export interface AliasEntry {
  name: string
  cn: string
  global: string
}

export interface AliasMeta {
  path: string
  exists: boolean
  size: number
  mod_time?: string
  backup_path?: string
  backup_at?: string
  parse_error?: string
  /** 提示语：与 config.json 不同，本文件热生效（无需重启）。 */
  restart_note: string
}

export interface AliasesResponse {
  aliases: AliasEntry[] | null
  meta: AliasMeta
}

export interface ContainerInfo {
  name: string
  available: boolean
  exists: boolean
  running: boolean
  status: string
  health: string
  image: string
  started_at?: string
  error?: string
  disabled: boolean
}

export interface SystemInfo {
  version: string
  started_at: string
  uptime_sec: number
  read_only: boolean
  dangerous_ops: boolean
  auth_dir: string
  config_file: string
  gateway_url: string
  using_default_password: boolean
  docker_available: boolean
  container: ContainerInfo
  gateway_health?: Health
  gateway_health_error?: string
}

export interface ChatResult {
  content: string
  reasoning_content?: string
  model: string
  finish_reason?: string
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  raw?: string
}

export interface ChatMessage {
  role: 'system' | 'user' | 'assistant'
  content: string
}

export interface AccountProfile {
  account: Account
  credits?: Credits
  credits_error?: string
  buddy?: { id: number; name: string } | null
  buddy_error?: string
  travel?: { state: string; daily_limit_reached: boolean; record_id: number; reward_credit: number }
  travel_error?: string
  upstream_error?: string
}

/** SSE 流式聊天的增量帧。 */
export interface ChatDelta {
  content?: string
  reasoning?: string
  done?: boolean
  error?: string
  usage?: { prompt_tokens: number; completion_tokens: number; total_tokens: number }
  elapsed_ms?: number
  ttfb_ms?: number
}

/**
 * 单个域的统计明细（对应网关 models[].realms[]）。
 *
 * 不含 model/bare/realms/last_seen：那些是跨域的标识与记账字段，属于父条目。
 */
export interface RealmStat {
  /**
   * `'cn'` 国内版 / `'global'` 国际版；
   * `''` = 请求未被路由到任何账号（选号失败 503、模型名解析不出），独立一组。
   */
  realm: '' | 'cn' | 'global'
  requests: number
  success: number
  failed: number
  streaming: number
  /** 平均首字延迟（毫秒） */
  avg_ttfb_ms: number
  /** 平均端到端耗时（毫秒） */
  avg_latency_ms: number
  /** 生成吞吐（输出 token / 秒） */
  tokens_per_sec: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cache_hit_tokens: number
  cache_miss_tokens: number
  cache_write_tokens: number
  /** 缓存命中率 0~1 */
  cache_hit_rate: number
  credit: number
  credit_per_req: number
}

/**
 * 单个模型的派生统计（对应网关 /v1/stats 的 models[]）。
 *
 * 统一调度改造后一行 = 一个**裸名**：父条目是跨域合计，`realms` 是各域独立明细。
 * 因此这里的数值列不直接上表——表格按域分行渲染 `realms`（见 StatsPage），
 * 父条目的数值只在「无分域明细」的降级路径下才用。
 */
export interface ModelStat {
  model: string
  /**
   * 裸模型名（剥掉 cn:/global: 前缀）。统计按裸名成组，故用它做行标识；
   * 老网关不下发时回退用 model。
   */
  bare?: string
  /** 各域明细（cn → global → 未路由）。单域模型也带一条；整段缺席=老网关，退化为一行。 */
  realms?: RealmStat[] | null
  requests: number
  success: number
  failed: number
  streaming: number
  /** 平均首字延迟（毫秒） */
  avg_ttfb_ms: number
  /** 平均端到端耗时（毫秒） */
  avg_latency_ms: number
  /** 生成吞吐（输出 token / 秒） */
  tokens_per_sec: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cache_hit_tokens: number
  cache_miss_tokens: number
  cache_write_tokens: number
  /** 缓存命中率 0~1 */
  cache_hit_rate: number
  credit: number
  credit_per_req: number
  last_seen?: string
}

/** 时间序列上的一个数据点。 */
export interface RangePoint {
  key: string
  start: string
  end: string
  stats: ModelStat
  derived: ModelStat
}

/** 时间范围聚合结果。 */
export interface RangeResult {
  interval: 'hour' | 'day' | 'week'
  from: string
  to: string
  points: RangePoint[] | null
  total: ModelStat
  models: string[] | null
}

/** 网关 /v1/stats 响应。 */
export interface Stats {
  enabled: boolean
  message?: string
  since: string
  now: string
  uptime_sec: number
  total: ModelStat
  models: ModelStat[] | null
  /** 时间序列桶数（判断数据可回溯范围） */
  series_buckets?: number
  /** 时间维度查询结果（带时间参数时返回） */
  range?: RangeResult
}

/** /api/stats 的完整响应。 */
export interface StatsResponse {
  stats: Stats
}
