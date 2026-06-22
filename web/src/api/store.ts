/**
 * 实时数据存储：HTTP 拉取服务器列表 + WebSocket 订阅实时上报。
 *
 * 订阅按粒度拆三类，避免「任意一台上报 → 全首页重渲染」：
 *   - statListeners：每台服务器各自一组，仅该台 tick 时通知（卡片/表格行/详情页自订阅）。
 *   - listListeners：服务器增删、在线↔离线翻转时通知（列表排序/数量变化）。
 *   - aggListeners ：任意 tick 都通知，单调版本号驱动（StatsBar 全局合计专用，单组件承担）。
 */
import { useCallback, useSyncExternalStore } from 'react'
import type { LivePoint, LiveStats, ServerMeta } from '../types'

const zeroStats: LiveStats = {
  cpu: 0, memUsed: 0, swapUsed: 0, diskUsed: 0,
  netUp: 0, netDown: 0, totalUp: 0, totalDown: 0,
  tcp: 0, udp: 0, processes: 0, load1: 0, load5: 0, load15: 0,
}

let servers: ServerMeta[] = []
const stats: Record<string, LiveStats> = {}
const bufs: Record<string, LivePoint[]> = {}

// 三类订阅注册表
const statListeners = new Map<string, Set<() => void>>() // serverID → 监听者
const listListeners = new Set<() => void>()
const aggListeners = new Set<() => void>()
let aggVersion = 0

const emitStat = (id: string) => statListeners.get(id)?.forEach((f) => f())
const emitList = () => listListeners.forEach((f) => f())
const emitAgg = () => {
  aggVersion++
  aggListeners.forEach((f) => f())
}
// 列表/聚合 + 所有已订阅卡片一起刷新（整体拉取后用）
const emitAll = () => {
  emitList()
  emitAgg()
  statListeners.forEach((set) => set.forEach((f) => f()))
}

async function fetchServers() {
  try {
    const res = await fetch('/api/servers', { cache: 'no-store' })
    if (!res.ok) return
    const data: Array<ServerMeta & { stats: LiveStats }> = await res.json()
    servers = data.map(({ stats: st, ...meta }) => {
      stats[meta.id] = st
      return meta as ServerMeta
    })
    emitAll()
  } catch {
    // 拉取失败时保留旧数据，等待下次刷新
  }
}

export const pct = (used: number, total: number) => (total > 0 ? (used / total) * 100 : 0)

function pushBuf(id: string, st: LiveStats, time?: number) {
  const meta = servers.find((s) => s.id === id)
  if (!meta) return
  const buf = bufs[id] ?? (bufs[id] = [])
  buf.push({
    // 优先用服务端落点时间（与 /recent、历史同源）；缺省才退回浏览器时钟。
    // 统一时钟源后，实时点与回填点不再因服务端/浏览器时差出现 ~Δ 秒的空窗。
    time: time ?? Date.now(),
    cpu: st.cpu,
    mem: pct(st.memUsed, meta.memTotal),
    disk: pct(st.diskUsed, meta.diskTotal),
    swap: pct(st.swapUsed, meta.swapTotal),
    load1: st.load1,
    netUp: st.netUp,
    netDown: st.netDown,
    tcp: st.tcp,
    processes: st.processes,
  })
  if (buf.length > 90) buf.shift()
}

interface WsMsg {
  type: 'stats' | 'online' | 'offline' | 'meta'
  id?: string
  stats?: LiveStats
  uptimeSec?: number
  time?: number // 服务端落点时间（毫秒）；与 /recent、历史接口同源
}

let ws: WebSocket | null = null
let reconnectTimer: ReturnType<typeof setTimeout> | null = null

/** 断开后 3 秒重连；后台标签页暂不重连，回到前台再连，避免无限空转 */
function scheduleReconnect() {
  if (reconnectTimer != null) return
  if (document.hidden) return
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null
    fetchServers()
    connect()
  }, 3000)
}

