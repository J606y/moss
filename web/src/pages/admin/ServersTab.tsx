import { useCallback, useEffect, useState } from 'react'
import { ArrowUpCircle, Eye, EyeOff, GripVertical, Loader2, Pencil, Play, Plus, Terminal, Trash2 } from 'lucide-react'
import { del, get, post, put } from '../../api/client'
import type { AdminServer, GcpCredential, GcpSettings } from '../../types'
import { CmdBlock, ConfirmDelete, CopyBtn, Modal, StatusPill, Switch } from '../../components/ui'
import Flag from '../../components/Flag'
import { errMsg, hintText, installCmds, maskIp } from '../../utils/admin'
import { btnGhost, btnPrimary, card, iconBtn, input, td, th } from '../../ui'
import { useOptimisticList } from '../../hooks/useOptimisticList'
import { useReorder } from '../../hooks/useReorder'
import { fmtTime } from '../../utils/format'
import { getLang, translate, useT, type TextKey } from '../../i18n'
import { emptyServerForm, ServerFormModal } from './ServerFormModal'
import type { Toast } from './types'

/** 升级阶段 → 文案 key。终态两项单列，其余都是进行中。 */
const UPGRADE_STAGE_KEY: Record<string, TextKey> = {
  downloading: 'srv.upgrade.downloading',
  verified: 'srv.upgrade.verified',
  replaced: 'srv.upgrade.replaced',
  restarting: 'srv.upgrade.restarting',
  success: 'srv.upgrade.success',
  failed: 'srv.upgrade.failed',
}

const isUpgrading = (s: AdminServer) =>
  !!s.upgradeStage && s.upgradeStage !== 'success' && s.upgradeStage !== 'failed'

/**
 * 是否显示更新按钮。
 *
 * 离线机器不显示：整行已有离线标识，再挂一个点不动的按钮只是噪音。
 * 但升级进行中要显示——那时机器正因为重启而离线，恰恰是最该看到状态的时候。
 */
const showUpgradeBtn = (s: AdminServer) =>
  isUpgrading(s) || s.upgradeStage === 'failed' || (s.online && (s.upgradable || !!s.upgradeHint))

// 模块级纯函数，拿不到 useT 的 t；这些串只在 title 属性里出现，
// 取调用当刻的语言即可，不需要跟着订阅重渲染。
const upgradeTitle = (s: AdminServer) => {
  const lang = getLang()
  if (isUpgrading(s)) {
    const key = UPGRADE_STAGE_KEY[s.upgradeStage!]
    return translate(lang, 'srv.upgrade.inProgress', {
      stage: key ? translate(lang, key) : s.upgradeStage!,
    })
  }
  if (s.upgradeStage === 'failed') {
    return translate(lang, 'srv.upgrade.failedHint', {
      err: s.upgradeErr || translate(lang, 'upd.failed.unknown'),
    })
  }
  if (s.upgradeHint) return hintText(s.upgradeHint, s.upgradeHintCode, s.upgradeHintDetail)
  const from = s.agentVersion ? `v${s.agentVersion}` : translate(lang, 'srv.upgrade.noVersion')
  return translate(lang, 'srv.upgrade.start', { from, to: s.targetVersion })
}

