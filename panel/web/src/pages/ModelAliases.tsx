// ModelAliases.tsx 模型别名映射维护页。
//
// 解决的运维问题：统一调度后裸名走全池，但两域的模型名未必一致（甚至只在一域存在）。
// 别名表把「对外统一名」映射到各域真实上游名，网关按选中账号的域改写后出站。
//
// 与「网关配置」页的两处关键差异（UI 上必须说清，否则运维会按配置页的习惯去重启容器）：
//  1. 保存后**无需重启**：网关每 5s 轮询该文件并热加载；
//  2. 校验在面板侧前置完成：网关对非法表是"静默保持旧表"，从界面看不出没生效。
import { useCallback, useEffect, useMemo, useState } from 'react'
import { api, ApiError } from '../api'
import type { AliasEntry, AliasMeta, Model, SessionInfo } from '../types'
import { Alert, ConfirmDialog, fmtISO, Spinner } from '../ui'
import { SuggestInput } from '../SuggestInput'
import { modelRealms, type Realm } from '../realm'

/** 空行模板：新加的行默认只有对外名待填。 */
const emptyRow = (): AliasEntry => ({ name: '', cn: '', global: '' })

/**
 * 模型目录按域切分，供三列下拉各自取候选。
 *
 * 为什么要分列而不是三列共用一份并集：**候选列表本身就是"哪个名字属于哪个域"的说明书**。
 * 共用一份并集时，用户在「国际真实名」格里也会看到只在 CN 存在的模型，选它等于配置了
 * 一条永远走不通的映射（该域账号被选中后吃一次 11102）。分列后每格只列该域真实存在的名字。
 *
 * 域判定走 `modelRealms`（服务端 `realms` 优先，缺席回退按前缀推断），与「模型与倍率」
 * 页同一口径——两页对同一个模型必须给出相同的域结论。
 *
 * 老网关不下发 `realms` 时 `modelRealms` 恒返回单域，候选会退化，故调用方须据
 * `realmsKnown` 提示"无法按域区分"而不是假装列表是准的。
 */
function splitCatalog(models: Model[]) {
  const all: string[] = []
  const byRealm: Record<Realm, string[]> = { cn: [], global: [] }
  let realmsKnown = false
  for (const m of models) {
    const id = (m.id || '').trim()
    if (!id) continue
    if (m.realms && m.realms.length > 0) realmsKnown = true
    all.push(id)
    for (const r of modelRealms(m)) byRealm[r].push(id)
  }
  const uniq = (xs: string[]) => Array.from(new Set(xs)).sort()
  return { all: uniq(all), byRealm: { cn: uniq(byRealm.cn), global: uniq(byRealm.global) }, realmsKnown }
}

