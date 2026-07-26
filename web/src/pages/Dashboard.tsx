import { memo, useCallback, useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { LayoutGrid, List } from 'lucide-react'
import type { ServerMeta } from '../types'
import { pct, useLiveStats, useServers } from '../api/store'
import StatsBar from '../components/StatsBar'
import ServerCard from '../components/ServerCard'
import Flag from '../components/Flag'
import Ticker from '../components/Ticker'
import { MiniBar } from '../components/ProgressBar'
import { StatusPill } from '../components/ui'
import { fmtBytes, fmtSpeed, fmtUptime, shortOS } from '../utils/format'
import { safeLocalGet, safeLocalSet } from '../utils/storage'
import { card, td, th } from '../ui'

/** 表格行：自订阅单台 stats，A 上报只重渲染 A 行（memo 化） */
const ServerRow = memo(function ServerRow({
  server,
  onOpen,
}: {
  server: ServerMeta
  onOpen: (id: string) => void
}) {
  const st = useLiveStats(server.id)
  return (
    <tr
      onClick={() => onOpen(server.id)}
      className={`cursor-pointer border-b border-zinc-500/10 transition last:border-0 hover:bg-white/40 dark:border-white/5 dark:hover:bg-white/5 ${
        server.online ? '' : 'opacity-60'
      }`}
    >
      <td className={td}>
        <StatusPill online={server.online} />
      </td>
      {/* 名称由用户自填、长度无上限，此前没有任何宽度约束，够长就会把整表撑出
          横向滚动条（与移动端卡片曾修过的同类问题同源）。限宽截断，完整名挂 title。 */}
      <td className={`${td} font-medium`}>
        <span className="flex items-center">
          <Flag code={server.flag} className="mr-1.5 shrink-0" />
          <span className="min-w-0 truncate" title={server.name}>
            {server.name}
          </span>
        </span>
      </td>
      {/* 系统名长短悬殊（"Debian 13" vs "Microsoft Windows Server 2022 Datacenter 21H2"），
          td 自带 whitespace-nowrap，auto 布局表格会被最长的一台撑出横向滚动条。
          shortOS 先剥掉无判读价值的填充，再限宽兜底钉死列宽上限；
          两者叠加后常见系统名可完整显示，完整原值挂 title 供悬停查看。 */}
      <td className={`${td} text-zinc-500`}>
        <span className="block truncate" title={server.os}>
          {shortOS(server.os)}
        </span>
      </td>
      <td className={td}>
        <MiniBar pct={st.cpu} />
      </td>
      <td className={td}>
        <MiniBar pct={pct(st.memUsed, server.memTotal)} />
      </td>
      <td className={td}>
        <MiniBar pct={pct(st.diskUsed, server.diskTotal)} />
      </td>
      {/* 网速文本长度逐帧变化，自动布局表格会随之重算列宽引起外框/滚动条抖动：
          两侧各固定最小宽度并向斜杠对齐，让列宽保持稳定 */}
      <td className={`${td} tabular-nums text-zinc-600 dark:text-zinc-300`}>
        <span className="inline-flex items-center gap-1">
          <span className="inline-block min-w-[5rem] text-right">
            <Ticker value={st.netUp} format={fmtSpeed} />
          </span>
          <span className="text-zinc-400 dark:text-zinc-500">/</span>
          <span className="inline-block min-w-[5rem]">
            <Ticker value={st.netDown} format={fmtSpeed} />
          </span>
        </span>
      </td>
      <td className={`${td} tabular-nums text-zinc-600 dark:text-zinc-300`}>
        {fmtBytes(st.totalUp)} / {fmtBytes(st.totalDown)}
      </td>
      <td className={`${td} tabular-nums text-zinc-500`}>{fmtUptime(server.uptimeSec)}</td>
    </tr>
  )
})

export default function Dashboard() {
  const servers = useServers()
  const navigate = useNavigate()
  const onOpen = useCallback((id: string) => navigate(`/server/${id}`), [navigate])
  const [group, setGroup] = useState('全部')
  const [view, setView] = useState<'grid' | 'table'>(
    () => (safeLocalGet('moss-view') as 'grid' | 'table') || 'grid',
  )
  useEffect(() => {
    safeLocalSet('moss-view', view)
  }, [view])

  // 真实分组去重并排除空值；再剔除恰好叫「全部」的分组，避免与硬编码项 key 重复、筛选错乱
  const real = Array.from(new Set(servers.map((s) => s.group).filter(Boolean))).filter(
    (g) => g !== '全部',
  )
  const groups = ['全部', ...real]
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
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
          {list.map((s) => (
            <ServerCard key={s.id} server={s} />
          ))}
        </div>
      ) : (
        <div className={`${card} overflow-x-auto`}>
          {/* table-fixed + colgroup：列宽由声明决定而非内容决定。此前是 auto 布局，
              九列宽度全由最长的一行数据推导，宽屏容器（max-w-7xl≈1246px）余量本就
              不足 30px，流量涨到 TB / 在线时长上三位数就整表溢出、底部冒出横条。
              固定宽度按各列内容实测上限分配（速度 77px、流量 135px、时长 85px，
              均含 px-2.5 左右内边距 20px），名称列吃掉剩余弹性空间并截断兜底。 */}
          <table className="w-full min-w-[1150px] table-fixed">
            <colgroup>
              <col className="w-[72px]" />
              <col />
              <col className="w-[156px]" />
              <col className="w-[108px]" />
              <col className="w-[108px]" />
              <col className="w-[108px]" />
              <col className="w-[200px]" />
              <col className="w-[160px]" />
              <col className="w-[106px]" />
            </colgroup>
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
              {list.map((s) => (
                <ServerRow key={s.id} server={s} onOpen={onOpen} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
