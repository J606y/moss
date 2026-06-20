import { useCallback, useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import {
  Bell,
  Check,
  Copy,
  Eye,
  EyeOff,
  LogOut,
  Pencil,
  Plus,
  Radar,
  Server,
  SlidersHorizontal,
  Terminal,
  Trash2,
  X,
} from 'lucide-react'
import { del, get, post, put } from '../api/client'
import type { ApiError } from '../api/client'
import type { AdminServer, NotifySettings, PingTask, Settings } from '../types'
import { StatusPill } from '../components/ServerCard'
import Flag from '../components/Flag'
import { btnDanger, btnGhost, btnPrimary, card, formLabel, iconBtn, input, td, th } from '../ui'

type TabKey = 'servers' | 'tasks' | 'notify' | 'settings'
type Toast = (msg: string) => void

const tabs: Array<{ key: TabKey; label: string; icon: typeof Server }> = [
  { key: 'servers', label: '服务器', icon: Server },
  { key: 'tasks', label: '探测任务', icon: Radar },
  { key: 'notify', label: '通知告警', icon: Bell },
  { key: 'settings', label: '站点设置', icon: SlidersHorizontal },
]

const errMsg = (e: unknown) => (e instanceof Error ? e.message : '请求失败')

function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/30 p-4 backdrop-blur-sm" onClick={onClose}>
      <div
        className="max-h-[85vh] w-full max-w-md overflow-y-auto rounded-2xl border border-white/50 bg-white/80 p-5 shadow-2xl backdrop-blur-2xl dark:border-white/10 dark:bg-zinc-900/80"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="mb-4 flex items-center justify-between">
          <h3 className="font-semibold">{title}</h3>
          <button onClick={onClose} className={iconBtn}>
            <X className="h-4 w-4" />
          </button>
        </div>
        {children}
      </div>
    </div>
  )
}

function Switch({ on, onChange }: { on: boolean; onChange: (v: boolean) => void }) {
  return (
    <button
      onClick={() => onChange(!on)}
      className={`relative h-5 w-9 shrink-0 rounded-full transition ${on ? 'bg-emerald-500' : 'bg-zinc-500/30 dark:bg-white/15'}`}
    >
      <span
        className={`absolute top-0.5 h-4 w-4 rounded-full bg-white shadow transition-all ${on ? 'left-[18px]' : 'left-0.5'}`}
      />
    </button>
  )
}

function CopyBtn({ text }: { text: string }) {
  const [ok, setOk] = useState(false)
  return (
    <button
      className={iconBtn}
      title="复制"
      onClick={() => {
        navigator.clipboard.writeText(text).catch(() => {})
        setOk(true)
        setTimeout(() => setOk(false), 1500)
      }}
    >
      {ok ? <Check className="h-3.5 w-3.5 text-emerald-500" /> : <Copy className="h-3.5 w-3.5" />}
    </button>
  )
}

function maskIp(ip: string) {
  if (!ip) return '—'
  const parts = ip.split('.')
  if (parts.length !== 4) return '…'
  return `${parts[0]}.${parts[1]}.*.*`
}

/** 三端通用同一个 endpoint + token，只是安装命令按系统区分 */
function installCmds(token: string) {
  const origin = window.location.origin
  return {
    sh: `curl -fsSL ${origin}/install.sh | bash -s -- --endpoint ${origin} --token ${token}`,
    ps: `powershell -ExecutionPolicy Bypass -Command "& ([scriptblock]::Create((iwr -useb ${origin}/install.ps1))) -Endpoint '${origin}' -Token '${token}'"`,
  }
}

function CmdBlock({ label, cmd }: { label: string; cmd: string }) {
  return (
    <div>
      <div className="mb-1 flex items-center justify-between">
        <span className="text-xs font-medium text-zinc-500">{label}</span>
        <CopyBtn text={cmd} />
      </div>
      <pre className="overflow-x-auto whitespace-pre-wrap break-all rounded-xl border border-white/10 bg-zinc-950/85 p-3 text-xs leading-relaxed text-emerald-300 backdrop-blur">
        {cmd}
      </pre>
    </div>
  )
}

