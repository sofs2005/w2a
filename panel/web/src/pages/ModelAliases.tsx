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
import type { AliasEntry, AliasMeta, SessionInfo } from '../types'
import { Alert, ConfirmDialog, fmtISO, Spinner } from '../ui'

/** 空行模板：新加的行默认只有对外名待填。 */
const emptyRow = (): AliasEntry => ({ name: '', cn: '', global: '' })

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
  const [catalog, setCatalog] = useState<string[]>([])

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
        const ids = (res.data ?? []).map((m) => m.id).filter(Boolean)
        setCatalog(Array.from(new Set(ids)).sort())
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
          磁盘上的别名文件不是合法 JSON（{meta.parse_error}）。网关此刻**仍在用上一份可用映射**
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
              <th style={{ width: '30%' }}>对外模型名</th>
              <th style={{ width: '28%' }}>国内（cn）真实名</th>
              <th style={{ width: '28%' }}>国际（global）真实名</th>
              <th style={{ width: 70 }} />
            </tr>
          </thead>
          <tbody>
            {rows.map((row, idx) => (
              <tr key={idx}>
                <td>
                  <input
                    type="text"
                    list="alias-catalog"
                    value={row.name}
                    placeholder="my-model"
                    onChange={(e) => update(idx, { name: e.target.value })}
                  />
                </td>
                <td>
                  <input
                    type="text"
                    list="alias-catalog"
                    value={row.cn}
                    placeholder="留空 = 国内无此模型"
                    onChange={(e) => update(idx, { cn: e.target.value })}
                  />
                </td>
                <td>
                  <input
                    type="text"
                    list="alias-catalog"
                    value={row.global}
                    placeholder="留空 = 国际无此模型"
                    onChange={(e) => update(idx, { global: e.target.value })}
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

        {/* 现有模型名建议：datalist 让输入框自带下拉，但不强制取值（允许填任意名）。 */}
        <datalist id="alias-catalog">
          {catalog.map((id) => (
            <option key={id} value={id} />
          ))}
        </datalist>

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
