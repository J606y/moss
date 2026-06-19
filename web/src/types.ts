export interface ServerMeta {
  id: string
  name: string
  region: string
  flag: string
  os: string
  arch: string
  virtualization: string
  cpuModel: string
  cpuCores: number
  memTotal: number
  swapTotal: number
  diskTotal: number
  agentVersion: string
  intervalSec: number
  online: boolean
  uptimeSec: number
  group: string
  expireAt?: string
  note?: string
}

export interface LiveStats {
  cpu: number
  memUsed: number
  swapUsed: number
  diskUsed: number
  netUp: number
  netDown: number
  totalUp: number
  totalDown: number
  tcp: number
  udp: number
  processes: number
  load1: number
  load5: number
  load15: number
}

export interface LivePoint {
  time: number
  cpu: number
  mem: number
  disk: number
  swap: number
  load1: number
  netUp: number
  netDown: number
  tcp: number
  processes: number
}

export type HistoryPoint = LivePoint

/* ---------- 延迟探测 ---------- */

export interface PingPt {
  time: number
  ms: number | null // null = 丢包
}

export interface PingData {
  tasks: Array<{ id: number; name: string }>
  series: Record<string, PingPt[]>
}

/* ---------- 管理后台 ---------- */

export interface AdminServer {
  id: string
  name: string
  group: string
  region: string
  flag: string
  note: string
  expireAt: string
  token: string
  ip: string
  online: boolean
  lastSeen: number
  createdAt: number
}

export interface PingTask {
  id: number
  name: string
  type: 'icmp' | 'tcp' | 'http'
  target: string
  interval: number
  enabled: boolean
  serverId: string // '' = 全部服务器
}

export interface NotifySettings {
  tgToken: string
  tgChat: string
  offlineOn: boolean
  offlineDelay: number
  loadOn: boolean
  cpuThreshold: number
  memThreshold: number
  diskThreshold: number
  loadMinutes: number
}

export interface Settings {
  siteName: string
  siteDesc: string
  reportInterval: number
  sampleInterval: number
  historyDays: number
  pingDays: number
}
