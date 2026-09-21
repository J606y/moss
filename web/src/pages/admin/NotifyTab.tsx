import { useEffect, useState } from 'react'
import { get, post, put } from '../../api/client'
import type { NotifySettings, WebhookSettings } from '../../types'
import { NumberInput, Toggle } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { btnGhost, btnPrimary, card, formLabel, input } from '../../ui'
import { useT } from '../../i18n'
import type { Toast } from './types'

export function NotifyTab({ toast }: { toast: Toast }) {
  const { t } = useT()
  const [n, setN] = useState<NotifySettings | null>(null)
  const [testing, setTesting] = useState(false)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    get<NotifySettings>('/api/admin/notify')
      .then(setN)
      .catch((e) => toast(errMsg(e)))
  }, [toast])

  if (!n) return <p className="text-sm text-zinc-500">{t('common.loading')}</p>

  const num = (k: keyof NotifySettings) => (v: number) => setN({ ...n, [k]: v })

  const save = async () => {
    setSaving(true)
    try {
      await put('/api/admin/notify', n)
      const saved = await get<NotifySettings>('/api/admin/notify')
      setN(saved)
      toast(t('notify.saved'))
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setSaving(false)
    }
  }

  const test = async () => {
    setTesting(true)
    try {
      await put('/api/admin/notify', n) // 先保存再测试，避免测到旧配置
      await post('/api/admin/notify/test', {})
      toast(t('notify.test.tg.sent'))
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setTesting(false)
    }
  }

  return (
    <div className="mx-auto max-w-xl space-y-4">
      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">{t('notify.tg')}</h3>
        <div>
          <label className={formLabel}>Bot Token</label>
          <input
            className={input}
            placeholder={t('notify.tg.token.placeholder')}
            value={n.tgToken}
            onChange={(e) => setN({ ...n, tgToken: e.target.value })}
          />
        </div>
        <div>
          <label className={formLabel}>Chat ID</label>
          <input
            className={input}
            placeholder={t('notify.tg.chat.placeholder')}
            value={n.tgChat}
            onChange={(e) => setN({ ...n, tgChat: e.target.value })}
          />
        </div>
        <div className="flex justify-end">
          <button className={btnGhost} onClick={test} disabled={testing || saving}>
            {t(testing ? 'notify.testing' : 'notify.test')}
          </button>
        </div>
      </div>

      {/* 两个推送通道排在一起，后面才是告警规则——按「发到哪去」和「什么时候发」
          分组，比按实现先后排列好懂。Webhook 原先落在页面最末、还在「保存设置」
          按钮下方，看起来像上面那张表单的附属品，实际它走的是独立接口。 */}
      <WebhookSection toast={toast} />

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">{t('notify.offline')}</h3>
        <Toggle
          checked={n.offlineOn}
          label={t('notify.offline.toggle')}
          onChange={(v) => setN({ ...n, offlineOn: v })}
        />
        <div>
          <label className={formLabel}>{t('notify.offline.delay')}</label>
          <NumberInput min={30} max={3600} value={n.offlineDelay} onChange={num('offlineDelay')} />
          <p className="mt-1 text-xs text-zinc-400">{t('notify.offline.delay.hint')}</p>
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">{t('notify.load')}</h3>
        <Toggle checked={n.loadOn} label={t('notify.load.toggle')} onChange={(v) => setN({ ...n, loadOn: v })} />
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>{t('notify.cpu')}</label>
            <NumberInput min={1} max={100} value={n.cpuThreshold} onChange={num('cpuThreshold')} />
          </div>
          <div>
            <label className={formLabel}>{t('notify.mem')}</label>
            <NumberInput min={1} max={100} value={n.memThreshold} onChange={num('memThreshold')} />
          </div>
          <div>
            <label className={formLabel}>{t('notify.disk')}</label>
            <NumberInput min={1} max={100} value={n.diskThreshold} onChange={num('diskThreshold')} />
          </div>
          <div>
            <label className={formLabel}>{t('notify.recover')}</label>
            <NumberInput min={10} max={3600} value={n.recoverSec} onChange={num('recoverSec')} />
          </div>
          <div>
            <label className={formLabel}>{t('notify.duration')}</label>
            <NumberInput min={1} max={120} value={n.loadMinutes} onChange={num('loadMinutes')} />
          </div>
        </div>
        {/* 恢复条件必须写准：实现是 val < 阈值×0.9（见 notify.go 的迟滞判定），
            原文案写的「回落至阈值 5% 以下」既数值不对，字面还会被读成
            「低于阈值的 5%」——阈值 90% 时那是 4.5%，永远等不到恢复通知。 */}
        <p className="text-xs text-zinc-400">{t('notify.load.hint')}</p>

        <div className="space-y-3 border-t border-zinc-500/10 pt-3 dark:border-white/5">
          <Toggle checked={n.netOn} label={t('notify.net.toggle')} onChange={(v) => setN({ ...n, netOn: v })} />
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className={formLabel}>{t('notify.net')}</label>
              <NumberInput min={1} max={100000} value={n.netThreshold} onChange={num('netThreshold')} />
            </div>
            <div>
              <label className={formLabel}>{t('notify.net.sec')}</label>
              <NumberInput min={10} max={3600} value={n.netSeconds} onChange={num('netSeconds')} />
            </div>
          </div>
          <p className="text-xs text-zinc-400">{t('notify.net.hint')}</p>
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">{t('notify.expire')}</h3>
        <Toggle checked={n.expireOn} label={t('notify.expire.toggle')} onChange={(v) => setN({ ...n, expireOn: v })} />
        <div>
          <label className={formLabel}>{t('notify.expire.days')}</label>
          <NumberInput min={1} max={7} value={n.expireDays} onChange={num('expireDays')} />
          <p className="mt-1 text-xs text-zinc-400">{t('notify.expire.hint')}</p>
        </div>
      </div>

      <div className="flex justify-end">
        <button className={btnPrimary} onClick={save} disabled={saving || testing}>
          {t(saving ? 'common.saving' : 'settings.save')}
        </button>
      </div>
    </div>
  )
}

