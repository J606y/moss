import { useEffect, useState } from 'react'
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
import { StatusPill } from '../components/ServerCard'
import { ProgressBar, barColor, clampPct } from '../components/ProgressBar'
import Flag from '../components/Flag'
import Ticker from '../components/Ticker'
import { axisProps, ChartCard, ChartTip, gridStroke, palette, SeriesChips } from '../components/Charts'
import { fmtAxisTime, fmtBytes, fmtDateTime, fmtPercent, fmtSpeed, fmtTime, fmtUptime } from '../utils/format'
import { card } from '../ui'

const ranges = [
  { label: '实时', h: 0 },
  { label: '1 小时', h: 1 },
  { label: '6 小时', h: 6 },
  { label: '24 小时', h: 24 },
  { label: '7 天', h: 168 },
]

const tabs = [
  { key: 'load' as const, label: '负载监控', icon: Activity },
  { key: 'ping' as const, label: '延迟监控', icon: Radar },
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
      <div className="flex items-baseline justify-between">
        <span className="text-xs text-zinc-500">{title}</span>
        <span className="hidden text-xs tabular-nums text-zinc-400 sm:inline">{detail}</span>
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

const pingPalette = ['#10b981', '#0ea5e9', '#f59e0b', '#8b5cf6', '#f43f5e', '#14b8a6']

export default function ServerDetail() {
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
  const pollSec = server?.intervalSec ?? 2
  useEffect(() => {
    if (!id) return
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
  }, [id, hours, pollSec])

  // 回填服务端滚动缓冲，让实时图立即有数据
  useEffect(() => {
    if (id) ensureBuf(id)
  }, [id])

  if (!server) {
    // 列表尚未首次拉取完成时先显示「加载中」，避免刷新瞬间 serverList 为空被误判为「未找到」
    if (!serversReady()) {
      return <div className="py-20 text-center text-sm text-zinc-400">加载中…</div>
    }
    return (
      <div className="py-20 text-center text-zinc-500">
        未找到该服务器
        <div className="mt-4">
          <Link to="/" className="text-emerald-600 hover:underline dark:text-emerald-400">
            返回首页
          </Link>
        </div>
      </div>
    )
  }

  const tf = (t: number) => (isLive ? fmtTime(t) : fmtAxisTime(t, hours))
  // 刷选条拖动时两端显示的时间文字（短格式：月/日 时:分）
  const brushTf = (t: number) => {
    const d = new Date(t)
    return `${d.getMonth() + 1}/${d.getDate()} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  }

  // 按刷选窗口过滤数据
  const winFilter = <T extends { time: number }>(arr: T[]): T[] =>
    timeWin ? arr.filter((d) => d.time >= timeWin[0] && d.time <= timeWin[1]) : arr
  // 实时模式下浅拷贝缓冲数组，保证每次 tick 触发图表更新
  const histView = isLive ? [...getLiveBuf(server.id)] : winFilter(history)

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
            '时间轴 · 拖动两端手柄框选时间段，定位具体时间点'
          )}
        </span>
        {timeWin && (
          <button
            onClick={() => setTimeWin(null)}
            className="shrink-0 rounded-md px-2 py-0.5 text-xs font-medium text-emerald-600 transition hover:bg-emerald-500/10 dark:text-emerald-400"
          >
            重置
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
              stroke="#71717a"
              strokeWidth={1}
              fill="#71717a"
              fillOpacity={0.12}
              dot={false}
              isAnimationActive={false}
            />
            <Brush
              key={hours}
              dataKey="time"
              height={26}
              travellerWidth={8}
              stroke="#10b981"
              fill="rgba(120,120,128,0.12)"
              tickFormatter={brushTf}
              onChange={onBrush}
            />
          </AreaChart>
        </ResponsiveContainer>
      </div>
    </div>
  )

  // 实时模式：延迟图只展示最近 5 分钟。锚定到最新数据点的时间（而非浏览器 Date.now），
  // 避免浏览器与服务端时钟偏差导致窗口整体错位、把点全滤掉。
  const LIVE_PING_WIN_MS = 5 * 60_000
  let pingMaxTime = 0
  if (isLive)
    for (const k in pingData.series)
      for (const p of pingData.series[k]) if (p.time > pingMaxTime) pingMaxTime = p.time
  const liveCut = pingMaxTime - LIVE_PING_WIN_MS
  const liveTrim = <T extends { time: number }>(arr: T[]): T[] =>
    isLive && pingMaxTime ? arr.filter((d) => d.time >= liveCut) : arr

  const pingStats = pingData.tasks.map((t, i) => {
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

  return (
    <div className="space-y-4">
      {/* 标题栏 */}
      <div className="flex flex-wrap items-center gap-3">
        <Link
          to="/"
          className="glass rounded-xl p-1.5 text-zinc-500 transition hover:text-zinc-900 dark:hover:text-zinc-100"
        >
          <ChevronLeft className="h-4 w-4" />
        </Link>
        <Flag code={server.flag} className="text-xl" />
        <h1 className="text-xl font-bold">{server.name}</h1>
        <StatusPill online={server.online} />
        <span className="text-sm text-zinc-500">
          {server.region}
          {server.note ? ` · ${server.note}` : ''}
        </span>
        {server.online && (
          <span className="ml-auto text-sm tabular-nums text-zinc-500">
            在线 {fmtUptime(server.uptimeSec)}
          </span>
        )}
      </div>

      {/* 基本信息（静态配置） */}
      <div className={`${card} p-4`}>
        <dl className="grid grid-cols-2 gap-x-6 gap-y-3 md:grid-cols-3 lg:grid-cols-4">
          <Info k="操作系统" v={server.os} />
          <Info k="架构 / 虚拟化" v={`${server.arch} / ${server.virtualization}`} />
          <Info k="CPU" v={`${server.cpuModel} (${server.cpuCores} 核)`} />
          <Info k="内存 / 交换" v={`${fmtBytes(server.memTotal, 0)} / ${server.swapTotal > 0 ? fmtBytes(server.swapTotal, 0) : 'off'}`} />
          <Info k="硬盘" v={fmtBytes(server.diskTotal, 0)} />
          <Info k="Agent 版本" v={`v${server.agentVersion} · ${server.intervalSec}s 上报`} />
          <Info k="分组" v={server.group} />
          <Info k="到期时间" v={server.expireAt ?? '长期'} />
        </dl>
      </div>

      {/* 页签 + 时间范围 */}
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="glass flex gap-1 rounded-xl p-1">
          {tabs.map((t) => (
            <button
              key={t.key}
              onClick={() => setTab(t.key)}
              className={`flex items-center gap-1.5 rounded-lg px-4 py-1.5 text-sm transition ${
                tab === t.key
                  ? 'bg-emerald-500/15 font-medium text-emerald-600 dark:text-emerald-400'
                  : 'text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200'
              }`}
            >
              <t.icon className="h-4 w-4" />
              {t.label}
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
              {r.label}
            </button>
          ))}
        </div>
      </div>

      {tab === 'load' ? (
        <>
          {/* 实时监控 */}
          <section>
            <h2 className="mb-2 text-sm font-semibold text-zinc-500">实时监控</h2>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
              <div className={`${card} p-4`}>
                <div className="text-xs text-zinc-500">系统负载</div>
                <div className="mt-1 text-lg font-semibold tabular-nums">{st.load1.toFixed(2)}</div>
                <div className="mt-0.5 text-xs tabular-nums text-zinc-500">
                  5 分 {st.load5.toFixed(2)} · 15 分 {st.load15.toFixed(2)}
                </div>
              </div>
              <div className={`${card} p-4`}>
                <div className="text-xs text-zinc-500">内存</div>
                <div className="mt-3 space-y-3">
                  <ProgressBar
                    label="内存"
                    right={`${fmtBytes(st.memUsed)} / ${fmtBytes(server.memTotal)}`}
                    pct={pct(st.memUsed, server.memTotal)}
                  />
                  <ProgressBar
                    label="交换"
                    right={server.swapTotal > 0 ? `${fmtBytes(st.swapUsed)} / ${fmtBytes(server.swapTotal)}` : 'off'}
                    pct={pct(st.swapUsed, server.swapTotal)}
                  />
                </div>
              </div>
              <GaugeCard
                title="硬盘"
                pct={pct(st.diskUsed, server.diskTotal)}
                detail={`${fmtBytes(st.diskUsed)} / ${fmtBytes(server.diskTotal)}`}
              />
              <div className={`${card} p-4`}>
                <div className="grid grid-cols-2 gap-3">
                  <div>
                    <div className="text-xs text-zinc-500">实时网速</div>
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
                    <div className="text-xs text-zinc-500">总流量</div>
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
            <h2 className="mb-2 text-sm font-semibold text-zinc-500">历史记录</h2>
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
                title="内存 / 交换 (%)"
                right={
                  <SeriesChips
                    items={[
                      { name: '内存', color: palette.sky },
                      { name: '交换', color: palette.rose },
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
                    <Line type="monotone" dataKey="mem" name="内存" stroke={palette.sky} dot={false} strokeWidth={1.5} isAnimationActive={!isLive} />
                    <Line type="monotone" dataKey="swap" name="交换" stroke={palette.rose} dot={false} strokeWidth={1.5} isAnimationActive={!isLive} />
                  </LineChart>
                </ResponsiveContainer>
              </ChartCard>
              <ChartCard
                title="网络速率"
                right={
                  <SeriesChips
                    items={[
                      { name: '上行', color: palette.green },
                      { name: '下行', color: palette.sky },
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
                      name="上行"
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
                      name="下行"
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
              <ChartCard title="硬盘 (%)">
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
                    <Line type="monotone" dataKey="disk" name="硬盘" stroke={palette.amber} dot={false} strokeWidth={1.5} isAnimationActive={!isLive} />
                  </LineChart>
                </ResponsiveContainer>
              </ChartCard>
              <div className="lg:col-span-2">
                <ChartCard
                  title="连接数 / 进程数"
                  right={
                    <SeriesChips
                      items={[
                        { name: 'TCP', color: palette.violet },
                        { name: '进程', color: palette.amber },
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
                        name="进程"
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
              暂无探测任务 · 可在 管理后台 → 探测任务 中添加
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
                <span className="text-xs tabular-nums text-zinc-400">丢包 {p.loss.toFixed(1)}%</span>
              </div>
              <div className="mt-1 flex items-baseline gap-2">
                <span className="text-lg font-semibold tabular-nums">
                  {p.cur == null ? '—' : `${p.cur} ms`}
                </span>
                <span className="text-xs tabular-nums text-zinc-500">平均 {Math.round(p.avg)} ms</span>
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
