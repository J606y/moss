import { useEffect, useRef, useState } from 'react'
import { Check, ChevronDown } from 'lucide-react'
import { glassPanel, input } from '../../ui'

/** 液态玻璃下拉选择，替代原生 <select>，下拉面板与弹窗同款毛玻璃质感 */
export function Select<T extends string>({
  value,
  options,
  onChange,
}: {
  value: T
  options: Array<{ value: T; label: string }>
  onChange: (v: T) => void
}) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDoc)
    return () => document.removeEventListener('mousedown', onDoc)
  }, [open])
  const current = options.find((o) => o.value === value)
  return (
    <div ref={ref} className="relative">
      <button type="button" onClick={() => setOpen((v) => !v)} className={`${input} flex items-center justify-between text-left`}>
        <span>{current?.label ?? value}</span>
        <ChevronDown className={`h-4 w-4 shrink-0 text-zinc-400 transition ${open ? 'rotate-180' : ''}`} />
      </button>
      {open && (
        <div className={`${glassPanel} absolute z-20 mt-1 w-full overflow-hidden rounded-xl p-1`}>
          {options.map((o) => {
            const active = o.value === value
            return (
              <button
                key={o.value}
                type="button"
                onClick={() => {
                  onChange(o.value)
                  setOpen(false)
                }}
                className={`flex w-full items-center justify-between rounded-lg px-2.5 py-1.5 text-left text-sm transition hover:bg-white/55 dark:hover:bg-white/10 ${
                  active ? 'font-medium text-emerald-600 dark:text-emerald-400' : ''
                }`}
              >
                {o.label}
                {active && <Check className="h-3.5 w-3.5" />}
              </button>
            )
          })}
        </div>
      )}
    </div>
  )
}
