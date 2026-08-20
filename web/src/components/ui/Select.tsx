import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Check, ChevronDown } from 'lucide-react'
import { glassPanel, input } from '../../ui'

/** 下拉面板与触发按钮之间的间距，与原来的 mt-1 一致。 */
const GAP = 4

/**
 * 液态玻璃下拉选择，替代原生 <select>。
 *
 * **下拉面板挂到 document.body，不能就地渲染。**
 * 就地渲染时它要同时受制于祖先与兄弟：
 *   - 祖先带 overflow-hidden → 面板被裁掉一半；
 *   - 祖先或兄弟带 backdrop-filter（本项目的 .glass 就有）→ 各自形成独立的层叠上下文，
 *     面板的 z-index 只在自己那张卡片内部有效，压不过 DOM 里排在后面的卡片。
 *
 * 这两种情况在本项目里都真实出现过。挂到 body 之后，使用者不必再关心
 * 自己被放在什么容器里——这与 Modal 的处理是同一个理由。
 *
 * 代价是位置要自己算：打开时按触发按钮定位，之后跟着它走。
 */
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
  const [rect, setRect] = useState<{ top: number; left: number; width: number } | null>(null)
  const ref = useRef<HTMLDivElement>(null)
  const panelRef = useRef<HTMLDivElement>(null)

  const place = useCallback(() => {
    const el = ref.current
    if (!el) return
    const r = el.getBoundingClientRect()
    // 同值不重设：面板自身内部滚动也会触发重定位，位置没变就别白渲染一次。
    setRect((prev) =>
      prev && prev.top === r.bottom + GAP && prev.left === r.left && prev.width === r.width
        ? prev
        : { top: r.bottom + GAP, left: r.left, width: r.width },
    )
  }, [])

  // 位置必须在浏览器绘制前算好，否则面板会先闪现在左上角再跳到位。
  useLayoutEffect(() => {
    if (open) place()
  }, [open, place])

  useEffect(() => {
    if (!open) return
    const onDoc = (e: MouseEvent) => {
      const t = e.target as Node
      // 面板已不在触发器的 DOM 子树里，两处都要判，否则点选项会被当成"点了外面"
      if (ref.current?.contains(t) || panelRef.current?.contains(t)) return
      setOpen(false)
    }
    // 滚动时跟着触发按钮走，而不是关掉下拉。
    //
    // 原来是一滚就关：鼠标停在下拉上随手滚一格，选项就没了——而"滚动着找选项"
    // 恰恰是面对一列机器名时最自然的动作，选不中任何东西。
    // 只有触发按钮整个滚出视口才关闭：那时面板悬在半空，已经指不到任何东西了。
    let frame = 0
    const follow = () => {
      if (frame) return // 每帧至多重算一次，滚动时不至于抖
      frame = requestAnimationFrame(() => {
        frame = 0
        const el = ref.current
        if (!el) return
        const r = el.getBoundingClientRect()
        if (r.bottom < 0 || r.top > window.innerHeight) {
          setOpen(false)
          return
        }
        place()
      })
    }
    document.addEventListener('mousedown', onDoc)
    // capture: true —— 内层可滚动容器的滚动不冒泡到 window，不捕获就漏掉
    window.addEventListener('scroll', follow, true)
    window.addEventListener('resize', follow)
    return () => {
      document.removeEventListener('mousedown', onDoc)
      window.removeEventListener('scroll', follow, true)
      window.removeEventListener('resize', follow)
      if (frame) cancelAnimationFrame(frame)
    }
  }, [open, place])

  const current = options.find((o) => o.value === value)

  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className={`${input} press flex items-center justify-between text-left`}
      >
        <span className="truncate">{current?.label ?? value}</span>
        <ChevronDown className={`h-4 w-4 shrink-0 text-zinc-400 transition ${open ? 'rotate-180' : ''}`} />
      </button>
      {open &&
        rect &&
        createPortal(
          <div
            ref={panelRef}
            className={`${glassPanel} fixed z-50 max-h-64 overflow-y-auto rounded-xl p-1`}
            style={{ top: rect.top, left: rect.left, width: rect.width }}
          >
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
                  className={`flex w-full items-center justify-between gap-2 rounded-lg px-2.5 py-1.5 text-left text-sm transition duration-100 select-none hover:bg-white/55 active:bg-white/80 dark:hover:bg-white/10 dark:active:bg-white/15 ${
                    active ? 'font-medium text-emerald-600 dark:text-emerald-400' : ''
                  }`}
                >
                  <span className="truncate">{o.label}</span>
                  {active && <Check className="h-3.5 w-3.5 shrink-0" />}
                </button>
              )
            })}
          </div>,
          document.body,
        )}
    </div>
  )
}
