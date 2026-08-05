import { useCallback, useEffect, useState } from 'react'
import type React from 'react'
import { ChevronLeft, ChevronRight, ScrollText, ShieldAlert } from 'lucide-react'
import { get } from '../../api/client'
import type { AdminServer, ExecAudit, ExecAuditDetail } from '../../types'
import { Modal, Select } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { btnGhost, card, formLabel, td, th } from '../../ui'
import { fmtDateTime, fmtTime } from '../../utils/format'
import type { Toast } from './types'

/**
 * 每页条数。与审计条数上限 5000 对应，最多 50 页。
 *
 * 用分页而不是「加载更多」：后者会把记录不断堆进同一页，5000 条时既要滑很久，
 * 又因为 DOM 节点太多而卡顿。分页任何时候都只渲染 100 行。
 */
const PAGE = 100

/**
 * 生成页码序列，中间用 … 省略。
 *
 * 50 页全列出来会占满一整行且没人会去点第 27 页；只保留首尾与当前页附近，
 * 既能一步跳到最早的记录，也能微调前后一页。
 */
function pageNumbers(current: number, total: number): Array<number | '…'> {
  if (total <= 7) return Array.from({ length: total }, (_, i) => i + 1)
  if (current <= 4) return [1, 2, 3, 4, 5, '…', total]
  if (current >= total - 3) return [1, '…', total - 4, total - 3, total - 2, total - 1, total]
  return [1, '…', current - 1, current, current + 1, '…', total]
}

/**
 * 执行审计。
 *
 * 从「AI 接入」页拆出来单独成页：那一页里接入方式与密钥都是有限内容，
 * 只有审计会随使用无限增长，挤在第三块里既看不清也翻不到。
 */
export function AuditTab({ toast }: { toast: Toast }) {
  const [rows, setRows] = useState<ExecAudit[]>([])
  const [detail, setDetail] = useState<ExecAuditDetail | null>(null)
  const [servers, setServers] = useState<AdminServer[]>([])
  const [serverId, setServerId] = useState('')
  const [onlyBlocked, setOnlyBlocked] = useState(false)
  const [loading, setLoading] = useState(false)
  const [page, setPage] = useState(1)
  const [total, setTotal] = useState(0)

  useEffect(() => {
    get<AdminServer[]>('/api/admin/servers')
      .then(setServers)
      .catch(() => {
        /* 机器列表只用于填筛选下拉，取不到不影响看审计本身 */
      })
  }, [])

  const load = useCallback(() => {
    setLoading(true)
    const p = new URLSearchParams({ limit: String(PAGE), offset: String((page - 1) * PAGE) })
    if (serverId) p.set('server', serverId)
    if (onlyBlocked) p.set('blocked', '1')
    get<{ items: ExecAudit[]; total: number }>(`/api/admin/exec-audit?${p}`)
      .then((res) => {
        setRows(res.items)
        setTotal(res.total)
      })
      .catch((e) => toast(errMsg(e)))
      .finally(() => setLoading(false))
  }, [page, serverId, onlyBlocked, toast])

  useEffect(load, [load])

  // 换筛选条件时回到第一页：停在第 7 页而新条件只有 2 页，会看到一片空白。
  const changeFilter = (fn: () => void) => {
    fn()
    setPage(1)
  }

  const pages = Math.max(1, Math.ceil(total / PAGE))

  const open = async (jobId: string) => {
    try {
      setDetail(await get<ExecAuditDetail>(`/api/admin/exec-audit/${jobId}`))
    } catch (e) {
      toast(errMsg(e))
    }
  }

  return (
    <section className={`${card} p-4 sm:p-5`}>
      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <h2 className="flex items-center gap-2 font-semibold">
          <ScrollText className="h-4 w-4 text-emerald-500" />
          执行审计
        </h2>
        <div className="flex flex-wrap items-center gap-2">
          {/* 「仅拦截」单独给一个按钮而不是塞进下拉：被拦下的尝试是这里最该被
              一眼捞出来的记录，混在日常流水里很快就被刷到翻不到的地方。 */}
          <button
            className={`press inline-flex items-center gap-1.5 rounded-lg px-2.5 py-1.5 text-xs transition ${
              onlyBlocked
                ? 'bg-rose-500/15 text-rose-600 dark:text-rose-400'
                : 'text-zinc-500 hover:bg-white/50 active:bg-white/75 dark:hover:bg-white/5 dark:active:bg-white/12'
            }`}
            onClick={() => changeFilter(() => setOnlyBlocked((v) => !v))}
          >
            <ShieldAlert className="h-3.5 w-3.5" />
            仅看拦截
          </button>
          <div className="w-40">
            <Select
              value={serverId}
              onChange={(v) => changeFilter(() => setServerId(v))}
              options={[{ value: '', label: '全部机器' }, ...servers.map((s) => ({ value: s.id, label: s.name }))]}
            />
          </div>
          <button className={btnGhost} onClick={load}>
            刷新
          </button>
        </div>
      </div>

      {rows.length === 0 ? (
        <p className="py-8 text-center text-sm text-zinc-400">
          {loading
            ? '加载中…'
            : onlyBlocked
              ? '没有被拦截的记录。'
              : '暂无记录。AI 执行的每一条命令都会出现在这里。'}
        </p>
      ) : (
        <>
          <div className="overflow-x-auto">
            <table className="w-full">
              <thead>
                <tr className="border-b border-white/40 dark:border-white/10">
                  <th className={th}>时间</th>
                  <th className={th}>机器</th>
                  <th className={`${th} hidden md:table-cell`}>调用方</th>
                  <th className={th}>命令</th>
                  <th className={th}>结果</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr
                    key={r.jobId}
                    className="cursor-pointer border-b border-white/25 transition hover:bg-white/40 dark:border-white/5 dark:hover:bg-white/5"
                    onClick={() => open(r.jobId)}
                  >
                    {/* 窄屏只给时分秒，完整日期在这里不值得占掉「结果」列的位置 */}
                    <td className={td}>
                      <span className="md:hidden">{fmtTime(r.startedAt)}</span>
                      <span className="hidden md:inline">{fmtDateTime(r.startedAt)}</span>
                    </td>
                    <td className={td}>{r.serverName || r.serverId}</td>
                    <td className={`${td} hidden max-w-[10rem] truncate md:table-cell`} title={r.caller}>
                      {r.caller}
                    </td>
                    <td className={`${td} max-w-[7rem] truncate font-mono text-xs sm:max-w-[20rem]`} title={r.cmd}>
                      {r.cmd}
                    </td>
                    <td className={td}>
                      <AuditStatus row={r} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-xs text-zinc-400">
            <span>
              共 {total.toLocaleString()} 条 · 第 {page} / {pages} 页
            </span>
            {pages > 1 && (
              <div className="flex items-center gap-1">
                <PageBtn disabled={page === 1 || loading} onClick={() => setPage(page - 1)}>
                  <ChevronLeft className="h-3.5 w-3.5" />
                </PageBtn>
                {pageNumbers(page, pages).map((n, i) =>
                  n === '…' ? (
                    <span key={`gap-${i}`} className="px-1">
                      …
                    </span>
                  ) : (
                    <PageBtn key={n} active={n === page} disabled={loading} onClick={() => setPage(n)}>
                      {n}
                    </PageBtn>
                  ),
                )}
                <PageBtn disabled={page === pages || loading} onClick={() => setPage(page + 1)}>
                  <ChevronRight className="h-3.5 w-3.5" />
                </PageBtn>
              </div>
            )}
          </div>
        </>
      )}

      {detail && <AuditDetailModal d={detail} onClose={() => setDetail(null)} />}
    </section>
  )
}

