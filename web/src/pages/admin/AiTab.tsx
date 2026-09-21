import { useCallback, useEffect, useState } from 'react'
import { KeyRound, Plus, Pencil, Power, PowerOff, Trash2, ShieldAlert } from 'lucide-react'
import { del, get, post, put } from '../../api/client'
import type { ApiKey, AdminServer } from '../../types'
import { CheckBox, CopyBtn, Modal, ConfirmDelete } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { fmtDateTime } from '../../utils/format'
import { btnGhost, btnPrimary, card, formLabel, input, iconBtn, td, th } from '../../ui'
import { getLang, translate, useT, type TextKey } from '../../i18n'
import type { Toast } from './types'

/** 能力集与后端 apikey.go 的常量一一对应，改动需同步。 */
const CAPS: Array<{ key: string; labelKey: TextKey; descKey: TextKey }> = [
  { key: 'read', labelKey: 'ai.cap.read', descKey: 'ai.cap.read.desc' },
  { key: 'exec', labelKey: 'ai.cap.exec', descKey: 'ai.cap.exec.desc' },
  { key: 'write', labelKey: 'ai.cap.write', descKey: 'ai.cap.write.desc' },
]

/** 认不出的能力码原样显示：后端新增了能力而前端还没跟上时，不至于变成空白。 */
const capLabel = (k: string) => {
  const cap = CAPS.find((c) => c.key === k)
  return cap ? translate(getLang(), cap.labelKey) : k
}

/**
 * 有效期上限：10 年。
 *
 * 输入框只过滤非数字、不限长度，手滑多按几个 0 就能算出几十亿年后的时间戳，
 * 效果等同永不过期，界面上却什么都不说。真要永久就该显式留空，而不是靠一个
 * 大到没有意义的数字绕过去。
 */
const MAX_DAYS = 3650

