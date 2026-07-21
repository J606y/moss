import { useState } from 'react'
import { Modal, Toggle } from '../../components/ui'
import { btnGhost, btnPrimary, formLabel, input } from '../../ui'

export interface ServerFormData {
  name: string
  group: string
  region: string
  flag: string
  expireAt: string
  note: string
  gcpEnabled: boolean
  gcpProject: string
  gcpZone: string
  gcpInstance: string
}

export const emptyServerForm: ServerFormData = {
  name: '', group: '', region: '', flag: '', expireAt: '', note: '',
  gcpEnabled: false, gcpProject: '', gcpZone: '', gcpInstance: '',
}

export function ServerFormModal({
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
            onChange={(v) => setF((prev) => ({ ...prev, gcpEnabled: v }))}
          />
          {f.gcpEnabled && (
            <>
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
                <input className={input} placeholder="留空使用凭证中的 project_id" value={f.gcpProject} onChange={set('gcpProject')} />
              </div>
              <p className="text-xs text-zinc-400">
                需先在「GCP 守护」页配置 Service Account 凭证并开启总开关；人为关机前请先关闭此开关，否则会被自动拉起。
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
            disabled={busy || !f.name.trim() || (f.gcpEnabled && (!f.gcpZone.trim() || !f.gcpInstance.trim()))}
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
