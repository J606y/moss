/**
 * 冒烟测试用的假接口数据。
 *
 * 字段照 web/src/types.ts，取值刻意挑过，好让尽量多的文案分支渲染出来：
 * 停用的密钥、过期的密钥、解不开的凭证、被占用的凭证、非零退出码的审计……
 * 每多一个分支，「英文界面不得残留汉字」这条断言就多盖住一块。
 *
 * 所有字符串一律用英文：扫描要断言页面上没有汉字，中文的假数据会变成假阳性。
 *
 * **唯一的例外是错误码那几条兜底串**（upgradeHint / hostHint）。它们照抄后端
 * 的中文原文是故意的：界面认出 code 就该显示译文，那句中文永远不该出现。
 * 一旦扫描在英文界面上扫出它，说明翻译那条路断了——这是真阳性，正是要抓的。
 * 契约见 docs/error-codes.md。
 */

export const ADMIN_SERVER = {
  id: 'demo-1',
  name: 'demo-node',
  group: 'prod',
  region: 'Tokyo',
  flag: 'jp',
  autoFlag: 'jp',
  note: 'primary box',
  expireAt: '2027-01-31',
  token: 'tok_demo',
  ip: '203.0.113.7',
  ipv6: '2001:db8::7',
  online: true,
  agentVersion: '2.0.0',
  targetVersion: '2.1.0',
  upgradable: true,
  gcpEnabled: true,
  gcpCredId: 'cred-1',
  gcpProject: 'demo-project',
  gcpZone: 'us-west1-b',
  gcpInstance: 'demo-vm',
  gcpTries: 2,
  gcpLastTry: 1758000000,
  gcpLastErr: '',
}

/**
 * 第二台机器：触发 upgradeTitle 的「不能一键升级」分支。
 *
 * upgradeHint 是后端的中文兜底，upgradeHintCode / upgradeHintDetail 是翻译依据。
 * 界面该显示译文并把 v1.4.0 插在末尾；显示成这句中文就是 bug（见文件头说明）。
 */
export const ADMIN_SERVER_STALE = {
  ...ADMIN_SERVER,
  id: 'demo-2',
  name: 'old-node',
  ipv6: '',
  agentVersion: '1.4.0',
  upgradable: false,
  upgradeHint: 'agent 过旧，不认识升级指令，需用安装命令手动升级一次，当前 v1.4.0',
  upgradeHintCode: 'upgrade.agent_too_old',
  upgradeHintDetail: 'v1.4.0',
  gcpEnabled: false,
  gcpCredId: '',
  gcpTries: 0,
  gcpLastTry: 0,
}

export const PING_TASK = {
  id: 1,
  name: 'edge probe',
  type: 'icmp',
  target: '1.1.1.1',
  interval: 60,
  enabled: true,
  serverId: '',
}

export const GCP_CRED = {
  id: 'cred-1',
  projectId: 'demo-project',
  clientEmail: 'moss-starter@demo-project.iam.gserviceaccount.com',
  serverCount: 2,
  createdAt: 1757000000,
  // 解不开的凭证：触发「无法解密」徽章那一条分支
  decryptable: false,
}

export const API_KEYS = [
  {
    id: 1,
    name: 'watcher',
    prefix: 'moss_ab12',
    caps: ['read'],
    servers: [],
    expiresAt: 0,
    createdAt: 1757000000,
    lastUsedAt: 0,
    disabled: true, // 触发「已停用」徽章 + 「永久」+「从未」
  },
  {
    id: 2,
    name: 'operator',
    prefix: 'moss_cd34',
    caps: ['read', 'exec', 'write'],
    servers: ['demo-1'],
    expiresAt: 1700000000, // 已过期：触发「已过期」徽章
    createdAt: 1690000000,
    lastUsedAt: 1699000000,
    disabled: false,
  },
]

export const EXEC_AUDIT = {
  items: [
    {
      jobId: 'job-1',
      serverId: 'demo-1',
      serverName: 'demo-node',
      caller: 'openclaw',
      cmd: 'df -h',
      startedAt: 1758000000000,
      finishedAt: 1758000001000,
      exitCode: 0,
      error: '',
      truncated: false,
    },
    {
      jobId: 'job-2',
      serverId: 'demo-1',
      serverName: 'demo-node',
      caller: 'openclaw',
      cmd: 'systemctl restart nginx',
      startedAt: 1758000100000,
      finishedAt: 1758000101000,
      exitCode: 1, // 触发「退出码 N」分支
      error: '',
      truncated: false,
    },
  ],
  total: 2,
}

export const NOTIFY = {
  tgToken: '',
  tgChat: '',
  offlineOn: true,
  offlineDelay: 120,
  loadOn: true,
  cpuThreshold: 90,
  memThreshold: 90,
  diskThreshold: 90,
  loadMinutes: 5,
  recoverSec: 60,
  netOn: true,
  netThreshold: 100,
  netSeconds: 60,
  expireOn: true,
  expireDays: 3,
}

export const PANEL_UPDATE = {
  current: '2.1.0',
  config: { channel: 'beta', auto: false, hostServer: '' },
  latest: {
    version: 'v2.2.0',
    name: 'Moss v2.2.0',
    notes: '## v2.2.0\n\n- **Added** an English interface\n- Fixed `group filter` losing state',
    published: '2026-09-20T00:00:00Z',
    prerelease: true, // 触发「· 测试版」后缀
  },
  avail: { action: 'update' },
  // hostReady: false 同时打开两条分支：更新按钮的禁用态，以及下面那句提示。
  // hostHint 与服务器那条同理——中文是兜底，界面该显示译文（见文件头说明）。
  hostReady: false,
  hostHint: '面板所在服务器当前离线：old-node',
  hostHintCode: 'panel.host_offline',
  hostHintDetail: 'old-node',
}

export const SETTINGS = {
  username: 'admin',
  siteName: 'Moss',
  siteDesc: 'Control Center',
  lang: 'auto',
  reportInterval: 2,
  sampleInterval: 10,
  historyDays: 7,
  pingDays: 7,
  execAuditDays: 90,
  execAuditMaxRows: 5000,
}

/** 路径片段 → 响应体。按后缀匹配，命中即返回。 */
export const ADMIN_ROUTES = [
  ['/api/admin/me', { ok: true }],
  ['/api/admin/servers', [ADMIN_SERVER, ADMIN_SERVER_STALE]],
  ['/api/admin/tasks', [PING_TASK]],
  ['/api/admin/notify', NOTIFY],
  ['/api/admin/webhook', { url: 'https://example.com/hooks/moss', on: true, secretSet: true }],
  [
    '/api/admin/gcp',
    { credentials: [GCP_CRED], unboundCount: 2, autoOn: true, delay: 120, cooldown: 300, maxTries: 3 },
  ],
  ['/api/admin/keys', API_KEYS],
  ['/api/admin/exec-audit', EXEC_AUDIT],
  ['/api/admin/panel-update', PANEL_UPDATE],
  ['/api/admin/settings', SETTINGS],
]
