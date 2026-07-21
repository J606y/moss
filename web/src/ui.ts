export const card = 'glass rounded-2xl'

// 毛玻璃面板：弹窗 / 下拉等浮层共用的边框 + 背景 + 阴影 + 模糊底座。
// 各处在此基础上追加尺寸、圆角、定位等类名（Modal / Select 下拉）。
export const glassPanel =
  'glass-sheen border border-white/50 bg-white/80 shadow-2xl backdrop-blur-2xl dark:border-white/10 dark:bg-zinc-900/80'

export const btnPrimary =
  'inline-flex items-center gap-1.5 rounded-xl border border-emerald-400/40 bg-emerald-500/85 px-3 py-1.5 text-sm font-medium text-white shadow-lg shadow-emerald-500/20 backdrop-blur-md transition hover:bg-emerald-500 dark:border-emerald-400/25 dark:bg-emerald-500/75 dark:hover:bg-emerald-500/90'

export const btnGhost =
  'glass-sheen inline-flex items-center gap-1.5 rounded-xl border border-white/45 bg-white/40 px-3 py-1.5 text-sm shadow-sm backdrop-blur-md transition hover:bg-white/65 dark:border-white/10 dark:bg-white/5 dark:hover:bg-white/10'

export const btnDanger =
  'inline-flex items-center gap-1.5 rounded-xl border border-rose-300/50 bg-rose-500/10 px-3 py-1.5 text-sm text-rose-600 backdrop-blur-md transition hover:bg-rose-500/20 dark:border-rose-500/25 dark:text-rose-400'

export const iconBtn =
  'rounded-lg p-1.5 text-zinc-500 transition hover:bg-white/55 hover:text-zinc-800 dark:hover:bg-white/10 dark:hover:text-zinc-200'

// text-base sm:text-sm：手机端用 16px 字体，避免 iOS Safari 聚焦输入框时自动放大页面；
// 桌面端（≥640px）恢复 14px 紧凑观感。
export const input =
  'glass-sheen w-full rounded-xl border border-white/50 bg-white/45 px-3 py-1.5 text-base shadow-sm outline-none backdrop-blur-md transition placeholder:text-zinc-400 focus:border-emerald-400/60 focus:ring-2 focus:ring-emerald-400/25 sm:text-sm dark:border-white/10 dark:bg-zinc-900/40 dark:placeholder:text-zinc-600 dark:focus:border-emerald-500/50'

export const formLabel = 'mb-1 block text-xs font-medium text-zinc-500 dark:text-zinc-400'

export const th = 'px-3 py-2 text-left text-xs font-medium text-zinc-500 whitespace-nowrap'

export const td = 'px-3 py-2.5 text-sm whitespace-nowrap'
