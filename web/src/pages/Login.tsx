import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { post } from '../api/client'
import MossEye from '../components/MossEye'
import { btnPrimary, card, formLabel, input } from '../ui'
import { useT } from '../i18n'
import { errMsg } from '../utils/admin'

export default function Login() {
  const navigate = useNavigate()
  const { t } = useT()
  const [user, setUser] = useState('admin')
  const [pwd, setPwd] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async () => {
    if (!user || !pwd || busy) return
    setBusy(true)
    setErr('')
    try {
      await post('/api/login', { username: user, password: pwd })
      navigate('/admin')
    } catch (e) {
      // errMsg 按后端返回的错误码翻译（如 auth.bad_credentials），认不出
      // 就退回后端给的中文原文。非 Error 的意外情况仍用登录页自己的兜底，
      // 它比通用的「请求失败」更贴这个场景。
      setErr(e instanceof Error ? errMsg(e) : t('login.failed'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-[60vh] items-center justify-center">
      <form
        className={`${card} w-full max-w-sm p-6`}
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <div className="mb-6 text-center">
          <MossEye className="mx-auto h-10 w-10" />
          <h1 className="mt-2 text-lg font-bold">{t('login.title')}</h1>
        </div>
        <label className={formLabel}>{t('login.username')}</label>
        <input
          type="text"
          className={input}
          placeholder={t('login.username')}
          value={user}
          onChange={(e) => setUser(e.target.value)}
          autoFocus
        />
        <label className={`${formLabel} mt-3`}>{t('login.password')}</label>
        <input
          type="password"
          className={input}
          placeholder={t('login.password.placeholder')}
          value={pwd}
          onChange={(e) => setPwd(e.target.value)}
        />
        {err && <p className="mt-2 text-xs text-rose-500">{err}</p>}
        <button type="submit" disabled={busy} className={`${btnPrimary} mt-4 w-full justify-center py-2 disabled:opacity-60`}>
          {busy ? t('login.submitting') : t('login.submit')}
        </button>
      </form>
    </div>
  )
}
