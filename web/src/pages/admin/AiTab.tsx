import { useCallback, useEffect, useState } from 'react'
import { KeyRound, Plus, Ban, Trash2, ShieldAlert } from 'lucide-react'
import { del, get, post } from '../../api/client'
import type { ApiKey, AdminServer } from '../../types'
import { CheckBox, CopyBtn, Modal, ConfirmDelete } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { fmtDateTime } from '../../utils/format'
import { btnGhost, btnPrimary, card, formLabel, input, iconBtn, td, th } from '../../ui'
import type { Toast } from './types'

/** 能力集与后端 apikey.go 的常量一一对应，改动需同步。 */
const CAPS: Array<{ key: string; label: string; desc: string }> = [
  { key: 'read', label: '读取', desc: '列出机器、读实时指标' },
  { key: 'exec', label: '执行命令', desc: '在机器上跑命令' },
  { key: 'write', label: '写入文件', desc: '写配置文件' },
]

const capLabel = (k: string) => CAPS.find((c) => c.key === k)?.label ?? k

export function AiTab({ toast }: { toast: Toast }) {
  const [keys, setKeys] = useState<ApiKey[]>([])
  const [servers, setServers] = useState<AdminServer[]>([])
  const [creating, setCreating] = useState(false)
  const [newKey, setNewKey] = useState<string | null>(null)
  const [revoking, setRevoking] = useState<ApiKey | null>(null)
  const [deleting, setDeleting] = useState<ApiKey | null>(null)

  const load = useCallback(() => {
    get<ApiKey[]>('/api/admin/keys')
      .then(setKeys)
      .catch((e) => toast(errMsg(e)))
  }, [toast])

  useEffect(() => {
    load()
    get<AdminServer[]>('/api/admin/servers')
      .then(setServers)
      .catch(() => {})
  }, [load])

  const revoke = async (k: ApiKey) => {
    try {
      await post(`/api/admin/keys/${k.id}/revoke`)
      toast(`已吊销「${k.name}」`)
      setRevoking(null)
      load()
    } catch (e) {
      toast(errMsg(e))
    }
  }

  const remove = async (k: ApiKey) => {
    try {
      await del(`/api/admin/keys/${k.id}`)
      toast(`已删除「${k.name}」`)
      setDeleting(null)
      load()
    } catch (e) {
      toast(errMsg(e))
    }
  }

  return (
    <div className="space-y-4">
      <ConnectGuide />

      <section className={`${card} p-4 sm:p-5`}>
        <div className="mb-4 flex items-center justify-between gap-3">
          <h2 className="flex items-center gap-2 font-semibold">
            <KeyRound className="h-4 w-4 text-emerald-500" />
            接入密钥
          </h2>
          <button className={btnPrimary} onClick={() => setCreating(true)}>
            <Plus className="h-4 w-4" />
            新建密钥
          </button>
        </div>

        {keys.length === 0 ? (
          <p className="py-8 text-center text-sm text-zinc-400">
            还没有密钥。新建一把后，把它填进 AI 客户端即可接入。
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full">
              <thead>
                {/* 次要列在窄屏隐藏：手机上要能直接够到吊销/删除，
                    而不是横滑一段才摸得到操作按钮 */}
                <tr className="border-b border-white/40 dark:border-white/10">
                  <th className={th}>名称</th>
                  <th className={`${th} hidden md:table-cell`}>密钥</th>
                  <th className={th}>能力</th>
                  <th className={`${th} hidden lg:table-cell`}>机器范围</th>
                  <th className={`${th} hidden md:table-cell`}>有效期</th>
                  <th className={`${th} hidden lg:table-cell`}>最后使用</th>
                  <th className={th}></th>
                </tr>
              </thead>
              <tbody>
                {keys.map((k) => (
                  <KeyRow
                    key={k.id}
                    k={k}
                    servers={servers}
                    onRevoke={() => setRevoking(k)}
                    onDelete={() => setDeleting(k)}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {creating && (
        <KeyFormModal
          servers={servers}
          onClose={() => setCreating(false)}
          onCreated={(plain) => {
            setCreating(false)
            setNewKey(plain)
            load()
          }}
          toast={toast}
        />
      )}

      {newKey && <NewKeyModal plain={newKey} onClose={() => setNewKey(null)} />}

      {revoking && (
        <ConfirmDelete title="吊销密钥" onCancel={() => setRevoking(null)} onConfirm={() => revoke(revoking)}>
          吊销「{revoking.name}」后，正在使用它的 AI 客户端会立刻失去访问权限。
          记录会保留，历史审计仍可追溯到这把密钥。
        </ConfirmDelete>
      )}

      {deleting && (
        <ConfirmDelete title="删除密钥" onCancel={() => setDeleting(null)} onConfirm={() => remove(deleting)}>
          删除「{deleting.name}」后不可恢复。若只是想停用，建议改用吊销——
          删除会让历史审计记录失去对应的密钥信息。
        </ConfirmDelete>
      )}
    </div>
  )
}

function KeyRow({
  k,
  servers,
  onRevoke,
  onDelete,
}: {
  k: ApiKey
  servers: AdminServer[]
  onRevoke: () => void
  onDelete: () => void
}) {
  const expired = k.expiresAt > 0 && Date.now() / 1000 > k.expiresAt
  const dead = k.revoked || expired

  const scope =
    k.servers.length === 0
      ? '全部机器'
      : k.servers
          .map((id) => servers.find((s) => s.id === id)?.name ?? id)
          .join('、')

  return (
    <tr className={`border-b border-white/25 dark:border-white/5 ${dead ? 'opacity-50' : ''}`}>
      <td className={td}>
        <div className="flex items-center gap-2">
          {k.name}
          {k.revoked && <span className="rounded bg-rose-500/10 px-1.5 py-0.5 text-xs text-rose-500">已吊销</span>}
          {!k.revoked && expired && (
            <span className="rounded bg-amber-500/10 px-1.5 py-0.5 text-xs text-amber-600">已过期</span>
          )}
        </div>
      </td>
      <td className={`${td} hidden font-mono text-xs text-zinc-500 md:table-cell`}>{k.prefix}…</td>
      {/* whitespace-normal 覆盖 td 的 nowrap：窄屏下让能力标签换行堆叠，
          否则三个标签横排会把操作列挤出可视区 */}
      <td className={`${td} whitespace-normal`}>
        <div className="flex flex-wrap gap-1">
          {k.caps.map((c) => (
            <span key={c} className="rounded bg-emerald-500/10 px-1.5 py-0.5 text-xs text-emerald-600 dark:text-emerald-400">
              {capLabel(c)}
            </span>
          ))}
        </div>
      </td>
      <td className={`${td} hidden max-w-[14rem] truncate lg:table-cell`} title={scope}>
        {scope}
      </td>
      <td className={`${td} hidden md:table-cell`}>
        {k.expiresAt === 0 ? '永久' : fmtDateTime(k.expiresAt * 1000)}
      </td>
      <td className={`${td} hidden lg:table-cell`}>
        {k.lastUsedAt === 0 ? '从未' : fmtDateTime(k.lastUsedAt * 1000)}
      </td>
      <td className={`${td} text-right`}>
        <div className="flex justify-end gap-1">
          {!k.revoked && (
            <button className={iconBtn} title="吊销" onClick={onRevoke}>
              <Ban className="h-4 w-4" />
            </button>
          )}
          <button className={iconBtn} title="删除" onClick={onDelete}>
            <Trash2 className="h-4 w-4" />
          </button>
        </div>
      </td>
    </tr>
  )
}

/* ---------- 新建密钥 ---------- */

function KeyFormModal({
  servers,
  onClose,
  onCreated,
  toast,
}: {
  servers: AdminServer[]
  onClose: () => void
  onCreated: (plain: string) => void
  toast: Toast
}) {
  const [name, setName] = useState('')
  const [caps, setCaps] = useState<string[]>(['read'])
  const [scope, setScope] = useState('') // 逗号分隔；空串表示全部机器
  const [days, setDays] = useState('')
  const [busy, setBusy] = useState(false)

  const toggleCap = (c: string) =>
    setCaps((prev) => (prev.includes(c) ? prev.filter((x) => x !== c) : [...prev, c]))

  const submit = async () => {
    if (!name.trim()) return toast('请填写名称')
    if (caps.length === 0) return toast('至少选择一项能力')
    setBusy(true)
    try {
      const n = Number(days)
      const expiresAt = days.trim() && n > 0 ? Math.floor(Date.now() / 1000) + n * 86400 : 0
      const res = await post<{ id: number; key: string }>('/api/admin/keys', {
        name: name.trim(),
        caps,
        servers: scope ? scope.split(',') : [],
        expiresAt,
      })
      onCreated(res.key)
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setBusy(false)
    }
  }

  const all = scope === ''
  const picked = new Set(scope ? scope.split(',') : [])
  const toggleServer = (id: string) => {
    const next = new Set(picked)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    setScope(next.size === 0 ? '' : servers.filter((s) => next.has(s.id)).map((s) => s.id).join(','))
  }
  const row =
    'flex w-full cursor-pointer select-none items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm transition hover:bg-white/50 dark:hover:bg-white/10'

  return (
    <Modal title="新建接入密钥" onClose={onClose}>
      <div className="space-y-4">
        <div>
          <label className={formLabel}>名称</label>
          <input
            className={input}
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="例如 OpenClaw 值守"
            autoFocus
          />
        </div>

        <div>
          <label className={formLabel}>能力</label>
          <div className="glass-sheen space-y-0.5 rounded-xl border border-white/50 bg-white/45 p-1.5 dark:border-white/10 dark:bg-zinc-900/40">
            {CAPS.map((c) => (
              <button
                key={c.key}
                type="button"
                role="checkbox"
                aria-checked={caps.includes(c.key)}
                className={row}
                onClick={() => toggleCap(c.key)}
              >
                <CheckBox checked={caps.includes(c.key)} />
                <span>{c.label}</span>
                <span className="text-xs text-zinc-400">{c.desc}</span>
              </button>
            ))}
          </div>
          <p className="mt-1 text-xs text-zinc-400">按需最小授予。只做值守的密钥不必给执行权限。</p>
        </div>

        <div>
          <label className={formLabel}>机器范围</label>
          <div className="glass-sheen max-h-44 space-y-0.5 overflow-y-auto rounded-xl border border-white/50 bg-white/45 p-1.5 dark:border-white/10 dark:bg-zinc-900/40">
            <button type="button" role="checkbox" aria-checked={all} className={row} onClick={() => setScope('')}>
              <CheckBox checked={all} />
              <span className={all ? 'font-medium' : ''}>全部机器</span>
            </button>
            {servers.map((s) => {
              const on = !all && picked.has(s.id)
              return (
                <button
                  key={s.id}
                  type="button"
                  role="checkbox"
                  aria-checked={on}
                  className={row}
                  onClick={() => toggleServer(s.id)}
                >
                  <CheckBox checked={on} />
                  <span>{s.name}</span>
                </button>
              )
            })}
            {servers.length === 0 && <p className="px-2 py-1.5 text-sm text-zinc-400">暂无服务器</p>}
          </div>
        </div>

        <div>
          <label className={formLabel}>有效期（天）</label>
          <input
            className={input}
            value={days}
            onChange={(e) => setDays(e.target.value.replace(/\D/g, ''))}
            placeholder="留空表示永不过期"
            inputMode="numeric"
          />
        </div>

        <div className="flex justify-end gap-2 pt-1">
          <button className={btnGhost} onClick={onClose}>
            取消
          </button>
          <button className={btnPrimary} onClick={submit} disabled={busy}>
            {busy ? '创建中…' : '创建'}
          </button>
        </div>
      </div>
    </Modal>
  )
}

/** 明文只在创建时出现这一次，之后库里只有哈希，任何人都取不回来。 */
function NewKeyModal({ plain, onClose }: { plain: string; onClose: () => void }) {
  return (
    <Modal title="密钥已创建" onClose={onClose}>
      <div className="space-y-3">
        <div className="flex items-start gap-2 rounded-xl border border-amber-400/30 bg-amber-500/10 p-3 text-sm text-amber-700 dark:text-amber-400">
          <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" />
          <span>请立即复制保存。关闭后将无法再次查看——服务端只存哈希，找不回原文。</span>
        </div>
        <div className="glass-sheen flex items-center gap-2 rounded-xl border border-white/50 bg-white/45 p-3 dark:border-white/10 dark:bg-zinc-900/40">
          <code className="flex-1 break-all font-mono text-sm">{plain}</code>
          <CopyBtn text={plain} />
        </div>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={onClose}>
            我已保存
          </button>
        </div>
      </div>
    </Modal>
  )
}

/* ---------- 接入说明 ---------- */

function ConnectGuide() {
  const endpoint = `${window.location.origin}/mcp`
  const snippet = JSON.stringify(
    {
      mcpServers: {
        moss: {
          type: 'http',
          url: endpoint,
          headers: { Authorization: 'Bearer 你的密钥' },
        },
      },
    },
    null,
    2,
  )
  return (
    <section className={`${card} p-4 sm:p-5`}>
      <h2 className="mb-3 font-semibold">接入方式</h2>
      <p className="mb-3 text-sm text-zinc-500 dark:text-zinc-400">
        moss 以 MCP 协议对外提供服务。把下面的地址和密钥填进 AI 客户端（OpenClaw、Claude Code 等），
        它就能查看机器状态、执行命令、写配置文件——所有操作都会被记录在下方的审计里。
      </p>
      <div className="mb-3">
        <label className={formLabel}>服务地址</label>
        <div className="glass-sheen flex items-center gap-2 rounded-xl border border-white/50 bg-white/45 px-3 py-2 dark:border-white/10 dark:bg-zinc-900/40">
          <code className="flex-1 break-all font-mono text-sm">{endpoint}</code>
          <CopyBtn text={endpoint} />
        </div>
      </div>
      <div>
        <label className={formLabel}>配置示例</label>
        <div className="glass-sheen relative rounded-xl border border-white/50 bg-white/45 p-3 dark:border-white/10 dark:bg-zinc-900/40">
          <div className="absolute right-2 top-2">
            <CopyBtn text={snippet} />
          </div>
          <pre className="overflow-x-auto pr-8 font-mono text-xs leading-relaxed">{snippet}</pre>
        </div>
      </div>
    </section>
  )
}
