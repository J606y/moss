import { CopyBtn } from './CopyBtn'

export function CmdBlock({ label, cmd }: { label: string; cmd: string }) {
  return (
    <div>
      <div className="mb-1 flex items-center justify-between">
        <span className="text-xs font-medium text-zinc-500">{label}</span>
        <CopyBtn text={cmd} />
      </div>
      <pre className="overflow-x-auto whitespace-pre-wrap break-all rounded-xl border border-white/10 bg-zinc-950/85 p-3 text-xs leading-relaxed text-emerald-300 backdrop-blur">
        {cmd}
      </pre>
    </div>
  )
}
