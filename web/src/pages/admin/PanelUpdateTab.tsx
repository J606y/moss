import { useCallback, useEffect, useRef, useState } from 'react'
import type React from 'react'
import { ArrowDownToLine, CheckCircle2, ChevronDown, Loader2, RefreshCw, TriangleAlert } from 'lucide-react'
import { get, post, put } from '../../api/client'
import type { AdminServer, PanelUpdate } from '../../types'
import { Select } from '../../components/ui'
import { errMsg } from '../../utils/admin'
import { btnGhost, btnPrimary, card } from '../../ui'
import type { Toast } from './types'

/** 更新期间面板会重启，轮询间隔。 */
const POLL_MS = 3000

/** 设置项控件的统一宽度。三行同宽才能右边缘对齐，读起来像一组而不是三个零件。 */
const ctlWidth = 'w-36'

/**
 * 面板更新。
 *
 * 版本判定全在后端：前端只看 avail.action 决定显示什么，不自己比较版本号——
 * 「2.0.0-beta.3 和 2.0.0 谁更新」这种规则散落两处，改一处就会漏。
 */
export function PanelUpdateTab({ toast }: { toast: Toast }) {
  const [d, setD] = useState<PanelUpdate | null>(null)
  const [servers, setServers] = useState<AdminServer[]>([])
  const [checking, setChecking] = useState(false)
  const [starting, setStarting] = useState(false)
  const [showNotes, setShowNotes] = useState(false)
  // 更新期间面板会重启，请求必然连不上。记下目标版本，靠版本号变化判定成功。
  const updatingTo = useRef<string | null>(null)
  const [updating, setUpdating] = useState(false)

  const load = useCallback(
    async (check = false) => {
      const data = await get<PanelUpdate>(`/api/admin/panel-update${check ? '?check=1' : ''}`)
      setD(data)
      return data
    },
    [],
  )

  useEffect(() => {
    load().catch((e) => toast(errMsg(e)))
    get<AdminServer[]>('/api/admin/servers')
      .then(setServers)
      .catch(() => {
        /* 机器列表只用于填「面板所在服务器」下拉，取不到不影响看版本 */
      })
  }, [load, toast])

  // 更新中轮询：面板重启期间请求会失败，这是预期的，不能报错也不能停。
  useEffect(() => {
    if (!updating) return
    const t = setInterval(async () => {
      try {
        const data = await load()
        const target = updatingTo.current?.replace(/^v/, '')
        if (target && data.current === target) {
          setUpdating(false)
          updatingTo.current = null
          toast(`已更新到 v${target}`)
        } else if (data.stage === 'failed') {
          setUpdating(false)
          updatingTo.current = null
          toast(`更新失败：${data.stageErr || '原因未知'}`)
        }
      } catch {
        /* 面板正在重启，连不上是正常的，继续等 */
      }
    }, POLL_MS)
    return () => clearInterval(t)
  }, [updating, load, toast])

  const save = async (patch: Partial<PanelUpdate['config']>) => {
    if (!d) return
    try {
      setD(await put<PanelUpdate>('/api/admin/panel-update', { ...d.config, ...patch }))
    } catch (e) {
      toast(errMsg(e))
    }
  }

  const check = async () => {
    setChecking(true)
    try {
      await load(true)
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setChecking(false)
    }
  }

  const start = async () => {
    if (!d?.latest) return
    setStarting(true)
    try {
      const res = await post<{ message: string; target: string }>('/api/admin/panel-update/start')
      updatingTo.current = res.target
      setUpdating(true)
      toast(res.message)
    } catch (e) {
      toast(errMsg(e))
    } finally {
      setStarting(false)
    }
  }

  if (!d) {
    return (
      <div className={`${card} flex items-center justify-center gap-2 p-10 text-sm text-zinc-400`}>
        <Loader2 className="h-4 w-4 animate-spin" />
        加载中…
      </div>
    )
  }

  const canUpdate = d.avail.action === 'update' && d.hostReady && !updating

  return (
    <div className="mx-auto max-w-xl space-y-4">
      {/* ── 设置：照 iOS 设置页的分组列表，一张卡片装三行 ──
          原先三项各占一张大卡片，纵向拉得很长，而它们本就是同一类东西。 */}
      {/* 不能加 overflow-hidden：会把 Select 展开的下拉面板一起裁掉。
          分隔线本来也碰不到圆角——首尾两行没有分隔线，中间的线离圆角还远。 */}
      <section className={card}>
        {/* 三行的控件统一用下拉、且同宽：右边缘对齐，读起来才是一组设置，
            而不是三个各自为政的控件。 */}
        <Row
          title="自动更新"
          desc="发现新版本时自动安装。启动失败会自动回滚，但更新期间面板会短暂中断。"
          control={
            <div className={ctlWidth}>
              <Select
                value={d.config.auto ? 'on' : 'off'}
                onChange={(v) => save({ auto: v === 'on' })}
                options={[
                  { value: 'off', label: '关闭' },
                  { value: 'on', label: '开启' },
                ]}
              />
            </div>
          }
        />
        <Row
          title="更新通道"
          desc={
            d.config.channel === 'beta'
              ? '第一时间收到新功能，可能存在未发现的问题'
              : '只接收正式发布的版本，跳过测试版'
          }
          control={
            <div className={ctlWidth}>
              <Select
                value={d.config.channel}
                onChange={(v) => save({ channel: v as 'stable' | 'beta' })}
                options={[
                  { value: 'stable', label: '正式版' },
                  { value: 'beta', label: '测试版' },
                ]}
              />
            </div>
          }
        />
        {/* 不做自动探测：NAT、多网卡、反代都会让「出口 IP 对得上」这种猜法出错，
            而猜错的后果是把更新打到别的机器上。 */}
        <Row
          title="面板所在服务器"
          desc="更新由这台机器上的 agent 执行，需要它已开启远程执行能力"
          last
          control={
            <div className={ctlWidth}>
              <Select
                value={d.config.hostServer}
                onChange={(v) => save({ hostServer: v })}
                options={[
                  { value: '', label: '未指定' },
                  ...servers.map((s) => ({ value: s.id, label: s.name })),
                ]}
              />
            </div>
          }
        />
      </section>

      <p className="px-1 text-xs text-zinc-400">
        切换通道后只会提示版本号更高的更新。当前跑测试版时，要等正式版追上这个版本号才会出现更新提示；
        需要回退请到服务器上操作。
      </p>

      {/* ── 主体：版本状态 ── */}
      <section className={`${card} p-5`}>
        <div className="flex items-start gap-4">
          <StatusIcon updating={updating} action={d.avail.action} hasError={!!d.checkError} />
          <div className="min-w-0 flex-1">
            <Headline d={d} updating={updating} />

            {d.latest && d.avail.action === 'update' && (
              <p className="mt-1 text-xs text-zinc-500">
                {d.latest.name || d.latest.version}
                {d.latest.published && ` · ${d.latest.published.slice(0, 10)}`}
                {d.latest.prerelease && ' · 测试版'}
              </p>
            )}

            {/* 更新说明：CHANGELOG 原文，默认收起——macOS 也是点开才看 */}
            {d.latest?.notes && d.avail.action === 'update' && (
              <div className="mt-3">
                <button
                  className="inline-flex items-center gap-1 text-xs text-emerald-600 hover:underline dark:text-emerald-400"
                  onClick={() => setShowNotes((v) => !v)}
                >
                  <ChevronDown className={`h-3.5 w-3.5 transition ${showNotes ? 'rotate-180' : ''}`} />
                  {showNotes ? '收起更新说明' : '查看更新说明'}
                </button>
                {showNotes && (
                  <div className="glass-sheen mt-2 max-h-64 space-y-2 overflow-auto rounded-xl border border-white/50 bg-white/45 p-3 text-xs leading-relaxed dark:border-white/10 dark:bg-zinc-900/40">
                    <Notes md={d.latest.notes} />
                  </div>
                )}
              </div>
            )}

            {d.checkError && (
              <p className="mt-2 text-xs text-amber-600 dark:text-amber-400">{d.checkError}</p>
            )}
            {d.avail.action === 'downgrade' && (
              <p className="mt-2 text-xs text-zinc-500">
                {d.avail.reason}。面板只做升级，需要回退请到服务器上操作。
              </p>
            )}
            {!d.hostReady && d.avail.action === 'update' && (
              <p className="mt-2 flex items-start gap-1.5 text-xs text-amber-600 dark:text-amber-400">
                <TriangleAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" />
                {d.hostHint}
              </p>
            )}
            {d.stage === 'failed' && !updating && (
              <p className="mt-2 text-xs text-rose-500">上次更新失败：{d.stageErr}</p>
            )}
          </div>
        </div>

        <div className="mt-4 flex items-center justify-between gap-3">
          <button className={btnGhost} onClick={check} disabled={checking || updating}>
            <RefreshCw className={`h-4 w-4 ${checking ? 'animate-spin' : ''}`} />
            {checking ? '检查中…' : '检查更新'}
          </button>
          {canUpdate && (
            <button className={btnPrimary} onClick={start} disabled={starting}>
              <ArrowDownToLine className="h-4 w-4" />
              {starting ? '正在下发…' : '立即更新'}
            </button>
          )}
        </div>
      </section>
    </div>
  )
}

