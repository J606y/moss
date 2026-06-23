/**
 * localStorage 安全包装：隐私模式/禁用存储/配额超限等场景下 localStorage 访问会抛
 * SecurityError、QuotaExceededError 等异常，这里统一吞掉，避免连累整页渲染。
 */
export function safeLocalGet(key: string): string | null {
  try {
    return localStorage.getItem(key)
  } catch {
    return null
  }
}

export function safeLocalSet(key: string, val: string): void {
  try {
    localStorage.setItem(key, val)
  } catch {
    // 读写失败时静默忽略（隐私模式 / 配额超限）
  }
}
