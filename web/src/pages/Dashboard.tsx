import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { LayoutGrid, List } from 'lucide-react'
import { getLive, pct, useLive, useServers } from '../api/store'
import StatsBar from '../components/StatsBar'
import ServerCard, { StatusPill } from '../components/ServerCard'
import Flag from '../components/Flag'
import { MiniBar } from '../components/ProgressBar'
import { fmtBytes, fmtSpeed, fmtUptime } from '../utils/format'
import { card, td, th } from '../ui'

export default function Dashboard() {
  useLive()
  const servers = useServers()
  const navigate = useNavigate()
  const [group, setGroup] = useState('全部')
  const [view, setView] = useState<'grid' | 'table'>(
    () => (localStorage.getItem('moss-view') as 'grid' | 'table') || 'grid',
  )
  useEffect(() => {
    localStorage.setItem('moss-view', view)
  }, [view])

  const groups = ['全部', ...Array.from(new Set(servers.map((s) => s.group)))]
  const list = [...servers]
    .filter((s) => group === '全部' || s.group === group)
    .sort((a, b) => Number(b.online) - Number(a.online))

  return (
    <div className="space-y-4">
      <StatsBar />

      <div className="flex items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-1">
          {groups.map((g) => {
            const count = g === '全部' ? servers.length : servers.filter((s) => s.group === g).length
            return (
              <button
                key={g}
                onClick={() => setGroup(g)}
                className={`rounded-lg px-3 py-1.5 text-sm transition ${
                  group === g
                    ? 'bg-emerald-500/10 font-medium text-emerald-600 dark:text-emerald-400'
                    : 'text-zinc-500 hover:bg-white/50 hover:text-zinc-800 dark:hover:bg-white/10 dark:hover:text-zinc-200'
                }`}
              >
                {g}
                <span className="ml-1 text-xs opacity-60">{count}</span>
              </button>
            )
          })}
        </div>
        <div className="glass flex shrink-0 items-center gap-1 rounded-xl p-1">
          <button
            onClick={() => setView('grid')}
            className={`rounded-md p-1.5 transition ${view === 'grid' ? 'bg-white/70 text-zinc-800 shadow-sm dark:bg-white/15 dark:text-zinc-100' : 'text-zinc-400 hover:text-zinc-600 dark:hover:text-zinc-300'}`}
            title="卡片视图"
          >
            <LayoutGrid className="h-4 w-4" />
          </button>
          <button
            onClick={() => setView('table')}
            className={`rounded-md p-1.5 transition ${view === 'table' ? 'bg-white/70 text-zinc-800 shadow-sm dark:bg-white/15 dark:text-zinc-100' : 'text-zinc-400 hover:text-zinc-600 dark:hover:text-zinc-300'}`}
            title="列表视图"
          >
            <List className="h-4 w-4" />
          </button>
        </div>
      </div>

      {view === 'grid' ? (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {list.map((s) => (
            <ServerCard key={s.id} server={s} />
          ))}
        </div>
      ) : (
        <div className={`${card} overflow-x-auto`}>
          <table className="w-full min-w-[860px]">
            <thead className="border-b border-zinc-500/15 dark:border-white/10">
              <tr>
                <th className={th}>状态</th>
                <th className={th}>名称</th>
                <th className={th}>系统</th>
                <th className={th}>CPU</th>
                <th className={th}>内存</th>
                <th className={th}>硬盘</th>
                <th className={th}>网速 ↑ / ↓</th>
                <th className={th}>总流量 ↑ / ↓</th>
                <th className={th}>在线时长</th>
              </tr>
            </thead>
            <tbody>
              {list.map((s) => {
                const st = getLive(s.id)
                return (
                  <tr
                    key={s.id}
                    onClick={() => navigate(`/server/${s.id}`)}
                    className={`cursor-pointer border-b border-zinc-500/10 transition last:border-0 hover:bg-white/40 dark:border-white/5 dark:hover:bg-white/5 ${
                      s.online ? '' : 'opacity-60'
                    }`}
                  >
                    <td className={td}>
                      <StatusPill online={s.online} />
                    </td>
                    <td className={`${td} font-medium`}>
                      <Flag code={s.flag} className="mr-1.5" />
                      {s.name}
                    </td>
                    <td className={`${td} text-zinc-500`}>{s.os}</td>
                    <td className={td}>
                      <MiniBar pct={st.cpu} />
                    </td>
                    <td className={td}>
                      <MiniBar pct={pct(st.memUsed, s.memTotal)} />
                    </td>
                    <td className={td}>
                      <MiniBar pct={pct(st.diskUsed, s.diskTotal)} />
                    </td>
                    <td className={`${td} tabular-nums text-zinc-600 dark:text-zinc-300`}>
                      {fmtSpeed(st.netUp)} / {fmtSpeed(st.netDown)}
                    </td>
                    <td className={`${td} tabular-nums text-zinc-600 dark:text-zinc-300`}>
                      {fmtBytes(st.totalUp)} / {fmtBytes(st.totalDown)}
                    </td>
                    <td className={`${td} tabular-nums text-zinc-500`}>{fmtUptime(s.uptimeSec)}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