export default function ModelAliases({ session }: { session: SessionInfo }) {
  const [rows, setRows] = useState<AliasEntry[]>([])
  const [meta, setMeta] = useState<AliasMeta | null>(null)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [confirmReset, setConfirmReset] = useState(false)
  const [resetting, setResetting] = useState(false)
  const [dirty, setDirty] = useState(false)
  const [catalog, setCatalog] = useState<{ all: string[]; byRealm: Record<Realm, string[]>; realmsKnown: boolean }>({
    all: [],
    byRealm: { cn: [], global: [] },
    realmsKnown: false,
  })

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const res = await api.aliases()
      setRows(res.aliases && res.aliases.length > 0 ? res.aliases : [emptyRow()])
      setMeta(res.meta)
      setDirty(false)
      setError(null)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '加载别名映射失败')
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // 现有模型名做下拉建议（best-effort：拿不到就不给建议，不影响编辑）。
  useEffect(() => {
    let alive = true
    void api
      .models()
      .then((res) => {
        if (!alive) return
        setCatalog(splitCatalog(res.data ?? []))
      })
      .catch(() => undefined)
    return () => {
      alive = false
    }
  }, [])

  const update = (idx: number, patch: Partial<AliasEntry>) => {
    setRows((prev) => prev.map((r, i) => (i === idx ? { ...r, ...patch } : r)))
    setDirty(true)
  }

  const addRow = () => {
    setRows((prev) => [...prev, emptyRow()])
    setDirty(true)
  }

  const removeRow = (idx: number) => {
    setRows((prev) => {
      const next = prev.filter((_, i) => i !== idx)
      return next.length > 0 ? next : [emptyRow()]
    })
    setDirty(true)
  }

  const save = async () => {
    setSaving(true)
    setNotice(null)
    try {
      // 丢掉整行全空的行（用户加了一行又没填），其余交给服务端校验。
      const payload = rows.filter((r) => r.name.trim() || r.cn.trim() || r.global.trim())
      const res = await api.saveAliases(payload)
      setNotice(res.message)
      setError(null)
      setDirty(false)
      await load()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '保存失败')
    } finally {
      setSaving(false)
    }
  }

  const doReset = async () => {
    setResetting(true)
    try {
      const res = await api.resetAliases()
      setConfirmReset(false)
      setNotice(res.message)
      await load()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '恢复失败')
    } finally {
      setResetting(false)
    }
  }

  /** 统计每种域的覆盖情况，帮用户一眼看出"有多少模型只在一域存在"。 */
  const stats = useMemo(() => {
    let cn = 0
    let global = 0
    let dual = 0
    let total = 0
    for (const r of rows) {
      const hasName = r.name.trim() !== ''
      const hasCN = r.cn.trim() !== ''
      const hasGL = r.global.trim() !== ''
      if (!hasName && !hasCN && !hasGL) continue // 空模板行不计入
      total++
      if (hasCN && hasGL) dual++
      else if (hasCN) cn++
      else if (hasGL) global++
    }
    return { cn, global, dual, total }
  }, [rows])

  /**
   * 两域同名的模型：**无需登记别名**。
   *
   * 裸名本来就跨域通用（统一调度的核心语义），所以给同名模型写一条
   * `{name: x, cn: x, global: x}` 在路由上是彻底的 no-op —— 只是让文件变长。
   * 别名表的唯一用途是「两域名不同」与「只在一域存在」这两类。
   *
   * 因此这里只做**只读展示**（让运维确信这批已经能用），并提供「从目录导入」按钮
   * 供确有需要时一键补齐；绝不自动写文件（那会把用户的手工条目淹没在几十行噪声里）。
   */
  const identical = useMemo(() => {
    if (!catalog.realmsKnown) return []
    return catalog.byRealm.cn.filter((id) => catalog.byRealm.global.includes(id))
  }, [catalog])

  /** 已在表格里登记过的对外名（导入时跳过，避免重复行）。 */
  const existingNames = useMemo(() => new Set(rows.map((r) => r.name.trim()).filter(Boolean)), [rows])

  /** 把「两域同名」的模型补成显式行（no-op 条目，仅为可见性）。 */
  const importIdentical = () => {
    const add = identical.filter((id) => !existingNames.has(id)).map((id) => ({ name: id, cn: id, global: id }))
    if (add.length === 0) {
      setNotice('没有需要导入的同名模型（可能已全部登记，或网关未下发 realms 字段）')
      return
    }
    setRows((prev) => {
      const kept = prev.filter((r) => r.name.trim() || r.cn.trim() || r.global.trim())
      return [...kept, ...add]
    })
    setDirty(true)
    setNotice(`已填入 ${add.length} 条同名条目，确认后点「保存」才会落盘。`)
  }

  if (loading && !meta) return <Spinner label="正在读取别名映射…" />

  const writeDisabled = session.read_only

  return (
    <>
      <div className="page-head">
        <div>
          <h1>模型别名</h1>
          <p>
            把对外统一模型名映射到各域真实名 ·{' '}
            <span className="mono">{meta?.path || 'model_aliases.json'}</span>
            {meta?.mod_time && <span className="text-faint"> · 最后修改 {fmtISO(meta.mod_time)}</span>}
          </p>
        </div>
        <div className="page-actions">
          <button className="btn" onClick={() => void load()} disabled={loading}>
            {loading ? <Spinner /> : '🔄'} 重新读取
          </button>
          {meta?.backup_path && (
            <button
              className="btn btn-danger"
              onClick={() => setConfirmReset(true)}
              disabled={writeDisabled || !session.dangerous_ops}
              title={!session.dangerous_ops ? '需在服务端开启 dangerous_ops' : '从首次备份恢复'}
            >
              ↩️ 恢复备份
            </button>
          )}
        </div>
      </div>

      {writeDisabled && <Alert kind="warn">服务端已开启只读模式，别名映射无法保存。</Alert>}
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
      {meta?.parse_error && (
        <Alert kind="error">
          磁盘上的别名文件不是合法 JSON（{meta.parse_error}）。网关此刻<b>仍在用上一份可用映射</b>
          （解析失败时保持旧表），修好并保存即可恢复。
        </Alert>
      )}

      <Alert kind="info">
        <strong>保存后无需重启</strong>：网关每 5 秒轮询该文件，数秒内自动热加载。
        <div style={{ marginTop: 5, fontSize: 12.5 }}>
          裸模型名走全池调度（国内 + 国际账号同一套粘性/加权逻辑）。若两域的模型名不同，
          在这里登记别名：请求 <span className="mono">my-model</span> 时，选中
          <span className="mono"> cn </span>账号就出站 <span className="mono">cn 名</span>、
          选中 <span className="mono">global</span> 账号就出站 <span className="mono">global 名</span>。
          <br />
          只填一域 = 该模型只在该域存在，另一域账号不参与选号（不会白跑一次上游）。
          <span className="mono"> cn:</span> / <span className="mono">global:</span> 前缀仍可用，
          作用是把请求钉死在指定域。
        </div>
      </Alert>

      <div className="card">
        <div className="card-head">
          <h2>别名映射表</h2>
          <span className="hint">
            共 {stats.total} 条 · 双域 {stats.dual} · 仅国内 {stats.cn} · 仅国际 {stats.global}
          </span>
        </div>

        <table className="table">
          <thead>
            <tr>
              <th style={{ width: '30%' }}>
                对外模型名
                <span className="hint">（共 {catalog.all.length} 个）</span>
              </th>
              <th style={{ width: '28%' }}>
                国内（cn）真实名
                <span className="hint">（共 {catalog.byRealm.cn.length} 个）</span>
              </th>
              <th style={{ width: '28%' }}>
                国际（global）真实名
                <span className="hint">（共 {catalog.byRealm.global.length} 个）</span>
              </th>
              <th style={{ width: 70 }} />
            </tr>
          </thead>
          <tbody>
            {rows.map((row, idx) => (
              <tr key={idx}>
                <td>
                  <SuggestInput
                    value={row.name}
                    onChange={(v) => update(idx, { name: v })}
                    options={catalog.all}
                    placeholder="my-model"
                    disabled={writeDisabled}
                    ariaLabel="对外模型名"
                  />
                </td>
                <td>
                  <SuggestInput
                    value={row.cn}
                    onChange={(v) => update(idx, { cn: v })}
                    options={catalog.byRealm.cn}
                    placeholder="留空 = 国内无此模型"
                    disabled={writeDisabled}
                    ariaLabel="国内真实模型名"
                  />
                </td>
                <td>
                  <SuggestInput
                    value={row.global}
                    onChange={(v) => update(idx, { global: v })}
                    options={catalog.byRealm.global}
                    placeholder="留空 = 国际无此模型"
                    disabled={writeDisabled}
                    ariaLabel="国际真实模型名"
                  />
                </td>
                <td>
                  <button
                    className="btn btn-sm btn-ghost"
                    title="删除该行"
                    onClick={() => removeRow(idx)}
                    disabled={writeDisabled}
                  >
                    ✕
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>

        <div className="page-actions" style={{ marginTop: 13 }}>
          <button className="btn" onClick={addRow} disabled={writeDisabled}>
            ➕ 添加一行
          </button>
          <button className="btn btn-primary" onClick={() => void save()} disabled={saving || writeDisabled}>
            {saving ? <Spinner /> : '💾'} 保存{dirty ? '（有未保存修改）' : ''}
          </button>
          <button className="btn" onClick={() => void load()} disabled={saving}>
            放弃修改
          </button>
        </div>
        <div className="desc" style={{ marginTop: 8 }}>
          留空整行（三个格子都不填）在保存时会被忽略。对外名不能含冒号（与
          <span className="mono"> cn:</span> / <span className="mono">global:</span> 前缀冲突）；
          两域名不能同时为空（否则没有任何域能出站）。
        </div>
      </div>

      {/* 两域同名的模型：只读展示。给它们登记别名在路由上是 no-op（裸名本就跨域通用），
          所以默认不写文件；确有需要时用「导入」按钮一键补进表格。 */}
      {catalog.realmsKnown && identical.length > 0 && (
        <div className="card">
          <div className="card-head">
            <h2>两域同名，无需配置</h2>
            <span className="hint">共 {identical.length} 个</span>
          </div>
          <div className="desc" style={{ marginBottom: 10 }}>
            下列模型在国内外<b>名字完全一致</b>，裸名已经可以两域通用，别名表不必登记它们。
            只有「两域名不同」或「只在一域存在」的模型才需要在上表里手工配置。
          </div>
          <div className="alias-chips">
            {identical.map((id) => (
              <span key={id} className="badge badge-dim mono">
                {id}
              </span>
            ))}
          </div>
          <div className="page-actions" style={{ marginTop: 13 }}>
            <button className="btn btn-sm" onClick={importIdentical} disabled={writeDisabled}>
              ⤵ 从目录导入这 {identical.length} 条
            </button>
            <span className="hint">
              导入后仍需点「保存」才会落盘；这些条目在路由上与不配置等价。
            </span>
          </div>
        </div>
      )}

      {!catalog.realmsKnown && (
        <Alert kind="warn">
          网关未下发 <span className="mono">realms</span> 字段，无法判断模型属于哪个域，
          三列的下拉候选退化为同一份列表。升级网关后此页会自动按域区分。
        </Alert>
      )}

      {meta && !meta.exists && (
        <Alert kind="warn">
          别名文件尚不存在（{meta.path}）。保存后会创建；网关当前按"无别名"运行，
          即裸名原样出站、全池调度。
        </Alert>
      )}

      {confirmReset && (
        <ConfirmDialog
          title="从备份恢复别名映射"
          danger
          confirmText="确认恢复"
          busy={resetting}
          onCancel={() => setConfirmReset(false)}
          onConfirm={() => void doReset()}
          message={
            <>
              将用首次保存前的备份覆盖当前 <span className="mono">{meta?.path}</span>。
              <br />
              备份路径：<span className="mono">{meta?.backup_path}</span>
            </>
          }
        />
      )}
    </>
  )
}
