// Models.tsx 模型与倍率：按域（国际版 / 国内版）列出上游支持的模型、积分倍率、
// 能力旗标与限时优惠，并汇总该域账号的积分到期情况。
//
// 数据源三处，都是既有接口，未新增网关端点：
//   · 网关 /v1/models（经 /api/models 代理）→ 模型、倍率、能力、描述
//   · 上游 /v3/config 的 modelPromotions（GUI 后端直连取，见 ops.PromotionsForRealm）
//     → 「限时免费至 9-25」「夜间五折」这类活动信息；网关不解析这一段
//   · /api/accounts → 账号积分；其中「到期」来自 GUI 侧主动查询积分包
//     响应里的 CycleEndTime，没刷过积分时为空（页面会提示点刷新）。
import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api'
import type { Model, ModelPromotion } from '../types'
import { Alert, Badge, Empty, Spinner, fmtNum, upstreamSec } from '../ui'
import { REALMS, bareID, modelRealm, type Realm } from '../realm'

type SortKey = 'rate-asc' | 'rate-desc' | 'promo' | 'name' | 'context'

/** 从倍率原文取数值；无倍率（上游未标）返回 null。 */
function parseRate(s?: string): number | null {
  if (!s) return null
  const m = /x\s*([0-9]*\.?[0-9]+)/i.exec(s)
  if (!m) return null
  const n = Number(m[1])
  return Number.isFinite(n) ? n : null
}

/** 倍率展示文本：保留上游原文精度（x0.00 不能压成 x0），去掉冗余的 "credits" 后缀。
 *  解析不出数值时原样回显，不假装成没有。 */
function rateText(s?: string): string {
  if (!s) return '—'
  const m = /x\s*([0-9]*\.?[0-9]+)/i.exec(s)
  return m ? `x${m[1]}` : s.trim()
}

/** 描述里上游会重复写一遍倍率（"[x0.34 credit] 响应快…"），本页已有独立倍率列，去掉避免重复。 */
function cleanDesc(s?: string): string {
  return (s || '').replace(/^\[x[\d.]+\s*credits?\]\s*/i, '').trim()
}