function PageBtn({
  children,
  active,
  disabled,
  onClick,
}: {
  children: React.ReactNode
  active?: boolean
  disabled?: boolean
  onClick: () => void
}) {
  return (
    <button
      disabled={disabled}
      onClick={onClick}
      className={`press-sm min-w-[1.75rem] rounded-lg px-2 py-1 transition disabled:opacity-40 ${
        active
          ? 'bg-emerald-500/15 font-medium text-emerald-600 dark:text-emerald-400'
          : 'hover:bg-white/50 active:bg-white/75 dark:hover:bg-white/5 dark:active:bg-white/12'
      }`}
    >
      {children}
    </button>
  )
}

function AuditStatus({ row }: { row: ExecAudit }) {
  if (row.finishedAt === 0) {
    return <span className="rounded bg-sky-500/10 px-1.5 py-0.5 text-xs text-sky-600">执行中</span>
  }
  if (row.error) {
    // 被拦截的命令是审计里最值得注意的记录，单独标红并原样展示原因。
    const blocked = row.error.includes('拦截')
    return (
      <span
        className={`rounded px-1.5 py-0.5 text-xs ${
          blocked ? 'bg-rose-500/15 text-rose-600' : 'bg-amber-500/10 text-amber-600'
        }`}
        title={row.error}
      >
        {blocked ? '已拦截' : '失败'}
      </span>
    )
  }
  if (row.exitCode !== 0) {
    return <span className="rounded bg-amber-500/10 px-1.5 py-0.5 text-xs text-amber-600">退出码 {row.exitCode}</span>
  }
  return <span className="rounded bg-emerald-500/10 px-1.5 py-0.5 text-xs text-emerald-600">成功</span>
}

function AuditDetailModal({ d, onClose }: { d: ExecAuditDetail; onClose: () => void }) {
  const r = d.record
  return (
    <Modal title="执行详情" onClose={onClose}>
      <div className="space-y-3 text-sm">
        <Field label="机器" value={r.serverName || r.serverId} />
        <Field label="调用方" value={r.caller} />
        <Field label="时间" value={fmtDateTime(r.startedAt)} />
        {r.dir && <Field label="工作目录" value={r.dir} />}
        <div>
          <label className={formLabel}>命令</label>
          <pre className="glass-sheen overflow-x-auto rounded-xl border border-white/50 bg-white/45 p-2.5 font-mono text-xs dark:border-white/10 dark:bg-zinc-900/40">
            {r.cmd}
          </pre>
        </div>
        {r.error && (
          <div className="rounded-xl border border-rose-400/30 bg-rose-500/10 p-2.5 text-sm text-rose-600 dark:text-rose-400">
            {r.error}
          </div>
        )}
        {d.stdout && <OutputBlock label="标准输出" text={d.stdout} />}
        {d.stderr && <OutputBlock label="标准错误" text={d.stderr} />}
        {r.truncated && <p className="text-xs text-zinc-400">输出较长，审计中只保留了开头部分。</p>}
        <div className="flex justify-end">
          <button className={btnGhost} onClick={onClose}>
            关闭
          </button>
        </div>
      </div>
    </Modal>
  )
}

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex justify-between gap-3">
      <span className="text-zinc-500 dark:text-zinc-400">{label}</span>
      <span className="text-right">{value}</span>
    </div>
  )
}

function OutputBlock({ label, text }: { label: string; text: string }) {
  return (
    <div>
      <label className={formLabel}>{label}</label>
      <pre className="glass-sheen max-h-48 overflow-auto rounded-xl border border-white/50 bg-white/45 p-2.5 font-mono text-xs leading-relaxed dark:border-white/10 dark:bg-zinc-900/40">
        {text}
      </pre>
    </div>
  )
}
