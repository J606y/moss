import { Globe2, HardDriveDownload, Server, Wifi } from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import type { ReactNode } from 'react'
import { getLive, useServers } from '../api/store'
import { fmtBytes, fmtSpeed } from '../utils/format'
import Ticker from './Ticker'
import { card } from '../ui'

function Stat({
  icon: Icon,
  label,
  value,
  sub,
  tint,
}: {
  icon: LucideIcon
  label: string
  value: ReactNode
  sub?: ReactNode
  tint: string
}) {
  return (
    <div className={`${card} flex items-center gap-3 p-4`}>
      <div className={`flex h-10 w-10 shrink-0 items-center justify-center rounded-lg ${tint}`}>
        <Icon className="h-5 w-5" />
      </div>
      <div className="min-w-0">
        <div className="text-xs text-zinc-500">{label}</div>
        <div className="truncate text-base font-semibold tabular-nums">{value}</div>
        {sub && <div className="truncate text-xs tabular-nums text-zinc-500">{sub}</div>}
      </div>
    </div>
  )
}

export default function StatsBar() {
  const servers = useServers()
  const online = servers.filter((s) => s.online)
  let up = 0
  let down = 0
  let totalUp = 0
  let totalDown = 0
  for (const s of servers) {
    const st = getLive(s.id)
    up += st.netUp
    down += st.netDown
    totalUp += st.totalUp
    totalDown += st.totalDown
  }
  const regions = new Set(servers.map((s) => s.region)).size

  return (
    <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
      <Stat
        icon={Server}
        label="服务器"
        value={`${online.length} / ${servers.length} 在线`}
        sub={servers.length - online.length > 0 ? `${servers.length - online.length} 台离线` : '全部正常'}
        tint="bg-emerald-500/10 text-emerald-600 dark:text-emerald-400"
      />
      <Stat
        icon={Wifi}
        label="实时网速"
        value={<Ticker value={`↑ ${fmtSpeed(up)}`} />}
        sub={<Ticker value={`↓ ${fmtSpeed(down)}`} />}
        tint="bg-sky-500/10 text-sky-600 dark:text-sky-400"
      />
      <Stat
        icon={HardDriveDownload}
        label="总流量"
        value={`↑ ${fmtBytes(totalUp)}`}
        sub={`↓ ${fmtBytes(totalDown)}`}
        tint="bg-violet-500/10 text-violet-600 dark:text-violet-400"
      />
      <Stat
        icon={Globe2}
        label="地区分布"
        value={`${regions} 个地区`}
        sub="点击卡片查看详情"
        tint="bg-amber-500/10 text-amber-600 dark:text-amber-400"
      />
    </div>
  )
}
