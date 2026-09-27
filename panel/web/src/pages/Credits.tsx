// Credits.tsx 积分到期：把「哪个号还有多少积分、什么时候作废」单独成一页。
//
// 数据源两处：
//   · /api/accounts → 每个账号的积分与到期（来自主动查询积分包，见 ops.AccountView）
//   · /api/accounts/<uid>/credits → 单账号的积分包明细（点开行才拉，避免一次打 18 个上游）
//
// ⚠️ 积分不是实时值：显示的是**上次查询时刻**的快照（页面上标了时间）。
// 上游积分包按周期结束作废，不主动查就不知道 —— 所以「刷新全部」是这一页的主操作。
import { Fragment, useCallback, useEffect, useMemo, useState } from 'react'
import { api } from '../api'
import type { Account, CreditPack } from '../types'
import { REALMS, accountRealm, realmLabel, type Realm } from '../realm'
import { Alert, Badge, Empty, Spinner, displayName, fmtNum, fmtTime, upstreamSec } from '../ui'

type SortKey = 'expire-asc' | 'remain-desc' | 'name'
type RealmFilter = 'all' | Realm

const DAY = 86400

/** 快到期阈值：7 天内标红、30 天内标黄。 */
const SOON_DAYS = 7
const NEAR_DAYS = 30

interface Row {
  a: Account
  /** 剩余积分；未查询过为 undefined（不是 0）。 */
  remain?: number
  /** 最近一次积分包到期时间（Unix 秒）。 */
  expireSec?: number
  /** 与该到期时间同时刻作废的积分量。 */
  expiring?: number
  /** 距到期秒数（负数 = 已过期）。阈值判定用它，不用天数 ——
   *  floor 会把 7.7 天压成 7，导致「8 天后到期」被算进「7 天内作废」。
   *  展示用 fmtTime（内部 round），两者不冲突。 */
  left?: number
}

function buildRow(a: Account, packs?: CreditPack[] | null): Row {
  const queried = !!a.credits_at
  let remain = queried ? (a.live_credits ?? a.credits) : undefined
  let expireSec = upstreamSec(a.credits_expire_at)
  let expiring = expireSec !== undefined ? a.credits_expiring : undefined

  // 该号没走过批量查询、但展开过明细时，用明细补齐：
  // 否则行上写着「未查询 / —」，底下却列着一堆有余额的积分包，自相矛盾。
  if (!queried && packs && packs.length > 0) {
    remain = packs.reduce((n, p) => n + (p.remain || 0), 0)
    const ends = packs
      .map((p) => upstreamSec(p.end_time))
      .filter((x): x is number => x !== undefined)
    if (ends.length > 0) {
      expireSec = Math.min(...ends)
      expiring = packs
        .filter((p) => upstreamSec(p.end_time) === expireSec)
        .reduce((n, p) => n + (p.remain || 0), 0)
    }
  }

  const left = expireSec !== undefined ? expireSec - Date.now() / 1000 : undefined
  return {
    a,
    remain,
    expireSec,
    expiring,
    left,
  }
}