/* ---------- 服务器表单 ---------- */

interface ServerFormData {
  name: string
  group: string
  region: string
  flag: string
  expireAt: string
  note: string
}

const emptyServerForm: ServerFormData = { name: '', group: '', region: '', flag: '', expireAt: '', note: '' }

function ServerFormModal({
  title,
  init,
  onClose,
  onSubmit,
}: {
  title: string
  init: ServerFormData
  onClose: () => void
  onSubmit: (f: ServerFormData) => Promise<void>
}) {
  const [f, setF] = useState(init)
  const [busy, setBusy] = useState(false)
  const set = (k: keyof ServerFormData) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setF((prev) => ({ ...prev, [k]: e.target.value }))

  return (
    <Modal title={title} onClose={onClose}>
      <div className="space-y-3">
        <div>
          <label className={formLabel}>名称 *</label>
          <input className={input} placeholder="例如：HK-Lite" value={f.name} onChange={set('name')} autoFocus />
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>分组</label>
            <input className={input} placeholder="生产 / 测试…" value={f.group} onChange={set('group')} />
          </div>
          <div>
            <label className={formLabel}>地区</label>
            <input className={input} placeholder="香港 / 东京…" value={f.region} onChange={set('region')} />
          </div>
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>国旗代码</label>
            <input className={input} placeholder="hk / jp / us…" value={f.flag} onChange={set('flag')} />
          </div>
          <div>
            <label className={formLabel}>到期时间（可选）</label>
            <input className={input} placeholder="2026-12-31" value={f.expireAt} onChange={set('expireAt')} />
          </div>
        </div>
        <div>
          <label className={formLabel}>备注（可选）</label>
          <input className={input} placeholder="备注信息" value={f.note} onChange={set('note')} />
        </div>
        <div className="flex justify-end gap-2 pt-2">
          <button className={btnGhost} onClick={onClose}>
            取消
          </button>
          <button
            className={btnPrimary}
            disabled={busy || !f.name.trim()}
            onClick={async () => {
              setBusy(true)
              try {
                await onSubmit(f)
              } finally {
                setBusy(false)
              }
            }}
          >
            保存
          </button>
        </div>
      </div>
    </Modal>
  )
}

/* ---------- 服务器管理 ---------- */

