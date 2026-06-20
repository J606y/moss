import { useEffect, useRef, useState } from 'react'

// 数值滚动计数器：目标值变化后，用 requestAnimationFrame 在 duration 内把「显示值」
// 从当前值平滑补间到新目标，每帧用 format 重新格式化输出。
//
// 这样单台服务器的数字也会像顶部「所有服务器合计」那样高频小步变化 ——
// 配合 tabular-nums，就是「低位数字一位位滚动、高位不动」，而不是每次整段一次性替换。
export default function Ticker({
  value,
  format,
  duration = 700,
  className = '',
}: {
  value: number
  format: (n: number) => string
  duration?: number
  className?: string
}) {
  const [shown, setShown] = useState(value)
  const shownRef = useRef(value)
  shownRef.current = shown

  useEffect(() => {
    const from = shownRef.current
    const to = value
    if (from === to) return
    let raf = 0
    let start = 0
    const tick = (ts: number) => {
      if (!start) start = ts
      const t = Math.min(1, (ts - start) / duration)
      const eased = 1 - Math.pow(1 - t, 3) // easeOutCubic：先快后慢，落点平稳
      setShown(from + (to - from) * eased)
      if (t < 1) raf = requestAnimationFrame(tick)
    }
    raf = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(raf)
  }, [value, duration])

  return <span className={`tabular-nums ${className}`}>{format(shown)}</span>
}
