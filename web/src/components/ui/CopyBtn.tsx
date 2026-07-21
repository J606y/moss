import { useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { iconBtn } from '../../ui'

export function CopyBtn({ text }: { text: string }) {
  const [ok, setOk] = useState(false)
  return (
    <button
      className={iconBtn}
      title="复制"
      onClick={() => {
        navigator.clipboard.writeText(text).catch(() => {})
        setOk(true)
        setTimeout(() => setOk(false), 1500)
      }}
    >
      {ok ? <Check className="h-3.5 w-3.5 text-emerald-500" /> : <Copy className="h-3.5 w-3.5" />}
    </button>
  )
}
