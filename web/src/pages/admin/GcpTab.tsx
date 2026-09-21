import { useCallback, useEffect, useState } from 'react'
import { PlugZap, Trash2 } from 'lucide-react'
import { del, get, post, put } from '../../api/client'
import type { GcpCredential, GcpSettings } from '../../types'
import { ConfirmDelete, NumberInput, Toggle } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { fmtDateTime } from '../../utils/format'
import { btnPrimary, card, formLabel, iconBtn, input, td, th } from '../../ui'
import { useT } from '../../i18n'
import type { Toast } from './types'

export function GcpTab({ toast }: { toast: Toast }) {
  const { t } = useT()
  const [g, setG] = useState<GcpSettings | null>(null)
  const [saving, setSaving] = useState(false)
  const [saJson, setSaJson] = useState('')
  // 凭证上的按钮按 id 门控：多份凭证并列时，测试第一份不该把第二份的按钮也禁掉。
  // 值形如 'test:abc123'，同一时刻只允许一个凭证在操作中。
  const [credBusy, setCredBusy] = useState('')
  const [deleting, setDeleting] = useState<GcpCredential | null>(null)

  const load = useCallback(() => {
    get<GcpSettings>('/api/admin/gcp')
      .then(setG)
      .catch((e) => toast(errMsg(e)))
  }, [toast])
  useEffect(load, [load])

  if (!g) return <p className="text-sm text-zinc-500">{t('common.loading')}</p>

  const num = (k: 'delay' | 'cooldown' | 'maxTries') => (v: number) => setG({ ...g, [k]: v })
  const refresh = async () => setG(await get<GcpSettings>('/api/admin/gcp'))

  const onAdd = async () => {
    setCredBusy('add')
    try {
      await post('/api/admin/gcp/credentials', { saJson })
      setSaJson('')
      await refresh()
      toast(t('gcp.cred.added'))
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setCredBusy('')
    }
  }

  const onTest = async (c: GcpCredential) => {
    setCredBusy('test:' + c.id)
    try {
      const res = await post<{ clientEmail: string }>(`/api/admin/gcp/credentials/${c.id}/test`, {})
      toast(t('gcp.test.ok', { email: res.clientEmail }))
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setCredBusy('')
    }
  }

  const onDelete = async (c: GcpCredential) => {
    setDeleting(null)
    setCredBusy('del:' + c.id)
    try {
      await del(`/api/admin/gcp/credentials/${c.id}`)
      await refresh()
      toast(t('gcp.cred.deleted'))
    } catch (e) {
      // 被节点占用时后端返回的是一句写明了机器名与出路的话，原样透传
      toast(errMsg(e))
    } finally {
      setCredBusy('')
    }
  }

  const onSaveSettings = async () => {
    setSaving(true)
    try {
      await put('/api/admin/gcp', {
        autoOn: g.autoOn, delay: g.delay, cooldown: g.cooldown, maxTries: g.maxTries,
      })
      toast(t('gcp.saved'))
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="space-y-4">
      <section className={`${card} mx-auto max-w-4xl space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">{t('gcp.add.title')}</h3>
        <div>
          <label className={formLabel}>{t('gcp.add.label')}</label>
          <textarea
            className={`${input} min-h-28 font-mono text-xs`}
            placeholder='{ "type": "service_account", ... }'
            value={saJson}
            onChange={(e) => setSaJson(e.target.value)}
          />
          <p className="mt-1 text-xs text-zinc-400">{t('gcp.add.hint')}</p>
        </div>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={onAdd} disabled={credBusy !== '' || !saJson.trim()}>
            {t(credBusy === 'add' ? 'common.saving' : 'gcp.add.submit')}
          </button>
        </div>
      </section>

      <section className={`${card} mx-auto max-w-4xl p-4`}>
        <h3 className="mb-3 text-sm font-semibold">{t('gcp.list.title')}</h3>
        {g.credentials.length === 0 ? (
          <p className="py-8 text-center text-sm text-zinc-400">{t('gcp.list.empty')}</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full">
              <thead>
                {/* 次要列窄屏隐藏：手机上要能直接够到测试/删除，而不是横滑一段才摸得到 */}
                <tr className="border-b border-white/40 dark:border-white/10">
                  <th className={th}>{t('gcp.col.project')}</th>
                  <th className={`${th} hidden md:table-cell`}>Service Account</th>
                  <th className={th}>{t('gcp.col.inUse')}</th>
                  <th className={`${th} hidden lg:table-cell`}>{t('gcp.col.added')}</th>
                  <th className={th}></th>
                </tr>
              </thead>
              <tbody>
                {g.credentials.map((c) => (
                  <tr key={c.id} className="border-b border-white/25 dark:border-white/5">
                    <td className={td}>
                      <div className="flex items-center gap-2">
                        {c.projectId}
                        {!c.decryptable && (
                          <span
                            className="rounded bg-rose-500/10 px-1.5 py-0.5 text-xs text-rose-500"
                            title={t('gcp.undecryptable.title')}
                          >
                            {t('gcp.undecryptable')}
                          </span>
                        )}
                      </div>
                    </td>
                    {/* 邮箱形如 moss-starter@<项目>.iam.gserviceaccount.com，能有 60 字符。
                        不截断的话它会把整张表撑出容器，操作按钮被挤到横向滚动区外——
                        看得见凭证却点不到删除。 */}
                    <td
                      className={`${td} hidden max-w-[15rem] truncate font-mono text-xs text-zinc-500 md:table-cell`}
                      title={c.clientEmail}
                    >
                      {c.clientEmail}
                    </td>
                    <td className={td}>
                      {c.serverCount > 0 ? (
                        t('gcp.inUse.count', { n: c.serverCount })
                      ) : (
                        <span className="text-zinc-400">{t('gcp.inUse.none')}</span>
                      )}
                    </td>
                    <td className={`${td} hidden lg:table-cell`}>{fmtDateTime(c.createdAt * 1000)}</td>
                    <td className={`${td} text-right`}>
                      <div className="flex justify-end gap-1">
                        <button
                          className={iconBtn}
                          title={t('gcp.test')}
                          onClick={() => onTest(c)}
                          disabled={credBusy !== ''}
                        >
                          <PlugZap className="h-4 w-4" />
                        </button>
                        <button
                          className={iconBtn}
                          title={t('common.delete')}
                          onClick={() => setDeleting(c)}
                          disabled={credBusy !== ''}
                        >
                          <Trash2 className="h-4 w-4" />
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {g.unboundCount > 0 && (
          <p className="mt-3 text-xs text-amber-500/90">{t('gcp.unbound', { n: g.unboundCount })}</p>
        )}
      </section>

      <section className={`${card} mx-auto max-w-4xl space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">{t('gcp.auto.title')}</h3>
        <Toggle
          checked={g.autoOn}
          label={t('gcp.auto.toggle')}
          onChange={(v) => setG({ ...g, autoOn: v })}
        />
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>{t('gcp.delay')}</label>
            <NumberInput min={60} max={3600} value={g.delay} onChange={num('delay')} />
          </div>
          <div>
            <label className={formLabel}>{t('gcp.cooldown')}</label>
            <NumberInput min={60} max={3600} value={g.cooldown} onChange={num('cooldown')} />
          </div>
          <div>
            <label className={formLabel}>{t('gcp.maxTries')}</label>
            <NumberInput min={1} max={10} value={g.maxTries} onChange={num('maxTries')} />
          </div>
        </div>
        <p className="text-xs text-zinc-400">{t('gcp.hint')}</p>
        <p className="text-xs text-amber-500/90">{t('gcp.warn')}</p>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={onSaveSettings} disabled={saving || credBusy !== ''}>
            {t(saving ? 'common.saving' : 'settings.save')}
          </button>
        </div>
      </section>

      {deleting && (
        <ConfirmDelete
          title={t('gcp.del.title')}
          onCancel={() => setDeleting(null)}
          onConfirm={() => onDelete(deleting)}
        >
          {t('gcp.del.before')}
          <b>{deleting.projectId}</b>
          {t('gcp.del.after', { email: deleting.clientEmail })}
          {deleting.serverCount > 0
            ? t('gcp.del.inUse', { n: deleting.serverCount })
            : t('gcp.del.gone')}
        </ConfirmDelete>
      )}
    </div>
  )
}
