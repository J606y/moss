/**
 * Moss 图标：《流浪地球》MOSS 量子计算机的服务器机柜极简化，机柜同时点题「服务器监控」。
 * 配色完全取自站内 UI 两套主题：
 *  - 机柜：亮色＝亮色玻璃卡片材质（白→zinc-200、zinc 描边），暗色＝暗色卡片材质（zinc-800→zinc-950、白透描边）
 *  - MOSS 之眼：取自主页左下角背景光斑 —— 亮色＝粉（pink-400 系），暗色＝黑红（rose-700→rose-950 余烬感）
 *  - 状态灯：亮色＝sky-600（同首页网速卡图标的蓝，emerald 在白瓷柜身上对比不足），暗色＝emerald-400
 *  - 液态玻璃质感：机柜左上角一道镜面高光（亮色明显、暗色收敛），柜身微透明
 * favicon 无法感知页内主题开关，用 prefers-color-scheme 跟随系统主题近似。
 * 修改造型时同步更新 index.html 里同款绘制的 favicon data URI。
 */
export default function MossEye({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 100 100" className={className} aria-hidden="true">
      <defs>
        <linearGradient id="moss-cab" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopOpacity="0.94" className="[stop-color:#fdfdfd] dark:[stop-color:#27272a]" />
          <stop offset="1" stopOpacity="0.97" className="[stop-color:#e4e4e7] dark:[stop-color:#0c0c0e]" />
        </linearGradient>
        <radialGradient id="moss-eye-halo">
          <stop offset="0" stopOpacity="0.5" className="[stop-color:#f472b6] dark:[stop-color:#be123c]" />
          <stop offset="1" stopOpacity="0" className="[stop-color:#f472b6] dark:[stop-color:#be123c]" />
        </radialGradient>
        <radialGradient id="moss-eye-core" cx="0.4" cy="0.38" r="0.65">
          <stop offset="0" className="[stop-color:#fce7f3] dark:[stop-color:#fb7185]" />
          <stop offset="0.45" className="[stop-color:#f472b6] dark:[stop-color:#9f1239]" />
          <stop offset="1" className="[stop-color:#db2777] dark:[stop-color:#4c0519]" />
        </radialGradient>
        <radialGradient id="moss-cab-sheen">
          <stop offset="0" stopColor="#fff" stopOpacity="0.9" />
          <stop offset="1" stopColor="#fff" stopOpacity="0" />
        </radialGradient>
        <clipPath id="moss-cab-clip">
          <rect x="27" y="10" width="46" height="80" rx="10" />
        </clipPath>
      </defs>
      {/* 机柜本体：与玻璃卡片同材质，微透明 */}
      <rect
        x="27"
        y="10"
        width="46"
        height="80"
        rx="10"
        fill="url(#moss-cab)"
        strokeWidth="1.5"
        className="stroke-zinc-500/40 dark:stroke-white/20"
      />
      {/* 液态玻璃：左上斜向镜面高光，裁剪进柜身 */}
      <g clipPath="url(#moss-cab-clip)">
        <ellipse
          cx="39"
          cy="19"
          rx="27"
          ry="13"
          fill="url(#moss-cab-sheen)"
          transform="rotate(-16 39 19)"
          className="opacity-60 dark:opacity-20"
        />
      </g>
      {/* MOSS 之眼：随主题取色（亮=粉 / 暗=黑红），唯一焦点 */}
      <circle cx="50" cy="36" r="15" fill="url(#moss-eye-halo)" />
      <circle cx="50" cy="36" r="7.5" fill="url(#moss-eye-core)" />
      <circle cx="47.5" cy="33" r="2" fill="#fff" className="opacity-75 dark:opacity-50" />
      {/* 下方两个机架单元：分隔线 + emerald 状态灯 + 格栅线 */}
      <line x1="33" y1="60" x2="67" y2="60" strokeWidth="2" strokeLinecap="round" className="stroke-zinc-500/35 dark:stroke-white/25" />
      <line x1="33" y1="75" x2="67" y2="75" strokeWidth="2" strokeLinecap="round" className="stroke-zinc-500/35 dark:stroke-white/25" />
      <circle cx="36.5" cy="67.5" r="2.3" className="fill-sky-600 dark:fill-emerald-400" />
      <line x1="44" y1="67.5" x2="64" y2="67.5" strokeWidth="3" strokeLinecap="round" className="stroke-zinc-500/35 dark:stroke-white/25" />
      <circle cx="36.5" cy="82.5" r="2.3" opacity="0.55" className="fill-sky-600 dark:fill-emerald-400" />
      <line x1="44" y1="82.5" x2="64" y2="82.5" strokeWidth="3" strokeLinecap="round" className="stroke-zinc-500/35 dark:stroke-white/25" />
    </svg>
  )
}
