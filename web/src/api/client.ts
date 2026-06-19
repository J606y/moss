/** 简单的 fetch 封装：JSON 收发，非 2xx 抛出带 status 的错误 */

export interface ApiError extends Error {
  status: number
}

export async function api<T = unknown>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  const body = await res.json().catch(() => null)
  if (!res.ok) {
    const err = new Error(
      (body as { error?: string } | null)?.error ?? `HTTP ${res.status}`,
    ) as ApiError
    err.status = res.status
    throw err
  }
  return body as T
}

export const get = <T>(path: string) => api<T>(path)
export const post = <T = unknown>(path: string, body?: unknown) =>
  api<T>(path, { method: 'POST', body: JSON.stringify(body ?? {}) })
export const put = <T = unknown>(path: string, body?: unknown) =>
  api<T>(path, { method: 'PUT', body: JSON.stringify(body ?? {}) })
export const del = <T = unknown>(path: string) => api<T>(path, { method: 'DELETE' })
