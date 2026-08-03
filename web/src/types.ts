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
  autoFlag: string
  note: string
  expireAt: string
  token: string
  ip: string
  ipv6: string
  online: boolean
  // GCP Spot 自动开机配置与运行态（运行态为内存值，面板重启归零）
  gcpEnabled: boolean
  gcpProject: string
  gcpZone: string
  gcpInstance: string
  gcpTries: number
  gcpLastTry: number
  gcpLastErr: string
}

export interface PingTask {
  id: number
  name: string
  type: 'icmp' | 'tcp' | 'http'
  target: string
  interval: number
  enabled: boolean
  serverId: string // '' = 全部服务器；否则为逗号分隔的服务器 ID 列表
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
  netOn: boolean
  netThreshold: number
  netSeconds: number
  expireOn: boolean
  expireDays: number
}

/** 通用 Webhook 推送配置。后端不回传密钥明文，仅用 secretSet 标记是否已配置。 */
export interface WebhookSettings {
  url: string
  on: boolean
  secretSet: boolean
}

export interface GcpSettings {
  configured: boolean
  clientEmail: string
  projectId: string
  autoOn: boolean
  delay: number
  cooldown: number
  maxTries: number
}

export interface Settings {
  username: string
  siteName: string
  siteDesc: string
  reportInterval: number
  sampleInterval: number
  historyDays: number
  pingDays: number
}

/** AI 接入用的 API Key。明文只在创建时返回一次，此处只有前缀。 */
export interface ApiKey {
  id: number
  name: string
  prefix: string
  caps: string[]
  servers: string[]
  /** 秒级时间戳，0 表示永不过期 */
  expiresAt: number
  createdAt: number
  /** 0 表示从未使用 */
  lastUsedAt: number
  revoked: boolean
}

/** 执行审计列表项，不含输出正文。 */
export interface ExecAudit {
  jobId: string
  serverId: string
  serverName: string
  caller: string
  cmd: string
  dir: string
  /** 毫秒级时间戳 */
  startedAt: number
  /** 0 表示仍在执行 */
  finishedAt: number
  exitCode: number
  error?: string
  truncated?: boolean
}

export interface ExecAuditDetail {
  record: ExecAudit
  timeout: number
  stdout: string
  stderr: string
}
