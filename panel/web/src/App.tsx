// App.tsx 应用外壳：登录门禁 + 侧边导航 + 路由。
import { useCallback, useEffect, useMemo, useState } from 'react'
import { NavLink, Navigate, Route, Routes, useNavigate } from 'react-router-dom'
import { api, setUnauthorizedHandler } from './api'
import type { SessionInfo } from './types'
import { Alert, Spinner, ThemeSwitch } from './ui'
import {
  ArrowLeftRight,
  Coins,
  Cpu,
  LayoutDashboard,
  LineChart,
  MessageSquare,
  Power,
  Server,
  Settings,
  UserPlus,
  Users,
} from './icons'
import Login from './pages/Login'
import Dashboard from './pages/Dashboard'
import StatsPage from './pages/StatsPage'
import Models from './pages/Models'
import Credits from './pages/Credits'
import Accounts from './pages/Accounts'
import LoginWizard from './pages/LoginWizard'
import Playground from './pages/Playground'
import ModelAliases from './pages/ModelAliases'
import ConfigPage from './pages/ConfigPage'
import System from './pages/System'

const NAV = [
  { to: '/', label: '仪表盘', icon: LayoutDashboard, end: true },
  { to: '/accounts', label: '账号管理', icon: Users },
  { to: '/stats', label: '请求统计', icon: LineChart },
  { to: '/models', label: '模型与倍率', icon: Cpu },
  { to: '/credits', label: '积分到期', icon: Coins },
  { to: '/login', label: '添加账号', icon: UserPlus },
  { to: '/playground', label: '聊天测试', icon: MessageSquare },
  { to: '/aliases', label: '模型别名', icon: ArrowLeftRight },
  { to: '/config', label: '网关配置', icon: Settings },
  { to: '/system', label: '系统', icon: Server },
]

export default function App() {
  const [session, setSession] = useState<SessionInfo | null>(null)
  const [loading, setLoading] = useState(true)
  const [banner, setBanner] = useState<string | null>(null)
  const navigate = useNavigate()

  const refreshSession = useCallback(async () => {
    try {
      const s = await api.session()
      setSession(s)
      return s
    } catch {
      // 会话接口失败（后端未起来）也要结束 loading，否则页面永久空白。
      setSession(null)
      return null
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void refreshSession()
  }, [refreshSession])

  // 任何接口返回 401 时统一回到登录页，避免用户在坏会话里反复点。
  useEffect(() => {
    setUnauthorizedHandler(() => {
      setSession((prev) => (prev ? { ...prev, authenticated: false } : prev))
      setBanner('会话已过期，请重新登录')
      navigate('/')
    })
  }, [navigate])

  const handleLoggedOut = useCallback(async () => {
    await api.logout().catch(() => undefined)
    setSession((s) => (s ? { ...s, authenticated: false, username: '' } : s))
  }, [])

  if (loading) {
    return (
      <div className="login-wrap">
        <Spinner label="正在连接服务…" />
      </div>
    )
  }

  if (!session?.authenticated) {
    return <Login info={session} banner={banner} onCloseBanner={() => setBanner(null)} onSuccess={refreshSession} />
  }

  return (
    <Shell session={session} onLogout={handleLoggedOut} onSessionRefresh={refreshSession} />
  )
}

function Shell({
  session,
  onLogout,
  onSessionRefresh,
}: {
  session: SessionInfo
  onLogout: () => void
  onSessionRefresh: () => Promise<SessionInfo | null>
}) {
  const warnings = useMemo(() => {
    const list: string[] = []
    if (session.using_default_password) {
      list.push('面板正在使用默认口令（admin / workbuddy），建议在服务端配置中修改 ui.password')
    }
    if (session.read_only) {
      list.push('服务端处于只读模式：登录、签到、改配置等写操作全部禁用')
    }
    return list
  }, [session])

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">
          <div className="brand-dot">WB</div>
          <div className="brand-text">
            <strong>WorkBuddy 控制台</strong>
            <span>workbuddy2api 网关</span>
          </div>
        </div>
        {NAV.map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.end}
            className={({ isActive }) => `nav-item ${isActive ? 'active' : ''}`}
          >
            <span className="nav-icon">
              <item.icon size={15} />
            </span>
            {item.label}
          </NavLink>
        ))}
        <div className="nav-spacer" />
        {/* 窄屏侧栏是一条横向滚动带，导航项总宽远超屏宽：主题切换和退出登录
            如果按普通流排在末尾，会被推到滚动区最右端（375px 下离屏约 800px），
            用户得横滑很久才够得着——等于没有入口。所以这两个包成 .nav-tail，
            窄屏下 sticky 钉在带宽右缘，宽屏下退回普通纵向排布。 */}
        <div className="nav-tail">
          <div className="nav-theme">
            <ThemeSwitch />
          </div>
          {/* 窄屏下 .nav-foot 整体隐藏，退出登录就靠这个图标按钮兜底。
              只读模式另有顶部 Alert 提示，这里不再重复放徽标。 */}
          <button
            className="btn btn-sm nav-logout-mobile"
            onClick={onLogout}
            title={`退出登录（${session.username}）`}
            aria-label="退出登录"
          >
            <Power size={14} />
          </button>
        </div>
        <div className="nav-foot">
          <div style={{ marginBottom: 7, wordBreak: 'break-all', fontFamily: 'var(--mono)' }}>
            {session.gateway_url}
          </div>
          <div style={{ marginBottom: 8 }}>
            <span className="text-faint">登录身份：</span>
            {session.username}
            {session.read_only && <span className="badge badge-warn" style={{ marginLeft: 6 }}>只读</span>}
          </div>
          <button className="btn btn-sm" style={{ width: '100%' }} onClick={onLogout}>
            退出登录
          </button>
        </div>
      </aside>

      <main className="main">
        {warnings.map((w) => (
          <Alert key={w} kind="warn">
            {w}
          </Alert>
        ))}
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/accounts" element={<Accounts session={session} />} />
          <Route path="/stats" element={<StatsPage session={session} />} />
          <Route path="/models" element={<Models />} />
          <Route path="/credits" element={<Credits />} />
          <Route path="/login" element={<LoginWizard session={session} onDone={onSessionRefresh} />} />
          <Route path="/playground" element={<Playground />} />
          <Route path="/aliases" element={<ModelAliases session={session} />} />
          <Route path="/config" element={<ConfigPage session={session} />} />
          <Route path="/system" element={<System session={session} onSessionRefresh={onSessionRefresh} />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </main>
    </div>
  )
}
