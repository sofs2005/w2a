// SuggestInput.tsx 带候选建议的文本框（自绘弹层，不用原生 <datalist>）。
//
// 为什么单独成文件而不是塞进 ui.tsx：本 fork 对上游面板的纪律是「优先纯新增文件」
// （见 panel/HOST-PATCHES.md），新组件放这里就不必让 ui.tsx 与上游产生差异。
//
// 为什么自绘而不用 <datalist>：datalist 的弹层由**浏览器**绘制，样式不可控，而且
// **会被浏览器密码管理器抢占**——在已保存登录口令的站点上（面板登录页就存了），
// Chrome 会把紧随登录表单之后的文本框当成新的凭据字段，点它弹出的是「自动填充密码」
// 提示而非候选列表。实测症状：表头明明标着「共 27 个」候选，输入框却怎么点都没下拉。
// 这类拦截无法从应用侧关闭（autocomplete="off" 对密码管理器只是建议），只能不用它。
//
// 其余行为对齐 datalist：不强制取值（可填任意名）、输入即过滤、支持键盘上下选择。
import React, { useLayoutEffect, useRef, useState } from 'react'

export function SuggestInput({
  value,
  onChange,
  options,
  placeholder,
  disabled,
  ariaLabel,
}: {
  value: string
  onChange: (v: string) => void
  options: string[]
  placeholder?: string
  disabled?: boolean
  ariaLabel?: string
}) {
  const [open, setOpen] = useState(false)
  const [hi, setHi] = useState(-1)
  const [box, setBox] = useState<{ left: number; top: number; width: number } | null>(null)
  const inputRef = useRef<HTMLInputElement>(null)

  const q = value.trim().toLowerCase()
  // 过滤：候选通常几十条，直接线性过滤（每行一个实例，量级很小，无需 memo）。
  const matches = q ? options.filter((o) => o.toLowerCase().includes(q)) : options

  const syncBox = () => {
    const el = inputRef.current
    if (!el) return
    const r = el.getBoundingClientRect()
    setBox({ left: r.left, top: r.bottom + 2, width: r.width })
  }

  useLayoutEffect(() => {
    if (!open) return
    syncBox()
    // 滚动/缩放时跟随；捕获阶段监听以覆盖任意可滚动祖先（表格外面还有整页滚动条）。
    const onMove = () => syncBox()
    window.addEventListener('scroll', onMove, true)
    window.addEventListener('resize', onMove)
    return () => {
      window.removeEventListener('scroll', onMove, true)
      window.removeEventListener('resize', onMove)
    }
  }, [open])

  // 输入后重新打开（用户改了字就该看到新候选）；已完全匹配单个候选时收起，
  // 避免弹层一直贴着输入框挡住下面的行。
  const handleChange = (v: string) => {
    onChange(v)
    setHi(-1)
    setOpen(!(options.length === 1 && options[0] === v))
  }

  const commit = (v: string) => {
    onChange(v)
    setOpen(false)
    setHi(-1)
  }

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Escape') {
      setOpen(false)
      return
    }
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      if (!open) {
        setOpen(true)
        return
      }
      if (matches.length === 0) return
      const d = e.key === 'ArrowDown' ? 1 : -1
      setHi((h) => (h < 0 ? (d > 0 ? 0 : matches.length - 1) : (h + d + matches.length) % matches.length))
      return
    }
    if (e.key === 'Enter' && open && hi >= 0 && hi < matches.length) {
      e.preventDefault() // 拦下回车，避免误提交外层表单
      commit(matches[hi])
    }
  }

  return (
    <div className="suggest">
      <input
        ref={inputRef}
        type="text"
        value={value}
        placeholder={placeholder}
        disabled={disabled}
        // 双保险：给密码管理器一个明确的"这不是凭据字段"信号。注意它只是建议，
        // 真正解决问题的是不再用原生弹层（见文件头注释）。
        autoComplete="off"
        name={'suggest-' + (ariaLabel || 'field')}
        aria-label={ariaLabel}
        role="combobox"
        aria-expanded={open}
        aria-autocomplete="list"
        onChange={(e) => handleChange(e.target.value)}
        onFocus={() => setOpen(true)}
        onKeyDown={onKeyDown}
        // 用 mousedown 而非 click：blur 会先于 click 触发，等 click 时弹层已被卸载。
        onMouseDown={(e) => {
          if ((e.target as HTMLElement).tagName !== 'INPUT') e.preventDefault()
        }}
        onBlur={() => setOpen(false)}
      />
      {open && box && (
        // fixed + 实测坐标（而非 absolute）：`.card` 目前没有 overflow 裁剪，但一旦
        // 将来被放进 `.table-wrap`（有 overflow-x: auto）就会被裁掉，fixed 不受影响。
        <div className="suggest-pop" style={{ left: box.left, top: box.top, width: box.width }}>
          {matches.length === 0 ? (
            <div className="suggest-empty">无匹配（可直接输入任意名字）</div>
          ) : (
            matches.map((o, i) => (
              <div
                key={o}
                className={`suggest-item mono ${i === hi ? 'active' : ''}`}
                title={o}
                onMouseEnter={() => setHi(i)}
                onMouseDown={(e) => {
                  e.preventDefault()
                  commit(o)
                }}
              >
                {o}
              </div>
            ))
          )}
        </div>
      )}
    </div>
  )
}
