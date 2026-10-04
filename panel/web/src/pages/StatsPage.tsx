// StatsPage.tsx 请求统计：按模型分开统计首字延迟、吞吐、缓存命中、输入输出与扣费。
//
// 数据源是网关的 /v1/stats —— 网关是所有流量的必经点，因此这里看到的**包含**
// 绕过本面板的其他客户端（比如你自己的工具/脚本）的调用。
//
// 本 fork 的网关**只实现了 /v1/stats 的累计快照**，时间维度查询未实现，故
// 「时间趋势」卡片与其时间维度 state 目前整体注释掉（详见该处说明与 HOST-PATCHES.md 第 4 条）。
import { useCallback, useEffect, useMemo, useState } from 'react'
import { api, ApiError } from '../api'
import type { ModelStat, RealmStat, SessionInfo, StatsResponse } from '../types'
import { Alert, Empty, fmtDuration, fmtISO, fmtNum, Spinner } from '../ui'
import { realmLabel } from '../realm'
// 时间趋势启用时需一并恢复（见下方注释块）：
// import TrendChart, { type TrendMetric } from './TrendChart'

/** 数值格式化：大数用千分位，小数保留位数。 */
function fmtMs(v: number): string {
  if (!v) return '—'
  return v >= 1000 ? `${(v / 1000).toFixed(2)}s` : `${Math.round(v)}ms`
}
function fmtRate(v: number): string {
  return v ? v.toFixed(1) : '—'
}
function fmtPct(v: number): string {
  if (!v) return '0%'
  return `${(v * 100).toFixed(1)}%`
}
function fmtCredit(v: number): string {
  return v ? v.toFixed(4) : '0'
}
/** 大 token 数缩写（1.2M / 345.6K / 123）。 */
function fmtTok(v: number): string {
  if (!v) return '0'
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(2)}M`
  if (v >= 1_000) return `${(v / 1_000).toFixed(1)}K`
  return String(v)
}

/** 缓存命中率配色：越高越省（命中部分通常便宜得多）。 */
function hitTone(rate: number): string {
  if (rate >= 0.5) return 'text-ok'
  if (rate >= 0.1) return 'text-warn'
  return 'text-dim'
}

/**
 * 域的展示名。与 realm.ts 的 REALMS 同词（国内版/国际版），空域是「未被路由」——
 * 选号失败 503 或模型名解析不出，网关不猜归属，面板照实显示而不是硬塞进某一侧。
 */
function statRealmLabel(realm: RealmStat['realm']): string {
  return realm === '' ? '未路由' : realmLabel(realm)
}

/** 域徽章配色：国内蓝、国际绿、未路由灰（异常路径不该抢眼）。 */
function statRealmBadge(realm: RealmStat['realm']): string {
  if (realm === '') return 'badge-dim'
  return realm === 'cn' ? 'badge-accent' : 'badge-ok'
}

/**
 * 一行明细：父行（裸名合计）或某个域的明细行。
 *
 * 父行不单独渲染任何数值列——用户要的是「每个裸名下按域分两行」，父行的合计
 * 只用于排序与"哪些模型在跑"的判断。把父行数值也画出来会让每行出现一组
 * 无法归属到任何域的数字，正是这次改造要消除的混淆。
 */
function StatRow({ model }: { model: ModelStat }) {
  const bare = model.bare || model.model
  // 分域明细恒存在（网关每个条目至少一个域子条目），缺席只发生在老网关/手写载荷：
  // 那时退化为一行「未标注域」——宁可少一行，也不要按前缀猜测把数据塞进 cn 或 global，
  // 归属错了比没有更糟。realm=null 表示"这一行就是父条目自身"，与真的空域（未路由）区分开。
  const rows: { v: RealmStat; realm: RealmStat['realm'] | null }[] =
    model.realms && model.realms.length > 0
      ? model.realms.map((r) => ({ v: r, realm: r.realm }))
      : [{ v: { ...model, realm: '' }, realm: null }]

  return (
    <>
      {rows.map(({ v, realm }, i) => {
        const first = i === 0
        // 首行承载跨整组的单元格（裸名），行数 n → rowSpan=n。
        // 组的最后一行标 stat-group-end：底边由它画，中间行不画（见 styles.css）。
        const last = i === rows.length - 1
        const groupCls = [
          first ? 'stat-group-start' : 'stat-group-mid',
          last ? 'stat-group-end' : '',
        ]
          .filter(Boolean)
          .join(' ')
        return (
          <tr key={`${model.model}:${realm ?? '__self'}`} className={groupCls}>
            {first && (
              <td rowSpan={rows.length} className="stat-group">
                <div className="mono" style={{ fontSize: 12.5 }}>
                  {bare}
                </div>
                <div className="text-faint" style={{ fontSize: 11 }}>
                  合计 {fmtNum(model.requests)} 次
                </div>
                {model.last_seen && (
                  <div className="text-faint" style={{ fontSize: 11 }}>
                    {fmtISO(model.last_seen)}
                  </div>
                )}
              </td>
            )}
            <td>
              <span className={`badge ${realm === null ? 'badge-dim' : statRealmBadge(realm)}`}>
                {realm === null ? '未标注域' : statRealmLabel(realm)}
              </span>
            </td>
            <td className="num">
              {fmtNum(v.requests)}
              {v.streaming > 0 && (
                <div className="text-faint" style={{ fontSize: 11 }}>
                  流式 {v.streaming}
                </div>
              )}
              {v.failed > 0 && (
                <div className="text-danger" style={{ fontSize: 11 }}>
                  失败 {v.failed}
                </div>
              )}
            </td>
            <td className="num">{fmtMs(v.avg_ttfb_ms)}</td>
            <td className="num">
              {fmtRate(v.tokens_per_sec)}
              <div className="text-faint" style={{ fontSize: 11 }}>
                tok/s
              </div>
            </td>
            <td className="num" title={fmtNum(v.prompt_tokens)}>
              {fmtTok(v.prompt_tokens)}
            </td>
            <td className="num" title={fmtNum(v.completion_tokens)}>
              {fmtTok(v.completion_tokens)}
            </td>
            <td className="num">
              <span className={hitTone(v.cache_hit_rate)}>{fmtPct(v.cache_hit_rate)}</span>
              <div
                className="text-faint"
                style={{ fontSize: 11 }}
                title={`命中 ${fmtNum(v.cache_hit_tokens)} / 未命中 ${fmtNum(v.cache_miss_tokens)}`}
              >
                {fmtTok(v.cache_hit_tokens)} hit
              </div>
            </td>
            <td className="num">
              {fmtCredit(v.credit)}
              <div className="text-faint" style={{ fontSize: 11 }}>
                {fmtCredit(v.credit_per_req)}/次
              </div>
            </td>
          </tr>
        )
      })}
    </>
  )
}

export default function StatsPage({ session }: { session: SessionInfo }) {
  const [resp, setResp] = useState<StatsResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [autoRefresh, setAutoRefresh] = useState(true)
  const [sortKey, setSortKey] = useState<keyof ModelStat>('requests')
  const [resetting, setResetting] = useState(false)

  // ── 时间维度：网关尚未实现，暂时整体注释 ──
  // 这组 state 与下方「时间趋势」卡片是一体的。保留注释而非删除，是为了网关补齐
  // 时间序列后能整块恢复（恢复步骤见卡片处说明）。
  //
  // 快捷区间：today / yesterday / 7d / 30d / 90d / all / custom
  // const [range, setRange] = useState('all')
  // const [interval, setInterval] = useState<'hour' | 'day' | 'week'>('day')
  // const [metric, setMetric] = useState<TrendMetric>('requests')
  // // 自定义区间（range=custom 时生效）
  // const [customFrom, setCustomFrom] = useState('')
  // const [customTo, setCustomTo] = useState('')
  // // 模型筛选（空 = 全部）
  // //
  // // 注意：这个筛选**即使恢复渲染也不生效** —— 它只是把 model 参数发给网关，而网关的
  // // /v1/stats 不解析该参数，「按模型明细」表格用的也是未过滤的 resp.stats.models。
  // const [modelFilter, setModelFilter] = useState('')

  const stats = resp?.stats ?? null

  const load = useCallback(async (silent = false) => {
    if (!silent) setLoading(true)
    try {
      // 时间维度参数已随「时间趋势」一并停发（网关不解析它们）。
      // 恢复趋势卡片时，把 range/from/to/interval/model 加回来。
      const s = await api.stats()
      setResp(s)
      setError(null)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '加载统计失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // 自动刷新：统计是累计值，10 秒一次足够看出趋势。
  useEffect(() => {
    if (!autoRefresh) return
    const timer = window.setInterval(() => {
      void load(true)
    }, 10_000)
    return () => clearInterval(timer)
  }, [autoRefresh, load])

  const doReset = async () => {
    setResetting(true)
    setNotice(null)
    try {
      const r = await api.resetStats()
      setNotice(r.message || '统计已重置')
      await load(true)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '重置失败')
    } finally {
      setResetting(false)
    }
  }

  // 排序后的模型列表（默认按请求数降序，热点模型在最上面）。
  //
  // 排序键取**父条目**（裸名合计），而不是某个域：表格按裸名成组，若按单域排序，
  // 一个模型的上下位置会随哪个域更忙而跳变，组内两行的相对顺序也不再稳定。
  const models = useMemo(() => {
    const list = [...(stats?.models ?? [])]
    list.sort((a, b) => {
      const av = a[sortKey]
      const bv = b[sortKey]
      if (typeof av === 'number' && typeof bv === 'number') return bv - av
      return String(av).localeCompare(String(bv))
    })
    return list
  }, [stats, sortKey])

  if (loading && !stats) return <Spinner label="正在加载统计…" />

  // 统计未启用（服务端 metrics_enabled=false）。
  if (stats && !stats.enabled) {
    return (
      <>
        <div className="page-head">
          <div>
            <h1>请求统计</h1>
            <p>按模型聚合的首字延迟、吞吐、缓存命中与扣费</p>
          </div>
        </div>
        <Alert kind="warn">
          <strong>网关未启用统计。</strong>
          <div style={{ marginTop: 4 }}>{stats.message || '请在网关配置中设置 server.metrics_enabled=true'}</div>
        </Alert>
      </>
    )
  }

  const t = stats?.total

  return (
    <>
      <div className="page-head">
        <div>
          <h1>请求统计</h1>
          <p>
            统计<strong>所有</strong>经过网关的请求（含绕过本面板的客户端），按模型分开
            {stats && <span className="text-faint"> · 已运行 {fmtDuration(stats.uptime_sec)}</span>}
          </p>
        </div>
        <div className="page-actions">
          <label className="checkbox">
            <input type="checkbox" checked={autoRefresh} onChange={(e) => setAutoRefresh(e.target.checked)} />
            自动刷新（10s）
          </label>
          <button className="btn" onClick={() => void load()} disabled={loading}>
            {loading ? <Spinner /> : '🔄'} 刷新
          </button>
          <button
            className="btn btn-danger"
            onClick={() => void doReset()}
            disabled={resetting || session.read_only}
            title={session.read_only ? '只读模式' : '清空累计统计，便于观察之后的增量'}
          >
            {resetting ? <Spinner /> : '🧹'} 重置统计
          </button>
        </div>
      </div>

      {notice && (
        <Alert kind="ok" onClose={() => setNotice(null)}>
          {notice}
        </Alert>
      )}
      {error && (
        <Alert kind="error" onClose={() => setError(null)}>
          {error}
        </Alert>
      )}

      {/* 汇总卡片 */}
      {t && (
        <div className="grid grid-stats" style={{ marginBottom: 16 }}>
          <Stat
            label="总请求"
            value={fmtNum(t.requests)}
            sub={`成功 ${fmtNum(t.success)}${t.failed ? ` · 失败 ${fmtNum(t.failed)}` : ''} · 流式 ${fmtNum(t.streaming)}`}
          />
          <Stat
            label="平均首字"
            value={fmtMs(t.avg_ttfb_ms)}
            sub={`平均耗时 ${fmtMs(t.avg_latency_ms)}`}
            tone={t.avg_ttfb_ms > 5000 ? 'warn' : undefined}
          />
          <Stat
            label="生成吞吐"
            value={t.tokens_per_sec ? `${fmtRate(t.tokens_per_sec)} tok/s` : '—'}
            sub="输出 token / 生成秒数"
          />
          <Stat
            label="输入 / 输出"
            value={`${fmtTok(t.prompt_tokens)} / ${fmtTok(t.completion_tokens)}`}
            sub={`合计 ${fmtTok(t.total_tokens)} token`}
          />
          <Stat
            label="缓存命中率"
            value={fmtPct(t.cache_hit_rate)}
            sub={`命中 ${fmtTok(t.cache_hit_tokens)} · 未命中 ${fmtTok(t.cache_miss_tokens)}`}
            tone={t.cache_hit_rate >= 0.5 ? 'ok' : undefined}
          />
          <Stat label="累计扣费（积分）" value={fmtCredit(t.credit)} sub={`平均每请求 ${fmtCredit(t.credit_per_req)} 积分`} />
        </div>
      )}

      {/* 时间趋势：网关未实现时间维度查询，整块隐藏（含卡片本身，不留占位） */}
      {/*
        本模块依赖网关 /v1/stats 的**时间维度**能力，而本 fork 的网关尚未实现：
        internal/server/metrics.go 的 stats handler 只调 MetricsSnapshotOf()，既不解析
        range/from/to/interval/model 参数，MetricsSnapshot 结构体里也没有 Range 字段，
        故响应恒无 range 键 → 面板恒走「正在加载趋势数据…」降级分支，趋势图永远空白。
        （注意区分：/v1/stats 的**累计快照**是已实现的，本页其余部分正常。）

        另有两条与事实不符的旧文案一并去掉：`series_buckets` 网关从未赋值（那句
        「可回溯 N 个时间桶」永远不显示），且它宣称的「按小时落盘、保留 30 天、重启不清零」
        与实现相反 —— 统计纯内存累加，进程重启即清零。

        网关补齐后，取消下面整块注释、并恢复上方 TrendChart import 与时间维度 state 即可。

      <div className="card">
        <div className="card-head">
          <h2>时间趋势</h2>
          <span className="hint">
            {stats?.series_buckets !== undefined && `可回溯 ${stats.series_buckets} 个时间桶`}
          </span>
        </div>

        <div className="row" style={{ marginBottom: 6 }}>
          <div className="field" style={{ flex: '0 0 150px', minWidth: 130 }}>
            <label>时间范围</label>
            <select value={range} onChange={(e) => setRange(e.target.value)}>
              <option value="today">今天</option>
              <option value="yesterday">昨天</option>
              <option value="7d">最近 7 天</option>
              <option value="30d">最近 30 天</option>
              <option value="90d">最近 90 天</option>
              <option value="all">全部</option>
              <option value="custom">自定义…</option>
            </select>
          </div>
          <div className="field" style={{ flex: '0 0 130px', minWidth: 110 }}>
            <label>聚合粒度</label>
            <select value={interval} onChange={(e) => setInterval(e.target.value as 'hour' | 'day' | 'week')}>
              <option value="hour">按小时</option>
              <option value="day">按天</option>
              <option value="week">按周</option>
            </select>
          </div>
          <div className="field" style={{ flex: '0 0 170px', minWidth: 140 }}>
            <label>模型筛选</label>
            <select value={modelFilter} onChange={(e) => setModelFilter(e.target.value)}>
              <option value="">全部模型</option>
              {(resp?.stats.models ?? []).map((m) => (
                <option key={m.model} value={m.model}>
                  {m.model}
                </option>
              ))}
            </select>
          </div>
          {range === 'custom' && (
            <>
              <div className="field" style={{ flex: '0 0 190px', minWidth: 160 }}>
                <label>开始时间</label>
                <input
                  type="datetime-local"
                  value={customFrom}
                  onChange={(e) => setCustomFrom(e.target.value)}
                />
              </div>
              <div className="field" style={{ flex: '0 0 190px', minWidth: 160 }}>
                <label>结束时间</label>
                <input type="datetime-local" value={customTo} onChange={(e) => setCustomTo(e.target.value)} />
              </div>
            </>
          )}
        </div>

        {resp?.stats.range ? (
          <>
            <div className="grid grid-stats" style={{ margin: '10px 0 14px' }}>
              <div className="stat">
                <div className="stat-label">区间请求数</div>
                <div className="stat-value small">{fmtNum(resp.stats.range.total?.requests ?? 0)}</div>
                <div className="stat-sub">
                  成功 {fmtNum(resp.stats.range.total?.success ?? 0)}
                  {(resp.stats.range.total?.failed ?? 0) > 0 && ` · 失败 ${fmtNum(resp.stats.range.total?.failed ?? 0)}`}
                </div>
              </div>
              <div className="stat">
                <div className="stat-label">区间输出 token</div>
                <div className="stat-value small">{fmtTok(resp.stats.range.total?.completion_tokens ?? 0)}</div>
                <div className="stat-sub">输入 {fmtTok(resp.stats.range.total?.prompt_tokens ?? 0)}</div>
              </div>
              <div className="stat">
                <div className="stat-label">区间平均首字</div>
                <div className="stat-value small">{fmtMs(resp.stats.range.total?.avg_ttfb_ms ?? 0)}</div>
                <div className="stat-sub">平均耗时 {fmtMs(resp.stats.range.total?.avg_latency_ms ?? 0)}</div>
              </div>
              <div className="stat">
                <div className="stat-label">区间缓存命中率</div>
                <div className="stat-value small">{fmtPct(resp.stats.range.total?.cache_hit_rate ?? 0)}</div>
                <div className="stat-sub">吞吐 {fmtRate(resp.stats.range.total?.tokens_per_sec ?? 0)} tok/s</div>
              </div>
            </div>

            <TrendChart
              points={resp.stats.range.points ?? []}
              interval={resp.stats.range.interval}
              metric={metric}
              onMetricChange={setMetric}
            />

            {resp.stats.range.from && (
              <div className="desc" style={{ marginTop: 10 }}>
                实际区间：{fmtISO(resp.stats.range.from)} → {fmtISO(resp.stats.range.to)}
                {' · '}
                粒度：
                {resp.stats.range.interval === 'hour' ? '小时' : resp.stats.range.interval === 'day' ? '天' : '周'}
                {(resp.stats.range.points?.length ?? 0) > 0 &&
                  ` · ${resp.stats.range.points?.length} 个数据点`}
              </div>
            )}
          </>
        ) : (
          <Empty>
            正在加载趋势数据…
            <div style={{ marginTop: 6, fontSize: 12 }}>
              时间趋势从启用统计后开始累积（网关重启不清零，但重新部署前的历史不可回溯）。
            </div>
          </Empty>
        )}
      </div>
      */}

      {/* 按模型明细 */}
      <div className="card">
        <div className="card-head">
          <h2>按模型明细</h2>
          <div className="page-actions">
            <span className="hint">排序（按裸名合计）</span>
            <select
              value={String(sortKey)}
              onChange={(e) => setSortKey(e.target.value as keyof ModelStat)}
              style={{ width: 150 }}
            >
              <option value="requests">请求数</option>
              <option value="avg_ttfb_ms">首字延迟</option>
              <option value="tokens_per_sec">吞吐</option>
              <option value="cache_hit_rate">缓存命中率</option>
              <option value="credit">扣费</option>
              <option value="completion_tokens">输出 token</option>
            </select>
          </div>
        </div>

        {models.length === 0 ? (
          <Empty>
            还没有统计数据。
            <div style={{ marginTop: 6, fontSize: 12 }}>
              向网关发一次请求（可用「聊天测试」页）后即可看到。
            </div>
          </Empty>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>模型</th>
                  <th>域</th>
                  <th className="num">请求</th>
                  <th className="num">首字</th>
                  <th className="num">吞吐</th>
                  <th className="num">输入</th>
                  <th className="num">输出</th>
                  <th className="num">缓存命中</th>
                  <th className="num">扣费</th>
                </tr>
              </thead>
              <tbody>
                {models.map((m) => (
                  <StatRow key={m.model} model={m} />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="card">
        <div className="card-head">
          <h2>指标说明</h2>
        </div>
        <dl className="kv">
          <dt>按域分行</dt>
          <dd>
            统一调度后一个裸模型名会在<strong>国内版</strong>与<strong>国际版</strong>账号之间调度，
            两域的单价、限免活动、上下文长度都不同，混在一起只能看到加权平均。
            故每行 = 一个裸名，其下按<strong>实际承接请求的账号域</strong>分行统计（不是按请求里的
            前缀——前缀只说"允许打哪"，实际落在哪由选号决定）。
            「未路由」是选号失败（503）或模型名解析不出的请求，网关不猜归属。
          </dd>
          <dt>首字延迟</dt>
          <dd>请求发出到收到第一个 token 的时间（TTFB）。只对流式请求有意义，非流式显示为 —。</dd>
          <dt>吞吐</dt>
          <dd>输出 token ÷ 生成秒数（已剔除首字等待），反映模型的真实出字速度。</dd>
          <dt>缓存命中</dt>
          <dd>
            上游 prompt cache 的命中比例。命中的输入 token 计费远低于未命中，
            所以这个数字直接关系到实际花费 —— 同一会话反复追问同一长上下文时命中率会很高。
          </dd>
          <dt>扣费</dt>
          <dd>
            上游返回的 credit 累计（非估算）。<strong>单位是账号积分</strong>（套餐按 500/1500/100 积分计），
            与「官方应付」的<strong>元</strong>不是同一量纲，故两者只并列展示、不做相减。
          </dd>
          <dt>模型列</dt>
          <dd>
            显示<strong>裸名</strong>（不含 cn:/global: 前缀）与整个模型的合计请求数、
            最近一次请求时间。这些是跨域的量，故用跨行的单元格承载，不跟着每个域重复。
          </dd>
          <dt>统计范围</dt>
          <dd>
            网关是所有流量的必经点，因此这里<strong>包含其他客户端</strong>（脚本、第三方工具）的调用，
            不限于本面板发起的请求。统计为<strong>进程内累计</strong>，网关重启即清零。
          </dd>
          <dt>统计起点</dt>
          <dd>{stats ? fmtISO(stats.since) : '—'}</dd>
        </dl>
      </div>
    </>
  )
}

function Stat({
  label,
  value,
  sub,
  tone,
}: {
  label: string
  value: string
  sub?: string
  tone?: 'ok' | 'warn' | 'danger'
}) {
  const cls = tone === 'ok' ? 'text-ok' : tone === 'warn' ? 'text-warn' : tone === 'danger' ? 'text-danger' : ''
  return (
    <div className="stat">
      <div className="stat-label">{label}</div>
      <div className={`stat-value small ${cls}`}>{value}</div>
      {sub && <div className="stat-sub">{sub}</div>}
    </div>
  )
}
