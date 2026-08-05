import { Check } from 'lucide-react'

/**
 * 液态玻璃勾选框（纯展示，点击交给外层容器处理）：未选=毛玻璃，选中=祖母绿。
 *
 * 按下反馈由 group-active 驱动：使用它的都是整行按钮，整行缩放会让文字跟着位移，
 * 所以行只加深背景，缩放收敛到这个 16px 的方块上——反馈落在真正的点击目标上。
 * 未被 group 容器包裹时该变体不生效，不会出错。
 */
export function CheckBox({ checked }: { checked: boolean }) {
  return (
    <span
      className={`flex h-4 w-4 shrink-0 items-center justify-center rounded-[5px] border transition duration-100 group-active:scale-90 ${
        checked
          ? 'border-emerald-400/40 bg-emerald-500/85 text-white shadow-sm shadow-emerald-500/20'
          : 'glass-control'
      }`}
    >
      {checked && <Check className="h-3 w-3" strokeWidth={3} />}
    </span>
  )
}
