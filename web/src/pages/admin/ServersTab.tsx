import { useCallback, useEffect, useState } from 'react'
import { Eye, EyeOff, GripVertical, Pencil, Play, Plus, Terminal, Trash2 } from 'lucide-react'
import { del, get, post, put } from '../../api/client'
import type { AdminServer } from '../../types'
import { CmdBlock, ConfirmDelete, CopyBtn, Modal, StatusPill } from '../../components/ui'
import Flag from '../../components/Flag'
import { errMsg, installCmds, maskIp } from '../../utils/admin'
import { btnGhost, btnPrimary, card, iconBtn, input, td, th } from '../../ui'
import { useOptimisticList } from '../../hooks/useOptimisticList'
import { useReorder } from '../../hooks/useReorder'
import { emptyServerForm, ServerFormModal } from './ServerFormModal'
import type { Toast } from './types'

export function ServersTab({ toast }: { toast: Toast }) {
  const { items: list, setItems: setList, mutate } = useOptimisticList<AdminServer>([])
  const [modal, setModal] = useState<'add' | AdminServer | null>(null)
  const [install, setInstall] = useState<{ name: string; token: string } | null>(null)
  const [confirmDel, setConfirmDel] = useState<AdminServer | null>(null)
  const [revealed, setRevealed] = useState<Set<string>>(new Set())
  const [search, setSearch] = useState('')

  const load = useCallback(() => {
    get<AdminServer[]>('/api/admin/servers')
      .then(setList)
      .catch((e) => toast(errMsg(e)))
  }, [toast, setList])
  useEffect(load, [load])

  const filtered = list.filter(
    (s) => !search || s.name.toLowerCase().includes(search.toLowerCase()) || s.region.includes(search),
  )

  // 拖拽重排：把 fromId 移动到 toId 的位置，乐观更新后持久化 sort
  const { dragId, setDragId, reorder } = useReorder<AdminServer, string>({
    items: list,
    setItems: setList,
    getId: (s) => s.id,
    persist: (ids) => post('/api/admin/servers/reorder', ids),
    onError: (e) => {
      toast(errMsg(e))
      load()
    },
  })

  const toggleReveal = (id: string) => {
    setRevealed((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  // GCP 手动开机：后端会先查实例状态，RUNNING 时不会重复开机
  const [gcpStarting, setGcpStarting] = useState<string | null>(null)
  const gcpStart = async (s: AdminServer) => {
    setGcpStarting(s.id)
    try {
      const res = await post<{ message: string }>(`/api/admin/servers/${s.id}/gcp-start`)
      toast(res.message)
      load()
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setGcpStarting(null)
    }
  }
  const gcpTitle = (s: AdminServer) => {
    let t = 'GCP 立即开机'
    if (s.gcpTries > 0) {
      t += ` | 已自动尝试 ${s.gcpTries} 次`
      if (s.gcpLastTry > 0) t += `，最近 ${new Date(s.gcpLastTry * 1000).toLocaleTimeString()}`
    }
    if (s.gcpLastErr) t += ` | ${s.gcpLastErr}`
    return t
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between gap-2">
        <input
          className={`${input} max-w-60`}
          placeholder="搜索名称 / 地区…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <button className={`${btnPrimary} shrink-0 whitespace-nowrap`} onClick={() => setModal('add')}>
          <Plus className="h-4 w-4" /> 添加服务器
        </button>
      </div>

      <p className="hidden text-xs text-zinc-400 md:block">
        拖拽行首 <GripVertical className="inline h-3 w-3 align-text-bottom" /> 可调整服务器顺序，首页按此顺序展示{search ? '（搜索时暂不可拖拽）' : ''}
      </p>

      <div className={`${card} hidden overflow-x-auto md:block`}>
        <table className="w-full min-w-[760px]">
          <thead className="border-b border-zinc-500/15 dark:border-white/10">
            <tr>
              <th className={`${th} w-8`} />
              <th className={th}>名称</th>
              <th className={th}>分组</th>
              <th className={th}>状态</th>
              <th className={th}>IP 地址</th>
              <th className={th}>到期</th>
              <th className={`${th} text-right`}>操作</th>
            </tr>
          </thead>
          <tbody>
            {filtered.length === 0 && (
              <tr>
                <td className={`${td} text-center text-zinc-400`} colSpan={7}>
                  暂无服务器，点击右上角「添加服务器」开始
                </td>
              </tr>
            )}
            {filtered.map((s) => {
              const shown = revealed.has(s.id)
              const draggable = !search
              return (
                <tr
                  key={s.id}
                  draggable={draggable}
                  onDragStart={() => draggable && setDragId(s.id)}
                  onDragOver={(e) => dragId && e.preventDefault()}
                  onDrop={(e) => {
                    e.preventDefault()
                    if (dragId) reorder(dragId, s.id)
                    setDragId(null)
                  }}
                  onDragEnd={() => setDragId(null)}
                  className={`border-b border-zinc-500/10 transition last:border-0 dark:border-white/5 ${
                    dragId === s.id ? 'opacity-40' : ''
                  }`}
                >
                  <td className={`${td} text-zinc-300 dark:text-zinc-600`}>
                    {draggable && <GripVertical className="h-4 w-4 cursor-grab active:cursor-grabbing" />}
                  </td>
                  <td className={`${td} font-medium`}>
                    {(s.flag || s.autoFlag) && <Flag code={s.flag || s.autoFlag} className="mr-1.5" />}
                    {s.name}
                  </td>
                  <td className={`${td} text-zinc-500`}>{s.group}</td>
                  <td className={td}>
                    <StatusPill online={s.online} />
                  </td>
                  <td className={`${td} tabular-nums text-zinc-500`}>
                    <div className="flex items-start gap-1">
                      <div className="flex flex-col gap-0.5 leading-tight">
                        <span className="inline-flex items-center gap-1">
                          {shown ? s.ip || '—' : maskIp(s.ip)}
                          {s.ip && <CopyBtn text={s.ip} />}
                        </span>
                        {s.ipv6 && (
                          <span className="inline-flex items-center gap-1 text-[11px] text-zinc-400">
                            {shown ? s.ipv6 : maskIp(s.ipv6)}
                            <CopyBtn text={s.ipv6} />
                          </span>
                        )}
                      </div>
                      {(s.ip || s.ipv6) && (
                        <button className={iconBtn} onClick={() => toggleReveal(s.id)} title={shown ? '隐藏' : '显示'}>
                          {shown ? <EyeOff className="h-3.5 w-3.5" /> : <Eye className="h-3.5 w-3.5" />}
                        </button>
                      )}
                    </div>
                  </td>
                  <td className={`${td} text-zinc-500`}>{s.expireAt || '长期'}</td>
                  <td className={`${td} text-right`}>
                    <span className="inline-flex items-center gap-0.5">
                      {s.gcpEnabled && (
                        <button
                          className={`${iconBtn} ${s.gcpLastErr ? '!text-amber-500' : ''}`}
                          title={gcpTitle(s)}
                          disabled={gcpStarting === s.id}
                          onClick={() => gcpStart(s)}
                        >
                          <Play className="h-3.5 w-3.5" />
                        </button>
                      )}
                      <button className={iconBtn} title="安装命令" onClick={() => setInstall(s)}>
                        <Terminal className="h-3.5 w-3.5" />
                      </button>
                      <button className={iconBtn} title="编辑" onClick={() => setModal(s)}>
                        <Pencil className="h-3.5 w-3.5" />
                      </button>
                      <button className={`${iconBtn} hover:!text-rose-500`} title="删除" onClick={() => setConfirmDel(s)}>
                        <Trash2 className="h-3.5 w-3.5" />
                      </button>
                    </span>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>

      {/* 移动端卡片列表：纵向堆叠各字段，避免表格横向滚动截断信息 */}
      <div className="space-y-2 md:hidden">
        {filtered.length === 0 && (
          <div className={`${card} p-4 text-center text-sm text-zinc-400`}>
            暂无服务器，点击右上角「添加服务器」开始
          </div>
        )}
        {filtered.map((s) => {
          const shown = revealed.has(s.id)
          return (
            <div key={s.id} className={`${card} space-y-2.5 p-3.5`}>
              <div className="flex items-start justify-between gap-2">
                <div className="flex min-w-0 items-center gap-1.5 font-medium">
                  {(s.flag || s.autoFlag) && <Flag code={s.flag || s.autoFlag} />}
                  <span className="truncate">{s.name}</span>
                </div>
                <StatusPill online={s.online} />
              </div>
              <dl className="space-y-1.5 text-sm">
                <div className="flex items-center justify-between gap-3">
                  <dt className="shrink-0 text-zinc-400">分组</dt>
                  <dd className="truncate text-zinc-600 dark:text-zinc-300">{s.group || '—'}</dd>
                </div>
                <div className="flex items-start justify-between gap-3">
                  <dt className="shrink-0 text-zinc-400">IP 地址</dt>
                  <dd className="flex min-w-0 flex-col items-end gap-0.5 tabular-nums text-zinc-600 dark:text-zinc-300">
                    <span className="inline-flex items-center gap-1">
                      {shown ? s.ip || '—' : maskIp(s.ip)}
                      {s.ip && <CopyBtn text={s.ip} />}
                      {(s.ip || s.ipv6) && (
                        <button className={iconBtn} onClick={() => toggleReveal(s.id)} title={shown ? '隐藏' : '显示'}>
                          {shown ? <EyeOff className="h-3.5 w-3.5" /> : <Eye className="h-3.5 w-3.5" />}
                        </button>
                      )}
                    </span>
                    {s.ipv6 && (
                      <span className="inline-flex items-center gap-1 text-[11px] text-zinc-400">
                        {shown ? s.ipv6 : maskIp(s.ipv6)}
                        <CopyBtn text={s.ipv6} />
                      </span>
                    )}
                  </dd>
                </div>
                <div className="flex items-center justify-between gap-3">
                  <dt className="shrink-0 text-zinc-400">到期</dt>
                  <dd className="text-zinc-600 dark:text-zinc-300">{s.expireAt || '长期'}</dd>
                </div>
              </dl>
              <div className="flex justify-end gap-0.5 border-t border-zinc-500/10 pt-2 dark:border-white/5">
                {s.gcpEnabled && (
                  <button
                    className={`${iconBtn} ${s.gcpLastErr ? '!text-amber-500' : ''}`}
                    title={gcpTitle(s)}
                    disabled={gcpStarting === s.id}
                    onClick={() => gcpStart(s)}
                  >
                    <Play className="h-4 w-4" />
                  </button>
                )}
                <button className={iconBtn} title="安装命令" onClick={() => setInstall(s)}>
                  <Terminal className="h-4 w-4" />
                </button>
                <button className={iconBtn} title="编辑" onClick={() => setModal(s)}>
                  <Pencil className="h-4 w-4" />
                </button>
                <button className={`${iconBtn} hover:!text-rose-500`} title="删除" onClick={() => setConfirmDel(s)}>
                  <Trash2 className="h-4 w-4" />
                </button>
              </div>
            </div>
          )
        })}
      </div>

      {modal === 'add' && (
        <ServerFormModal
          title="添加服务器"
          init={emptyServerForm}
          onClose={() => setModal(null)}
          onSubmit={async (f) => {
            const tempId = `tmp-${Date.now()}`
            const optimistic: AdminServer = {
              id: tempId, name: f.name, group: f.group, region: f.region, flag: f.flag,
              autoFlag: '', note: f.note, expireAt: f.expireAt, token: '',
              ip: '', ipv6: '', online: false,
              gcpEnabled: f.gcpEnabled, gcpProject: f.gcpProject, gcpZone: f.gcpZone,
              gcpInstance: f.gcpInstance, gcpTries: 0, gcpLastTry: 0, gcpLastErr: '',
            }
            setModal(null)
            await mutate(
              (l) => [...l, optimistic],
              async () => {
                const res = await post<{ id: string; token: string }>('/api/admin/servers', f)
                setList((l) => l.map((x) => (x.id === tempId ? { ...optimistic, id: res.id, token: res.token } : x)))
                setInstall({ name: f.name, token: res.token })
                load() // 后台对账，补齐服务端计算的 autoFlag 等字段
              },
              { onError: (e) => toast(errMsg(e)) },
            )
          }}
        />
      )}
      {modal && modal !== 'add' && (
        <ServerFormModal
          title={`编辑 ${modal.name}`}
          init={{
            name: modal.name,
            group: modal.group,
            region: modal.region,
            flag: modal.flag,
            expireAt: modal.expireAt,
            note: modal.note,
            gcpEnabled: modal.gcpEnabled,
            gcpProject: modal.gcpProject,
            gcpZone: modal.gcpZone,
            gcpInstance: modal.gcpInstance,
          }}
          onClose={() => setModal(null)}
          onSubmit={async (f) => {
            const id = modal.id
            setModal(null)
            await mutate(
              (l) => l.map((x) => (x.id === id ? { ...x, ...f } : x)),
              () => put(`/api/admin/servers/${id}`, f),
              { onSuccess: () => toast('已保存'), onError: (e) => toast(errMsg(e)) },
            )
          }}
        />
      )}

      {install && (
        <Modal title={`Agent 安装命令 · ${install.name}`} onClose={() => setInstall(null)}>
          <p className="mb-3 text-xs text-zinc-500">
            三个系统共用同一个地址和 Token，按目标系统选择对应命令执行即可：
          </p>
          <div className="space-y-3">
            <CmdBlock label="Linux / macOS" cmd={installCmds(install.token).sh} />
            <CmdBlock label="Windows（管理员 PowerShell）" cmd={installCmds(install.token).ps} />
            <div className="flex items-center gap-1 text-xs text-zinc-500">
              Token：<code className="rounded bg-zinc-500/10 px-1.5 py-0.5 dark:bg-white/10">{install.token}</code>
              <CopyBtn text={install.token} />
            </div>
          </div>
          <div className="mt-3 flex justify-end">
            <button className={btnGhost} onClick={() => setInstall(null)}>
              关闭
            </button>
          </div>
        </Modal>
      )}

      {confirmDel && (
        <ConfirmDelete
          title="删除服务器"
          onCancel={() => setConfirmDel(null)}
          onConfirm={() => {
            const target = confirmDel
            setConfirmDel(null)
            mutate(
              (l) => l.filter((x) => x.id !== target.id),
              () => del(`/api/admin/servers/${target.id}`),
              { onSuccess: () => toast('已删除'), onError: (e) => toast(errMsg(e)) },
            )
          }}
        >
          确定删除 <span className="font-semibold">{confirmDel.name}</span>
          ？其全部历史与探测数据将一并删除，且无法恢复。
        </ConfirmDelete>
      )}
    </div>
  )
}
