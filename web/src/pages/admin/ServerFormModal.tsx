import { useState } from 'react'
import { Modal, Select, Toggle } from '../../components/ui'
import { btnGhost, btnPrimary, formLabel, input } from '../../ui'
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
  const [f, setF] = useState(init)
  const [busy, setBusy] = useState(false)
  const set = (k: keyof ServerFormData) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setF((prev) => ({ ...prev, [k]: e.target.value }))

  const credOptions = [
    { value: '', label: '请选择凭证' },
    ...(creds ?? []).map((c) => ({ value: c.id, label: c.projectId })),
    // 绑定的凭证已被删除时补一个占位项：否则 Select 会把裸 id 当标签显示出来。
    ...(f.gcpCredId && creds && !creds.some((c) => c.id === f.gcpCredId)
      ? [{ value: f.gcpCredId, label: '（凭证已删除，请重新选择）' }]
      : []),
  ]

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
            <label className={formLabel}>地区（不填则自动）</label>
            <input className={input} placeholder="香港 / 东京…" value={f.region} onChange={set('region')} />
          </div>
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>国旗代码（不填则自动）</label>
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

        <div className="space-y-3 border-t border-zinc-500/10 pt-3 dark:border-white/5">
          <Toggle
            checked={f.gcpEnabled}
            label="GCP 自动开机（Spot 实例被抢占后自动拉起）"
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
                <label className={formLabel}>凭证 *</label>
                <Select
                  value={f.gcpCredId}
                  options={credOptions}
                  onChange={(v) => setF((prev) => ({ ...prev, gcpCredId: v }))}
                />
                {creds?.length === 0 && (
                  <p className="mt-1 text-xs text-amber-500/90">
                    尚未添加任何凭证，请先到「GCP 守护」页添加。
                  </p>
                )}
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className={formLabel}>Zone *</label>
                  <input className={input} placeholder="us-central1-a" value={f.gcpZone} onChange={set('gcpZone')} />
                </div>
                <div>
                  <label className={formLabel}>实例名 *</label>
                  <input className={input} placeholder="GCP 控制台里的实例名称" value={f.gcpInstance} onChange={set('gcpInstance')} />
                </div>
              </div>
              <div>
                <label className={formLabel}>项目 ID（可选）</label>
                <input className={input} placeholder="留空使用所选凭证的 project_id" value={f.gcpProject} onChange={set('gcpProject')} />
              </div>
              <p className="text-xs text-zinc-400">
                凭证决定用哪个 GCP 账号开机；同一账号下实例在别的项目时，才需要填项目 ID。
                需在「GCP 守护」页开启总开关；人为关机前请先关闭此开关，否则会被自动拉起。
              </p>
            </>
          )}
        </div>

        <div className="flex justify-end gap-2 pt-2">
          <button className={btnGhost} onClick={onClose}>
            取消
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
            保存
          </button>
        </div>
      </div>
    </Modal>
  )
}
