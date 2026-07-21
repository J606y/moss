import { useCallback, useEffect, useState } from 'react'
import { GripVertical, Pencil, Plus, Trash2 } from 'lucide-react'
import { del, get, post, put } from '../../api/client'
import type { AdminServer, PingTask } from '../../types'
import { ConfirmDelete, Switch } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { btnPrimary, card, iconBtn, td, th } from '../../ui'
import { useOptimisticList } from '../../hooks/useOptimisticList'
import { useReorder } from '../../hooks/useReorder'
import { emptyTaskForm, TaskFormModal } from './TaskFormModal'
import type { Toast } from './types'

const typeBadge: Record<string, string> = {
  icmp: 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
  tcp: 'bg-sky-500/10 text-sky-600 dark:text-sky-400',
  http: 'bg-amber-500/10 text-amber-600 dark:text-amber-400',
}

export function TasksTab({ toast }: { toast: Toast }) {
  const { items: tasks, setItems: setTasks, mutate } = useOptimisticList<PingTask>([])
  const [servers, setServers] = useState<AdminServer[]>([])
  const [modal, setModal] = useState<'add' | PingTask | null>(null)
  const [confirmDel, setConfirmDel] = useState<PingTask | null>(null)

  const load = useCallback(() => {
    get<PingTask[]>('/api/admin/tasks')
      .then(setTasks)
      .catch((e) => toast(errMsg(e)))
    get<AdminServer[]>('/api/admin/servers')
      .then(setServers)
      .catch(() => {})
  }, [toast, setTasks])
  useEffect(load, [load])

  const serverNames = (ids: string) =>
    ids
      ? ids
          .split(',')
          .map((id) => servers.find((s) => s.id === id)?.name ?? id)
          .join('、')
      : '全部服务器'

  // 拖拽重排：把 fromId 移动到 toId 的位置，乐观更新后持久化 sort
  const { dragId, setDragId, reorder } = useReorder<PingTask, number>({
    items: tasks,
    setItems: setTasks,
    getId: (t) => t.id,
    persist: (ids) => post('/api/admin/tasks/reorder', ids),
    onError: (e) => {
      toast(errMsg(e))
      load()
    },
  })

  const toggle = async (t: PingTask, enabled: boolean) => {
    setTasks((prev) => prev.map((x) => (x.id === t.id ? { ...x, enabled } : x)))
    try {
      await put(`/api/admin/tasks/${t.id}`, { ...t, enabled })
    } catch (e) {
      toast(errMsg(e))
      load()
    }
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between gap-3">
        <p className="text-sm text-zinc-500">对服务器下发 ICMP / TCP / HTTP 探测，生成延迟与可用性图表。</p>
        <button className={`${btnPrimary} shrink-0 whitespace-nowrap`} onClick={() => setModal('add')}>
          <Plus className="h-4 w-4" /> 添加任务
        </button>
      </div>

      <p className="hidden text-xs text-zinc-400 md:block">
        拖拽行首 <GripVertical className="inline h-3 w-3 align-text-bottom" /> 可调整任务顺序，延迟图表按此顺序展示
      </p>

      <div className={`${card} hidden overflow-x-auto md:block`}>
        <table className="w-full min-w-[680px]">
          <thead className="border-b border-zinc-500/15 dark:border-white/10">
            <tr>
              <th className={`${th} w-8`} />
              <th className={th}>名称</th>
              <th className={th}>类型</th>
              <th className={th}>目标</th>
              <th className={th}>间隔</th>
              <th className={th}>应用于</th>
              <th className={th}>启用</th>
              <th className={`${th} text-right`}>操作</th>
            </tr>
          </thead>
          <tbody>
            {tasks.length === 0 && (
              <tr>
                <td className={`${td} text-center text-zinc-400`} colSpan={8}>
                  暂无探测任务
                </td>
              </tr>
            )}
            {tasks.map((t) => (
              <tr
                key={t.id}
                draggable
                onDragStart={() => setDragId(t.id)}
                onDragOver={(e) => dragId !== null && e.preventDefault()}
                onDrop={(e) => {
                  e.preventDefault()
                  if (dragId !== null) reorder(dragId, t.id)
                  setDragId(null)
                }}
                onDragEnd={() => setDragId(null)}
                className={`border-b border-zinc-500/10 transition last:border-0 dark:border-white/5 ${
                  dragId === t.id ? 'opacity-40' : ''
                }`}
              >
                <td className={`${td} text-zinc-300 dark:text-zinc-600`}>
                  <GripVertical className="h-4 w-4 cursor-grab active:cursor-grabbing" />
                </td>
                <td className={`${td} font-medium`}>{t.name}</td>
                <td className={td}>
                  <span className={`rounded-md px-2 py-0.5 text-xs font-medium uppercase ${typeBadge[t.type]}`}>
                    {t.type}
                  </span>
                </td>
                <td className={`${td} tabular-nums text-zinc-500`}>{t.target}</td>
                <td className={`${td} tabular-nums text-zinc-500`}>{t.interval}s</td>
                <td className={`${td} max-w-[220px] !whitespace-normal text-zinc-500`}>{serverNames(t.serverId)}</td>
                <td className={td}>
                  <Switch on={t.enabled} onChange={(v) => toggle(t, v)} />
                </td>
                <td className={`${td} text-right`}>
                  <span className="inline-flex items-center gap-0.5">
                    <button className={iconBtn} title="编辑" onClick={() => setModal(t)}>
                      <Pencil className="h-3.5 w-3.5" />
                    </button>
                    <button className={`${iconBtn} hover:!text-rose-500`} title="删除" onClick={() => setConfirmDel(t)}>
                      <Trash2 className="h-3.5 w-3.5" />
                    </button>
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {/* 移动端卡片列表 */}
      <div className="space-y-2 md:hidden">
        {tasks.length === 0 && (
          <div className={`${card} p-4 text-center text-sm text-zinc-400`}>暂无探测任务</div>
        )}
        {tasks.map((t) => (
          <div key={t.id} className={`${card} space-y-2.5 p-3.5`}>
            <div className="flex items-start justify-between gap-2">
              <div className="flex min-w-0 items-center gap-2 font-medium">
                <span className="truncate">{t.name}</span>
                <span className={`shrink-0 rounded-md px-2 py-0.5 text-xs font-medium uppercase ${typeBadge[t.type]}`}>
                  {t.type}
                </span>
              </div>
              <Switch on={t.enabled} onChange={(v) => toggle(t, v)} />
            </div>
            <dl className="space-y-1.5 text-sm">
              <div className="flex items-start justify-between gap-3">
                <dt className="shrink-0 text-zinc-400">目标</dt>
                <dd className="break-all text-right tabular-nums text-zinc-600 dark:text-zinc-300">{t.target}</dd>
              </div>
              <div className="flex items-center justify-between gap-3">
                <dt className="shrink-0 text-zinc-400">间隔</dt>
                <dd className="tabular-nums text-zinc-600 dark:text-zinc-300">{t.interval}s</dd>
              </div>
              <div className="flex items-start justify-between gap-3">
                <dt className="shrink-0 text-zinc-400">应用于</dt>
                <dd className="text-right text-zinc-600 dark:text-zinc-300">{serverNames(t.serverId)}</dd>
              </div>
            </dl>
            <div className="flex justify-end gap-0.5 border-t border-zinc-500/10 pt-2 dark:border-white/5">
              <button className={iconBtn} title="编辑" onClick={() => setModal(t)}>
                <Pencil className="h-4 w-4" />
              </button>
              <button className={`${iconBtn} hover:!text-rose-500`} title="删除" onClick={() => setConfirmDel(t)}>
                <Trash2 className="h-4 w-4" />
              </button>
            </div>
          </div>
        ))}
      </div>

      {modal === 'add' && (
        <TaskFormModal
          title="添加探测任务"
          init={emptyTaskForm}
          servers={servers}
          onClose={() => setModal(null)}
          onSubmit={async (f) => {
            const tempId = -Date.now()
            const optimistic: PingTask = { ...f, id: tempId, enabled: true }
            setModal(null)
            await mutate(
              (t) => [...t, optimistic],
              async () => {
                const res = await post<{ id: number }>('/api/admin/tasks', { ...f, enabled: true })
                setTasks((t) => t.map((x) => (x.id === tempId ? { ...optimistic, id: res.id } : x)))
                toast('已添加，任务已下发给在线 Agent')
              },
              { onError: (e) => toast(errMsg(e)) },
            )
          }}
        />
      )}
      {modal && modal !== 'add' && (
        <TaskFormModal
          title={`编辑 ${modal.name}`}
          init={{ name: modal.name, type: modal.type, target: modal.target, interval: modal.interval, serverId: modal.serverId }}
          servers={servers}
          onClose={() => setModal(null)}
          onSubmit={async (f) => {
            const id = modal.id
            const enabled = modal.enabled
            setModal(null)
            await mutate(
              (t) => t.map((x) => (x.id === id ? { ...x, ...f, enabled } : x)),
              () => put(`/api/admin/tasks/${id}`, { ...f, enabled }),
              { onSuccess: () => toast('已保存'), onError: (e) => toast(errMsg(e)) },
            )
          }}
        />
      )}

      {confirmDel && (
        <ConfirmDelete
          title="删除探测任务"
          onCancel={() => setConfirmDel(null)}
          onConfirm={() => {
            const target = confirmDel
            setConfirmDel(null)
            mutate(
              (t) => t.filter((x) => x.id !== target.id),
              () => del(`/api/admin/tasks/${target.id}`),
              { onSuccess: () => toast('已删除'), onError: (e) => toast(errMsg(e)) },
            )
          }}
        >
          确定删除任务 <span className="font-semibold">{confirmDel.name}</span>
          ？其历史延迟数据将一并删除，且无法恢复。
        </ConfirmDelete>
      )}
    </div>
  )
}
