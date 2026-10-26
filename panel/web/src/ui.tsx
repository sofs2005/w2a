// ui.tsx 通用展示组件与格式化工具。

import React, { useEffect, useRef, useState } from 'react'
import { AlertTriangle, CheckCircle2, Inbox, Info, Monitor, Moon, Sun, X, XCircle } from './icons'

/** 把 Unix 秒格式化为本地时间字符串。 */
export function fmtTime(sec?: number): string {
  if (!sec || sec <= 0) return '—'
  const d = new Date(sec * 1000)
  const now = Date.now()
  const diff = d.getTime() - now
  const abs = Math.abs(diff)
  let rel = ''
  if (abs < 60_000) rel = diff > 0 ? '即将' : '刚刚'
  else if (abs < 3_600_000) rel = diff > 0 ? `${Math.round(abs / 60_000)} 分钟后` : `${Math.round(abs / 60_000)} 分钟前`
  else if (abs < 86_400_000) rel = diff > 0 ? `${Math.round(abs / 3_600_000)} 小时后` : `${Math.round(abs / 3_600_000)} 小时前`
  else rel = diff > 0 ? `${Math.round(abs / 86_400_000)} 天后` : `${Math.round(abs / 86_400_000)} 天前`
  return `${d.toLocaleString('zh-CN', { hour12: false })}（${rel}）`
}

/** 把 ISO 时间字符串格式化为本地时间（相对时间优先）。 */
/** 上游时间 → Unix 秒。兼容两种形态：
 *   · RFC3339 带时区偏移（促销 schedule，如 "2026-09-25T00:00:00+08:00"）
 *   · "2006-01-02 15:04:05"（积分包的 CycleEndTime，无时区，按本地时间解）
 *  带偏移的必须交给 Date.parse —— 自己拆数字会把 +08:00 的午夜按浏览器时区解，跨时区差一天。 */
export function upstreamSec(s?: string): number | undefined {
  const t = (s || '').trim()
  if (!t) return undefined
  if (/[zZ]$|[+-]\d{2}:?\d{2}$/.test(t)) {
    const ms = Date.parse(t)
    return Number.isNaN(ms) ? undefined : Math.floor(ms / 1000)
  }
  const m = /^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})/.exec(t)
  if (!m) return undefined
  const d = new Date(+m[1], +m[2] - 1, +m[3], +m[4], +m[5], +m[6])
  return Number.isNaN(d.getTime()) ? undefined : Math.floor(d.getTime() / 1000)
}

export function fmtISO(iso?: string): string {
  if (!iso) return '—'
  const t = Date.parse(iso)
  if (!t || t <= 0) return '—'
  // Go 的零值时间（0001-01-01）按「无记录」处理。
  if (new Date(t).getFullYear() < 1980) return '—'
  const d = new Date(t)
  const diff = Date.now() - t
  if (diff < 0) return d.toLocaleString('zh-CN', { hour12: false })
  if (diff < 60_000) return '刚刚'
  if (diff < 3_600_000) return `${Math.round(diff / 60_000)} 分钟前`
  if (diff < 86_400_000) return `${Math.round(diff / 3_600_000)} 小时前`
  if (diff < 30 * 86_400_000) return `${Math.round(diff / 86_400_000)} 天前`
  return d.toLocaleString('zh-CN', { hour12: false })
}

/** 格式化秒数为「1h 23m 45s」。 */
export function fmtDuration(sec?: number): string {
  if (sec === undefined || sec === null || sec <= 0) return '—'
  const h = Math.floor(sec / 3600)
  const m = Math.floor((sec % 3600) / 60)
  const s = Math.floor(sec % 60)
  if (h > 0) return `${h}小时${m}分`
  if (m > 0) return `${m}分${s}秒`
  return `${s}秒`
}

/** 千分位数字。 */
export function fmtNum(n?: number): string {
  if (n === undefined || n === null) return '—'
  return n.toLocaleString('en-US')
}

/** uid 缩写展示。 */
export function shortUID(uid: string): string {
  return uid.length > 8 ? uid.slice(0, 8) : uid
}

/** 账号展示名：昵称优先，其次 uid 前缀。 */
export function displayName(a: { nickname?: string; uid: string }): string {
  return a.nickname || shortUID(a.uid)
}