/** Unix 秒 → "MM-DD"（促销日期只关心月日，年份同一年时是噪音）。 */
function md(sec: number): string {
  const d = new Date(sec * 1000)
  return `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

/** 折扣系数的中文说法：0 = 免费，0.5 = 五折，0.8 = 八折，1 = 原价。 */
function factorText(f: number): string {
  if (f === 0) return '免费'
  if (f >= 1) return '原价'
  const n = Math.round(f * 100) / 10 // 0.5 → 5（五折），0.85 → 8.5
  return `${n}折`
}

/** 判断是否含中文：上游国际版给英文徽标（"Free now"），中文版给中文（"限时免费"）。 */
const CJK = /[\u4e00-\u9fff]/

interface PromoView {
  /** 列表里显示的短文案，如「限时免费 至 09-25」「夜间折扣 23:00–次日 7:50 5折」。 */
  text: string
  /** hover 全文：上游原文 + 精确起止时间。 */
  title: string
  cls: string
  /** 日期区间型促销的结束时间（Unix 秒）；时段型没有，用于排序与高亮。 */
  until?: number
}

/** 把上游促销条目翻成一句人话。
 *
 *  上游有三种形态，都要分别对待：
 *   1. 日期区间型 —— validUntil 有值（「限时免费至 9-25」）
 *   2. 每日时段型 —— daily 有值（「每晚 23:00–次日 7:50 五折」）
 *   3. 只挂徽标型 —— 没有 discount 块（上游用它做时段内/外的两张脸），
 *      factor 缺省成 0，**不能当免费报**
 *
 *  已过期的返回 null（后端已过滤一道，这里再兜一次底）。 */
function promoInfo(p: ModelPromotion): PromoView | null {
  const from = upstreamSec(p.valid_from)
  const until = upstreamSec(p.valid_until)
  if (until !== undefined && until * 1000 < Date.now()) return null

  const raw = (p.badge_label || '').trim()
  const label = CJK.test(raw) ? raw : '' // 英文徽标不直接上屏，避免中英混排
  // factor 缺席 = 上游没给 discount 块，此时按「无折扣」处理（缺省 1 = 原价）。
  // 不能缺省成 0 —— 那会把这批只挂徽标的条目读成「免费」。
  const factor = p.factor ?? 1
  const discounted = !!p.has_discount && factor < 1
  const pct = discounted ? factorText(factor) : ''

  // 时段：跨零点时补「次日」（end <= start 即跨零点）。
  // 上游时段不补零（"7:50"），与 "23:00" 并列时长短不齐，统一补成 HH:MM。
  const hhmm = (t: string) => {
    const m = /^(\d{1,2}):(\d{2})$/.exec(t.trim())
    return m ? `${m[1].padStart(2, '0')}:${m[2]}` : t
  }
  const windows = (p.daily ?? []).map((d) => {
    const allDay = d.start === '0:00' && (d.end === '23:59' || d.end === '24:00')
    return allDay ? '' : `${hhmm(d.start)}–${d.end <= d.start ? '次日 ' : ''}${hhmm(d.end)}`
  }).filter(Boolean)

  // 时段只在「真有折扣」时才上屏：上游还给每个时段配一张反面的徽标条目
  // （如 glm-5.2 的 07:50–23:00 白天条目、hy4 的 08:00–23:00），挂的是同一个
  // 「夜间折扣/夜间免费」标签。把窗口贴上去会读成「白天也夜间免费」，反而误导；
  // 这类条目只显示上游自己的标签，窗口留在 hover 里。
  let text: string
  if (windows.length > 0 && discounted) {
    text = [label || '时段优惠', windows.join('、'), pct].filter(Boolean).join(' ')
  } else if (windows.length > 0) {
    text = label || '活动'
  } else if (discounted && until !== undefined) {
    text = `${label || `限时${pct}`} 至 ${md(until)}`
  } else {
    text = [label || (discounted ? `限时${pct}` : '活动'), pct].filter(Boolean).join(' ')
  }

  // hover 里给全：上游原文徽标（含英文）+ 活动文案 + 精确起止。
  const detail: string[] = [text]
  if (raw && raw !== text) detail.push(`上游徽标：${raw}`)
  if (from !== undefined) detail.push(`${new Date(from * 1000).toLocaleString('zh-CN', { hour12: false })} 起`)
  if (until !== undefined) detail.push(`${new Date(until * 1000).toLocaleString('zh-CN', { hour12: false })} 止`)
  if (p.timezone) detail.push(`时区 ${p.timezone}`)
  if (p.text) detail.push(p.text)

  return {
    text,
    title: detail.join('\n'),
    cls: factor === 0 && discounted ? 'badge-ok' : discounted ? 'badge-accent' : 'badge-dim',
    until,
  }
}

/** 一个模型可能同时命中多条促销（如「限时免费」+「夜间折扣」，或折扣 + 同时段徽标）。
 *  后端已按 priority 降序，取第一条上屏，其余进 hover，避免一行塞满徽标。 */
function modelPromo(m: Model): PromoView | null {
  const views = (m.promotions ?? []).map(promoInfo).filter((v): v is PromoView => v !== null)
  if (views.length === 0) return null
  const v = views[0]
  return views.length > 1 ? { ...v, title: views.map((x) => x.text).join('\n') } : v
}

export default function Models() {
  const [models, setModels] = useState<Model[]>([])
  const [promoErrors, setPromoErrors] = useState<Record<string, string>>({})
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [realm, setRealm] = useState<Realm>('global')
  const [q, setQ] = useState('')
  const [sort, setSort] = useState<SortKey>('rate-asc')
  const [onlyFree, setOnlyFree] = useState(false)
  const [onlyPromo, setOnlyPromo] = useState(false)
  const [refreshedAt, setRefreshedAt] = useState<Date | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const m = await api.models()
      setModels(m.data ?? [])
      setPromoErrors(m.promo_errors ?? {})
      setError(null)
      setRefreshedAt(new Date())
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载模型列表失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // 按域统计，供 tab 显示数量。
  const countByRealm = useMemo(() => {
    const out: Record<Realm, number> = { global: 0, cn: 0 }
    for (const m of models) out[modelRealm(m.id)]++
    return out
  }, [models])

  // 首次加载后自动切到「有模型的域」。
  //
  // 默认 tab 原先是硬编码 global，但只有国内版账号时该 tab 恒为空 ——
  // 用户打开页面看到的是一片"没有匹配的模型"，像是功能坏了。
  // 只在用户**尚未手动切过** tab 时才自动选（之后尊重用户选择）。
  const [realmTouched, setRealmTouched] = useState(false)
  useEffect(() => {
    if (realmTouched || models.length === 0) return
    if (countByRealm[realm] > 0) return // 当前 tab 有内容，不必动
    const other: Realm = realm === 'global' ? 'cn' : 'global'
    if (countByRealm[other] > 0) setRealm(other)
  }, [models, countByRealm, realm, realmTouched])

  // 每个域里有优惠的模型数（含时段型），tab 上提示用。
  const promoByRealm = useMemo(() => {
    const out: Record<Realm, number> = { global: 0, cn: 0 }
    for (const m of models) if (modelPromo(m)) out[modelRealm(m.id)]++
    return out
  }, [models])

  // 过滤 + 排序。
  const rows = useMemo(() => {
    const kw = q.trim().toLowerCase()
    let list = models.filter((m) => modelRealm(m.id) === realm)
    if (kw) {
      list = list.filter((m) =>
        `${m.id} ${m.name ?? ''} ${m.description ?? ''} ${m.vendor ?? ''} ${(m.tags ?? []).join(' ')}`
          .toLowerCase()
          .includes(kw),
      )
    }
    if (onlyFree) list = list.filter((m) => parseRate(m.credits) === 0)
    if (onlyPromo) list = list.filter((m) => modelPromo(m) !== null)

    const rate = (m: Model) => parseRate(m.credits)
    return [...list].sort((a, b) => {
      switch (sort) {
        case 'name':
          return (a.name || a.id).localeCompare(b.name || b.id, 'zh-CN')
        case 'context':
          return (b.context_length || 0) - (a.context_length || 0)
        case 'promo': {
          // 按促销结束时间升序（快过期的排前面），无日期/无促销的排最后。
          const ua = modelPromo(a)?.until
          const ub = modelPromo(b)?.until
          if (ua === undefined) return ub === undefined ? 0 : 1
          if (ub === undefined) return -1
          return ua - ub
        }
        case 'rate-desc': {
          const ra = rate(a)
          const rb = rate(b)
          // 无倍率的恒排最后，避免被当成 0 混进最便宜的一档。
          if (ra === null) return rb === null ? 0 : 1
          if (rb === null) return -1
          return rb - ra
        }
        case 'rate-asc':
        default: {
          const ra = rate(a)
          const rb = rate(b)
          if (ra === null) return rb === null ? 0 : 1
          if (rb === null) return -1
          return ra - rb
        }
      }
    })
  }, [models, realm, q, sort, onlyFree, onlyPromo])

  const freeCount = useMemo(
    () => rows.filter((m) => parseRate(m.credits) === 0).length,
    [rows],
  )

  const realmMeta = REALMS.find((r) => r.key === realm)!

  return (
    <>
      <div className="page-head">
        <div>
          <h1>模型与倍率</h1>
          <p>
            上游各域支持的模型、积分倍率、能力与限时优惠
            {refreshedAt && (
              <span className="text-faint">
                {' '}
                · 更新于 {refreshedAt.toLocaleTimeString('zh-CN', { hour12: false })}
              </span>
            )}
          </p>
        </div>
        <div className="page-actions">
          <button className="btn" onClick={() => void load()} disabled={loading}>
            {loading ? <Spinner /> : '🔄'} 重新加载
          </button>
        </div>
      </div>

      {error && <Alert kind="error">{error}</Alert>}

      {Object.entries(promoErrors).map(([r, msg]) => (
        <Alert key={r} kind="warn">
          优惠信息拉取失败（{r === 'global' ? '国际版' : '国内版'}）：{msg} —— 模型列表不受影响，
          只是「优惠」列为空。
        </Alert>
      ))}

      <div className="tabs">
        {REALMS.map((r) => (
          <button
            key={r.key}
            className={`tab ${realm === r.key ? 'active' : ''}`}
            onClick={() => {
              setRealm(r.key)
              setRealmTouched(true) // 用户手动选过，之后不再自动切换
            }}
          >
            {r.label}
            <span className="text-faint"> · {countByRealm[r.key]}</span>
          </button>
        ))}
      </div>

      <div className="card">
        <div className="card-head">
          <strong>
            {realmMeta.label}模型（{rows.length}
            {rows.length !== countByRealm[realm] ? ` / 共 ${countByRealm[realm]}` : ''}）
          </strong>
          <span className="text-faint" style={{ fontSize: 12 }}>
            上游 <span className="mono">{realmMeta.upstream}</span>
            {freeCount > 0 && <> · 当前视图有 {freeCount} 个零倍率</>}
            {promoByRealm[realm] > 0 && <> · 全表 {promoByRealm[realm]} 个有优惠</>}
          </span>
        </div>

        <div className="row" style={{ marginBottom: 12 }}>
          <div>
            <input
              type="text"
              placeholder="搜索模型名 / id / 描述 / 厂商…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>
          <div className="shrink" style={{ minWidth: 160 }}>
            <select value={sort} onChange={(e) => setSort(e.target.value as SortKey)}>
              <option value="rate-asc">倍率从低到高</option>
              <option value="rate-desc">倍率从高到低</option>
              <option value="promo">优惠快到期优先</option>
              <option value="context">上下文从大到小</option>
              <option value="name">按名称</option>
            </select>
          </div>
          <label className="checkbox shrink" style={{ alignSelf: 'center' }}>
            <input type="checkbox" checked={onlyFree} onChange={(e) => setOnlyFree(e.target.checked)} />
            只看零倍率
          </label>
          <label className="checkbox shrink" style={{ alignSelf: 'center' }}>
            <input
              type="checkbox"
              checked={onlyPromo}
              onChange={(e) => setOnlyPromo(e.target.checked)}
            />
            只看有优惠
          </label>
        </div>

        {loading && models.length === 0 ? (
          <Spinner label="正在拉取模型列表…" />
        ) : rows.length === 0 ? (
          <Empty>
            没有匹配的模型。
            {q || onlyFree || onlyPromo ? '（试试清掉搜索或过滤条件）' : ''}
          </Empty>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>模型</th>
                  <th className="num">倍率</th>
                  <th>优惠</th>
                  <th className="num">上下文</th>
                  <th className="num">最大输出</th>
                  <th>能力</th>
                  <th>说明</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((m) => {
                  const rate = parseRate(m.credits)
                  const bare = bareID(m.id)
                  const promo = modelPromo(m)
                  const soon =
                    promo?.until !== undefined && promo.until - Date.now() / 1000 < 3 * 86400
                  return (
                    <tr key={m.id}>
                      <td>
                        <div>
                          {m.name || bare}
                          {m.is_default && (
                            <span className="badge badge-accent" style={{ marginLeft: 6 }}>
                              默认
                            </span>
                          )}
                        </div>
                        <div className="mono text-faint" style={{ fontSize: 11 }}>
                          {m.id}
                        </div>
                      </td>
                      <td className="num mono">
                        {rate === null ? (
                          <span className="text-faint">—</span>
                        ) : rate === 0 ? (
                          <span className="text-ok">{rateText(m.credits)}</span>
                        ) : (
                          rateText(m.credits)
                        )}
                      </td>
                      <td>
                        {promo ? (
                          <span
                            className={`badge ${promo.cls}`}
                            title={promo.title}
                            style={soon ? { borderStyle: 'dashed' } : undefined}
                          >
                            {promo.text}
                          </span>
                        ) : (
                          <span className="text-faint">—</span>
                        )}
                      </td>
                      <td className="num">{fmtNum(m.context_length)}</td>
                      <td className="num">{fmtNum(m.max_output_tokens)}</td>
                      <td>
                        {/* 只标工具与推理。
                            图片不标：上游 supportsImages 对国际版 hy* 系是错的（标 true 但认不出图，
                            实测二选一 8/20 = 瞎猜），旗标不可信就不该上屏 —— 显示「支持」会误导，
                            显示「不支持」也是拿一个不可信的字段反着用。要判断图片能力只能实测。 */}
                        <div style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                          <Badge cls={m.supports_tool_call ? 'badge-ok' : 'badge-dim'}>工具</Badge>
                          <Badge cls={m.supports_reasoning ? 'badge-ok' : 'badge-dim'}>推理</Badge>
                          {m.only_reasoning && <Badge cls="badge-accent">仅推理</Badge>}
                        </div>
                      </td>
                      <td>
                        <div style={{ fontSize: 12.5 }}>
                          {cleanDesc(m.description) || <span className="text-faint">—</span>}
                        </div>
                        {/* vendor 是上游内部单字母码（f/e/j…，33/20/10 个模型各占一档），
                            没有公开释义，显示出来像乱码，故不展示；tags 是词，保留。 */}
                        {m.tags && m.tags.length > 0 && (
                          <div className="text-faint" style={{ fontSize: 11, marginTop: 2 }}>
                            {m.tags.join(' / ')}
                          </div>
                        )}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}

        <p className="text-faint" style={{ fontSize: 12, marginTop: 10, lineHeight: 1.7 }}>
          倍率取自上游下发的 <span className="mono">credits</span> 字段原文，数值越小越省；
          <span className="text-faint"> — </span> 表示上游未标倍率（不等于免费）。
          「优惠」来自上游 <span className="mono">/v3/config</span> 的{' '}
          <span className="mono">modelPromotions</span>：<span className="badge badge-ok">限时免费</span>{' '}
          是活动期内倍率降到 0，<span className="badge badge-accent">夜间折扣</span>{' '}
          只在每天固定时段生效（跨零点写成 <span className="mono">23:00–次日 07:50</span>）。
          徽标虚线表示 3 天内到期，鼠标悬停可看上游原文与精确起止时间。
          <span className="text-faint">已结束的活动不展示（上游会把它们继续挂在配置里）。</span>
          调用时<strong>直接填第一列的模型 ID</strong>（如{' '}
          <span className="mono">glm-5.3</span>）—— 网关按账号所属域自动路由，
          <span className="text-faint">不要加「域前缀」，上游不认这种写法。</span>
          <br />
          能力列只列<span className="mono">工具</span>与<span className="mono">推理</span>：
          <span className="text-faint">图片能力不展示</span> —— 上游的{' '}
          <span className="mono">supportsImages</span> 对国际版{' '}
          <span className="mono">hy*</span> 系是错的（标 true 但认不出图，2026-09-23 实测二选一
          8/20，与瞎猜无异），旗标不可信就不上屏。「仅推理」表示输出前必然先产出思考 token，
          会额外吃 <span className="mono">max_tokens</span>。
        </p>
      </div>

      <p className="text-faint" style={{ fontSize: 12, marginTop: 12 }}>
        账号维度的剩余积分与作废时间已移到独立的{' '}
        <Link to="/credits">积分到期</Link> 页。
      </p>
    </>
  )
}
