import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { get, put } from '../../api/client'
import type { Settings } from '../../types'
import { NumberInput } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { btnPrimary, card, formLabel, input } from '../../ui'
import type { Toast } from './types'

export function SettingsTab({ toast }: { toast: Toast }) {
  const navigate = useNavigate()
  const [s, setS] = useState<Settings | null>(null)
  const [pwd, setPwd] = useState({ old: '', new1: '', new2: '' })

  useEffect(() => {
    get<Settings>('/api/admin/settings')
      .then(setS)
      .catch((e) => toast(errMsg(e)))
  }, [toast])

  if (!s) return <p className="text-sm text-zinc-500">加载中…</p>

  const num = (k: keyof Settings) => (v: number) => setS({ ...s, [k]: v })

  const save = async () => {
    try {
      await put('/api/admin/settings', s)
      const saved = await get<Settings>('/api/admin/settings')
      setS(saved)
      toast('设置已保存')
    } catch (e) {
      toast(errMsg(e))
    }
  }

  const changePwd = async () => {
    if (pwd.new1.length < 6) {
      toast('新密码至少 6 位')
      return
    }
    if (!pwd.new1 || pwd.new1 !== pwd.new2) {
      toast('两次输入的新密码不一致')
      return
    }
    try {
      await put('/api/admin/password', { old: pwd.old, new: pwd.new1 })
      toast('密码已修改，请重新登录')
      setTimeout(() => navigate('/login'), 800)
    } catch (e) {
      toast(errMsg(e))
    }
  }

  return (
    <div className="mx-auto max-w-xl space-y-4">
      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">站点信息</h3>
        <div>
          <label className={formLabel}>管理员用户名</label>
          <input className={input} value={s.username} onChange={(e) => setS({ ...s, username: e.target.value })} />
        </div>
        <div>
          <label className={formLabel}>站点名称</label>
          <input className={input} value={s.siteName} onChange={(e) => setS({ ...s, siteName: e.target.value })} />
        </div>
        <div>
          <label className={formLabel}>站点描述</label>
          <input className={input} value={s.siteDesc} onChange={(e) => setS({ ...s, siteDesc: e.target.value })} />
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">数据采集</h3>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>实时上报间隔（秒）</label>
            <NumberInput min={1} value={s.reportInterval} onChange={num('reportInterval')} />
          </div>
          <div>
            <label className={formLabel}>历史采样间隔（秒）</label>
            <NumberInput min={5} value={s.sampleInterval} onChange={num('sampleInterval')} />
          </div>
          <div>
            <label className={formLabel}>历史数据保留（天）</label>
            <NumberInput min={1} value={s.historyDays} onChange={num('historyDays')} />
          </div>
          <div>
            <label className={formLabel}>延迟数据保留（天）</label>
            <NumberInput min={1} value={s.pingDays} onChange={num('pingDays')} />
          </div>
        </div>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={save}>
            保存设置
          </button>
        </div>
      </div>

      <div className={`${card} space-y-3 p-4`}>
        <h3 className="text-sm font-semibold">修改密码</h3>
        <div>
          <label className={formLabel}>当前密码</label>
          <input
            type="password"
            className={input}
            value={pwd.old}
            onChange={(e) => setPwd({ ...pwd, old: e.target.value })}
          />
        </div>
        <div className="grid grid-cols-2 gap-3">
          <div>
            <label className={formLabel}>新密码（至少 6 位）</label>
            <input
              type="password"
              className={input}
              value={pwd.new1}
              onChange={(e) => setPwd({ ...pwd, new1: e.target.value })}
            />
          </div>
          <div>
            <label className={formLabel}>确认新密码</label>
            <input
              type="password"
              className={input}
              value={pwd.new2}
              onChange={(e) => setPwd({ ...pwd, new2: e.target.value })}
            />
          </div>
        </div>
        <p className="text-xs text-zinc-400">修改密码后所有登录会话将失效，需要重新登录。</p>
        <div className="flex justify-end">
          <button className={btnPrimary} onClick={changePwd}>
            修改密码
          </button>
        </div>
      </div>
    </div>
  )
}
