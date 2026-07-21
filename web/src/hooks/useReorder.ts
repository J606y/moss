import { useState } from 'react'

/**
 * 拖拽重排：把 fromId 移动到 toId 的位置，乐观更新后持久化顺序；
 * 持久化失败交给 onError 处理（通常是提示 + 重新拉取）。
 * 泛化 id 类型（string | number），服务器与探测任务共用同一套逻辑。
 */
export function useReorder<T, ID extends string | number>({
  items,
  setItems,
  getId,
  persist,
  onError,
}: {
  items: T[]
  setItems: (next: T[]) => void
  getId: (item: T) => ID
  persist: (ids: ID[]) => Promise<unknown>
  onError: (e: unknown) => void
}) {
  const [dragId, setDragId] = useState<ID | null>(null)

  const reorder = async (fromId: ID, toId: ID) => {
    if (fromId === toId) return
    const from = items.findIndex((it) => getId(it) === fromId)
    const to = items.findIndex((it) => getId(it) === toId)
    if (from < 0 || to < 0) return
    const next = [...items]
    const [moved] = next.splice(from, 1)
    next.splice(to, 0, moved)
    setItems(next)
    try {
      await persist(next.map(getId))
    } catch (e) {
      onError(e)
    }
  }

  return { dragId, setDragId, reorder }
}
