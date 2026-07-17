export function barColor(pct: number): string {
  if (pct < 60) return '#10b981'
  if (pct < 85) return '#f59e0b'
  return '#f43f5e'
}

/** 把任意输入钳制到 0~100，非有限值（NaN/Infinity）兜底为 0 */
export const clampPct = (pct: number): number =>
  Number.isFinite(pct) ? Math.min(100, Math.max(0, pct)) : 0

export function ProgressBar({ label, right, pct }: { label: string; right: string; pct: number }) {
  const p = clampPct(pct)
  return (
    <div>
      <div className="mb-1 flex items-baseline justify-between text-xs">
        <span className="text-zinc-500">{label}</span>
        <span className="tabular-nums text-zinc-600 dark:text-zinc-300">{right}</span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-zinc-500/15 dark:bg-white/10">
        <div
          className="h-full rounded-full transition-all duration-700"
          style={{ width: `${p}%`, background: barColor(p) }}
        />
      </div>
    </div>
  )
}

export function MiniBar({ pct }: { pct: number }) {
  const p = clampPct(pct)
  return (
    <div className="flex items-center gap-2">
      <div className="h-1.5 w-14 overflow-hidden rounded-full bg-zinc-500/15 dark:bg-white/10">
        <div className="h-full rounded-full" style={{ width: `${p}%`, background: barColor(p) }} />
      </div>
      {/* 固定宽度：位数变化（9%↔10%↔100%）不再影响表格列宽 */}
      <span className="w-8 shrink-0 text-right text-xs tabular-nums text-zinc-600 dark:text-zinc-300">{p.toFixed(0)}%</span>
    </div>
  )
}
