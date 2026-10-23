// Login.tsx 面板登录页。
import { useState } from 'react'
import { api, ApiError } from '../api'
import type { SessionInfo } from '../types'
import { Alert, Spinner, ThemeSwitch } from '../ui'

export default function Login({
  info,
  banner,
  onCloseBanner,
  onSuccess,
}: {
  info: SessionInfo | null
  banner: string | null
  onCloseBanner: () => void
  onSuccess: () => Promise<SessionInfo | null>
}) {
  const [username, setUsername] = useState('admin')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    setBusy(true)
    try {
      await api.login(username, password)
      await onSuccess()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : '登录失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="login-wrap">
      {/* 固定在右上角：不进卡片，避免「主题」这个外围选项挤占登录表单的视觉重心。 */}
      <div className="login-theme">
        <ThemeSwitch />
      </div>
      <div className="login-card">
        <div className="login-logo">W2A</div>
        <h1>W2A 控制台</h1>
        <div className="sub">W2A 网关管理面板</div>

        {banner && <Alert kind="warn" onClose={onCloseBanner}>{banner}</Alert>}
        {error && <Alert kind="error">{error}</Alert>}

        <form onSubmit={submit}>
          <div className="field">
            <label htmlFor="u">用户名</label>
            <input
              id="u"
              type="text"
              value={username}
              autoComplete="username"
              onChange={(e) => setUsername(e.target.value)}
              autoFocus
            />
          </div>
          <div className="field">
            <label htmlFor="p">密码</label>
            <input
              id="p"
              type="password"
              value={password}
              autoComplete="current-password"
              onChange={(e) => setPassword(e.target.value)}
            />
          </div>
          <button
            type="submit"
            className="btn btn-primary"
            style={{ width: '100%', justifyContent: 'center' }}
            disabled={busy}
          >
            {busy ? <Spinner /> : null}
            登录
          </button>
        </form>

        {info?.using_default_password && (
          <Alert kind="info">
            当前使用默认口令 <span className="mono">admin / w2a</span>。首次登录后请修改：在服务端配置文件中设置
            <span className="mono"> ui.username / ui.password</span>，或设置环境变量
            <span className="mono"> WBGUI_PASSWORD</span>。
          </Alert>
        )}
      </div>
    </div>
  )
}
