import { useEffect, useState } from 'react'
import { safeLocalGet, safeLocalSet } from './utils/storage'

export function useTheme() {
  const [dark, setDark] = useState<boolean>(() => {
    const saved = safeLocalGet('moss-theme')
    if (saved) return saved === 'dark'
    return window.matchMedia('(prefers-color-scheme: dark)').matches
  })

  useEffect(() => {
    document.documentElement.classList.toggle('dark', dark)
    safeLocalSet('moss-theme', dark ? 'dark' : 'light')
  }, [dark])

  // 跨标签页/多实例同步：另一个标签切主题时，storage 事件同步本标签状态
  useEffect(() => {
    const onStorage = (e: StorageEvent) => {
      if (e.key !== 'moss-theme' || e.newValue == null) return
      setDark(e.newValue === 'dark')
    }
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

  return { dark, toggle: () => setDark((d) => !d) }
}
