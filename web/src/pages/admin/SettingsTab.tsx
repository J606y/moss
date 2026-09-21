import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { get, put } from '../../api/client'
import type { Settings } from '../../types'
import { NumberInput, Select } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { btnPrimary, card, formLabel, input } from '../../ui'
import { getLang, setSiteLang, translate, useT, type LangMode } from '../../i18n'
import type { Toast } from './types'

export function SettingsTab({ toast }: { toast: Toast }) {
  const navigate = useNavigate()
  const { t } = useT()
  const [s, setS] = useState<Settings | null>(null)
  const [pwd, setPwd] = useState({ old: '', new1: '', new2: '' })
  const [saving, setSaving] = useState(false)
  // 改密单独一个门控：连点第二次会带着已经失效的旧密码再提交一遍，
  // 除了一条多余的失败 toast 什么也得不到。
  const [changing, setChanging] = useState(false)

  useEffect(() => {
    get<Settings>('/api/admin/settings')
      .then(setS)
      .catch((e) => toast(errMsg(e)))
  }, [toast])

  if (!s) return <p className="text-sm text-zinc-500">{t('common.loading')}</p>

  const num = (k: keyof Settings) => (v: number) => setS({ ...s, [k]: v })

  const langOptions: Array<{ value: LangMode; label: string }> = [
    { value: 'auto', label: t('settings.lang.auto') },
    { value: 'zh', label: t('settings.lang.zh') },
    { value: 'en', label: t('settings.lang.en') },
  ]

  const save = async () => {
    setSaving(true)
    try {
      await put('/api/admin/settings', s)
      const saved = await get<Settings>('/api/admin/settings')
      setS(saved)
      // 以服务端回读的值为准落档，整个界面当场换语言，不必刷新页面。
      // 这条 toast 要用落档后的语言取文案：闭包里的 t 还停在保存前那一门，
      // 刚把界面切成英文却弹出一句中文提示，正是最扎眼的地方。
      setSiteLang(saved.lang)
      toast(translate(getLang(), 'settings.saved'))
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setSaving(false)
    }
  }

  const changePwd = async () => {
    if (pwd.new1.length < 6) {
      toast(t('settings.password.tooShort'))
      return
    }
    if (!pwd.new1 || pwd.new1 !== pwd.new2) {
      toast(t('settings.password.mismatch'))
      return
    }
    setChanging(true)
    try {
      await put('/api/admin/password', { old: pwd.old, new: pwd.new1 })
      toast(t('settings.password.changed'))
      // 成功后不解除门控：跳转前的这 800ms 里旧密码已经失效，再点一次只会换来一条失败提示
      setTimeout(() => navigate('/login'), 800)
    } catch (e) {
      toast(errMsg(e))
      setChanging(false)
    }
  }

  return (
    <div className="mx-auto max-w-xl space-y-4">
      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">{t('settings.siteInfo')}</h3>
        <div>
          <label className={formLabel}>{t('settings.username')}</label>
          <input className={input} value={s.username} onChange={(e) => setS({ ...s, username: e.target.value })} />
        </div>
        <div>
          <label className={formLabel}>{t('settings.siteName')}</label>
          <input className={input} value={s.siteName} onChange={(e) => setS({ ...s, siteName: e.target.value })} />
        </div>
        <div>
          <label className={formLabel}>{t('settings.siteDesc')}</label>
          <input className={input} value={s.siteDesc} onChange={(e) => setS({ ...s, siteDesc: e.target.value })} />
        </div>
        <div>
          <label className={formLabel}>{t('settings.lang')}</label>
          <Select value={s.lang} options={langOptions} onChange={(lang) => setS({ ...s, lang })} />
          <p className="mt-1 text-xs text-zinc-400">{t('settings.lang.hint')}</p>
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">{t('settings.collect')}</h3>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>{t('settings.reportInterval')}</label>
            <NumberInput min={1} value={s.reportInterval} onChange={num('reportInterval')} />
          </div>
          <div>
            <label className={formLabel}>{t('settings.sampleInterval')}</label>
            <NumberInput min={5} value={s.sampleInterval} onChange={num('sampleInterval')} />
          </div>
          <div>
            <label className={formLabel}>{t('settings.historyDays')}</label>
            <NumberInput min={1} value={s.historyDays} onChange={num('historyDays')} />
          </div>
          <div>
            <label className={formLabel}>{t('settings.pingDays')}</label>
            <NumberInput min={1} value={s.pingDays} onChange={num('pingDays')} />
          </div>
          <div>
            <label className={formLabel}>{t('settings.execAuditDays')}</label>
            <NumberInput min={7} max={90} value={s.execAuditDays} onChange={num('execAuditDays')} />
            {/* 下限 7 天不是随手定的：少于一周，周末发生的事周一就查不到了，
                而周末恰恰是无人值守、AI 自主处置最多的时候。 */}
            <p className="mt-1 text-xs text-zinc-400">{t('settings.execAuditDays.hint')}</p>
          </div>
          <div>
            <label className={formLabel}>{t('settings.execAuditMaxRows')}</label>
            <NumberInput min={100} max={5000} value={s.execAuditMaxRows} onChange={num('execAuditMaxRows')} />
            <p className="mt-1 text-xs text-zinc-400">{t('settings.execAuditMaxRows.hint')}</p>
          </div>
        </div>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={save} disabled={saving}>
            {saving ? t('common.saving') : t('settings.save')}
          </button>
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">{t('settings.password')}</h3>
        <div>
          <label className={formLabel}>{t('settings.password.current')}</label>
          <input
            type="password"
            className={input}
            value={pwd.old}
            onChange={(e) => setPwd({ ...pwd, old: e.target.value })}
          />
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>{t('settings.password.new')}</label>
            <input
              type="password"
              className={input}
              value={pwd.new1}
              onChange={(e) => setPwd({ ...pwd, new1: e.target.value })}
            />
          </div>
          <div>
            <label className={formLabel}>{t('settings.password.confirm')}</label>
            <input
              type="password"
              className={input}
              value={pwd.new2}
              onChange={(e) => setPwd({ ...pwd, new2: e.target.value })}
            />
          </div>
        </div>
        <p className="text-xs text-zinc-400">{t('settings.password.hint')}</p>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={changePwd} disabled={changing}>
            {changing ? t('settings.password.submitting') : t('settings.password.submit')}
          </button>
        </div>
      </div>
    </div>
  )
}
