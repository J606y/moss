import { Check } from 'lucide-react'

/** 液态玻璃勾选框（纯展示，点击交给外层容器处理）：未选=毛玻璃，选中=祖母绿 */
export function CheckBox({ checked }: { checked: boolean }) {
  return (
    <span
      className={`flex h-4 w-4 shrink-0 items-center justify-center rounded-[5px] border transition ${
        checked
          ? 'border-emerald-400/40 bg-emerald-500/85 text-white shadow-sm shadow-emerald-500/20'
          : 'glass-control'
      }`}
    >
      {checked && <Check className="h-3 w-3" strokeWidth={3} />}
    </span>
  )
}