export function AiTab({ toast }: { toast: Toast }) {
  const { t } = useT()
  const [keys, setKeys] = useState<ApiKey[]>([])
  const [servers, setServers] = useState<AdminServer[]>([])
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<ApiKey | null>(null)
  const [newKey, setNewKey] = useState<string | null>(null)
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

  // 停用/启用不设确认弹窗：它可以随时切回来，代价接近零。
  // 需要确认的是删除——那个不可恢复。
  const toggle = async (k: ApiKey) => {
    try {
      await put(`/api/admin/keys/${k.id}/disabled`, { disabled: !k.disabled })
      toast(t(k.disabled ? 'ai.toast.enabled' : 'ai.toast.disabled', { name: k.name }))
      load()
    } catch (e) {
      toast(errMsg(e))
    }
  }

  const remove = async (k: ApiKey) => {
    try {
      await del(`/api/admin/keys/${k.id}`)
      toast(t('ai.toast.deleted', { name: k.name }))
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
            {t('ai.keys')}
          </h2>
          <button className={btnPrimary} onClick={() => setCreating(true)}>
            <Plus className="h-4 w-4" />
            {t('ai.new')}
          </button>
        </div>

        {keys.length === 0 ? (
          <p className="py-8 text-center text-sm text-zinc-400">{t('ai.empty')}</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full">
              <thead>
                {/* 次要列在窄屏隐藏：手机上要能直接够到停用/删除，
                    而不是横滑一段才摸得到操作按钮 */}
                <tr className="border-b border-white/40 dark:border-white/10">
                  <th className={th}>{t('dash.col.name')}</th>
                  <th className={`${th} hidden md:table-cell`}>{t('ai.col.key')}</th>
                  <th className={th}>{t('ai.col.caps')}</th>
                  <th className={`${th} hidden lg:table-cell`}>{t('ai.col.scope')}</th>
                  <th className={`${th} hidden md:table-cell`}>{t('ai.col.expiry')}</th>
                  <th className={`${th} hidden lg:table-cell`}>{t('ai.col.lastUsed')}</th>
                  <th className={th}></th>
                </tr>
              </thead>
              <tbody>
                {keys.map((k) => (
                  <KeyRow
                    key={k.id}
                    k={k}
                    servers={servers}
                    onEdit={() => setEditing(k)}
                    onToggle={() => toggle(k)}
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

      {editing && (
        <KeyFormModal
          servers={servers}
          edit={editing}
          onClose={() => setEditing(null)}
          onCreated={() => {
            setEditing(null)
            load()
          }}
          toast={toast}
        />
      )}

      {newKey && <NewKeyModal plain={newKey} onClose={() => setNewKey(null)} />}

      {deleting && (
        <ConfirmDelete
          title={t('ai.del.title')}
          onCancel={() => setDeleting(null)}
          onConfirm={() => remove(deleting)}
        >
          {t('ai.del.body', { name: deleting.name })}
        </ConfirmDelete>
      )}
    </div>
  )
}

function KeyRow({
  k,
  servers,
  onEdit,
  onToggle,
  onDelete,
}: {
  k: ApiKey
  servers: AdminServer[]
  onEdit: () => void
  onToggle: () => void
  onDelete: () => void
}) {
  const { t } = useT()
  const expired = k.expiresAt > 0 && Date.now() / 1000 > k.expiresAt
  const dead = k.disabled || expired

  const scope =
    k.servers.length === 0
      ? t('ai.scope.all')
      : k.servers.map((id) => servers.find((s) => s.id === id)?.name ?? id).join(t('list.sep'))

  return (
    <tr className={`border-b border-white/25 dark:border-white/5 ${dead ? 'opacity-50' : ''}`}>
      <td className={td}>
        <div className="flex items-center gap-2">
          {k.name}
          {k.disabled && (
            <span className="rounded bg-zinc-500/15 px-1.5 py-0.5 text-xs text-zinc-500">{t('ai.disabled')}</span>
          )}
          {!k.disabled && expired && (
            <span className="rounded bg-amber-500/10 px-1.5 py-0.5 text-xs text-amber-600">{t('ai.expired')}</span>
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
        {k.expiresAt === 0 ? t('ai.never') : fmtDateTime(k.expiresAt * 1000)}
      </td>
      <td className={`${td} hidden lg:table-cell`}>
        {k.lastUsedAt === 0 ? t('ai.neverUsed') : fmtDateTime(k.lastUsedAt * 1000)}
      </td>
      <td className={`${td} text-right`}>
        <div className="flex justify-end gap-1">
          <button className={iconBtn} title={t('common.edit')} onClick={onEdit}>
            <Pencil className="h-4 w-4" />
          </button>
          {/* 停用可来回切；删除才是不可恢复的那个 */}
          <button
            className={`${iconBtn} ${k.disabled ? '!text-emerald-500' : ''}`}
            title={t(k.disabled ? 'common.enable' : 'common.disable')}
            onClick={onToggle}
          >
            {k.disabled ? <Power className="h-4 w-4" /> : <PowerOff className="h-4 w-4" />}
          </button>
          <button className={iconBtn} title={t('common.delete')} onClick={onDelete}>
            <Trash2 className="h-4 w-4" />
          </button>
        </div>
      </td>
    </tr>
  )
}

/* ---------- 新建密钥 ---------- */

/**
 * 新建 / 编辑密钥。传 edit 即进入编辑模式。
 *
 * 编辑改不了密钥本身——库里只有哈希，改不出明文。这反而是对的：
 * 调整权限范围不该让已经填进客户端的那串字符失效。
 */
function KeyFormModal({
  servers,
  edit,
  onClose,
  onCreated,
  toast,
}: {
  servers: AdminServer[]
  edit?: ApiKey
  onClose: () => void
  onCreated: (plain: string) => void
  toast: Toast
}) {
  const { t } = useT()
  const [name, setName] = useState(edit?.name ?? '')
  const [caps, setCaps] = useState<string[]>(edit?.caps ?? ['read'])
  const [scope, setScope] = useState(edit?.servers.join(',') ?? '') // 逗号分隔；空串表示全部机器
  // 有效期在库里是绝对时间戳，表单里填的是「从现在起多少天」。
  // 编辑时换算回剩余天数，否则一打开就显示空白，保存等于把有效期抹成永久。
  // 老数据的剩余天数可能超过现在的上限，同样按上限显示，并由下方提示说明——
  // 截断可以，但不能悄悄发生。
  const initial = (() => {
    if (!edit || edit.expiresAt === 0) return { days: '', capped: false }
    const left = Math.ceil((edit.expiresAt - Date.now() / 1000) / 86400)
    if (left <= 0) return { days: '', capped: false }
    return { days: String(Math.min(left, MAX_DAYS)), capped: left > MAX_DAYS }
  })()
  const [days, setDays] = useState(initial.days)
  const [capped, setCapped] = useState(initial.capped)
  const [busy, setBusy] = useState(false)

  // days 只经此处与 initial 两个入口，两边都已卡在 MAX_DAYS 内
  const onDaysChange = (raw: string) => {
    const digits = raw.replace(/\D/g, '')
    const over = digits !== '' && Number(digits) > MAX_DAYS
    setDays(over ? String(MAX_DAYS) : digits)
    setCapped(over)
  }

  const toggleCap = (c: string) =>
    setCaps((prev) => (prev.includes(c) ? prev.filter((x) => x !== c) : [...prev, c]))

  const submit = async () => {
    if (!name.trim()) return toast(t('ai.form.nameRequired'))
    if (caps.length === 0) return toast(t('ai.form.capRequired'))
    setBusy(true)
    try {
      const n = Number(days)
      const expiresAt = days.trim() && n > 0 ? Math.floor(Date.now() / 1000) + n * 86400 : 0
      const body = { name: name.trim(), caps, servers: scope ? scope.split(',') : [], expiresAt }
      if (edit) {
        await put(`/api/admin/keys/${edit.id}`, body)
        toast(t('ai.toast.saved', { name: name.trim() }))
        onCreated('') // 编辑不产生新密钥，父组件只需刷新列表
        return
      }
      const res = await post<{ id: number; key: string }>('/api/admin/keys', body)
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
    'group flex w-full cursor-pointer select-none items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm transition duration-100 hover:bg-white/50 active:bg-white/75 dark:hover:bg-white/10 dark:active:bg-white/15'

  return (
    <Modal
      title={edit ? t('ai.form.edit', { name: edit.name }) : t('ai.form.new')}
      onClose={onClose}
    >
      <div className="space-y-4">
        <div>
          <label className={formLabel}>{t('ai.form.name')}</label>
          <input
            className={input}
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={t('ai.form.name.placeholder')}
            autoFocus
          />
        </div>

        <div>
          <label className={formLabel}>{t('ai.col.caps')}</label>
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
                <span>{t(c.labelKey)}</span>
                <span className="text-xs text-zinc-400">{t(c.descKey)}</span>
              </button>
            ))}
          </div>
          <p className="mt-1 text-xs text-zinc-400">{t('ai.form.caps.hint')}</p>
        </div>

        <div>
          <label className={formLabel}>{t('ai.col.scope')}</label>
          <div className="glass-sheen max-h-44 space-y-0.5 overflow-y-auto rounded-xl border border-white/50 bg-white/45 p-1.5 dark:border-white/10 dark:bg-zinc-900/40">
            <button type="button" role="checkbox" aria-checked={all} className={row} onClick={() => setScope('')}>
              <CheckBox checked={all} />
              <span className={all ? 'font-medium' : ''}>{t('ai.scope.all')}</span>
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
            {servers.length === 0 && (
              <p className="px-2 py-1.5 text-sm text-zinc-400">{t('task.scope.empty')}</p>
            )}
          </div>
        </div>

        <div>
          <label className={formLabel}>{t('ai.form.days')}</label>
          <input
            className={input}
            value={days}
            onChange={(e) => onDaysChange(e.target.value)}
            placeholder={t('ai.form.days.placeholder')}
            inputMode="numeric"
          />
          <p className={`mt-1 text-xs ${capped ? 'text-amber-600 dark:text-amber-500' : 'text-zinc-400'}`}>
            {t(capped ? 'ai.form.days.capped' : 'ai.form.days.hint', { max: MAX_DAYS })}
          </p>
        </div>

        <div className="flex justify-end gap-2 pt-1">
          <button className={btnGhost} onClick={onClose}>
            {t('common.cancel')}
          </button>
          <button className={btnPrimary} onClick={submit} disabled={busy}>
            {busy ? t('common.saving') : t(edit ? 'common.save' : 'common.create')}
          </button>
        </div>
      </div>
    </Modal>
  )
}

/** 明文只在创建时出现这一次，之后库里只有哈希，任何人都取不回来。 */
function NewKeyModal({ plain, onClose }: { plain: string; onClose: () => void }) {
  const { t } = useT()
  return (
    <Modal title={t('ai.newKey.title')} onClose={onClose}>
      <div className="space-y-3">
        <div className="flex items-start gap-2 rounded-xl border border-amber-400/30 bg-amber-500/10 p-3 text-sm text-amber-700 dark:text-amber-400">
          <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" />
          <span>{t('ai.newKey.warn')}</span>
        </div>
        <div className="glass-sheen flex items-center gap-2 rounded-xl border border-white/50 bg-white/45 p-3 dark:border-white/10 dark:bg-zinc-900/40">
          <code className="flex-1 break-all font-mono text-sm">{plain}</code>
          <CopyBtn text={plain} />
        </div>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={onClose}>
            {t('ai.newKey.ok')}
          </button>
        </div>
      </div>
    </Modal>
  )
}

/* ---------- 接入说明 ---------- */

function ConnectGuide() {
  const { t } = useT()
  const endpoint = `${window.location.origin}/mcp`
  const snippet = JSON.stringify(
    {
      mcpServers: {
        moss: {
          type: 'http',
          url: endpoint,
          headers: { Authorization: `Bearer ${t('ai.guide.token')}` },
        },
      },
    },
    null,
    2,
  )
  return (
    <section className={`${card} p-4 sm:p-5`}>
      <h2 className="mb-3 font-semibold">{t('ai.guide.title')}</h2>
      <p className="mb-3 text-sm text-zinc-500 dark:text-zinc-400">{t('ai.guide.intro')}</p>
      <div className="mb-3">
        <label className={formLabel}>{t('ai.guide.endpoint')}</label>
        <div className="glass-sheen flex items-center gap-2 rounded-xl border border-white/50 bg-white/45 px-3 py-2 dark:border-white/10 dark:bg-zinc-900/40">
          <code className="flex-1 break-all font-mono text-sm">{endpoint}</code>
          <CopyBtn text={endpoint} />
        </div>
      </div>
      <div>
        <label className={formLabel}>{t('ai.guide.snippet')}</label>
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
