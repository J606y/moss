import { useCallback, useEffect, useState } from 'react'
import { GripVertical, Pencil, Plus, Trash2 } from 'lucide-react'
import { del, get, post, put } from '../../api/client'
import type { AdminServer, PingTask } from '../../types'
import { ConfirmDelete, Switch } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { btnPrimary, card, iconBtn, td, th } from '../../ui'
import { useOptimisticList } from '../../hooks/useOptimisticList'
import { useReorder } from '../../hooks/useReorder'
import { useT } from '../../i18n'
import { emptyTaskForm, TaskFormModal } from './TaskFormModal'
import type { Toast } from './types'

const typeBadge: Record<string, string> = {
  icmp: 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
  tcp: 'bg-sky-500/10 text-sky-600 dark:text-sky-400',
  http: 'bg-amber-500/10 text-amber-600 dark:text-amber-400',
}

export function TasksTab({ toast }: { toast: Toast }) {
  const { t } = useT()
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

  /**
   * 「应用于」列的展示。
   *
   * 不把机器名全列出来：八台机器就是四行文字，那一行的高度是相邻行的四倍，
   * 整张表参差不齐，而挨个读完这串名字本来也不是在这里要做的事。
   * 超过两台只报数量，完整名单挂在 title 上；要改选哪几台去编辑弹窗。
   */
  const serverScope = (ids: string) => {
    if (!ids) return { text: t('task.scope.all'), title: t('task.scope.all') }
    const names = ids.split(',').map((id) => servers.find((s) => s.id === id)?.name ?? id)
    const full = names.join(t('list.sep'))
    return { text: names.length > 2 ? t('task.scope.count', { n: names.length }) : full, title: full }
  }

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

  const toggle = async (task: PingTask, enabled: boolean) => {
    setTasks((prev) => prev.map((x) => (x.id === task.id ? { ...x, enabled } : x)))
    try {
      await put(`/api/admin/tasks/${task.id}`, { ...task, enabled })
    } catch (e) {
      toast(errMsg(e))
      load()
    }
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between gap-3">
        <p className="text-sm text-zinc-500">{t('task.intro')}</p>
        <button className={`${btnPrimary} shrink-0 whitespace-nowrap`} onClick={() => setModal('add')}>
          <Plus className="h-4 w-4" /> {t('task.add')}
        </button>
      </div>

      <p className="hidden text-xs text-zinc-400 md:block">
        {t('task.dragHint.before')}
        <GripVertical className="inline h-3 w-3 align-text-bottom" />
        {t('task.dragHint.after')}
      </p>

      <div className={`${card} hidden overflow-x-auto md:block`}>
        <table className="w-full min-w-[680px]">
          <thead className="border-b border-zinc-500/15 dark:border-white/10">
            <tr>
              <th className={`${th} w-8`} />
              <th className={th}>{t('dash.col.name')}</th>
              <th className={th}>{t('task.type')}</th>
              <th className={th}>{t('task.col.target')}</th>
              <th className={th}>{t('task.col.interval')}</th>
              <th className={th}>{t('task.scope')}</th>
              <th className={th}>{t('task.col.enabled')}</th>
              <th className={`${th} text-right`}>{t('task.col.actions')}</th>
            </tr>
          </thead>
          <tbody>
            {tasks.length === 0 && (
              <tr>
                <td className={`${td} text-center text-zinc-400`} colSpan={8}>
                  {t('task.empty')}
                </td>
              </tr>
            )}
            {tasks.map((task) => (
              <tr
                key={task.id}
                draggable
                onDragStart={() => setDragId(task.id)}
                onDragOver={(e) => dragId !== null && e.preventDefault()}
                onDrop={(e) => {
                  e.preventDefault()
                  if (dragId !== null) reorder(dragId, task.id)
                  setDragId(null)
                }}
                onDragEnd={() => setDragId(null)}
                className={`border-b border-zinc-500/10 transition last:border-0 dark:border-white/5 ${
                  dragId === task.id ? 'opacity-40' : ''
                }`}
              >
                <td className={`${td} text-zinc-300 dark:text-zinc-600`}>
                  <GripVertical className="h-4 w-4 cursor-grab active:cursor-grabbing" />
                </td>
                <td className={`${td} font-medium`}>{task.name}</td>
                <td className={td}>
                  <span className={`rounded-md px-2 py-0.5 text-xs font-medium uppercase ${typeBadge[task.type]}`}>
                    {task.type}
                  </span>
                </td>
                <td className={`${td} tabular-nums text-zinc-500`}>{task.target}</td>
                <td className={`${td} tabular-nums text-zinc-500`}>{task.interval}s</td>
                <td className={`${td} max-w-[220px] truncate text-zinc-500`} title={serverScope(task.serverId).title}>
                  {serverScope(task.serverId).text}
                </td>
                <td className={td}>
                  <Switch on={task.enabled} onChange={(v) => toggle(task, v)} />
                </td>
                <td className={`${td} text-right`}>
                  <span className="inline-flex items-center gap-0.5">
                    <button className={iconBtn} title={t('common.edit')} onClick={() => setModal(task)}>
                      <Pencil className="h-3.5 w-3.5" />
                    </button>
                    <button
                      className={`${iconBtn} hover:!text-rose-500`}
                      title={t('common.delete')}
                      onClick={() => setConfirmDel(task)}
                    >
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
          <div className={`${card} p-4 text-center text-sm text-zinc-400`}>{t('task.empty')}</div>
        )}
        {tasks.map((task) => (
          <div key={task.id} className={`${card} space-y-2.5 p-3.5`}>
            <div className="flex items-start justify-between gap-2">
              <div className="flex min-w-0 items-center gap-2 font-medium">
                <span className="truncate">{task.name}</span>
                <span
                  className={`shrink-0 rounded-md px-2 py-0.5 text-xs font-medium uppercase ${typeBadge[task.type]}`}
                >
                  {task.type}
                </span>
              </div>
              <Switch on={task.enabled} onChange={(v) => toggle(task, v)} />
            </div>
            <dl className="space-y-1.5 text-sm">
              <div className="flex items-start justify-between gap-3">
                <dt className="shrink-0 text-zinc-400">{t('task.col.target')}</dt>
                <dd className="break-all text-right tabular-nums text-zinc-600 dark:text-zinc-300">{task.target}</dd>
              </div>
              <div className="flex items-center justify-between gap-3">
                <dt className="shrink-0 text-zinc-400">{t('task.col.interval')}</dt>
                <dd className="tabular-nums text-zinc-600 dark:text-zinc-300">{task.interval}s</dd>
              </div>
              <div className="flex items-start justify-between gap-3">
                <dt className="shrink-0 text-zinc-400">{t('task.scope')}</dt>
                {/* 手机上没有 hover，看不了 title；要确认是哪几台就去编辑弹窗 */}
                <dd className="text-right text-zinc-600 dark:text-zinc-300">{serverScope(task.serverId).text}</dd>
              </div>
            </dl>
            <div className="flex justify-end gap-0.5 border-t border-zinc-500/10 pt-2 dark:border-white/5">
              <button className={iconBtn} title={t('common.edit')} onClick={() => setModal(task)}>
                <Pencil className="h-4 w-4" />
              </button>
              <button
                className={`${iconBtn} hover:!text-rose-500`}
                title={t('common.delete')}
                onClick={() => setConfirmDel(task)}
              >
                <Trash2 className="h-4 w-4" />
              </button>
            </div>
          </div>
        ))}
      </div>

      {modal === 'add' && (
        <TaskFormModal
          title={t('task.modal.add')}
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
                toast(t('task.added'))
              },
              { onError: (e) => toast(errMsg(e)) },
            )
          }}
        />
      )}
      {modal && modal !== 'add' && (
        <TaskFormModal
          title={t('task.modal.edit', { name: modal.name })}
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
              { onSuccess: () => toast(t('common.saved')), onError: (e) => toast(errMsg(e)) },
            )
          }}
        />
      )}

      {confirmDel && (
        <ConfirmDelete
          title={t('task.del.title')}
          onCancel={() => setConfirmDel(null)}
          onConfirm={() => {
            const target = confirmDel
            setConfirmDel(null)
            mutate(
              (t) => t.filter((x) => x.id !== target.id),
              () => del(`/api/admin/tasks/${target.id}`),
              { onSuccess: () => toast(t('common.deleted')), onError: (e) => toast(errMsg(e)) },
            )
          }}
        >
          {t('task.del.before')}
          <span className="font-semibold">{confirmDel.name}</span>
          {t('task.del.after')}
        </ConfirmDelete>
      )}
    </div>
  )
}
