/** 在线 / 离线状态徽章：三端（Admin / ServerDetail / Dashboard / ServerCard）通用 */
export function StatusPill({ online }: { online: boolean }) {
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium ${
        online
          ? 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400'
          : 'bg-rose-500/10 text-rose-600 dark:text-rose-400'
      }`}
    >
      <span className={`h-1.5 w-1.5 rounded-full ${online ? 'animate-pulse bg-emerald-500' : 'bg-rose-500'}`} />
      {online ? '在线' : '离线'}
    </span>
  )
}