function StatusIcon({
  updating,
  action,
  hasError,
}: {
  updating: boolean
  action: PanelUpdate['avail']['action']
  hasError: boolean
}) {
  const base = 'flex h-12 w-12 shrink-0 items-center justify-center rounded-2xl'
  if (updating) {
    return (
      <div className={`${base} bg-sky-500/15 text-sky-600 dark:text-sky-400`}>
        <Loader2 className="h-6 w-6 animate-spin" />
      </div>
    )
  }
  if (hasError) {
    return (
      <div className={`${base} bg-amber-500/15 text-amber-600 dark:text-amber-400`}>
        <TriangleAlert className="h-6 w-6" />
      </div>
    )
  }
  if (action === 'update') {
    return (
      <div className={`${base} bg-emerald-500/15 text-emerald-600 dark:text-emerald-400`}>
        <ArrowDownToLine className="h-6 w-6" />
      </div>
    )
  }
  return (
    <div className={`${base} bg-emerald-500/15 text-emerald-600 dark:text-emerald-400`}>
      <CheckCircle2 className="h-6 w-6" />
    </div>
  )
}

/** 状态标题。措辞照 iOS：可更新时给版本号，最新时明确说"已是最新版本"。 */
function Headline({ d, updating }: { d: PanelUpdate; updating: boolean }) {
  if (updating) {
    return (
      <>
        <h2 className="font-semibold">正在更新…</h2>
        <p className="mt-0.5 text-sm text-zinc-500">面板即将重启，期间会短暂无法访问。</p>
      </>
    )
  }
  if (d.avail.action === 'update' && d.latest) {
    return (
      <>
        <h2 className="font-semibold">可更新至 {d.latest.version}</h2>
        <p className="mt-0.5 text-sm text-zinc-500">当前 v{d.current}</p>
      </>
    )
  }
  return (
    <>
      <h2 className="font-semibold">Moss v{d.current}</h2>
      <p className="mt-0.5 text-sm text-zinc-500">
        {d.checkError ? '无法检查更新' : d.avail.action === 'unknown' ? d.avail.reason : '已是最新版本'}
      </p>
    </>
  )
}

