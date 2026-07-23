import { memo } from 'react'
import { Link } from 'react-router-dom'
import { ArrowDown, ArrowUp } from 'lucide-react'
import type { ServerMeta } from '../types'
import { pct, useLiveStats } from '../api/store'
import { fmtBytes, fmtPercent, fmtSpeed, fmtUptime } from '../utils/format'
import { ProgressBar } from './ProgressBar'
import Flag from './Flag'
import Ticker from './Ticker'
import { StatusPill } from './ui'
import { card } from '../ui'

function ServerCard({ server }: { server: ServerMeta }) {
  const st = useLiveStats(server.id)
  const memPct = pct(st.memUsed, server.memTotal)
  const diskPct = pct(st.diskUsed, server.diskTotal)

  return (
    <Link
      to={`/server/${server.id}`}
      className={`${card} block p-4 transition hover:-translate-y-0.5 hover:bg-white/40 dark:hover:bg-zinc-900/50 ${
        server.online ? '' : 'opacity-60'
      }`}
    >
      <div className="flex items-center gap-2">
        <Flag code={server.flag} />
        <span className="min-w-0 truncate font-medium">{server.name}</span>
        <span className="ml-auto shrink-0">
          <StatusPill online={server.online} />
        </span>
      </div>
      <div className="mt-1.5 truncate text-xs text-zinc-500">
        {server.os} · {server.virtualization} · {server.arch}
        {server.online && <> · 在线 {fmtUptime(server.uptimeSec)}</>}
      </div>

      <div className="mt-3 space-y-2.5">
        <ProgressBar label="CPU" right={fmtPercent(st.cpu)} pct={st.cpu} />
        <ProgressBar
          label="内存"
          right={`${fmtBytes(st.memUsed)} / ${fmtBytes(server.memTotal)}`}
          pct={memPct}
        />
        <ProgressBar
          label="硬盘"
          right={`${fmtBytes(st.diskUsed)} / ${fmtBytes(server.diskTotal)}`}
          pct={diskPct}
        />
      </div>

      <div className="mt-3 grid grid-cols-2 gap-2 border-t border-zinc-500/10 pt-2.5 text-xs dark:border-white/10">
        <div>
          <div className="text-zinc-400 dark:text-zinc-500">网速</div>
          <div className="mt-0.5 space-y-0.5 tabular-nums text-zinc-600 dark:text-zinc-300">
            <div className="flex items-center gap-1">
              <ArrowUp className="h-3 w-3 text-emerald-500" />
              <Ticker value={st.netUp} format={fmtSpeed} />
            </div>
            <div className="flex items-center gap-1">
              <ArrowDown className="h-3 w-3 text-sky-500" />
              <Ticker value={st.netDown} format={fmtSpeed} />
            </div>
          </div>
        </div>
        <div>
          <div className="text-zinc-400 dark:text-zinc-500">总流量</div>
          <div className="mt-0.5 space-y-0.5 tabular-nums text-zinc-600 dark:text-zinc-300">
            <div className="flex items-center gap-1">
              <ArrowUp className="h-3 w-3 text-emerald-500" />
              {fmtBytes(st.totalUp)}
            </div>
            <div className="flex items-center gap-1">
              <ArrowDown className="h-3 w-3 text-sky-500" />
              {fmtBytes(st.totalDown)}
            </div>
          </div>
        </div>
      </div>
    </Link>
  )
}

export default memo(ServerCard)
