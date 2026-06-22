import { Suspense, useEffect, useState } from 'react'
import { Link, Outlet } from 'react-router-dom'
import { Moon, Settings, Sun } from 'lucide-react'
import { useTheme } from '../useTheme'
import { iconBtn } from '../ui'

function Clock() {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [])
  return (
    <span className="hidden text-sm tabular-nums text-zinc-500 sm:inline">
      {new Date(now).toLocaleTimeString('zh-CN', { hour12: false })}
    </span>
  )
}

export default function Layout() {
  const { dark, toggle } = useTheme()
  return (
    <div className="flex min-h-screen flex-col">
      {/* 背景彩色光斑，透过玻璃面板产生液态质感 */}
      <div className="pointer-events-none fixed inset-0 -z-10 overflow-hidden">
        <div className="animate-drift-a absolute -left-56 -top-56 h-[30rem] w-[30rem] rounded-full bg-emerald-400/20 blur-3xl dark:bg-emerald-500/8" />
        <div className="animate-drift-b absolute -right-40 top-1/4 h-[32rem] w-[32rem] rounded-full bg-sky-400/30 blur-3xl dark:bg-sky-500/15" />
        <div className="animate-drift-c absolute -bottom-40 left-1/3 h-[30rem] w-[30rem] rounded-full bg-violet-400/25 blur-3xl dark:bg-violet-500/10" />
        <div className="animate-drift-d absolute -bottom-32 -right-32 h-[28rem] w-[28rem] rounded-full bg-pink-400/25 blur-3xl dark:bg-pink-500/10" />
      </div>

      <header className="sticky top-3 z-40 px-3 sm:px-4">
        <div className="glass mx-auto flex h-14 max-w-7xl items-center justify-between rounded-2xl px-4">
          <Link to="/" className="flex items-center gap-2">
            <span className="text-xl">🌿</span>
            <span className="text-lg font-bold tracking-tight">Moss</span>
            <span className="mt-0.5 hidden text-xs text-zinc-500 sm:inline">轻量服务器监控</span>
          </Link>
          <div className="flex items-center gap-2">
            <Clock />
            <button onClick={toggle} className={iconBtn} title="切换主题">
              {dark ? <Sun className="h-4.5 w-4.5" /> : <Moon className="h-4.5 w-4.5" />}
            </button>
            <Link to="/login" className={iconBtn} title="管理后台">
              <Settings className="h-4.5 w-4.5" />
            </Link>
          </div>
        </div>
      </header>
      <main className="mx-auto w-full max-w-7xl flex-1 px-3 py-5 sm:px-4 sm:py-6">
        <Suspense fallback={<div className="flex items-center justify-center py-20 text-sm text-zinc-400">加载中…</div>}>
          <Outlet />
        </Suspense>
      </main>
      <footer className="py-5 text-center text-xs text-zinc-400 dark:text-zinc-600">
        Moss v0.4.2 · 轻量服务器监控
      </footer>
    </div>
  )
}