export function ServersTab({ toast }: { toast: Toast }) {
  const { t } = useT()
  const { items: list, setItems: setList, mutate } = useOptimisticList<AdminServer>([])
  const [modal, setModal] = useState<'add' | AdminServer | null>(null)
  const [install, setInstall] = useState<{ name: string; token: string } | null>(null)
  // 安装命令里带不带远程执行开关。默认关：装了 agent 不等于同意被远程操作。
  const [allowExec, setAllowExec] = useState(false)
  // 关闭时一并复位：安全默认不能因为上一台开过就延续到下一台。
  const closeInstall = () => {
    setInstall(null)
    setAllowExec(false)
  }
  const [confirmDel, setConfirmDel] = useState<AdminServer | null>(null)
  const [revealed, setRevealed] = useState<Set<string>>(new Set())
  const [search, setSearch] = useState('')

  const load = useCallback(() => {
    get<AdminServer[]>('/api/admin/servers')
      .then(setList)
      .catch((e) => toast(errMsg(e)))
  }, [toast, setList])
  useEffect(load, [load])

  // GCP 凭证列表供编辑弹窗里的凭证下拉使用。null 表示还没拉到——
  // 弹窗据此区分「加载中」与「一份都没有」，不然一打开就会闪一句「尚未添加凭证」。
  // 只在本页挂载时拉一次：凭证的增删在「GCP 守护」页，换页回来自然会重拉。
  const [creds, setCreds] = useState<GcpCredential[] | null>(null)
  useEffect(() => {
    get<GcpSettings>('/api/admin/gcp')
      .then((g) => setCreds(g.credentials))
      .catch(() => setCreds([])) // 拉不到就当没有，别把服务器管理页也卡住
  }, [])

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
      // 提示按后端返回的结构化字段自己渲染，不回显它那句中文 message：
      // 那串在英文界面下会原样冒出来。后端仍保留 message 给 API / MCP 消费者。
      const res = await post<{ status: string; started: boolean }>(`/api/admin/servers/${s.id}/gcp-start`)
      toast(
        res.started
          ? t('srv.gcp.started')
          : res.status === 'RUNNING'
            ? t('srv.gcp.alreadyRunning')
            : t('srv.gcp.notStarted', { status: res.status }),
      )
      load()
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setGcpStarting(null)
    }
  }
  // agent 一键升级。逐台点，不做批量：一次坏也只坏一台。
  // 能不能升由后端判定（upgradable / upgradeHint），前端不自己比较版本号。
  const [upgradeSubmitting, setUpgradeSubmitting] = useState<string | null>(null)
  const upgradeAgent = async (s: AdminServer) => {
    setUpgradeSubmitting(s.id)
    try {
      await post(`/api/admin/servers/${s.id}/upgrade`)
      toast(t('srv.upgrade.dispatched'))
      load()
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setUpgradeSubmitting(null)
    }
  }

  // 升级期间机器必然重启并短暂离线，状态只能靠轮询刷新。
  // 依赖用布尔而非 list：否则每次列表刷新都会重建定时器，永远等不满一个周期。
  const upgradeRunning = list.some((s) => isUpgrading(s))
  useEffect(() => {
    if (!upgradeRunning) return
    const t = setInterval(load, 3000)
    return () => clearInterval(t)
  }, [upgradeRunning, load])

  const gcpTitle = (s: AdminServer) => {
    let title = t('srv.gcp.start')
    // 多凭证下「用的是哪个账号」是排查的第一个问题，直接写进 tooltip
    const cred = creds?.find((c) => c.id === s.gcpCredId)
    if (cred) title += t('srv.gcp.credLabel', { project: cred.projectId })
    if (s.gcpTries > 0) {
      title += t('srv.gcp.tries', { n: s.gcpTries })
      if (s.gcpLastTry > 0) title += t('srv.gcp.lastTry', { time: fmtTime(s.gcpLastTry * 1000) })
    }
    if (s.gcpLastErr) title += ` | ${s.gcpLastErr}`
    return title
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between gap-2">
        <input
          className={`${input} max-w-60`}
          placeholder={t('srv.search')}
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <button className={`${btnPrimary} shrink-0 whitespace-nowrap`} onClick={() => setModal('add')}>
          <Plus className="h-4 w-4" /> {t('srv.add')}
        </button>
      </div>

      <p className="hidden text-xs text-zinc-400 md:block">
        {t('srv.dragHint.before')}
        <GripVertical className="inline h-3 w-3 align-text-bottom" />
        {t('srv.dragHint.after')}
        {search ? t('srv.dragHint.searching') : ''}
      </p>

      <div className={`${card} hidden overflow-x-auto md:block`}>
        <table className="w-full min-w-[760px]">
          <thead className="border-b border-zinc-500/15 dark:border-white/10">
            <tr>
              <th className={`${th} w-8`} />
              <th className={th}>{t('dash.col.name')}</th>
              <th className={th}>{t('srv.group')}</th>
              <th className={th}>{t('dash.col.status')}</th>
              <th className={th}>{t('srv.col.ip')}</th>
              <th className={th}>{t('srv.col.expire')}</th>
              <th className={`${th} text-right`}>{t('task.col.actions')}</th>
            </tr>
          </thead>
          <tbody>
            {filtered.length === 0 && (
              <tr>
                <td className={`${td} text-center text-zinc-400`} colSpan={7}>
                  {t('srv.empty')}
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
                        <button className={iconBtn} onClick={() => toggleReveal(s.id)} title={t(shown ? 'common.hide' : 'common.show')}>
                          {shown ? <EyeOff className="h-3.5 w-3.5" /> : <Eye className="h-3.5 w-3.5" />}
                        </button>
                      )}
                    </div>
                  </td>
                  <td className={`${td} text-zinc-500`}>{s.expireAt || t('detail.info.expire.never')}</td>
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
                      {showUpgradeBtn(s) && (
                        <button
                          className={`${iconBtn} ${
                            s.upgradeStage === 'failed'
                              ? '!text-rose-500'
                              : s.upgradeHint
                                ? '!text-amber-500'
                                : ''
                          }`}
                          title={upgradeTitle(s)}
                          disabled={isUpgrading(s) || upgradeSubmitting === s.id}
                          onClick={() => upgradeAgent(s)}
                        >
                          {isUpgrading(s) || upgradeSubmitting === s.id ? (
                            <Loader2 className="h-3.5 w-3.5 animate-spin" />
                          ) : (
                            <ArrowUpCircle className="h-3.5 w-3.5" />
                          )}
                        </button>
                      )}
                      <button className={iconBtn} title={t('srv.install')} onClick={() => setInstall(s)}>
                        <Terminal className="h-3.5 w-3.5" />
                      </button>
                      <button className={iconBtn} title={t('common.edit')} onClick={() => setModal(s)}>
                        <Pencil className="h-3.5 w-3.5" />
                      </button>
                      <button className={`${iconBtn} hover:!text-rose-500`} title={t('common.delete')} onClick={() => setConfirmDel(s)}>
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
          <div className={`${card} p-4 text-center text-sm text-zinc-400`}>{t('srv.empty')}</div>
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
                  <dt className="shrink-0 text-zinc-400">{t('srv.group')}</dt>
                  <dd className="truncate text-zinc-600 dark:text-zinc-300">{s.group || '—'}</dd>
                </div>
                <div className="flex items-start justify-between gap-3">
                  <dt className="shrink-0 text-zinc-400">{t('srv.col.ip')}</dt>
                  <dd className="flex min-w-0 flex-col items-end gap-0.5 tabular-nums text-zinc-600 dark:text-zinc-300">
                    <span className="inline-flex items-center gap-1">
                      {shown ? s.ip || '—' : maskIp(s.ip)}
                      {s.ip && <CopyBtn text={s.ip} />}
                      {(s.ip || s.ipv6) && (
                        <button className={iconBtn} onClick={() => toggleReveal(s.id)} title={t(shown ? 'common.hide' : 'common.show')}>
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
                  <dt className="shrink-0 text-zinc-400">{t('srv.col.expire')}</dt>
                  <dd className="text-zinc-600 dark:text-zinc-300">{s.expireAt || t('detail.info.expire.never')}</dd>
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
                {showUpgradeBtn(s) && (
                  <button
                    className={`${iconBtn} ${
                      s.upgradeStage === 'failed' ? '!text-rose-500' : s.upgradeHint ? '!text-amber-500' : ''
                    }`}
                    title={upgradeTitle(s)}
                    disabled={isUpgrading(s) || upgradeSubmitting === s.id}
                    onClick={() => upgradeAgent(s)}
                  >
                    {isUpgrading(s) || upgradeSubmitting === s.id ? (
                      <Loader2 className="h-4 w-4 animate-spin" />
                    ) : (
                      <ArrowUpCircle className="h-4 w-4" />
                    )}
                  </button>
                )}
                <button className={iconBtn} title={t('srv.install')} onClick={() => setInstall(s)}>
                  <Terminal className="h-4 w-4" />
                </button>
                <button className={iconBtn} title={t('common.edit')} onClick={() => setModal(s)}>
                  <Pencil className="h-4 w-4" />
                </button>
                <button className={`${iconBtn} hover:!text-rose-500`} title={t('common.delete')} onClick={() => setConfirmDel(s)}>
                  <Trash2 className="h-4 w-4" />
                </button>
              </div>
            </div>
          )
        })}
      </div>

      {modal === 'add' && (
        <ServerFormModal
          title={t('srv.add')}
          init={emptyServerForm}
          creds={creds}
          onClose={() => setModal(null)}
          onSubmit={async (f) => {
            const tempId = `tmp-${Date.now()}`
            const optimistic: AdminServer = {
              id: tempId, name: f.name, group: f.group, region: f.region, flag: f.flag,
              autoFlag: '', note: f.note, expireAt: f.expireAt, token: '',
              ip: '', ipv6: '', online: false,
              // 新建的机器还没装 agent，版本与可升级性都由后端在下次拉取时填。
              agentVersion: '', targetVersion: '', upgradable: false,
              gcpEnabled: f.gcpEnabled, gcpCredId: f.gcpCredId, gcpProject: f.gcpProject,
              gcpZone: f.gcpZone, gcpInstance: f.gcpInstance,
              gcpTries: 0, gcpLastTry: 0, gcpLastErr: '',
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
          title={t('srv.modal.edit', { name: modal.name })}
          init={{
            name: modal.name,
            group: modal.group,
            region: modal.region,
            flag: modal.flag,
            expireAt: modal.expireAt,
            note: modal.note,
            gcpEnabled: modal.gcpEnabled,
            gcpCredId: modal.gcpCredId,
            gcpProject: modal.gcpProject,
            gcpZone: modal.gcpZone,
            gcpInstance: modal.gcpInstance,
          }}
          creds={creds}
          onClose={() => setModal(null)}
          onSubmit={async (f) => {
            const id = modal.id
            setModal(null)
            await mutate(
              (l) => l.map((x) => (x.id === id ? { ...x, ...f } : x)),
              () => put(`/api/admin/servers/${id}`, f),
              { onSuccess: () => toast(t('common.saved')), onError: (e) => toast(errMsg(e)) },
            )
          }}
        />
      )}

      {install && (
        <Modal title={t('srv.install.title', { name: install.name })} onClose={closeInstall}>
          <p className="mb-3 text-xs text-zinc-500">{t('srv.install.intro')}</p>

          {/* 远程执行开关做进命令本身，而不是让人装完再去改配置文件。
              开关状态只影响复制出来的命令——控制权仍在真正去机器上执行它的人手里，
              面板改不了任何一台已装机器的这个设置。 */}
          <div className="mb-3 flex items-start justify-between gap-3 rounded-xl border border-white/40 bg-white/30 p-3 dark:border-white/10 dark:bg-white/5">
            <div className="min-w-0">
              <p className="text-sm font-medium">{t('srv.exec.title')}</p>
              <p className="mt-0.5 text-xs text-zinc-500">{t(allowExec ? 'srv.exec.on' : 'srv.exec.off')}</p>
            </div>
            <Switch on={allowExec} onChange={setAllowExec} />
          </div>

          <div className="space-y-3">
            <CmdBlock label="Linux / macOS" cmd={installCmds(install.token, allowExec).sh} />
            <CmdBlock label={t('srv.install.win')} cmd={installCmds(install.token, allowExec).ps} />
            <div className="flex items-center gap-1 text-xs text-zinc-500">
              Token：<code className="rounded bg-zinc-500/10 px-1.5 py-0.5 dark:bg-white/10">{install.token}</code>
              <CopyBtn text={install.token} />
            </div>
          </div>
          <div className="mt-3 flex justify-end">
            <button className={btnGhost} onClick={closeInstall}>
              {t('common.close')}
            </button>
          </div>
        </Modal>
      )}

      {confirmDel && (
        <ConfirmDelete
          title={t('srv.del.title')}
          onCancel={() => setConfirmDel(null)}
          onConfirm={() => {
            const target = confirmDel
            setConfirmDel(null)
            mutate(
              (l) => l.filter((x) => x.id !== target.id),
              () => del(`/api/admin/servers/${target.id}`),
              { onSuccess: () => toast(t('common.deleted')), onError: (e) => toast(errMsg(e)) },
            )
          }}
        >
          {t('srv.del.before')}
          <span className="font-semibold">{confirmDel.name}</span>
          {t('srv.del.after')}
        </ConfirmDelete>
      )}
    </div>
  )
}
