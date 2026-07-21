import { useCallback, useEffect, useState } from 'react'
import { get, post, put } from '../../api/client'
import type { GcpSettings } from '../../types'
import { NumberInput, Toggle } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { btnGhost, btnPrimary, card, formLabel, input } from '../../ui'
import type { Toast } from './types'

export function GcpTab({ toast }: { toast: Toast }) {
  const [g, setG] = useState<GcpSettings | null>(null)
  const [saJson, setSaJson] = useState('')
  const [busy, setBusy] = useState(false)

  const load = useCallback(() => {
    get<GcpSettings>('/api/admin/gcp')
      .then(setG)
      .catch((e) => toast(errMsg(e)))
  }, [toast])
  useEffect(load, [load])

  if (!g) return <p className="text-sm text-zinc-500">加载中…</p>

  const num = (k: 'delay' | 'cooldown' | 'maxTries') => (v: number) => setG({ ...g, [k]: v })

  // saJson 留空 = 保留已保存的凭证；clearSa = 显式清除
  const save = async (clearSa = false) => {
    await put('/api/admin/gcp', {
      saJson, clearSa, autoOn: g.autoOn, delay: g.delay, cooldown: g.cooldown, maxTries: g.maxTries,
    })
    setSaJson('')
    setG(await get<GcpSettings>('/api/admin/gcp'))
  }

  const onSave = async () => {
    try {
      await save()
      toast('GCP 设置已保存')
    } catch (e) {
      toast(errMsg(e))
    }
  }

  const onClear = async () => {
    try {
      await save(true)
      toast('凭证已清除')
    } catch (e) {
      toast(errMsg(e))
    }
  }

  const onTest = async () => {
    setBusy(true)
    try {
      await save() // 先保存再测试，避免测到旧凭证
      const res = await post<{ clientEmail: string }>('/api/admin/gcp/test', {})
      toast(`连接成功：${res.clientEmail}`)
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="mx-auto max-w-xl space-y-4">
      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">Service Account 凭证</h3>
        <p className="text-xs text-zinc-500">
          {g.configured ? (
            <>
              当前凭证：<code className="rounded bg-zinc-500/10 px-1.5 py-0.5 dark:bg-white/10">{g.clientEmail || '（无法解析）'}</code>
              {g.projectId && <> / 项目 <code className="rounded bg-zinc-500/10 px-1.5 py-0.5 dark:bg-white/10">{g.projectId}</code></>}
            </>
          ) : (
            '未配置。在 GCP 控制台创建 Service Account 并下载 JSON 密钥，粘贴到下方。'
          )}
        </p>
        <div>
          <label className={formLabel}>Service Account JSON 密钥</label>
          <textarea
            className={`${input} min-h-28 font-mono text-xs`}
            placeholder={g.configured ? '粘贴新的 JSON 以更换凭证（留空则保留现有凭证）' : '{ "type": "service_account", ... }'}
            value={saJson}
            onChange={(e) => setSaJson(e.target.value)}
          />
          <p className="mt-1 text-xs text-zinc-400">
            只需授予 compute.instances.get 与 compute.instances.start 两个权限；私钥保存后不再回显。
          </p>
        </div>
        <div className="flex justify-end gap-2">
          {g.configured && (
            <button className={`${btnGhost} !text-rose-500`} onClick={onClear}>
              清除凭证
            </button>
          )}
          <button className={btnGhost} onClick={onTest} disabled={busy}>
            {busy ? '测试中…' : '保存并测试连接'}
          </button>
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">自动开机</h3>
        <Toggle
          checked={g.autoOn}
          label="节点离线后自动调用 GCP API 开机（总开关）"
          onChange={(v) => setG({ ...g, autoOn: v })}
        />
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>离线确认延迟（秒，60 ~ 3600）</label>
            <NumberInput min={60} max={3600} value={g.delay} onChange={num('delay')} />
          </div>
          <div>
            <label className={formLabel}>重试冷却（秒，60 ~ 3600）</label>
            <NumberInput min={60} max={3600} value={g.cooldown} onChange={num('cooldown')} />
          </div>
          <div>
            <label className={formLabel}>最大尝试次数（1 ~ 10）</label>
            <NumberInput min={1} max={10} value={g.maxTries} onChange={num('maxTries')} />
          </div>
        </div>
        <p className="text-xs text-zinc-400">
          节点确认离线后查询实例状态，仅在 TERMINATED（Spot 被抢占）时调用开机；实例 RUNNING
          但节点离线（agent/网络故障）不会重复开机。达到最大尝试次数后停止并通知，节点重新上线自动复位计数。
          需在「服务器」页对具体节点开启 GCP 自动开机并填写 zone / 实例名。
        </p>
        <p className="text-xs text-amber-500/90">
          注意：面板本身请勿部署在被守护的 Spot 实例上；人为关机维护前请先关闭对应节点的自动开机开关，否则会被自动拉起。
        </p>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={onSave}>
            保存设置
          </button>
        </div>
      </div>
    </div>
  )
}