/**
 * 通用 Webhook 推送：设置来自独立接口（/api/admin/webhook），不属于 NotifySettings，
 * 因此单独管理状态与保存按钮，模式参考 GcpTab 的凭证区块——密钥只写不回显。
 */
function WebhookSection({ toast }: { toast: Toast }) {
  const { t } = useT()
  const [w, setW] = useState<WebhookSettings | null>(null)
  // 密钥单独用一个受控字段：留空提交 = 保留原密钥，绝不会被服务端回显的明文污染
  const [secret, setSecret] = useState('')
  // 三个按钮打的都是同一个 PUT /api/admin/webhook，各自一个进行态：
  // 既能显示自己的文案，又要在任一在途时互相禁用，否则并发提交会互相覆盖。
  const [testing, setTesting] = useState(false)
  const [saving, setSaving] = useState(false)
  const [clearing, setClearing] = useState(false)

  useEffect(() => {
    get<WebhookSettings>('/api/admin/webhook')
      .then(setW)
      .catch((e) => toast(errMsg(e)))
  }, [toast])

  if (!w) return null

  // clearSecret 显式清空密钥；否则 secret 留空即代表「不修改」，由后端保证语义
  const save = async (clearSecret = false) => {
    await put('/api/admin/webhook', { url: w.url, on: w.on, secret, clearSecret })
    setSecret('')
    setW(await get<WebhookSettings>('/api/admin/webhook'))
  }

  const onSave = async () => {
    setSaving(true)
    try {
      await save()
      toast(t('wh.saved'))
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setSaving(false)
    }
  }

  const onClearSecret = async () => {
    setClearing(true)
    try {
      await save(true)
      toast(t('wh.secret.cleared'))
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setClearing(false)
    }
  }

  const onTest = async () => {
    setTesting(true)
    try {
      await save() // 先保存再测试，避免测到旧配置
      await post('/api/admin/webhook/test', {})
      toast(t('wh.test.sent'))
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setTesting(false)
    }
  }

  return (
    <div className={`${card} space-y-3 p-4`}>
      <h3 className="text-sm font-semibold">{t('wh.title')}</h3>
      <p className="text-xs text-zinc-400">{t('wh.intro')}</p>
      <Toggle checked={w.on} label={t('wh.toggle')} onChange={(v) => setW({ ...w, on: v })} />
      <div>
        <label className={formLabel}>{t('wh.url')}</label>
        <input
          className={`${input} truncate`}
          placeholder="https://your-ai-gateway.example.com/hooks/moss"
          value={w.url}
          onChange={(e) => setW({ ...w, url: e.target.value })}
        />
      </div>
      <div>
        <label className={formLabel}>{t('wh.secret')}</label>
        <input
          className={input}
          type="password"
          placeholder={t(w.secretSet ? 'wh.secret.set' : 'wh.secret.unset')}
          value={secret}
          onChange={(e) => setSecret(e.target.value)}
        />
      </div>
      <div className="flex flex-wrap justify-end gap-2">
        {w.secretSet && (
          <button
            className={`${btnGhost} !text-rose-500`}
            onClick={onClearSecret}
            disabled={clearing || saving || testing}
          >
            {t(clearing ? 'wh.clearing' : 'wh.clear')}
          </button>
        )}
        <button className={btnGhost} onClick={onTest} disabled={testing || saving || clearing}>
          {t(testing ? 'notify.testing' : 'notify.test')}
        </button>
        <button className={btnPrimary} onClick={onSave} disabled={saving || testing || clearing}>
          {t(saving ? 'common.saving' : 'common.save')}
        </button>
      </div>
    </div>
  )
}
