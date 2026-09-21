/** 简单的 fetch 封装：JSON 收发，非 2xx 抛出带 status 的错误 */

export interface ApiError extends Error {
  status: number
  /**
   * 后端错误码，如 `auth.bad_credentials`。语言中立，供界面按访客语言翻译。
   *
   * 可能缺失：后端没登记这条错误，或它压根不是一条结构化错误（网络失败、
   * 非 JSON 响应）。缺失时界面退回显示 message 里的中文原文——漏一条只是
   * 不翻译，不会变成空白。契约见 docs/error-codes.md。
   */
  code?: string
  /** 不翻译的那一截：机器名、版本号、Go error 原文。译文里用 {detail} 接住。 */
  detail?: string
}

export async function api<T = unknown>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    cache: 'no-store', // API 实时数据不走浏览器缓存，避免增删后列表拿到旧响应
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  const body = await res.json().catch(() => null)
  if (!res.ok) {
    const e = body as { error?: string; code?: string; detail?: string } | null
    // message 始终取后端的中文兜底串：认不出码时它就是界面上显示的内容。
    const err = new Error(e?.error ?? `HTTP ${res.status}`) as ApiError
    err.status = res.status
    if (e?.code) err.code = e.code
    if (e?.detail) err.detail = e.detail
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
