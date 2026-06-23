import 'flag-icons/css/flag-icons.min.css'

/** SVG 国旗（flag-icons），code 为 ISO 3166-1 alpha-2 国家码（大小写均可），尺寸随 font-size 缩放 */
export default function Flag({ code, className = '' }: { code: string; className?: string }) {
  if (!code) return null
  const c = code.toLowerCase()
  return (
    <span
      className={`fi fi-${c} rounded-[3px] shadow-[0_0_0_1px_rgba(0,0,0,0.12)] ${className}`}
      title={c.toUpperCase()}
    />
  )
}
