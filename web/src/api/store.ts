/**
 * 实时数据存储：HTTP 拉取服务器列表 + WebSocket 订阅实时上报。
 * 对页面暴露与原 mock 层一致的接口：useLive / getLive / getLiveBuf。
 */
import { useEffect, useState } from 'react'
import type { LivePoint, LiveStats, ServerMeta } from '../types'

const zeroStats: LiveStats = {
  cpu: 0, memUsed: 0, swapUsed: 0, diskUsed: 0,
  netUp: 0, netDown: 0, totalUp: 0, totalDown: 0,
  tcp: 0, udp: 0, processes: 0, load1: 0, load5: 0, load15: 0,
}

let servers: ServerMeta[] = []
const stats: Record<string, LiveStats> = {}
const bufs: Record<string, LivePoint[]> = {}
const listeners = new Set<() => void>()
let started = false

function emit() {
  listeners.forEach((f) => f())
}

async function fetchServers() {
  try {
    const res = await fetch('/api/servers')
    if (!res.ok) return
    const data: Array<ServerMeta & { stats: LiveStats }> = await res.json()
    servers = data.map(({ stats: st, ...meta }) => {
      stats[meta.id] = st
      return meta as ServerMeta
    })
    emit()
  } catch {
    // 拉取失败时保留旧数据，等待下次刷新
  }
}

export const pct = (used: number, total: number) => (total > 0 ? (used / total) * 100 : 0)

function pushBuf(id: string, st: LiveStats) {
  const meta = servers.find((s) => s.id === id)
  if (!meta) return
  const buf = bufs[id] ?? (bufs[id] = [])
  buf.push({
    time: Date.now(),
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
      stats[msg.id] = msg.stats
      const meta = servers.find((s) => s.id === msg.id)
      if (meta) {
        meta.online = true
        meta.uptimeSec = msg.uptimeSec ?? meta.uptimeSec
      }
      pushBuf(msg.id, msg.stats)
      emit()
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

function ensureStarted() {
  if (started) return
  started = true
  fetchServers()
  connect()
}

/** 订阅实时数据，每次上报触发重渲染 */
export function useLive(): number {
  const [v, setV] = useState(0)
  useEffect(() => {
    ensureStarted()
    const f = () => setV((x) => x + 1)
    listeners.add(f)
    return () => {
      listeners.delete(f)
    }
  }, [])
  return v
}

/** 服务器列表（随实时数据一起更新） */
export function useServers(): ServerMeta[] {
  useLive()
  return servers
}

export const getLive = (id: string): LiveStats => stats[id] ?? zeroStats

export const getLiveBuf = (id: string): LivePoint[] => bufs[id] ?? []

/** 进入详情页时回填服务端的滚动缓冲，让实时图立即有数据 */
export async function ensureBuf(id: string) {
  try {
    const res = await fetch(`/api/servers/${id}/recent`)
    if (!res.ok) return
    const recent: LivePoint[] = await res.json()
    const existing = bufs[id] ?? []
    const lastTime = recent.length ? recent[recent.length - 1].time : 0
    bufs[id] = [...recent, ...existing.filter((p) => p.time > lastTime)].slice(-90)
    emit()
  } catch {
    // 忽略，等 WS 慢慢填充
  }
}
