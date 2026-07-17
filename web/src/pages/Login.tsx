import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { post } from '../api/client'
import MossEye from '../components/MossEye'
import { btnPrimary, card, formLabel, input } from '../ui'

export default function Login() {
  const navigate = useNavigate()
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
      setErr(e instanceof Error ? e.message : '登录失败，请稍后重试')
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
          <h1 className="mt-2 text-lg font-bold">Moss 管理后台</h1>
        </div>
        <label className={formLabel}>用户名</label>
        <input
          type="text"
          className={input}
          placeholder="用户名"
          value={user}
          onChange={(e) => setUser(e.target.value)}
          autoFocus
        />
        <label className={`${formLabel} mt-3`}>管理密码</label>
        <input
          type="password"
          className={input}
          placeholder="请输入密码"
          value={pwd}
          onChange={(e) => setPwd(e.target.value)}
        />
        {err && <p className="mt-2 text-xs text-rose-500">{err}</p>}
        <button type="submit" disabled={busy} className={`${btnPrimary} mt-4 w-full justify-center py-2 disabled:opacity-60`}>
          {busy ? '登录中…' : '登 录'}
        </button>
      </form>
    </div>
  )
}
