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
  // agent 版本与一键升级状态。
  // 版本比对规则全在后端（upgradeAvailability），前端只消费结论：
  // upgradable 决定按钮显不显示，upgradeHint 说明不能一键升级的原因。
  agentVersion: string
  targetVersion: string
  upgradable: boolean
  upgradeHint?: string
  upgradeStage?: string
  upgradeErr?: string
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
  /** 持续低于回差线多少秒才发恢复通知，负载与网速共用。10–3600，默认 60。 */
  recoverSec: number
  netOn: boolean
  netThreshold: number
  netSeconds: number
  expireOn: boolean
  expireDays: number
}

/** 面板自更新。版本比较与可否更新的判定全在后端，前端只消费结论。 */
export interface PanelUpdate {
  current: string
  config: { channel: 'stable' | 'beta'; auto: boolean; hostServer: string }
  latest?: {
    version: string
    name: string
    notes: string
    published: string
    prerelease: boolean
  }
  checkError?: string
  /** none=已是最新 update=可更新 downgrade=目标更旧（不允许） unknown=无法比较 */
  avail: { action: 'none' | 'update' | 'downgrade' | 'unknown'; reason?: string }
  hostReady: boolean
  hostHint?: string
  stage?: string
  stageErr?: string
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
  /** 执行审计保留天数，7–90。单条记录不可删除，只能整体设定保留时长。 */
  execAuditDays: number
  /** 执行审计条数上限，100–5000，与保留天数取先触发者。 */
  execAuditMaxRows: number
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
  /** 已停用。停用是临时关闭、可再启用；删除才是彻底移除。 */
  disabled: boolean
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
