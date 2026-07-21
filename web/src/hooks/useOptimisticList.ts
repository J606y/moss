import { useState } from 'react'

export interface OptimisticOpts {
  onSuccess?: () => void
  onError?: (e: unknown) => void
}

/**
 * 列表的乐观更新：持有列表状态，并提供 mutate 做「先改本地、失败回滚到操作前快照」。
 * 适用于编辑 / 删除 / 新增等有明确前态可回滚的操作；成功/失败回调里做提示与后续副作用。
 */
export function useOptimisticList<T>(initial: T[] = []) {
  const [items, setItems] = useState<T[]>(initial)

  /**
   * 乐观执行一次变更：
   *  1) 记录操作前快照 prev，立即用 apply(prev) 更新本地列表；
   *  2) 发起 request；成功触发 onSuccess，失败则回滚到 prev 并触发 onError。
   * 返回的 Promise 便于调用方 await（如保持表单 busy 态）。
   */
  const mutate = async (
    apply: (prev: T[]) => T[],
    request: () => Promise<unknown>,
    opts: OptimisticOpts = {},
  ) => {
    const prev = items
    setItems((cur) => apply(cur))
    try {
      await request()
      opts.onSuccess?.()
    } catch (e) {
      setItems(prev)
      opts.onError?.(e)
    }
  }

  return { items, setItems, mutate }
}
