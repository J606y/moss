import { useState } from 'react'
import { Modal, Select, Toggle } from '../../components/ui'
import { btnGhost, btnPrimary, formLabel, input } from '../../ui'
import { useT } from '../../i18n'
import type { GcpCredential } from '../../types'

export interface ServerFormData {
  name: string
  group: string
  region: string
  flag: string
  expireAt: string
  note: string
  gcpEnabled: boolean
  gcpCredId: string
  gcpProject: string
  gcpZone: string
  gcpInstance: string
}

export const emptyServerForm: ServerFormData = {
  name: '', group: '', region: '', flag: '', expireAt: '', note: '',
  gcpEnabled: false, gcpCredId: '', gcpProject: '', gcpZone: '', gcpInstance: '',
}

export function ServerFormModal({
  title,
  init,
  creds,
  onClose,
  onSubmit,
}: {
  title: string
  init: ServerFormData
  /** GCP 凭证列表；null = 仍在加载，用于避免一进来就闪一下「尚未添加凭证」 */
  creds: GcpCredential[] | null
  onClose: () => void
  onSubmit: (f: ServerFormData) => Promise<void>
}) {
  const { t } = useT()
  const [f, setF] = useState(init)
  const [busy, setBusy] = useState(false)
  const set = (k: keyof ServerFormData) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setF((prev) => ({ ...prev, [k]: e.target.value }))

  const credOptions = [
    { value: '', label: t('srv.gcp.cred.pick') },
    ...(creds ?? []).map((c) => ({ value: c.id, label: c.projectId })),
    // 绑定的凭证已被删除时补一个占位项：否则 Select 会把裸 id 当标签显示出来。
    ...(f.gcpCredId && creds && !creds.some((c) => c.id === f.gcpCredId)
      ? [{ value: f.gcpCredId, label: t('srv.gcp.cred.deleted') }]
      : []),
  ]

  return (
    <Modal title={title} onClose={onClose}>
      <div className="space-y-3">
        <div>
          <label className={formLabel}>{t('srv.name')}</label>
          <input
            className={input}
            placeholder={t('srv.name.placeholder')}
            value={f.name}
            onChange={set('name')}
            autoFocus
          />
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>{t('srv.group')}</label>
            <input
              className={input}
              placeholder={t('srv.group.placeholder')}
              value={f.group}
              onChange={set('group')}
            />
          </div>
          <div>
            <label className={formLabel}>{t('srv.region')}</label>
            <input
              className={input}
              placeholder={t('srv.region.placeholder')}
              value={f.region}
              onChange={set('region')}
            />
          </div>
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>{t('srv.flag')}</label>
            <input className={input} placeholder="hk / jp / us…" value={f.flag} onChange={set('flag')} />
          </div>
          <div>
            <label className={formLabel}>{t('srv.expire')}</label>
            <input className={input} placeholder="2026-12-31" value={f.expireAt} onChange={set('expireAt')} />
          </div>
        </div>
        <div>
          <label className={formLabel}>{t('srv.note')}</label>
          <input className={input} placeholder={t('srv.note.placeholder')} value={f.note} onChange={set('note')} />
        </div>

        <div className="space-y-3 border-t border-zinc-500/10 pt-3 dark:border-white/5">
          <Toggle
            checked={f.gcpEnabled}
            label={t('srv.gcp.toggle')}
            onChange={(v) =>
              setF((prev) => ({
                ...prev,
                gcpEnabled: v,
                // 只有一份凭证就没什么可选的，打开开关时直接替用户选上
                gcpCredId: v && !prev.gcpCredId && creds?.length === 1 ? creds[0].id : prev.gcpCredId,
              }))
            }
          />
          {f.gcpEnabled && (
            <>
              <div>
                <label className={formLabel}>{t('srv.gcp.cred')}</label>
                <Select
                  value={f.gcpCredId}
                  options={credOptions}
                  onChange={(v) => setF((prev) => ({ ...prev, gcpCredId: v }))}
                />
                {creds?.length === 0 && (
                  <p className="mt-1 text-xs text-amber-500/90">{t('srv.gcp.cred.none')}</p>
                )}
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className={formLabel}>Zone *</label>
                  <input className={input} placeholder="us-central1-a" value={f.gcpZone} onChange={set('gcpZone')} />
                </div>
                <div>
                  <label className={formLabel}>{t('srv.gcp.instance')}</label>
                  <input
                    className={input}
                    placeholder={t('srv.gcp.instance.placeholder')}
                    value={f.gcpInstance}
                    onChange={set('gcpInstance')}
                  />
                </div>
              </div>
              <div>
                <label className={formLabel}>{t('srv.gcp.project')}</label>
                <input
                  className={input}
                  placeholder={t('srv.gcp.project.placeholder')}
                  value={f.gcpProject}
                  onChange={set('gcpProject')}
                />
              </div>
              <p className="text-xs text-zinc-400">{t('srv.gcp.hint')}</p>
            </>
          )}
        </div>

        <div className="flex justify-end gap-2 pt-2">
          <button className={btnGhost} onClick={onClose}>
            {t('common.cancel')}
          </button>
          <button
            className={btnPrimary}
            disabled={
              busy ||
              !f.name.trim() ||
              (f.gcpEnabled && (!f.gcpCredId || !f.gcpZone.trim() || !f.gcpInstance.trim()))
            }
            onClick={async () => {
              setBusy(true)
              try {
                await onSubmit(f)
              } finally {
                setBusy(false)
              }
            }}
          >
            {t('common.save')}
          </button>
        </div>
      </div>
    </Modal>
  )
}