/** 状态徽章配色与文案。 */
export function statusBadge(status: string): { cls: string; text: string } {
  switch (status) {
    case 'healthy':
      return { cls: 'badge-ok', text: '正常' }
    case 'cooling':
      return { cls: 'badge-warn', text: '冷却中' }
    case 'disabled':
      return { cls: 'badge-danger', text: '已禁用' }
    case 'stopped':
      // 手动摘出：非故障（日常任务照常、凭证与积分都是活的），用警告色与
      // disabled 的危险色区分开。
      return { cls: 'badge-warn', text: '已摘出' }
    case 'token_expired':
      return { cls: 'badge-danger', text: 'Token 过期' }
    case 'gateway_unreachable':
      return { cls: 'badge-dim', text: '网关不可达' }
    case 'missing_credential':
      return { cls: 'badge-warn', text: '凭证缺失' }
    default:
      return { cls: 'badge-dim', text: '未加载' }
  }
}

/** 冷却原因的中文说明。 */
export function coolKindText(kind?: string): string {
  switch (kind) {
    case 'hard_credit':
      return '余额不足'
    case 'soft_rate':
      return '被限流'
    default:
      return '冷却'
  }
}

export function Badge({
  cls,
  children,
  title,
}: {
  cls: string
  children: React.ReactNode
  /** 悬停说明（可选）。徽标本身很短，装不下的口径差异写这里。 */
  title?: string
}) {
  return (
    <span className={`badge ${cls}`} title={title}>
      <span className="badge-dot" />
      {children}
    </span>
  )
}

export function Alert({
  kind,
  children,
  onClose,
}: {
  kind: 'error' | 'warn' | 'ok' | 'info'
  children: React.ReactNode
  onClose?: () => void
}) {
  // 图标按 kind 选，颜色由 CSS 的 .alert-<kind> .alert-icon 决定（正文保持中性色）。
  const Icon = { error: XCircle, warn: AlertTriangle, ok: CheckCircle2, info: Info }[kind]
  return (
    <div className={`alert alert-${kind}`}>
      <span className="alert-icon">
        <Icon size={15} />
      </span>
      <div style={{ flex: 1, minWidth: 0 }}>{children}</div>
      {onClose && (
        <button className="btn btn-ghost btn-sm" onClick={onClose} aria-label="关闭">
          <X size={14} />
        </button>
      )}
    </div>
  )
}

export function Modal({
  title,
  onClose,
  children,
  wide,
  footer,
}: {
  title: string
  onClose: () => void
  children: React.ReactNode
  wide?: boolean
  footer?: React.ReactNode
}) {
  // Esc 关闭 + 打开期间锁定页面滚动。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      document.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [onClose])

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(e) => {
        // 只有点击遮罩本身才关闭，避免拖选文本误触。
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className={`modal ${wide ? 'modal-wide' : ''}`} role="dialog" aria-modal="true">
        <div className="modal-head">
          <h2>{title}</h2>
          <button className="btn btn-ghost btn-sm" onClick={onClose} aria-label="关闭">
            <X size={14} />
          </button>
        </div>
        {children}
        {footer && <div className="modal-foot">{footer}</div>}
      </div>
    </div>
  )
}

export function Spinner({ label }: { label?: string }) {
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
      <span className="spinner" />
      {label && <span className="text-dim">{label}</span>}
    </span>
  )
}

/** 空状态。默认配一个描边图标（比 emoji 更贴合整站的细线风格），可用 icon={null} 关掉。 */
export function Empty({
  children,
  icon,
}: {
  children: React.ReactNode
  icon?: React.ReactNode
}) {
  const glyph = icon === undefined ? <Inbox size={30} /> : icon
  return (
    <div className="empty">
      {glyph && <span className="empty-icon">{glyph}</span>}
      <div>{children}</div>
    </div>
  )
}

/** Toast 浮层提示：单条、自动淡出，用于「操作已完成」这类不需要占位的反馈。
 *
 *  与 Alert 的分工：Alert 是**行内常驻**信息（只读模式、加载失败、字段说明），
 *  该留在页面上；Toast 是**一次性**反馈（保存成功、需重启），飘一下就过去。
 *
 *  props 刻意做成「key 变化即重置」：调用方每次提示换一个新对象（含 id），
 *  定时器随之重来，连点保存不会出现「上一条的计时器把新一条提前关掉」。
 *  */
export interface ToastMessage {
  /** 唯一标识：同一内容再次提示也要重新计时。 */
  id: number
  kind: 'error' | 'warn' | 'ok' | 'info'
  text: React.ReactNode
  /** 停留时长（毫秒）；0 = 不自动消失，只能手动关闭。 */
  duration: number
}