/**
 * 更新说明的极简 Markdown 渲染。
 *
 * 不引入 Markdown 引擎：为一段更新说明拖进 remark 那套生态（上百 KB）不划算，
 * 而这里的输入是本项目自己写的 CHANGELOG，用到的语法就这么几种。
 * 认不出的行原样输出，不会因为将来多写了一种语法就报错或吞内容。
 */
function Notes({ md }: { md: string }) {
  const lines = md.trim().split('\n')
  const out: React.ReactNode[] = []

  lines.forEach((raw, i) => {
    const line = raw.trimEnd()
    if (!line.trim()) return // 空行只作分隔，由外层 space-y 体现

    // 标题：CHANGELOG 里是 ## 版本号 / ### 分类
    const h = /^(#{1,6})\s+(.*)$/.exec(line)
    if (h) {
      out.push(
        <p key={i} className="pt-1 text-sm font-semibold">
          {inlineMd(h[2])}
        </p>,
      )
      return
    }
    // 引用：CHANGELOG 用它放"测试版"这类提示
    if (line.startsWith('> ')) {
      out.push(
        <p
          key={i}
          className="border-l-2 border-amber-400/60 pl-2 text-amber-700 dark:text-amber-400/90"
        >
          {inlineMd(line.slice(2))}
        </p>,
      )
      return
    }
    // 列表：保留缩进层级，二级列表在 CHANGELOG 里用来展开细项
    const li = /^(\s*)[-*]\s+(.*)$/.exec(line)
    if (li) {
      out.push(
        <p key={i} className="flex gap-1.5" style={{ paddingLeft: `${li[1].length * 0.5}rem` }}>
          <span className="select-none text-zinc-400">·</span>
          <span className="min-w-0">{inlineMd(li[2])}</span>
        </p>,
      )
      return
    }
    out.push(
      <p key={i} className={/^\s{2,}/.test(raw) ? 'pl-3' : ''}>
        {inlineMd(line.trim())}
      </p>,
    )
  })

  return <>{out}</>
}

/** 行内语法：**粗体** 与 `代码`。两者都用同一次扫描切分，避免嵌套时互相吃掉。 */
function inlineMd(text: string): React.ReactNode[] {
  const parts: React.ReactNode[] = []
  const re = /\*\*(.+?)\*\*|`([^`]+)`/g
  let last = 0
  let m: RegExpExecArray | null
  let k = 0
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) parts.push(text.slice(last, m.index))
    if (m[1] !== undefined) {
      parts.push(
        <strong key={k++} className="font-semibold">
          {m[1]}
        </strong>,
      )
    } else {
      parts.push(
        <code key={k++} className="rounded bg-zinc-500/10 px-1 py-0.5 font-mono dark:bg-white/10">
          {m[2]}
        </code>,
      )
    }
    last = m.index + m[0].length
  }
  if (last < text.length) parts.push(text.slice(last))
  return parts
}

/**
 * iOS 设置页那种一行一项：左边标题与说明，右边控件，行间一条不通到头的分隔线。
 *
 * 分隔线左侧留出与标题对齐的缩进（ml-4），是 iOS 分组列表的标志性细节——
 * 通到底的线会把一张卡片切成几块，留缩进才读得出"这几行是一组"。
 */
function Row({
  title,
  desc,
  control,
  last,
}: {
  title: string
  desc?: string
  control: React.ReactNode
  last?: boolean
}) {
  return (
    <div className={last ? '' : 'border-b border-white/40 dark:border-white/10'}>
      <div className="flex items-center justify-between gap-4 p-4">
        <div className="min-w-0">
          <p className="text-sm font-medium">{title}</p>
          {desc && <p className="mt-0.5 text-xs leading-relaxed text-zinc-400">{desc}</p>}
        </div>
        <div className="shrink-0">{control}</div>
      </div>
    </div>
  )
}