function connect() {
  // 单实例保护：已有连接（连接中/已连接）时不再叠加新 socket
  if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) return
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  const sock = new WebSocket(`${proto}://${location.host}/api/ws`)
  ws = sock
  sock.onmessage = (e) => {
    let msg: WsMsg
    try {
      msg = JSON.parse(e.data)
    } catch {
      return
    }
    if (msg.type === 'stats' && msg.id && msg.stats) {
      stats[msg.id] = msg.stats // 每次都是新对象，作为 useLiveStats 的快照
      const meta = servers.find((s) => s.id === msg.id)
      let flipped = false
      if (meta) {
        if (!meta.online) flipped = true // 识别 离线→在线 翻转
        meta.online = true
        meta.uptimeSec = msg.uptimeSec ?? meta.uptimeSec
      }
      pushBuf(msg.id, msg.stats, msg.time)
      emitStat(msg.id) // 只重渲染这一台的卡片/行/详情页
      emitAgg() // StatsBar 全局合计（单组件）
      if (flipped) {
        servers = [...servers] // 翻转：换数组引用，让 useServers 重排序
        emitList()
      }
    } else {
      // online / offline / meta：服务器列表或状态变化，整体刷新
      fetchServers()
    }
  }
  // onerror 与 onclose 共用同一套清理 + 重连逻辑，避免重复触发重连
  const handleDown = () => {
    if (ws !== sock) return // 已被新连接替换，忽略旧 socket 的回调
    ws = null
    scheduleReconnect()
  }
  sock.onerror = handleDown
  sock.onclose = handleDown
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    // 回到前台且当前无活动连接时，立刻补连
    if (!document.hidden && started && !ws) {
      fetchServers()
      connect()
    }
  })
}

let started = false
function ensureStarted() {
  if (started) return
  started = true
  fetchServers()
  connect()
}

/** 订阅单台服务器的实时 stats；id 缺省时返回零值且不订阅 */
export function useLiveStats(id?: string): LiveStats {
  const subscribe = useCallback(
    (cb: () => void) => {
      ensureStarted()
      if (!id) return () => {}
      let set = statListeners.get(id)
      if (!set) {
        set = new Set()
        statListeners.set(id, set)
      }
      set.add(cb)
      return () => {
        set!.delete(cb)
        if (set!.size === 0) statListeners.delete(id) // 清理空 Set，防 Map 泄漏
      }
    },
    [id],
  )
  const getSnapshot = useCallback(() => (id ? getLive(id) : zeroStats), [id])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

/** 服务器列表：仅增删 / 在线翻转 / 整体刷新时变化 */
export function useServers(): ServerMeta[] {
  const subscribe = useCallback((cb: () => void) => {
    ensureStarted()
    listListeners.add(cb)
    return () => {
      listListeners.delete(cb)
    }
  }, [])
  const getSnapshot = useCallback(() => servers, [])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

/** 任意一台 tick 都自增的版本号，供需要全局合计的组件（StatsBar）订阅 */
export function useAllStatsVersion(): number {
  const subscribe = useCallback((cb: () => void) => {
    ensureStarted()
    aggListeners.add(cb)
    return () => {
      aggListeners.delete(cb)
    }
  }, [])
  const getSnapshot = useCallback(() => aggVersion, [])
  return useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

export const getLive = (id: string): LiveStats => stats[id] ?? zeroStats

export const getLiveBuf = (id: string): LivePoint[] => bufs[id] ?? []

/** 进入详情页时回填服务端的滚动缓冲，让实时图立即有数据 */
export async function ensureBuf(id: string) {
  try {
    const res = await fetch(`/api/servers/${id}/recent`, { cache: 'no-store' })
    if (!res.ok) return
    const recent: LivePoint[] = await res.json()
    const existing = bufs[id] ?? []
    const lastTime = recent.length ? recent[recent.length - 1].time : 0
    bufs[id] = [...recent, ...existing.filter((p) => p.time > lastTime)].slice(-90)
    emitStat(id) // 只刷新当前详情页对应的订阅者
  } catch {
    // 忽略，等 WS 慢慢填充
  }
}