export function Toast({ message, onClose }: { message: ToastMessage; onClose: () => void }) {
  // leaving 期间先播淡出动画再真正卸载：直接消失会显得很突兀。
  const [leaving, setLeaving] = useState(false)
  // onClose 通常是行内箭头函数，每次渲染都是新引用。若直接放进 effect 依赖，
  // 父组件任何一次重渲染都会重置计时器 —— 提示会一直挂着不消失。用 ref 固定。
  const closeRef = useRef(onClose)
  closeRef.current = onClose

  useEffect(() => {
    setLeaving(false)
    if (message.duration <= 0) return
    const hide = window.setTimeout(() => setLeaving(true), message.duration)
    // 淡出动画（200ms，见 styles.css 的 .toast-leaving）走完再交给父组件卸载。
    const remove = window.setTimeout(() => closeRef.current(), message.duration + 200)
    return () => {
      window.clearTimeout(hide)
      window.clearTimeout(remove)
    }
  }, [message.id, message.duration])

  const Icon = { error: XCircle, warn: AlertTriangle, ok: CheckCircle2, info: Info }[message.kind]
  return (
    <div className="toast-wrap" role="status" aria-live="polite">
      <div className={`toast alert-${message.kind} ${leaving ? 'toast-leaving' : ''}`}>
        <span className="alert-icon">
          <Icon size={15} />
        </span>
        <div style={{ flex: 1, minWidth: 0 }}>{message.text}</div>
        <button className="btn btn-ghost btn-sm" onClick={onClose} aria-label="关闭">
          <X size={14} />
        </button>
      </div>
    </div>
  )
}

/** ConfirmDialog 危险操作二次确认。 */
export function ConfirmDialog({
  title,
  message,
  confirmText = '确认',
  danger,
  onConfirm,
  onCancel,
  busy,
}: {
  title: string
  message: React.ReactNode
  confirmText?: string
  danger?: boolean
  onConfirm: () => void
  onCancel: () => void
  busy?: boolean
}) {
  return (
    <Modal
      title={title}
      onClose={onCancel}
      footer={
        <>
          <button className="btn" onClick={onCancel} disabled={busy}>
            取消
          </button>
          <button
            className={`btn ${danger ? 'btn-danger' : 'btn-primary'}`}
            onClick={onConfirm}
            disabled={busy}
          >
            {busy ? <Spinner /> : null}
            {confirmText}
          </button>
        </>
      }
    >
      <div style={{ fontSize: 13, lineHeight: 1.7, color: 'var(--text-dim)' }}>{message}</div>
    </Modal>
  )
}

/* ── 主题切换 ───────────────────────────────────────── */

type ThemeMode = 'system' | 'dark' | 'light'

/** localStorage 键。index.html 的防闪烁内联脚本读的是同一个键，改这里要同步改那边。 */
const THEME_KEY = 'wb-theme'

/** 读当前选择。localStorage 在隐私模式下可能抛异常，一律退化为「跟随系统」。 */
function readThemeMode(): ThemeMode {
  try {
    const m = localStorage.getItem(THEME_KEY)
    return m === 'dark' || m === 'light' ? m : 'system'
  } catch {
    return 'system'
  }
}

/** 把选择落到 DOM 与 localStorage。「跟随系统」不写 data-theme，交给 CSS 的 media 查询，
    这样系统主题变化时浏览器自动切换，React 侧无需监听 matchMedia。 */
function applyThemeMode(m: ThemeMode) {
  if (m === 'system') document.documentElement.removeAttribute('data-theme')
  else document.documentElement.dataset.theme = m
  try {
    if (m === 'system') localStorage.removeItem(THEME_KEY)
    else localStorage.setItem(THEME_KEY, m)
  } catch {
    // 存不下只是下次打开回到跟随系统，不影响本次切换。
  }
}

const THEME_OPTIONS: { mode: ThemeMode; text: string; title: string; Icon: (p: { size?: number }) => JSX.Element }[] = [
  { mode: 'system', text: '系统', title: '跟随系统主题', Icon: Monitor },
  { mode: 'dark', text: '深色', title: '固定深色主题', Icon: Moon },
  { mode: 'light', text: '浅色', title: '固定浅色主题', Icon: Sun },
]

/** 三态主题切换。侧栏底部与登录页各放一个（登录页没有侧栏，需要单独一份）。 */
export function ThemeSwitch() {
  const [mode, setMode] = useState<ThemeMode>(readThemeMode)
  return (
    <div className="theme-switch" role="group" aria-label="主题">
      {THEME_OPTIONS.map(({ mode: m, text, title, Icon }) => (
        <button
          key={m}
          type="button"
          title={title}
          aria-pressed={mode === m}
          onClick={() => {
            setMode(m)
            applyThemeMode(m)
          }}
        >
          <Icon size={13} />
          {text}
        </button>
      ))}
    </div>
  )
}
