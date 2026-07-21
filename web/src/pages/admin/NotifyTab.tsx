import { useEffect, useState } from 'react'
import { get, post, put } from '../../api/client'
import type { NotifySettings } from '../../types'
import { NumberInput, Toggle } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { btnGhost, btnPrimary, card, formLabel, input } from '../../ui'
import type { Toast } from './types'

export function NotifyTab({ toast }: { toast: Toast }) {
  const [n, setN] = useState<NotifySettings | null>(null)
  const [testing, setTesting] = useState(false)

  useEffect(() => {
    get<NotifySettings>('/api/admin/notify')
      .then(setN)
      .catch((e) => toast(errMsg(e)))
  }, [toast])

  if (!n) return <p className="text-sm text-zinc-500">加载中…</p>

  const num = (k: keyof NotifySettings) => (v: number) => setN({ ...n, [k]: v })

  const save = async () => {
    try {
      await put('/api/admin/notify', n)
      const saved = await get<NotifySettings>('/api/admin/notify')
      setN(saved)
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
    <div className="mx-auto max-w-xl space-y-4">
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
          <NumberInput min={30} max={3600} value={n.offlineDelay} onChange={num('offlineDelay')} />
          <p className="mt-1 text-xs text-zinc-400">离线超过该时长才告警，避免网络抖动产生骚扰。</p>
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">负载告警</h3>
        <Toggle checked={n.loadOn} label="资源使用率持续超阈值时推送通知" onChange={(v) => setN({ ...n, loadOn: v })} />
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>CPU 阈值（%）</label>
            <NumberInput min={1} max={100} value={n.cpuThreshold} onChange={num('cpuThreshold')} />
          </div>
          <div>
            <label className={formLabel}>内存阈值（%）</label>
            <NumberInput min={1} max={100} value={n.memThreshold} onChange={num('memThreshold')} />
          </div>
          <div>
            <label className={formLabel}>硬盘阈值（%）</label>
            <NumberInput min={1} max={100} value={n.diskThreshold} onChange={num('diskThreshold')} />
          </div>
          <div>
            <label className={formLabel}>持续时间（分钟）</label>
            <NumberInput min={1} max={120} value={n.loadMinutes} onChange={num('loadMinutes')} />
          </div>
        </div>
        <p className="text-xs text-zinc-400">超阈值持续指定时间才告警；回落至阈值 5% 以下时发送恢复通知。</p>

        <div className="space-y-3 border-t border-zinc-500/10 pt-3 dark:border-white/5">
          <Toggle
            checked={n.netOn}
            label="单台服务器网速持续超阈值时推送通知"
            onChange={(v) => setN({ ...n, netOn: v })}
          />
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className={formLabel}>网速阈值（MB/s）</label>
              <NumberInput min={1} max={100000} value={n.netThreshold} onChange={num('netThreshold')} />
            </div>
            <div>
              <label className={formLabel}>持续时间（秒，10 ~ 3600）</label>
              <NumberInput min={10} max={3600} value={n.netSeconds} onChange={num('netSeconds')} />
            </div>
          </div>
          <p className="text-xs text-zinc-400">上行或下行任一方向超过阈值并持续指定时长即告警，与上方开关相互独立。</p>
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">到期提醒</h3>
        <Toggle checked={n.expireOn} label="服务器临近到期时推送通知" onChange={(v) => setN({ ...n, expireOn: v })} />
        <div>
          <label className={formLabel}>提前天数（1 ~ 7）</label>
          <NumberInput min={1} max={7} value={n.expireDays} onChange={num('expireDays')} />
          <p className="mt-1 text-xs text-zinc-400">
            到期时间需为 YYYY-MM-DD 格式方可识别；每个到期日只提醒一次，续期修改日期后会按新日期重新提醒。
          </p>
        </div>
      </div>

      <div className="flex justify-end">
        <button className={btnPrimary} onClick={save}>
          保存设置
        </button>
      </div>
    </div>
  )
}
