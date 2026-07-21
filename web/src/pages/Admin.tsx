import { useCallback, useEffect, useRef, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { Bell, Cloud, LogOut, Radar, Server, SlidersHorizontal } from 'lucide-react'
import { get, post } from '../api/client'
import type { ApiError } from '../api/client'
import { btnGhost } from '../ui'
import { ServersTab } from './admin/ServersTab'
import { TasksTab } from './admin/TasksTab'
import { NotifyTab } from './admin/NotifyTab'
import { GcpTab } from './admin/GcpTab'
import { SettingsTab } from './admin/SettingsTab'

type TabKey = 'servers' | 'tasks' | 'notify' | 'gcp' | 'settings'

const tabs: Array<{ key: TabKey; label: string; icon: typeof Server }> = [
  { key: 'servers', label: '服务器', icon: Server },
  { key: 'tasks', label: '探测任务', icon: Radar },
  { key: 'notify', label: '通知告警', icon: Bell },
  { key: 'gcp', label: 'GCP 守护', icon: Cloud },
  { key: 'settings', label: '站点设置', icon: SlidersHorizontal },
]

// 定义在模块顶层：若放在 Admin 函数体内，每次 setTab 都会重建组件类型，导致按钮被卸载重挂
function TabButton({ t, tab, setTab }: { t: (typeof tabs)[number]; tab: TabKey; setTab: (k: TabKey) => void }) {
  return (
    <button
      onClick={() => setTab(t.key)}
      className={`flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-sm transition ${
        tab === t.key
          ? 'bg-emerald-500/10 font-medium text-emerald-600 dark:text-emerald-400'
          : 'text-zinc-500 hover:bg-white/50 hover:text-zinc-800 dark:hover:bg-white/10 dark:hover:text-zinc-200'
      }`}
    >
      <t.icon className="h-4 w-4 shrink-0" />
      {t.label}
    </button>
  )
}

/* ---------- 主页面 ---------- */

export default function Admin() {
  const navigate = useNavigate()
  const [tab, setTab] = useState<TabKey>('servers')
  const [toast, setToast] = useState<string | null>(null)
  const [authed, setAuthed] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  // 未登录跳转
  useEffect(() => {
    get('/api/admin/me')
      .then(() => setAuthed(true))
      .catch((e) => {
        if ((e as ApiError).status === 401) navigate('/login')
      })
  }, [navigate])

  const showToast = useCallback((msg: string) => {
    setToast(msg)
    if (timer.current) clearTimeout(timer.current)
    timer.current = setTimeout(() => setToast(null), 2200)
  }, [])

  const logout = async () => {
    try {
      await post('/api/logout')
    } finally {
      navigate('/login')
    }
  }

  if (!authed) return null

  return (
    // 桌面端做成固定外壳：标题 + 左栏不动，仅右侧内容区内部滚动；移动端仍为整页滚动
    <div className="space-y-4 md:flex md:h-[calc(100dvh-11rem)] md:flex-col md:gap-4 md:space-y-0">
      <div className="flex items-center justify-between md:shrink-0">
        <h1 className="text-xl font-bold">管理后台</h1>
        <div className="flex items-center gap-2">
          <Link to="/" className={btnGhost}>
            返回首页
          </Link>
          <button onClick={logout} className={`${btnGhost} !text-rose-500`}>
            <LogOut className="h-4 w-4" /> 退出
          </button>
        </div>
      </div>

      {/* 移动端横向 Tab */}
      <div className="flex gap-1 overflow-x-auto md:hidden">
        {tabs.map((t) => (
          <div key={t.key} className="shrink-0">
            <TabButton t={t} tab={tab} setTab={setTab} />
          </div>
        ))}
      </div>

      <div className="flex gap-6 md:min-h-0 md:flex-1">
        {/* 桌面端侧边栏：在不滚动的外壳里，始终固定不动 */}
        <aside className="hidden w-44 shrink-0 flex-col gap-1 md:flex">
          {tabs.map((t) => (
            <TabButton key={t.key} t={t} tab={tab} setTab={setTab} />
          ))}
        </aside>

        {/* 仅此区域在桌面端内部纵向滚动 */}
        <div className="min-w-0 flex-1 md:overflow-y-auto md:pr-1">
          {tab === 'servers' && <ServersTab toast={showToast} />}
          {tab === 'tasks' && <TasksTab toast={showToast} />}
          {tab === 'notify' && <NotifyTab toast={showToast} />}
          {tab === 'gcp' && <GcpTab toast={showToast} />}
          {tab === 'settings' && <SettingsTab toast={showToast} />}
        </div>
      </div>

      {toast && (
        <div className="fixed bottom-6 right-6 z-50 rounded-xl border border-white/10 bg-zinc-900/85 px-4 py-2 text-sm text-white shadow-xl backdrop-blur-xl dark:border-white/20 dark:bg-white/85 dark:text-zinc-900">
          {toast}
        </div>
      )}
    </div>
  )
}
