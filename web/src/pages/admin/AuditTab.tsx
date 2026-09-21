import { useCallback, useEffect, useRef, useState } from 'react'
import type React from 'react'
import { ChevronLeft, ChevronRight, ScrollText, ShieldAlert } from 'lucide-react'
import { get } from '../../api/client'
import type { AdminServer, ApiKey, ExecAudit, ExecAuditDetail } from '../../types'
import { Modal, Select } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { btnGhost, card, formLabel, td, th } from '../../ui'
import { fmtDateTime, fmtTime } from '../../utils/format'
import { useT } from '../../i18n'
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
  const { t } = useT()
  const [rows, setRows] = useState<ExecAudit[]>([])
  const [detail, setDetail] = useState<ExecAuditDetail | null>(null)
  const [servers, setServers] = useState<AdminServer[]>([])
  const [serverId, setServerId] = useState('')
  const [keys, setKeys] = useState<ApiKey[]>([])
  const [keyId, setKeyId] = useState('')
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
    // 密钥列表同理，只用于填下拉。已停用的密钥也要列出来：
    // 停用只是不再放行新调用，它此前执行过的记录仍在审计里，
    // 排掉的话恰恰查不了「这个被我停掉的密钥当时都干了什么」。
    get<ApiKey[]>('/api/admin/keys')
      .then(setKeys)
      .catch(() => {
        /* 同上，取不到只是少一个筛选维度 */
      })
  }, [])

  // latest-wins：翻页按钮在 loading 时会禁用，但机器筛选与「仅看拦截」不会——
  // A→B 快速切换时若 A 的响应后到，就会渲染成「筛选显示 B、表格是 A」。
  // 每次请求领一个序号，回来时不是最新的就整条丢弃（含 loading 与错误提示）。
  const seqRef = useRef(0)

  const load = useCallback(() => {
    const seq = ++seqRef.current
    setLoading(true)
    const p = new URLSearchParams({ limit: String(PAGE), offset: String((page - 1) * PAGE) })
    if (serverId) p.set('server', serverId)
    if (keyId) p.set('key', keyId)
    if (onlyBlocked) p.set('blocked', '1')
    get<{ items: ExecAudit[]; total: number }>(`/api/admin/exec-audit?${p}`)
      .then((res) => {
        if (seq !== seqRef.current) return
        setRows(res.items)
        setTotal(res.total)
      })
      .catch((e) => {
        if (seq !== seqRef.current) return
        toast(errMsg(e))
      })
      .finally(() => {
        // 已有更新的请求在途时不复位 loading，交给它自己收尾
        if (seq === seqRef.current) setLoading(false)
      })
  }, [page, serverId, keyId, onlyBlocked, toast])

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
          {t('admin.tab.audit')}
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
            {t('audit.onlyBlocked')}
          </button>
          <div className="w-40">
            <Select
              value={serverId}
              onChange={(v) => changeFilter(() => setServerId(v))}
              options={[
                { value: '', label: t('audit.allServers') },
                ...servers.map((s) => ({ value: s.id, label: s.name })),
              ]}
            />
          </div>
          <div className="w-40">
            <Select
              value={keyId}
              onChange={(v) => changeFilter(() => setKeyId(v))}
              options={[
                { value: '', label: t('audit.allKeys') },
                ...keys.map((k) => ({ value: String(k.id), label: k.name })),
              ]}
            />
          </div>
          <button className={btnGhost} onClick={load}>
            {t('common.refresh')}
          </button>
        </div>
      </div>

      {rows.length === 0 ? (
        <p className="py-8 text-center text-sm text-zinc-400">
          {loading ? t('common.loading') : t(onlyBlocked ? 'audit.empty.blocked' : 'audit.empty')}
        </p>
      ) : (
        <>
          <div className="overflow-x-auto">
            <table className="w-full">
              <thead>
                <tr className="border-b border-white/40 dark:border-white/10">
                  <th className={th}>{t('audit.col.time')}</th>
                  <th className={th}>{t('audit.col.server')}</th>
                  <th className={`${th} hidden md:table-cell`}>{t('audit.col.caller')}</th>
                  <th className={th}>{t('audit.col.cmd')}</th>
                  <th className={th}>{t('audit.col.result')}</th>
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
            <span>{t('audit.pager', { total: total.toLocaleString(), page, pages })}</span>
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
  const { t } = useT()
  if (row.finishedAt === 0) {
    return <span className="rounded bg-sky-500/10 px-1.5 py-0.5 text-xs text-sky-600">{t('audit.status.running')}</span>
  }
  if (row.error) {
    // 被拦截的命令是审计里最值得注意的记录，单独标红并原样展示原因。
    //
    // 判据仍是中文子串，且**不该**跟着 HTTP 错误码一起改：这里的 row.error
    // 是当初落库的历史文本，不是错误响应——exec_audit 表没有 code 列，后端
    // 自己的「仅看拦截」筛选也是 error LIKE '命令被拦截：%'。要改得先加列并
    // 回填，属于 docs/error-codes.md 的 D 段，届时这一处与后端同步改。
    const blocked = row.error.includes('拦截')
    return (
      <span
        className={`rounded px-1.5 py-0.5 text-xs ${
          blocked ? 'bg-rose-500/15 text-rose-600' : 'bg-amber-500/10 text-amber-600'
        }`}
        title={row.error}
      >
        {t(blocked ? 'audit.status.blocked' : 'audit.status.failed')}
      </span>
    )
  }
  if (row.exitCode !== 0) {
    return (
      <span className="rounded bg-amber-500/10 px-1.5 py-0.5 text-xs text-amber-600">
        {t('audit.status.exit', { code: row.exitCode })}
      </span>
    )
  }
  return (
    <span className="rounded bg-emerald-500/10 px-1.5 py-0.5 text-xs text-emerald-600">{t('audit.status.ok')}</span>
  )
}

function AuditDetailModal({ d, onClose }: { d: ExecAuditDetail; onClose: () => void }) {
  const { t } = useT()
  const r = d.record
  return (
    <Modal title={t('audit.detail')} onClose={onClose}>
      <div className="space-y-3 text-sm">
        <Field label={t('audit.col.server')} value={r.serverName || r.serverId} />
        <Field label={t('audit.col.caller')} value={r.caller} />
        <Field label={t('audit.col.time')} value={fmtDateTime(r.startedAt)} />
        {r.dir && <Field label={t('audit.field.dir')} value={r.dir} />}
        <div>
          <label className={formLabel}>{t('audit.col.cmd')}</label>
          <pre className="glass-sheen overflow-x-auto rounded-xl border border-white/50 bg-white/45 p-2.5 font-mono text-xs dark:border-white/10 dark:bg-zinc-900/40">
            {r.cmd}
          </pre>
        </div>
        {r.error && (
          <div className="rounded-xl border border-rose-400/30 bg-rose-500/10 p-2.5 text-sm text-rose-600 dark:text-rose-400">
            {r.error}
          </div>
        )}
        {d.stdout && <OutputBlock label={t('audit.stdout')} text={d.stdout} />}
        {d.stderr && <OutputBlock label={t('audit.stderr')} text={d.stderr} />}
        {r.truncated && <p className="text-xs text-zinc-400">{t('audit.truncated')}</p>}
        <div className="flex justify-end">
          <button className={btnGhost} onClick={onClose}>
            {t('common.close')}
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
