import type { ReactNode } from 'react'
import { fmtDateTime } from '../utils/format'
import { card } from '../ui'

export const palette = {
  green: '#10b981',
  sky: '#0ea5e9',
  amber: '#f59e0b',
  rose: '#f43f5e',
  violet: '#8b5cf6',
}

/** Recharts 自定义 Tooltip，适配明暗主题 */
export function ChartTip({
  active,
  payload,
  label,
  fmt,
}: {
  active?: boolean
  payload?: Array<{ dataKey: string; name?: string; value: number | null; color?: string; stroke?: string }>
  label?: number | string
  fmt?: (value: number, key: string) => string
}) {
  if (!active || !payload || payload.length === 0) return null
  return (
    <div className="rounded-xl border border-white/50 bg-white/75 px-3 py-2 text-xs shadow-lg backdrop-blur-xl dark:border-white/10 dark:bg-zinc-900/75">
      <div className="mb-1 text-zinc-500">{typeof label === 'number' ? fmtDateTime(label) : label}</div>
      {payload.map((p) => (
        <div key={p.dataKey} className="flex items-center gap-2 py-0.5">
          <span className="inline-block h-2 w-2 rounded-full" style={{ background: p.color ?? p.stroke }} />
          <span className="text-zinc-500">{p.name ?? p.dataKey}</span>
          <span className="ml-auto pl-4 font-medium tabular-nums text-zinc-800 dark:text-zinc-100">
            {p.value == null ? '丢包' : fmt ? fmt(p.value, p.dataKey) : p.value}
          </span>
        </div>
      ))}
    </div>
  )
}

export function SeriesChips({ items }: { items: Array<{ name: string; color: string }> }) {
  return (
    <div className="flex items-center gap-3">
      {items.map((it) => (
        <span key={it.name} className="flex items-center gap-1.5 text-xs text-zinc-500">
          <span className="inline-block h-2 w-2 rounded-full" style={{ background: it.color }} />
          {it.name}
        </span>
      ))}
    </div>
  )
}

export function ChartCard({
  title,
  right,
  children,
}: {
  title: string
  right?: ReactNode
  children: ReactNode
}) {
  return (
    <div className={`${card} p-4`}>
      <div className="mb-3 flex items-center justify-between">
        <h3 className="text-sm font-medium text-zinc-700 dark:text-zinc-200">{title}</h3>
        {right}
      </div>
      <div className="h-56">{children}</div>
    </div>
  )
}

export const axisProps = {
  stroke: '#71717a',
  fontSize: 11,
  tickLine: false,
  axisLine: false,
} as const

export const gridStroke = '#88888830'
