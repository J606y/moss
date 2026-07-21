import { useState } from 'react'
import type { AdminServer, PingTask } from '../../types'
import { CheckBox, Modal, NumberInput, Select } from '../../components/ui'
import { btnGhost, btnPrimary, formLabel, input } from '../../ui'

export interface TaskFormData {
  name: string
  type: PingTask['type']
  target: string
  interval: number
  serverId: string
}

export const emptyTaskForm: TaskFormData = { name: '', type: 'icmp', target: '', interval: 60, serverId: '' }

/** 应用范围多选：value 为空字符串=全部服务器，否则为逗号分隔的服务器 ID 列表 */
function ServerPicker({
  servers,
  value,
  onChange,
}: {
  servers: AdminServer[]
  value: string
  onChange: (v: string) => void
}) {
  const all = value === ''
  const picked = new Set(value ? value.split(',') : [])
  const toggle = (id: string) => {
    const next = new Set(picked)
    if (next.has(id)) next.delete(id)
    else next.add(id)
    onChange(next.size === 0 ? '' : servers.filter((s) => next.has(s.id)).map((s) => s.id).join(','))
  }
  const row =
    'flex w-full cursor-pointer select-none items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm transition hover:bg-white/50 dark:hover:bg-white/10'
  return (
    <div className="glass-sheen max-h-44 space-y-0.5 overflow-y-auto rounded-xl border border-white/50 bg-white/45 p-1.5 dark:border-white/10 dark:bg-zinc-900/40">
      <button type="button" role="checkbox" aria-checked={all} className={row} onClick={() => onChange('')}>
        <CheckBox checked={all} />
        <span className={all ? 'font-medium' : ''}>全部服务器</span>
      </button>
      {servers.map((s) => {
        const on = !all && picked.has(s.id)
        return (
          <button key={s.id} type="button" role="checkbox" aria-checked={on} className={row} onClick={() => toggle(s.id)}>
            <CheckBox checked={on} />
            <span>{s.name}</span>
          </button>
        )
      })}
      {servers.length === 0 && <p className="px-2 py-1.5 text-sm text-zinc-400">暂无服务器</p>}
    </div>
  )
}

export function TaskFormModal({
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
            <Select
              value={f.type}
              options={[
                { value: 'icmp', label: 'ICMP' },
                { value: 'tcp', label: 'TCP' },
                { value: 'http', label: 'HTTP' },
              ]}
              onChange={(v) => setF({ ...f, type: v })}
            />
          </div>
          <div>
            <label className={formLabel}>间隔（秒）</label>
            <NumberInput min={10} value={f.interval} onChange={(v) => setF({ ...f, interval: v })} />
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
          <ServerPicker servers={servers} value={f.serverId} onChange={(v) => setF({ ...f, serverId: v })} />
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
