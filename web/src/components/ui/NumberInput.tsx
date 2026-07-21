import { ChevronDown, ChevronUp } from 'lucide-react'
import { input } from '../../ui'

/** 数字输入：原生上下箭头已全局隐藏，这里补一组液态玻璃风格的步进按钮 */
export function NumberInput({
  value,
  onChange,
  min,
  max,
  step = 1,
}: {
  value: number
  onChange: (v: number) => void
  min?: number
  max?: number
  step?: number
}) {
  const clamp = (v: number) => {
    if (min !== undefined && v < min) v = min
    if (max !== undefined && v > max) v = max
    return v
  }
  const bump = (d: number) => onChange(clamp((Number(value) || 0) + d))
  const stepBtn =
    'flex flex-1 items-center justify-center rounded-md px-1 text-zinc-400 transition hover:bg-white/55 hover:text-zinc-700 dark:hover:bg-white/10 dark:hover:text-zinc-200'
  return (
    <div className="relative">
      <input
        className={`${input} pr-8`}
        type="number"
        min={min}
        max={max}
        step={step}
        value={value}
        onChange={(e) => onChange(Number(e.target.value) || 0)}
        onBlur={() => onChange(clamp(value))}
      />
      <div className="absolute inset-y-1 right-1 flex w-6 flex-col gap-px">
        <button type="button" tabIndex={-1} title="增加" onClick={() => bump(step)} className={stepBtn}>
          <ChevronUp className="h-3 w-3" />
        </button>
        <button type="button" tabIndex={-1} title="减少" onClick={() => bump(-step)} className={stepBtn}>
          <ChevronDown className="h-3 w-3" />
        </button>
      </div>
    </div>
  )
}