function ServersTab({ toast }: { toast: Toast }) {
  const [list, setList] = useState<AdminServer[]>([])
  const [modal, setModal] = useState<'add' | AdminServer | null>(null)
  const [install, setInstall] = useState<{ name: string; token: string } | null>(null)
  const [confirmDel, setConfirmDel] = useState<AdminServer | null>(null)
  const [revealed, setRevealed] = useState<Set<string>>(new Set())
  const [search, setSearch] = useState('')

  const load = useCallback(() => {
    get<AdminServer[]>('/api/admin/servers')
      .then(setList)
      .catch((e) => toast(errMsg(e)))
  }, [toast])
  useEffect(load, [load])

  const filtered = list.filter(
    (s) => !search || s.name.toLowerCase().includes(search.toLowerCase()) || s.region.includes(search),
  )

  const toggleReveal = (id: string) => {
    setRevealed((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
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
        <button className={btnPrimary} onClick={() => setModal('add')}>
          <Plus className="h-4 w-4" /> 添加服务器
        </button>
      </div>

      <div className={`${card} overflow-x-auto`}>
        <table className="w-full min-w-[760px]">
          <thead className="border-b border-zinc-500/15 dark:border-white/10">
            <tr>
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
                <td className={`${td} text-center text-zinc-400`} colSpan={6}>
                  暂无服务器，点击右上角「添加服务器」开始
                </td>
              </tr>
            )}
            {filtered.map((s) => {
              const shown = revealed.has(s.id)
              return (
                <tr key={s.id} className="border-b border-zinc-500/10 last:border-0 dark:border-white/5">
                  <td className={`${td} font-medium`}>
                    {s.flag && <Flag code={s.flag} className="mr-1.5" />}
                    {s.name}
                  </td>
                  <td className={`${td} text-zinc-500`}>{s.group}</td>
                  <td className={td}>
                    <StatusPill online={s.online} />
                  </td>
                  <td className={`${td} tabular-nums text-zinc-500`}>
                    <span className="inline-flex items-center gap-1">
                      {shown ? s.ip || '—' : maskIp(s.ip)}
                      {s.ip && (
                        <button className={iconBtn} onClick={() => toggleReveal(s.id)} title={shown ? '隐藏' : '显示'}>
                          {shown ? <EyeOff className="h-3.5 w-3.5" /> : <Eye className="h-3.5 w-3.5" />}
                        </button>
                      )}
                    </span>
                  </td>
                  <td className={`${td} text-zinc-500`}>{s.expireAt || '长期'}</td>
                  <td className={`${td} text-right`}>
                    <span className="inline-flex items-center gap-0.5">
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

      {modal === 'add' && (
        <ServerFormModal
          title="添加服务器"
          init={emptyServerForm}
          onClose={() => setModal(null)}
          onSubmit={async (f) => {
            try {
              const res = await post<{ id: string; token: string }>('/api/admin/servers', f)
              setModal(null)
              load()
              setInstall({ name: f.name, token: res.token })
            } catch (e) {
              toast(errMsg(e))
            }
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
          }}
          onClose={() => setModal(null)}
          onSubmit={async (f) => {
            try {
              await put(`/api/admin/servers/${modal.id}`, f)
              setModal(null)
              load()
              toast('已保存')
            } catch (e) {
              toast(errMsg(e))
            }
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
        <Modal title="删除服务器" onClose={() => setConfirmDel(null)}>
          <p className="text-sm">
            确定删除 <span className="font-semibold">{confirmDel.name}</span>
            ？其全部历史与探测数据将一并删除，且无法恢复。
          </p>
          <div className="mt-4 flex justify-end gap-2">
            <button className={btnGhost} onClick={() => setConfirmDel(null)}>
              取消
            </button>
            <button
              className={btnDanger}
              onClick={async () => {
                try {
                  await del(`/api/admin/servers/${confirmDel.id}`)
                  setConfirmDel(null)
                  load()
                  toast('已删除')
                } catch (e) {
                  toast(errMsg(e))
                }
              }}
            >
              删除
            </button>
          </div>
        </Modal>
      )}
    </div>
  )
}

/* ---------- 探测任务 ---------- */

const typeBadge: Record<string, string> = {
  icmp: 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400',
  tcp: 'bg-sky-500/10 text-sky-600 dark:text-sky-400',
  http: 'bg-amber-500/10 text-amber-600 dark:text-amber-400',
}

interface TaskFormData {
  name: string
  type: PingTask['type']
  target: string
  interval: number
  serverId: string
}

const emptyTaskForm: TaskFormData = { name: '', type: 'icmp', target: '', interval: 60, serverId: '' }

function TaskFormModal({
  title,
  init,
  servers,
  onClose,
  onSubmit,
}: {
  title: string
  init: TaskFormData
  servers: AdminServer[]
  onClose: () => void
  onSubmit: (f: TaskFormData) => Promise<void>
}) {
  const [f, setF] = useState(init)
  const [busy, setBusy] = useState(false)

  return (
    <Modal title={title} onClose={onClose}>
      <div className="space-y-3">
        <div>
          <label className={formLabel}>名称 *</label>
          <input
            className={input}
            placeholder="例如：电信 ping"
            value={f.name}
            onChange={(e) => setF({ ...f, name: e.target.value })}
            autoFocus
          />
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>类型</label>
            <select className={input} value={f.type} onChange={(e) => setF({ ...f, type: e.target.value as PingTask['type'] })}>
              <option value="icmp">ICMP</option>
              <option value="tcp">TCP</option>
              <option value="http">HTTP</option>
            </select>
          </div>
          <div>
            <label className={formLabel}>间隔（秒）</label>
            <input
              className={input}
              type="number"
              min={10}
              value={f.interval}
              onChange={(e) => setF({ ...f, interval: Number(e.target.value) || 60 })}
            />
          </div>
        </div>
        <div>
          <label className={formLabel}>目标 *</label>
          <input
            className={input}
            placeholder={f.type === 'tcp' ? 'IP:端口，如 1.2.3.4:22' : f.type === 'http' ? 'https://example.com' : '域名或 IP'}
            value={f.target}
            onChange={(e) => setF({ ...f, target: e.target.value })}
          />
        </div>
        <div>
          <label className={formLabel}>应用于</label>
          <select className={input} value={f.serverId} onChange={(e) => setF({ ...f, serverId: e.target.value })}>
            <option value="">全部服务器</option>
            {servers.map((s) => (
              <option key={s.id} value={s.id}>
                {s.name}
              </option>
            ))}
          </select>
        </div>
        <div className="flex justify-end gap-2 pt-2">
          <button className={btnGhost} onClick={onClose}>
            取消
          </button>
          <button
            className={btnPrimary}
            disabled={busy || !f.name.trim() || !f.target.trim()}
            onClick={async () => {
              setBusy(true)
              try {
                await onSubmit(f)
              } finally {
                setBusy(false)
              }
            }}
          >
            保存
          </button>
        </div>
      </div>
    </Modal>
  )
}

function TasksTab({ toast }: { toast: Toast }) {
  const [tasks, setTasks] = useState<PingTask[]>([])
  const [servers, setServers] = useState<AdminServer[]>([])
  const [modal, setModal] = useState<'add' | PingTask | null>(null)

  const load = useCallback(() => {
    get<PingTask[]>('/api/admin/tasks')
      .then(setTasks)
      .catch((e) => toast(errMsg(e)))
    get<AdminServer[]>('/api/admin/servers')
      .then(setServers)
      .catch(() => {})
  }, [toast])
  useEffect(load, [load])

  const serverName = (id: string) => (id ? (servers.find((s) => s.id === id)?.name ?? id) : '全部服务器')

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
      <div className="flex items-center justify-between">
        <p className="text-sm text-zinc-500">对服务器下发 ICMP / TCP / HTTP 探测，生成延迟与可用性图表。</p>
        <button className={btnPrimary} onClick={() => setModal('add')}>
          <Plus className="h-4 w-4" /> 添加任务
        </button>
      </div>

      <div className={`${card} overflow-x-auto`}>
        <table className="w-full min-w-[680px]">
          <thead className="border-b border-zinc-500/15 dark:border-white/10">
            <tr>
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
                <td className={`${td} text-center text-zinc-400`} colSpan={7}>
                  暂无探测任务
                </td>
              </tr>
            )}
            {tasks.map((t) => (
              <tr key={t.id} className="border-b border-zinc-500/10 last:border-0 dark:border-white/5">
                <td className={`${td} font-medium`}>{t.name}</td>
                <td className={td}>
                  <span className={`rounded-md px-2 py-0.5 text-xs font-medium uppercase ${typeBadge[t.type]}`}>
                    {t.type}
                  </span>
                </td>
                <td className={`${td} tabular-nums text-zinc-500`}>{t.target}</td>
                <td className={`${td} tabular-nums text-zinc-500`}>{t.interval}s</td>
                <td className={`${td} text-zinc-500`}>{serverName(t.serverId)}</td>
                <td className={td}>
                  <Switch on={t.enabled} onChange={(v) => toggle(t, v)} />
                </td>
                <td className={`${td} text-right`}>
                  <span className="inline-flex items-center gap-0.5">
                    <button className={iconBtn} title="编辑" onClick={() => setModal(t)}>
                      <Pencil className="h-3.5 w-3.5" />
                    </button>
                    <button
                      className={`${iconBtn} hover:!text-rose-500`}
                      title="删除"
                      onClick={async () => {
                        try {
                          await del(`/api/admin/tasks/${t.id}`)
                          load()
                          toast('已删除')
                        } catch (e) {
                          toast(errMsg(e))
                        }
                      }}
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

      {modal === 'add' && (
        <TaskFormModal
          title="添加探测任务"
          init={emptyTaskForm}
          servers={servers}
          onClose={() => setModal(null)}
          onSubmit={async (f) => {
            try {
              await post('/api/admin/tasks', { ...f, enabled: true })
              setModal(null)
              load()
              toast('已添加，任务已下发给在线 Agent')
            } catch (e) {
              toast(errMsg(e))
            }
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
            try {
              await put(`/api/admin/tasks/${modal.id}`, { ...f, enabled: modal.enabled })
              setModal(null)
              load()
              toast('已保存')
            } catch (e) {
              toast(errMsg(e))
            }
          }}
        />
      )}
    </div>
  )
}

/* ---------- 通知告警（阶段 3） ---------- */

function Toggle({ checked, label, onChange }: { checked: boolean; label: string; onChange: (v: boolean) => void }) {
  return (
    <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
      <input
        type="checkbox"
        className="h-4 w-4 accent-emerald-600"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
      />
      {label}
    </label>
  )
}

function NotifyTab({ toast }: { toast: Toast }) {
  const [n, setN] = useState<NotifySettings | null>(null)
  const [testing, setTesting] = useState(false)

  useEffect(() => {
    get<NotifySettings>('/api/admin/notify')
      .then(setN)
      .catch((e) => toast(errMsg(e)))
  }, [toast])

  if (!n) return <p className="text-sm text-zinc-500">加载中…</p>

  const num = (k: keyof NotifySettings) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setN({ ...n, [k]: Number(e.target.value) || 0 })

  const save = async () => {
    try {
      await put('/api/admin/notify', n)
      toast('通知设置已保存')
    } catch (e) {
      toast(errMsg(e))
    }
  }

  const test = async () => {
    setTesting(true)
    try {
      await put('/api/admin/notify', n) // 先保存再测试，避免测到旧配置
      await post('/api/admin/notify/test', {})
      toast('测试消息已发送，请查看 Telegram')
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setTesting(false)
    }
  }

  return (
    <div className="max-w-xl space-y-4">
      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">Telegram 推送</h3>
        <div>
          <label className={formLabel}>Bot Token</label>
          <input
            className={input}
            placeholder="123456:ABC-xxx（找 @BotFather 创建）"
            value={n.tgToken}
            onChange={(e) => setN({ ...n, tgToken: e.target.value })}
          />
        </div>
        <div>
          <label className={formLabel}>Chat ID</label>
          <input
            className={input}
            placeholder="个人/群组 ID（找 @userinfobot 获取）"
            value={n.tgChat}
            onChange={(e) => setN({ ...n, tgChat: e.target.value })}
          />
        </div>
        <div className="flex justify-end">
          <button className={btnGhost} onClick={test} disabled={testing}>
            {testing ? '发送中…' : '保存并发送测试消息'}
          </button>
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">离线告警</h3>
        <Toggle checked={n.offlineOn} label="服务器离线时推送通知（恢复上线时同步通知）" onChange={(v) => setN({ ...n, offlineOn: v })} />
        <div>
          <label className={formLabel}>离线判定延迟（秒，30 ~ 3600）</label>
          <input className={input} type="number" min={30} max={3600} value={n.offlineDelay} onChange={num('offlineDelay')} />
          <p className="mt-1 text-xs text-zinc-400">离线超过该时长才告警，避免网络抖动产生骚扰。</p>
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">负载告警</h3>
        <Toggle checked={n.loadOn} label="资源使用率持续超阈值时推送通知" onChange={(v) => setN({ ...n, loadOn: v })} />
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>CPU 阈值（%）</label>
            <input className={input} type="number" min={1} max={100} value={n.cpuThreshold} onChange={num('cpuThreshold')} />
          </div>
          <div>
            <label className={formLabel}>内存阈值（%）</label>
            <input className={input} type="number" min={1} max={100} value={n.memThreshold} onChange={num('memThreshold')} />
          </div>
          <div>
            <label className={formLabel}>硬盘阈值（%）</label>
            <input className={input} type="number" min={1} max={100} value={n.diskThreshold} onChange={num('diskThreshold')} />
          </div>
          <div>
            <label className={formLabel}>持续时间（分钟）</label>
            <input className={input} type="number" min={1} max={120} value={n.loadMinutes} onChange={num('loadMinutes')} />
          </div>
        </div>
        <p className="text-xs text-zinc-400">超阈值持续指定时间才告警；回落至阈值 5% 以下时发送恢复通知。</p>
      </div>

      <div className="flex justify-end">
        <button className={btnPrimary} onClick={save}>
          保存设置
        </button>
      </div>
    </div>
  )
}

/* ---------- 站点设置 ---------- */

function SettingsTab({ toast }: { toast: Toast }) {
  const navigate = useNavigate()
  const [s, setS] = useState<Settings | null>(null)
  const [pwd, setPwd] = useState({ old: '', new1: '', new2: '' })

  useEffect(() => {
    get<Settings>('/api/admin/settings')
      .then(setS)
      .catch((e) => toast(errMsg(e)))
  }, [toast])

  if (!s) return <p className="text-sm text-zinc-500">加载中…</p>

  const num = (k: keyof Settings) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setS({ ...s, [k]: Number(e.target.value) || 0 })

  const save = async () => {
    try {
      await put('/api/admin/settings', s)
      toast('设置已保存')
    } catch (e) {
      toast(errMsg(e))
    }
  }

  const changePwd = async () => {
    if (!pwd.new1 || pwd.new1 !== pwd.new2) {
      toast('两次输入的新密码不一致')
      return
    }
    try {
      await put('/api/admin/password', { old: pwd.old, new: pwd.new1 })
      toast('密码已修改，请重新登录')
      setTimeout(() => navigate('/login'), 800)
    } catch (e) {
      toast(errMsg(e))
    }
  }

  return (
    <div className="max-w-xl space-y-4">
      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">站点信息</h3>
        <div>
          <label className={formLabel}>管理员用户名</label>
          <input className={input} value={s.username} onChange={(e) => setS({ ...s, username: e.target.value })} />
        </div>
        <div>
          <label className={formLabel}>站点名称</label>
          <input className={input} value={s.siteName} onChange={(e) => setS({ ...s, siteName: e.target.value })} />
        </div>
        <div>
          <label className={formLabel}>站点描述</label>
          <input className={input} value={s.siteDesc} onChange={(e) => setS({ ...s, siteDesc: e.target.value })} />
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">数据采集</h3>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>实时上报间隔（秒）</label>
            <input className={input} type="number" min={1} value={s.reportInterval} onChange={num('reportInterval')} />
          </div>
          <div>
            <label className={formLabel}>历史采样间隔（分钟）</label>
            <input className={input} type="number" min={1} value={s.sampleInterval} onChange={num('sampleInterval')} />
          </div>
          <div>
            <label className={formLabel}>历史数据保留（天）</label>
            <input className={input} type="number" min={1} value={s.historyDays} onChange={num('historyDays')} />
          </div>
          <div>
            <label className={formLabel}>延迟数据保留（天）</label>
            <input className={input} type="number" min={1} value={s.pingDays} onChange={num('pingDays')} />
          </div>
        </div>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={save}>
            保存设置
          </button>
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">修改密码</h3>
        <div>
          <label className={formLabel}>当前密码</label>
          <input
            type="password"
            className={input}
            value={pwd.old}
            onChange={(e) => setPwd({ ...pwd, old: e.target.value })}
          />
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>新密码（至少 6 位）</label>
            <input
              type="password"
              className={input}
              value={pwd.new1}
              onChange={(e) => setPwd({ ...pwd, new1: e.target.value })}
            />
          </div>
          <div>
            <label className={formLabel}>确认新密码</label>
            <input
              type="password"
              className={input}
              value={pwd.new2}
              onChange={(e) => setPwd({ ...pwd, new2: e.target.value })}
            />
          </div>
        </div>
        <p className="text-xs text-zinc-400">修改密码后所有登录会话将失效，需要重新登录。</p>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={changePwd}>
            修改密码
          </button>
        </div>
      </div>
    </div>
  )
}

/* ---------- 主页面 ---------- */

export default function Admin() {
  const navigate = useNavigate()
  const [tab, setTab] = useState<TabKey>('servers')
  const [toast, setToast] = useState<string | null>(null)
  const [authed, setAuthed] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  // 未登录跳转
  useEffect(() => {
    get('/api/admin/me')
      .then(() => setAuthed(true))
      .catch((e) => {
        if ((e as ApiError).status === 401) navigate('/login')
      })
  }, [navigate])

  const showToast = useCallback((msg: string) => {
    setToast(msg)
    if (timer.current) clearTimeout(timer.current)
    timer.current = setTimeout(() => setToast(null), 2200)
  }, [])

  const logout = async () => {
    try {
      await post('/api/logout')
    } finally {
      navigate('/login')
    }
  }

  if (!authed) return null

  const TabButton = ({ t }: { t: (typeof tabs)[number] }) => (
    <button
      onClick={() => setTab(t.key)}
      className={`flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-sm transition ${
        tab === t.key
          ? 'bg-emerald-500/10 font-medium text-emerald-600 dark:text-emerald-400'
          : 'text-zinc-500 hover:bg-white/50 hover:text-zinc-800 dark:hover:bg-white/10 dark:hover:text-zinc-200'
      }`}
    >
      <t.icon className="h-4 w-4 shrink-0" />
      {t.label}
    </button>
  )

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-bold">管理后台</h1>
        <div className="flex items-center gap-2">
          <Link to="/" className={btnGhost}>
            返回首页
          </Link>
          <button onClick={logout} className={`${btnGhost} !text-rose-500`}>
            <LogOut className="h-4 w-4" /> 退出
          </button>
        </div>
      </div>

      {/* 移动端横向 Tab */}
      <div className="flex gap-1 overflow-x-auto md:hidden">
        {tabs.map((t) => (
          <div key={t.key} className="shrink-0">
            <TabButton t={t} />
          </div>
        ))}
      </div>

      <div className="flex gap-6">
        {/* 桌面端侧边栏 */}
        <aside className="hidden w-44 shrink-0 flex-col gap-1 md:flex">
          {tabs.map((t) => (
            <TabButton key={t.key} t={t} />
          ))}
        </aside>

        <div className="min-w-0 flex-1">
          {tab === 'servers' && <ServersTab toast={showToast} />}
          {tab === 'tasks' && <TasksTab toast={showToast} />}
          {tab === 'notify' && <NotifyTab toast={showToast} />}
          {tab === 'settings' && <SettingsTab toast={showToast} />}
        </div>
      </div>

      {toast && (
        <div className="fixed bottom-6 right-6 z-50 rounded-xl border border-white/10 bg-zinc-900/85 px-4 py-2 text-sm text-white shadow-xl backdrop-blur-xl dark:border-white/20 dark:bg-white/85 dark:text-zinc-900">
          {toast}
        </div>
      )}
    </div>
  )
}
