import { Globe2, HardDriveDownload, Server, Wifi } from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import { getLive, useAllStatsVersion, useServers } from '../api/store'
import { fmtBytes, fmtSpeed } from '../utils/format'
import { card } from '../ui'
import { useT } from '../i18n'

function Stat({
  icon: Icon,
  label,
  value,
  sub,
  tint,
}: {
  icon: LucideIcon
  label: string
  value: string
  sub?: string
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
  const { t } = useT()
  const servers = useServers()
  useAllStatsVersion() // 全局合计：任意一台 tick 都重算（仅本组件承担）
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
        label={t('dash.stat.servers')}
        value={t('dash.stat.servers.value', { online: online.length, total: servers.length })}
        sub={
          servers.length - online.length > 0
            ? t('dash.stat.servers.offline', { n: servers.length - online.length })
            : t('dash.stat.servers.allOk')
        }
        tint="bg-emerald-500/10 text-emerald-600 dark:text-emerald-400"
      />
      <Stat
        icon={Wifi}
        label={t('dash.stat.speed')}
        value={`↑ ${fmtSpeed(up)}`}
        sub={`↓ ${fmtSpeed(down)}`}
        tint="bg-sky-500/10 text-sky-600 dark:text-sky-400"
      />
      <Stat
        icon={HardDriveDownload}
        label={t('dash.stat.traffic')}
        value={`↑ ${fmtBytes(totalUp)}`}
        sub={`↓ ${fmtBytes(totalDown)}`}
        tint="bg-violet-500/10 text-violet-600 dark:text-violet-400"
      />
      <Stat
        icon={Globe2}
        label={t('dash.stat.regions')}
        value={t('dash.stat.regions.value', { n: regions })}
        sub={t('dash.stat.regions.hint')}
        tint="bg-amber-500/10 text-amber-600 dark:text-amber-400"
      />
    </div>
  )
}
