import { useEffect, useState } from 'react'
import { safeLocalGet, safeLocalSet } from './utils/storage'

export type ThemeMode = 'light' | 'dark' | 'auto'

const DARK_QUERY = '(prefers-color-scheme: dark)'

function parseMode(val: string | null): ThemeMode {
  return val === 'light' || val === 'dark' ? val : 'auto'
}

export function useTheme() {
  const [mode, setMode] = useState<ThemeMode>(() => parseMode(safeLocalGet('moss-theme')))
  const [systemDark, setSystemDark] = useState<boolean>(() => window.matchMedia(DARK_QUERY).matches)

  const dark = mode === 'dark' || (mode === 'auto' && systemDark)

  useEffect(() => {
    document.documentElement.classList.toggle('dark', dark)
  }, [dark])

  useEffect(() => {
    safeLocalSet('moss-theme', mode)
  }, [mode])

  // 自动档跟随系统：系统亮暗切换时实时生效
  useEffect(() => {
    const mq = window.matchMedia(DARK_QUERY)
    const onChange = (e: MediaQueryListEvent) => setSystemDark(e.matches)
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [])

  // 跨标签页/多实例同步：另一个标签切主题时，storage 事件同步本标签状态
  useEffect(() => {
    const onStorage = (e: StorageEvent) => {
      if (e.key !== 'moss-theme' || e.newValue == null) return
      setMode(parseMode(e.newValue))
    }
    window.addEventListener('storage', onStorage)
    return () => window.removeEventListener('storage', onStorage)
  }, [])

  return {
    mode,
    dark,
    cycle: () => setMode((m) => (m === 'auto' ? 'light' : m === 'light' ? 'dark' : 'auto')),
  }
}
