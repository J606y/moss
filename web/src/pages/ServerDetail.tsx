import { useEffect, useMemo, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Activity, ArrowDown, ArrowUp, ChevronLeft, Radar } from 'lucide-react'
import {
  Area,
  AreaChart,
  Brush,
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { ensureBuf, getLiveBuf, pct, serversReady, useLiveStats, useServers } from '../api/store'
import { get } from '../api/client'
import type { HistoryPoint, PingData } from '../types'
import { StatusPill } from '../components/ui'
import { ProgressBar, barColor, clampPct } from '../components/ProgressBar'
import Flag from '../components/Flag'
import Ticker from '../components/Ticker'
import { axisProps, ChartCard, ChartTip, gridStroke, palette, SeriesChips } from '../components/Charts'
import { fmtAxisTime, fmtBytes, fmtDateTime, fmtPercent, fmtSpeed, fmtTime, fmtUptime } from '../utils/format'
import { brushFill, brushStroke, pingPalette, timelineStroke } from '../tokens'
import { card } from '../ui'
import { useT, type TextKey } from '../i18n'

// 标签只存 key，渲染时才查表：这两张表是模块级常量，在 t() 拿得到之前就已求值。
const ranges: Array<{ labelKey: TextKey; h: number }> = [
  { labelKey: 'detail.range.live', h: 0 },
  { labelKey: 'detail.range.1h', h: 1 },
  { labelKey: 'detail.range.6h', h: 6 },
  { labelKey: 'detail.range.24h', h: 24 },
  { labelKey: 'detail.range.7d', h: 168 },
]

const tabs = [
  { key: 'load' as const, labelKey: 'detail.tab.load' as TextKey, icon: Activity },
  { key: 'ping' as const, labelKey: 'detail.tab.ping' as TextKey, icon: Radar },
]

function Info({ k, v }: { k: string; v: string }) {
  return (
    <div>
      <dt className="text-xs text-zinc-500">{k}</dt>
      <dd className="mt-0.5 break-all text-sm font-medium">{v}</dd>
    </div>
  )
}

/** 实时小卡：带使用率进度条 */
function GaugeCard({
  title,
  pct,
  detail,
}: {
  title: string
  pct: number
  detail: string
}) {
  const p = clampPct(pct)
  return (
    <div className={`${card} p-4`}>
      {/* 具体用量在窄屏同样要显示：只给一个百分比，等于让人在手机上没法判断
          「还剩多少空间」——而这正是点进详情页要看的东西。窄屏时卡片是单列全宽，
          横向空间反而比桌面的四列网格更充裕，放得下。 */}
      <div className="flex items-baseline justify-between gap-2">
        <span className="text-xs text-zinc-500">{title}</span>
        <span className="truncate text-xs tabular-nums text-zinc-400">{detail}</span>
      </div>
      <div className="mt-1 text-lg font-semibold tabular-nums">{fmtPercent(p)}</div>
      <div className="mt-3 h-1.5 overflow-hidden rounded-full bg-zinc-500/15 dark:bg-white/10">
        <div
          className="h-full rounded-full transition-all duration-700"
          style={{ width: `${p}%`, background: barColor(p) }}
        />
      </div>
    </div>
  )
}

export default function ServerDetail() {
  const { t } = useT()
  const { id } = useParams<{ id: string }>()
  const serverList = useServers()
  const st = useLiveStats(id) // 仅当前服务器 tick 驱动实时数字/图（hooks 须在 early return 前调用）
  const [hours, setHours] = useState(0) // 默认「实时」
  const [tab, setTab] = useState<'load' | 'ping'>('load')
  // 时间轴刷选窗口 [起, 止] 时间戳，null 为完整范围
  const [timeWin, setTimeWin] = useState<[number, number] | null>(null)
  const server = serverList.find((s) => s.id === id)
  // 实时模式：图表吃滚动缓冲，不走历史数据
  const isLive = hours === 0

  const [history, setHistory] = useState<HistoryPoint[]>([])
  const [pingData, setPingData] = useState<PingData>({ tasks: [], series: {} })

  useEffect(() => {
    if (!id || hours === 0) {
      setHistory([])
      return
    }
    let dead = false
    get<HistoryPoint[]>(`/api/servers/${id}/history?hours=${hours}`)
      .then((d) => {
        if (!dead) setHistory(d)
      })
      .catch(() => {})
    return () => {
      dead = true
    }
  }, [id, hours])

  // 延迟探测是周期上报、没有 WS 实时流；服务端最小粒度为 1 小时（parseHours 下限），
  // 实时模式下前端再把每条曲线裁到最近 5 分钟（见下方 liveTrim）。
  // 「实时」按站点设置的「实时上报间隔」轮询刷新，与负载页实时节奏一致。
  // 只在「延迟监控」页签下拉取：停在负载页时这份数据没人看，实时模式却仍会
  // 按上报间隔一直打服务端。首次切到该页签会立刻 load 一次，不必等一个轮询周期。
  const pollSec = server?.intervalSec ?? 2
  useEffect(() => {
    if (!id || tab !== 'ping') return
    let dead = false
    const load = () =>
      get<PingData>(`/api/servers/${id}/ping?hours=${Math.max(hours, 1)}`)
        .then((d) => {
          if (!dead) setPingData(d)
        })
        .catch(() => {})
    load()
    const timer = hours === 0 ? setInterval(load, Math.max(pollSec, 1) * 1000) : undefined
    return () => {
      dead = true
      if (timer) clearInterval(timer)
    }
  }, [id, hours, pollSec, tab])

  // 回填服务端滚动缓冲，让实时图立即有数据
  useEffect(() => {
    if (id) ensureBuf(id)
  }, [id])

  // 以下派生在实时模式每 ~2s 轮询 + tick 下高频重渲染，统一用 useMemo 缓存，
  // 仅在真正的输入变化时重算（计算逻辑与缓存前完全一致）。
  // hooks 须在 early return 之前无条件调用，故这里对 server 缺省做内部兜底。

  // 按刷选窗口过滤数据
  const winFilter = <T extends { time: number }>(arr: T[]): T[] =>
    timeWin ? arr.filter((d) => d.time >= timeWin[0] && d.time <= timeWin[1]) : arr

  // 实时模式下浅拷贝缓冲数组（st 每 tick 换新引用，作为「缓冲已更新」信号驱动重算）；
  // 历史模式按刷选窗口裁剪 history。
  const histView = useMemo(
    () => (server ? (isLive ? [...getLiveBuf(server.id)] : winFilter(history)) : []),
    // st 作为实时缓冲更新信号；winFilter 仅依赖 timeWin，已在依赖中体现
    [server, isLive, history, timeWin, st], // eslint-disable-line react-hooks/exhaustive-deps
  )

  // 实时模式：延迟图只展示最近 5 分钟。锚定到最新数据点的时间（而非浏览器 Date.now），
  // 避免浏览器与服务端时钟偏差导致窗口整体错位、把点全滤掉。
  const LIVE_PING_WIN_MS = 5 * 60_000
  const pingMaxTime = useMemo(() => {
    let m = 0
    if (isLive)
      for (const k in pingData.series) for (const p of pingData.series[k]) if (p.time > m) m = p.time
    return m
  }, [isLive, pingData])

  const pingStats = useMemo(() => {
    const liveCut = pingMaxTime - LIVE_PING_WIN_MS
    const liveTrim = <T extends { time: number }>(arr: T[]): T[] =>
      isLive && pingMaxTime ? arr.filter((d) => d.time >= liveCut) : arr
    return pingData.tasks.map((t, i) => {
      const pts = liveTrim(winFilter(pingData.series[String(t.id)] ?? []))
      const vals = pts.map((p) => p.ms).filter((v): v is number => v != null)
      return {
        id: t.id,
        name: t.name,
        color: pingPalette[i % pingPalette.length],
        pts,
        cur: vals.length ? vals[vals.length - 1] : null,
        avg: vals.length ? vals.reduce((a, b) => a + b, 0) / vals.length : 0,
        loss: pts.length ? ((pts.length - vals.length) / pts.length) * 100 : 0,
      }
    })
    // winFilter 依赖 timeWin，已在依赖中体现
  }, [pingData, isLive, pingMaxTime, timeWin]) // eslint-disable-line react-hooks/exhaustive-deps

  if (!server) {
    // 列表尚未首次拉取完成时先显示「加载中」，避免刷新瞬间 serverList 为空被误判为「未找到」
    if (!serversReady()) {
      return <div className="py-20 text-center text-sm text-zinc-400">{t('common.loading')}</div>
    }
    return (
      <div className="py-20 text-center text-zinc-500">
        {t('detail.notFound')}
        <div className="mt-4">
          <Link to="/" className="text-emerald-600 hover:underline dark:text-emerald-400">
            {t('detail.backHome')}
          </Link>
        </div>
      </div>
    )
  }

  const tf = (ts: number) => (isLive ? fmtTime(ts) : fmtAxisTime(ts, hours))
  // 刷选条拖动时两端显示的时间文字（短格式：月/日 时:分）
  const brushTf = (ts: number) => {
    const d = new Date(ts)
    return `${d.getMonth() + 1}/${d.getDate()} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  }

  const onBrush = (range: { startIndex?: number; endIndex?: number }) => {
    if (range?.startIndex == null || range?.endIndex == null) return
    if (range.startIndex === 0 && range.endIndex === history.length - 1) {
      setTimeWin(null)
      return
    }
    const s = history[range.startIndex]?.time
    const e = history[range.endIndex]?.time
    if (s != null && e != null) setTimeWin([s, e])
  }

  // 时间轴刷选条：灰色缩略曲线 + 底部刷选带，拖动手柄框选时间段，图表同步缩放
  const timelineBar = (
    <div className={`${card} px-4 pb-2 pt-3`}>
      <div className="mb-1 flex items-center justify-between gap-2">
        <span className="text-xs text-zinc-500">
          {timeWin ? (
            <span className="tabular-nums">
              {fmtDateTime(timeWin[0])} — {fmtDateTime(timeWin[1])}
            </span>
          ) : (
            t('detail.timeline.hint')
          )}
        </span>
        {timeWin && (
          <button
            onClick={() => setTimeWin(null)}
            className="press shrink-0 rounded-md px-2 py-0.5 text-xs font-medium text-emerald-600 transition hover:bg-emerald-500/10 active:bg-emerald-500/20 dark:text-emerald-400"
          >
            {t('detail.reset')}
          </button>
        )}
      </div>
      <div className="h-20">
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={history} margin={{ top: 2, right: 4, bottom: 0, left: 4 }}>
            <XAxis dataKey="time" type="number" domain={['dataMin', 'dataMax']} hide />
            <YAxis hide domain={[0, 100]} />
            <Area
              type="monotone"
              dataKey="cpu"
              stroke={timelineStroke}
              strokeWidth={1}
              fill={timelineStroke}
              fillOpacity={0.12}
              dot={false}
              isAnimationActive={false}
            />
            <Brush
              key={hours}
              dataKey="time"
              height={26}
              travellerWidth={8}
              stroke={brushStroke}
              fill={brushFill}
              tickFormatter={brushTf}
              onChange={onBrush}
            />
          </AreaChart>
        </ResponsiveContainer>
      </div>
    </div>
  )

  return (
    <div className="space-y-4">
      {/* 标题栏：桌面单行（地区行内、时长靠右）；窄屏「地区 · 在线时长」靠右，
          与桌面端时长的位置一致——顶格排在标题左下方时，它比标题本身还靠外，
          像是从标题里掉出来的一截 */}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <Link
          to="/"
          className="glass rounded-xl p-1.5 text-zinc-500 transition hover:text-zinc-900 dark:hover:text-zinc-100"
        >
          <ChevronLeft className="h-4 w-4" />
        </Link>
        <Flag code={server.flag} className="text-xl" />
        <h1 className="text-xl font-bold">{server.name}</h1>
        <StatusPill online={server.online} />
        {(server.region || server.note || server.online) && (
          <span className="ml-auto text-sm tabular-nums text-zinc-500 sm:hidden">
            {[
              [server.region, server.note].filter(Boolean).join(' · '),
              server.online ? t('card.uptime', { t: fmtUptime(server.uptimeSec) }) : '',
            ]
              .filter(Boolean)
              .join(' · ')}
          </span>
        )}
        <span className="hidden text-sm text-zinc-500 sm:inline">
          {server.region}
          {server.note ? ` · ${server.note}` : ''}
        </span>
        {server.online && (
          <span className="ml-auto hidden text-sm tabular-nums text-zinc-500 sm:inline">
            {t('card.uptime', { t: fmtUptime(server.uptimeSec) })}
          </span>
        )}
      </div>

      {/* 基本信息（静态配置） */}
      <div className={`${card} p-4`}>
        <dl className="grid grid-cols-2 gap-x-6 gap-y-3 md:grid-cols-3 lg:grid-cols-4">
          <Info k={t('detail.info.os')} v={server.os} />
          <Info k={t('detail.info.arch')} v={`${server.arch} / ${server.virtualization}`} />
          <Info k="CPU" v={t('detail.info.cpu.value', { model: server.cpuModel, n: server.cpuCores })} />
          <Info
            k={t('detail.info.memSwap')}
            v={`${fmtBytes(server.memTotal, 0)} / ${server.swapTotal > 0 ? fmtBytes(server.swapTotal, 0) : 'off'}`}
          />
          <Info k={t('metric.disk')} v={fmtBytes(server.diskTotal, 0)} />
          <Info
            k={t('detail.info.agent')}
            v={t('detail.info.agent.value', { v: server.agentVersion, n: server.intervalSec })}
          />
          <Info k={t('detail.info.group')} v={server.group} />
          <Info k={t('detail.info.expire')} v={server.expireAt ?? t('detail.info.expire.never')} />
        </dl>
      </div>

      {/* 页签 + 时间范围 */}
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="glass flex gap-1 rounded-xl p-1">
          {tabs.map((tb) => (
            <button
              key={tb.key}
              onClick={() => setTab(tb.key)}
              className={`press flex items-center gap-1.5 rounded-lg px-4 py-1.5 text-sm transition ${
                tab === tb.key
                  ? 'bg-emerald-500/15 font-medium text-emerald-600 dark:text-emerald-400'
                  : 'text-zinc-500 hover:text-zinc-800 active:bg-white/60 dark:hover:text-zinc-200 dark:active:bg-white/10'
              }`}
            >
              <tb.icon className="h-4 w-4" />
              {t(tb.labelKey)}
            </button>
          ))}
        </div>
        <div className="glass flex gap-1 rounded-xl p-1">
          {ranges.map((r) => (
            <button
              key={r.h}
              onClick={() => {
                setHours(r.h)
                setTimeWin(null)
              }}
              className={`rounded-lg px-3 py-1.5 text-xs transition ${
                hours === r.h
                  ? 'bg-emerald-500/15 font-medium text-emerald-600 dark:text-emerald-400'
                  : 'text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200'
              }`}
            >
              {t(r.labelKey)}
            </button>
          ))}
        </div>
      </div>

      {tab === 'load' ? (
        <>
          {/* 实时监控 */}
          <section>
            <h2 className="mb-2 text-sm font-semibold text-zinc-500">{t('detail.live')}</h2>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
              <div className={`${card} p-4`}>
                <div className="text-xs text-zinc-500">{t('detail.load')}</div>
                <div className="mt-1 text-lg font-semibold tabular-nums">{st.load1.toFixed(2)}</div>
                <div className="mt-0.5 text-xs tabular-nums text-zinc-500">
                  {t('detail.load.sub', { l5: st.load5.toFixed(2), l15: st.load15.toFixed(2) })}
                </div>
              </div>
              <div className={`${card} p-4`}>
                <div className="text-xs text-zinc-500">{t('metric.mem')}</div>
                <div className="mt-3 space-y-3">
                  <ProgressBar
                    label={t('metric.mem')}
                    right={`${fmtBytes(st.memUsed)} / ${fmtBytes(server.memTotal)}`}
                    pct={pct(st.memUsed, server.memTotal)}
                  />
                  <ProgressBar
                    label={t('metric.swap')}
                    right={server.swapTotal > 0 ? `${fmtBytes(st.swapUsed)} / ${fmtBytes(server.swapTotal)}` : 'off'}
                    pct={pct(st.swapUsed, server.swapTotal)}
                  />
                </div>
              </div>
              <GaugeCard
                title={t('metric.disk')}
                pct={pct(st.diskUsed, server.diskTotal)}
                detail={`${fmtBytes(st.diskUsed)} / ${fmtBytes(server.diskTotal)}`}
              />
              <div className={`${card} p-4`}>
                <div className="grid grid-cols-2 gap-3">
                  <div>
                    <div className="text-xs text-zinc-500">{t('dash.stat.speed')}</div>
                    <div className="mt-1.5 space-y-1 text-sm font-medium tabular-nums">
                      <div className="flex items-center gap-1.5">
                        <ArrowUp className="h-3.5 w-3.5 text-emerald-500" />
                        <Ticker value={st.netUp} format={fmtSpeed} />
                      </div>
                      <div className="flex items-center gap-1.5">
                        <ArrowDown className="h-3.5 w-3.5 text-sky-500" />
                        <Ticker value={st.netDown} format={fmtSpeed} />
                      </div>
                    </div>
                  </div>
                  <div>
                    <div className="text-xs text-zinc-500">{t('metric.traffic')}</div>
                    <div className="mt-1.5 space-y-1 text-sm font-medium tabular-nums">
                      <div className="flex items-center gap-1.5">
                        <ArrowUp className="h-3.5 w-3.5 text-emerald-500" />
                        {fmtBytes(st.totalUp)}
                      </div>
                      <div className="flex items-center gap-1.5">
                        <ArrowDown className="h-3.5 w-3.5 text-sky-500" />
                        {fmtBytes(st.totalDown)}
                      </div>
                    </div>
                  </div>
                </div>
              </div>
            </div>
          </section>

          {!isLive && timelineBar}

          {/* 历史记录 */}
          <section>
            <h2 className="mb-2 text-sm font-semibold text-zinc-500">{t('detail.history')}</h2>
            <div className="grid gap-3 lg:grid-cols-2">
              <ChartCard title="CPU (%)">
                <ResponsiveContainer width="100%" height="100%">
                  <LineChart data={histView}>
                    <CartesianGrid stroke={gridStroke} vertical={false} />
                    <XAxis
                      dataKey="time"
                      type="number"
                      domain={['dataMin', 'dataMax']}
                      tickFormatter={tf}
                      minTickGap={48}
                      {...axisProps}
                    />
                    <YAxis domain={[0, 100]} width={36} {...axisProps} />
                    <Tooltip content={<ChartTip fmt={(v) => fmtPercent(v)} />} />
                    <Line type="monotone" dataKey="cpu" name="CPU" stroke={palette.green} dot={false} strokeWidth={1.5} isAnimationActive={!isLive} />
                  </LineChart>
                </ResponsiveContainer>
              </ChartCard>
              <ChartCard
                title={t('detail.chart.memSwap')}
                right={
                  <SeriesChips
                    items={[
                      { name: t('metric.mem'), color: palette.sky },
                      { name: t('metric.swap'), color: palette.rose },
                    ]}
                  />
                }
              >
                <ResponsiveContainer width="100%" height="100%">
                  <LineChart data={histView}>
                    <CartesianGrid stroke={gridStroke} vertical={false} />
                    <XAxis
                      dataKey="time"
                      type="number"
                      domain={['dataMin', 'dataMax']}
                      tickFormatter={tf}
                      minTickGap={48}
                      {...axisProps}
                    />
                    <YAxis domain={[0, 100]} width={36} {...axisProps} />
                    <Tooltip content={<ChartTip fmt={(v) => fmtPercent(v)} />} />
                    <Line type="monotone" dataKey="mem" name={t('metric.mem')} stroke={palette.sky} dot={false} strokeWidth={1.5} isAnimationActive={!isLive} />
                    <Line type="monotone" dataKey="swap" name={t('metric.swap')} stroke={palette.rose} dot={false} strokeWidth={1.5} isAnimationActive={!isLive} />
                  </LineChart>
                </ResponsiveContainer>
              </ChartCard>
              <ChartCard
                title={t('detail.chart.net')}
                right={
                  <SeriesChips
                    items={[
                      { name: t('series.up'), color: palette.green },
                      { name: t('series.down'), color: palette.sky },
                    ]}
                  />
                }
              >
                <ResponsiveContainer width="100%" height="100%">
                  <AreaChart data={histView}>
                    <CartesianGrid stroke={gridStroke} vertical={false} />
                    <XAxis
                      dataKey="time"
                      type="number"
                      domain={['dataMin', 'dataMax']}
                      tickFormatter={tf}
                      minTickGap={48}
                      {...axisProps}
                    />
                    <YAxis width={52} tickFormatter={(v: number) => fmtBytes(v, 0)} {...axisProps} />
                    <Tooltip content={<ChartTip fmt={(v) => fmtSpeed(v)} />} />
                    <Area
                      type="monotone"
                      dataKey="netUp"
                      name={t('series.up')}
                      stroke={palette.green}
                      fill={palette.green}
                      fillOpacity={0.12}
                      dot={false}
                      strokeWidth={1.5}
                      isAnimationActive={!isLive}
                    />
                    <Area
                      type="monotone"
                      dataKey="netDown"
                      name={t('series.down')}
                      stroke={palette.sky}
                      fill={palette.sky}
                      fillOpacity={0.12}
                      dot={false}
                      strokeWidth={1.5}
                      isAnimationActive={!isLive}
                    />
                  </AreaChart>
                </ResponsiveContainer>
              </ChartCard>
              <ChartCard title={t('detail.chart.disk')}>
                <ResponsiveContainer width="100%" height="100%">
                  <LineChart data={histView}>
                    <CartesianGrid stroke={gridStroke} vertical={false} />
                    <XAxis
                      dataKey="time"
                      type="number"
                      domain={['dataMin', 'dataMax']}
                      tickFormatter={tf}
                      minTickGap={48}
                      {...axisProps}
                    />
                    <YAxis domain={[0, 100]} width={36} {...axisProps} />
                    <Tooltip content={<ChartTip fmt={(v) => fmtPercent(v)} />} />
                    <Line type="monotone" dataKey="disk" name={t('metric.disk')} stroke={palette.amber} dot={false} strokeWidth={1.5} isAnimationActive={!isLive} />
                  </LineChart>
                </ResponsiveContainer>
              </ChartCard>
              <div className="lg:col-span-2">
                <ChartCard
                  title={t('detail.chart.conn')}
                  right={
                    <SeriesChips
                      items={[
                        { name: 'TCP', color: palette.violet },
                        { name: t('series.proc'), color: palette.amber },
                      ]}
                    />
                  }
                >
                  <ResponsiveContainer width="100%" height="100%">
                    <LineChart data={histView}>
                      <CartesianGrid stroke={gridStroke} vertical={false} />
                      <XAxis
                        dataKey="time"
                        type="number"
                        domain={['dataMin', 'dataMax']}
                        tickFormatter={tf}
                        minTickGap={48}
                        {...axisProps}
                      />
                      <YAxis width={40} {...axisProps} />
                      <Tooltip content={<ChartTip fmt={(v) => String(Math.round(v))} />} />
                      <Line type="monotone" dataKey="tcp" name="TCP" stroke={palette.violet} dot={false} strokeWidth={1.5} isAnimationActive={!isLive} />
                      <Line
                        type="monotone"
                        dataKey="processes"
                        name={t('series.proc')}
                        stroke={palette.amber}
                        dot={false}
                        strokeWidth={1.5}
                        isAnimationActive={!isLive}
                      />
                    </LineChart>
                  </ResponsiveContainer>
                </ChartCard>
              </div>
            </div>
          </section>
        </>
      ) : (
        <>
          {!isLive && timelineBar}
          {pingStats.length === 0 && (
            <div className={`${card} p-10 text-center text-sm text-zinc-500`}>
              {t('detail.ping.empty')}
            </div>
          )}
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
          {pingStats.map((p) => (
            <div key={p.id} className={`${card} p-4`}>
              <div className="flex items-baseline justify-between">
                <span className="flex items-center gap-1.5 text-xs text-zinc-500">
                  <span className="inline-block h-2 w-2 rounded-full" style={{ background: p.color }} />
                  {p.name}
                </span>
                <span className="text-xs tabular-nums text-zinc-400">
                  {t('detail.ping.loss', { n: p.loss.toFixed(1) })}
                </span>
              </div>
              <div className="mt-1 flex items-baseline gap-2">
                <span className="text-lg font-semibold tabular-nums">
                  {p.cur == null ? '—' : `${p.cur} ms`}
                </span>
                <span className="text-xs tabular-nums text-zinc-500">
                  {t('detail.ping.avg', { n: Math.round(p.avg) })}
                </span>
              </div>
              <div className="mt-2 h-32">
                <ResponsiveContainer width="100%" height="100%">
                  <AreaChart data={p.pts}>
                    <CartesianGrid stroke={gridStroke} vertical={false} />
                    <XAxis
                      dataKey="time"
                      type="number"
                      domain={['dataMin', 'dataMax']}
                      tickFormatter={tf}
                      minTickGap={56}
                      {...axisProps}
                      fontSize={10}
                    />
                    <YAxis width={32} {...axisProps} fontSize={10} />
                    <Tooltip content={<ChartTip fmt={(v) => `${Math.round(v)} ms`} />} />
                    <Area
                      type="monotone"
                      dataKey="ms"
                      name={p.name}
                      stroke={p.color}
                      fill={p.color}
                      fillOpacity={0.12}
                      dot={false}
                      strokeWidth={1.5}
                      isAnimationActive={!isLive}
                    />
                  </AreaChart>
                </ResponsiveContainer>
              </div>
            </div>
          ))}
          </div>
        </>
      )}
    </div>
  )
}