export default function Credits() {
  const [accounts, setAccounts] = useState<Account[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [realm, setRealm] = useState<RealmFilter>('all')
  const [sort, setSort] = useState<SortKey>('expire-asc')
  const [q, setQ] = useState('')
  const [onlySoon, setOnlySoon] = useState(false)
  const [onlyUnqueried, setOnlyUnqueried] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [progress, setProgress] = useState<string | null>(null)

  // 展开行的积分包明细：uid → 包列表（null = 加载中）。
  const [packs, setPacks] = useState<Record<string, CreditPack[] | null>>({})

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const a = await api.accounts()
      setAccounts(a.accounts ?? [])
      setError(null)
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载账号失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // 刷新全部：跑一次批量积分任务（空 uids = 全量），轮询进度到结束再重拉账号。
  const refreshAll = useCallback(async () => {
    setRefreshing(true)
    setProgress(null)
    try {
      const task = await api.batchCredits([])
      for (let i = 0; i < 300; i++) {
        const t = await api.task(task.id)
        if (t.total) setProgress(`${t.done ?? 0}/${t.total}`)
        if (!t.running) {
          if (t.error) setError(`刷新积分：${t.error}`)
          break
        }
        await new Promise((r) => setTimeout(r, 1000))
      }
      setPacks({}) // 明细跟着失效，展开时重新拉
      await load()
    } catch (err) {
      setError(err instanceof Error ? err.message : '刷新积分失败')
    } finally {
      setRefreshing(false)
      setProgress(null)
    }
  }, [load])

  const togglePacks = useCallback(async (uid: string) => {
    if (packs[uid] !== undefined) {
      setPacks((p) => {
        const n = { ...p }
        delete n[uid]
        return n
      })
      return
    }
    setPacks((p) => ({ ...p, [uid]: null }))
    try {
      const c = await api.accountCredits(uid)
      setPacks((p) => ({ ...p, [uid]: c.details ?? [] }))
    } catch (err) {
      setError(err instanceof Error ? err.message : '拉取积分包明细失败')
      setPacks((p) => {
        const n = { ...p }
        delete n[uid]
        return n
      })
    }
  }, [packs])

  const rows = useMemo(() => {
    const kw = q.trim().toLowerCase()
    let list = accounts.map((a) => buildRow(a, packs[a.uid]))
    if (realm !== 'all') list = list.filter((r) => accountRealm(r.a) === realm)
    if (kw) {
      list = list.filter((r) =>
        `${r.a.nickname} ${r.a.uid} ${r.a.enterprise_id ?? ''}`.toLowerCase().includes(kw),
      )
    }
    if (onlySoon) list = list.filter((r) => r.left !== undefined && r.left <= NEAR_DAYS * DAY)
    if (onlyUnqueried) list = list.filter((r) => r.remain === undefined)

    return [...list].sort((x, y) => {
      switch (sort) {
        case 'remain-desc':
          if (x.remain === undefined) return y.remain === undefined ? 0 : 1
          if (y.remain === undefined) return -1
          return y.remain - x.remain
        case 'name':
          return displayName(x.a).localeCompare(displayName(y.a), 'zh-CN')
        case 'expire-asc':
        default:
          // 有到期的排前面按到期升序；无到期信息的沉底。
          if (x.expireSec === undefined) return y.expireSec === undefined ? 0 : 1
          if (y.expireSec === undefined) return -1
          return x.expireSec - y.expireSec
      }
    })
  }, [accounts, packs, realm, q, sort, onlySoon, onlyUnqueried])

  const all = useMemo(() => accounts.map((a) => buildRow(a, packs[a.uid])), [accounts, packs])

  // 汇总：只在「查过的账号」上求和 —— 没查过的算 0 会把总额说少。
  const summary = useMemo(() => {
    let total = 0
    let unqueried = 0
    let soon = 0
    let near = 0
    let earliest: number | undefined
    for (const r of all) {
      if (r.remain === undefined) {
        unqueried++
        continue
      }
      total += r.remain
      if (r.expireSec !== undefined && r.left !== undefined && r.left > 0 && r.expiring) {
        if (r.left <= SOON_DAYS * DAY) soon += r.expiring
        if (r.left <= NEAR_DAYS * DAY) near += r.expiring
        if (earliest === undefined || r.expireSec < earliest) earliest = r.expireSec
      }
    }
    return { total, unqueried, soon, near, earliest, queried: all.length - unqueried }
  }, [all])

  const countByRealm = useMemo(() => {
    const out: Record<Realm, number> = { global: 0, cn: 0 }
    for (const a of accounts) out[accountRealm(a)]++
    return out
  }, [accounts])

  return (
    <>
      <div className="page-head">
        <div>
          <h1>积分到期</h1>
          <p>
            各账号剩余积分与积分包作废时间
            <span className="text-faint">
              {' '}
              · 积分是「上次查询时刻」的快照，不是实时值 —— 点右侧按钮去上游查一遍
            </span>
          </p>
        </div>
        <div className="page-actions">
          <button className="btn" onClick={() => void load()} disabled={loading}>
            {loading ? <Spinner /> : '🔄'} 重新加载
          </button>
          <button className="btn btn-primary" onClick={() => void refreshAll()} disabled={refreshing}>
            {refreshing ? <Spinner /> : '💎'} 刷新全部积分{progress ? `（${progress}）` : ''}
          </button>
        </div>
      </div>

      {error && <Alert kind="error">{error}</Alert>}

      <div className="grid grid-stats" style={{ marginBottom: 16 }}>
        <div className="stat">
          <div className="stat-label">已查账号剩余积分</div>
          <div className="stat-value">{fmtNum(summary.total)}</div>
          <div className="stat-sub">
            {summary.queried}/{all.length} 个账号查过
            {summary.unqueried > 0 && ` · ${summary.unqueried} 个未查`}
          </div>
        </div>
        <div className="stat">
          <div className="stat-label">{SOON_DAYS} 天内作废</div>
          <div className={`stat-value ${summary.soon > 0 ? 'text-warn' : ''}`}>
            {fmtNum(summary.soon)}
          </div>
          <div className="stat-sub">不用就归零的那部分</div>
        </div>
        <div className="stat">
          <div className="stat-label">{NEAR_DAYS} 天内作废</div>
          <div className="stat-value">{fmtNum(summary.near)}</div>
          <div className="stat-sub">含上面 {SOON_DAYS} 天的那批</div>
        </div>
        <div className="stat">
          <div className="stat-label">最近一次到期</div>
          <div className="stat-value small">
            {summary.earliest !== undefined ? fmtTime(summary.earliest) : '—'}
          </div>
          <div className="stat-sub">全池最早的一个积分包结束时间</div>
        </div>
      </div>

      <div className="tabs">
        {([['all', '全部'], ...REALMS.map((r) => [r.key, r.label])] as [RealmFilter, string][]).map(
          ([key, label]) => (
            <button
              key={key}
              className={`tab ${realm === key ? 'active' : ''}`}
              onClick={() => setRealm(key)}
            >
              {label}
              <span className="text-faint">
                {' '}
                · {key === 'all' ? accounts.length : countByRealm[key]}
              </span>
            </button>
          ),
        )}
      </div>

      <div className="card">
        <div className="card-head">
          <strong>账号积分（{rows.length}）</strong>
          <span className="text-faint" style={{ fontSize: 12 }}>
            点行首「明细」看该号的积分包构成
          </span>
        </div>

        <div className="row" style={{ marginBottom: 12 }}>
          <div>
            <input
              type="text"
              placeholder="搜索昵称 / UID…"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>
          <div className="shrink" style={{ minWidth: 170 }}>
            <select value={sort} onChange={(e) => setSort(e.target.value as SortKey)}>
              <option value="expire-asc">按到期时间（早→晚）</option>
              <option value="remain-desc">按剩余积分（多→少）</option>
              <option value="name">按昵称</option>
            </select>
          </div>
          <label className="checkbox shrink" style={{ alignSelf: 'center' }}>
            <input type="checkbox" checked={onlySoon} onChange={(e) => setOnlySoon(e.target.checked)} />
            {NEAR_DAYS} 天内到期
          </label>
          <label className="checkbox shrink" style={{ alignSelf: 'center' }}>
            <input
              type="checkbox"
              checked={onlyUnqueried}
              onChange={(e) => setOnlyUnqueried(e.target.checked)}
            />
            只看未查询
          </label>
        </div>

        {loading && accounts.length === 0 ? (
          <Spinner label="正在加载账号…" />
        ) : rows.length === 0 ? (
          <Empty>
            没有匹配的账号。
            {q || onlySoon || onlyUnqueried || realm !== 'all' ? '（试试清掉筛选条件）' : ''}
          </Empty>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>账号</th>
                  <th>域</th>
                  <th className="num">剩余积分</th>
                  <th>最近到期</th>
                  <th className="num">到期时剩余</th>
                  <th>状态</th>
                  <th>明细</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => {
                  const { a, remain, expireSec, expiring } = r
                  const open = packs[a.uid] !== undefined
                  const cls =
                    r.left !== undefined && r.left <= NEAR_DAYS * DAY ? 'text-warn' : ''
                  return (
                    // Fragment 才是列表项，key 必须挂在它上面（挂在内部 tr 上 React 不认）。
                    <Fragment key={a.uid}>
                      <tr>
                        <td>
                          <div>{displayName(a)}</div>
                          <div className="mono text-faint" style={{ fontSize: 11 }}>
                            {a.uid.slice(0, 8)}
                          </div>
                        </td>
                        <td>
                          <Badge cls={accountRealm(a) === 'global' ? 'badge-accent' : 'badge-dim'}>
                            {realmLabel(accountRealm(a))}
                          </Badge>
                        </td>
                        <td className="num">
                          {remain === undefined ? (
                            <span className="text-faint" title="还没查过，不是 0">
                              —
                            </span>
                          ) : (
                            fmtNum(remain)
                          )}
                        </td>
                        <td>
                          {expireSec !== undefined ? (
                            <>
                              {/* fmtTime 自带「N 天后 / N 天前」，别再补一个（会重复）。 */}
                              <div className={cls}>{fmtTime(expireSec)}</div>
                              <div className="text-faint mono" style={{ fontSize: 11 }}>
                                {a.credits_expire_at}
                              </div>
                            </>
                          ) : (
                            <span className="text-faint">
                              {remain === undefined ? '未查询' : '无到期'}
                            </span>
                          )}
                        </td>
                        <td className="num">
                          {expiring !== undefined && expireSec !== undefined ? (
                            <span className={cls}>{fmtNum(expiring)}</span>
                          ) : (
                            <span className="text-faint">—</span>
                          )}
                        </td>
                        <td>
                          {!a.credits_at && packs[a.uid] && packs[a.uid]!.length > 0 ? (
                            <Badge cls="badge-dim" title="该号没走过批量查询，这一行是按展开的积分包明细算的">
                              按明细
                            </Badge>
                          ) : a.disabled ? (
                            <Badge cls="badge-danger">已停用</Badge>
                          ) : a.cooling ? (
                            <Badge cls="badge-warn">冷却中</Badge>
                          ) : (
                            // 不标「未进池」：控制台读的凭证目录与网关读的可以不是同一个
                            // （本地面板 + 远端网关就是这种），本地有文件而池里没有是常态，
                            // 不是账号的毛病。只要这个号本身能用就算正常。
                            <Badge cls="badge-ok">正常</Badge>
                          )}
                        </td>
                        <td>
                          <button className="btn btn-sm" onClick={() => void togglePacks(a.uid)}>
                            {open ? '收起' : '明细'}
                          </button>
                        </td>
                      </tr>
                      {open && (
                        <tr>
                          <td colSpan={7} style={{ background: 'rgba(127,127,127,0.06)' }}>
                            {packs[a.uid] === null ? (
                              <Spinner label="正在拉积分包…" />
                            ) : (packs[a.uid] ?? []).length === 0 ? (
                              <span className="text-faint">该号没有积分包明细。</span>
                            ) : (
                              <table style={{ margin: 0 }}>
                                <thead>
                                  <tr>
                                    <th>积分包</th>
                                    <th className="num">剩余</th>
                                    <th className="num">总量</th>
                                    <th>到期时间</th>
                                  </tr>
                                </thead>
                                <tbody>
                                  {(packs[a.uid] ?? []).map((p, i) => (
                                    <tr key={i}>
                                      <td>{p.name || <span className="text-faint">—</span>}</td>
                                      <td className="num">{fmtNum(p.remain)}</td>
                                      <td className="num">{fmtNum(p.size)}</td>
                                      <td className="mono" style={{ fontSize: 12 }}>
                                        {p.end_time || <span className="text-faint">无到期</span>}
                                        {/* 周期边界与真实到期不同时才提示，避免噪音 */}
                                        {p.cycle_end_time && p.cycle_end_time !== p.end_time && (
                                          <div
                                            className="text-faint"
                                            style={{ fontSize: 11 }}
                                            title="上游 CycleEndTime（按周期发量的包，这里只是周期边界，不是到期）"
                                          >
                                            周期至 {p.cycle_end_time}
                                          </div>
                                        )}
                                      </td>
                                    </tr>
                                  ))}
                                </tbody>
                              </table>
                            )}
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  )
                })}
              </tbody>
            </table>
          </div>
        )}

        <p className="text-faint" style={{ fontSize: 12, marginTop: 10, lineHeight: 1.7 }}>
          「最近到期」是账号各积分包里最早的**真实到期时间**（上游{' '}
          <span className="mono">DeductionEndTime</span>，扣费截止），「到期时剩余」是同一时刻作废的那批积分量
          —— 两者一起看才知道哪些积分快打水漂。<span className="text-faint">
          官方赠送积分按批过期，不用就归零，所以选号算法会优先消耗快过期的那批。</span>
          <br />
          积分必须主动查询才有数据：<span className="mono">—</span> 表示还没查过（不是 0）。
          刷新走的是批量任务，18 个号大约十几秒。
          <span className="text-faint">快照存在控制台进程内存里，重启控制台会清空 —— 重启后回本页点一次「刷新全部积分」。</span>
        </p>
      </div>
    </>
  )
}
