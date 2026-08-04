// Package protocol 定义 server 与 agent 之间的 WebSocket 消息格式。
package protocol

// AgentInfo 注册时上报的主机静态信息。
type AgentInfo struct {
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	Virtualization string `json:"virtualization"`
	CPUModel       string `json:"cpuModel"`
	CPUCores       int    `json:"cpuCores"`
	MemTotal       uint64 `json:"memTotal"`
	SwapTotal      uint64 `json:"swapTotal"`
	DiskTotal      uint64 `json:"diskTotal"`
	AgentVersion   string `json:"agentVersion"`
	IP             string `json:"ip"`          // agent 自测的公网 IPv4，留空则 server 回退到连接来源 IP
	IPv6           string `json:"ipv6"`        // agent 自测的公网 IPv6，主机无 v6 时留空
	CountryCode    string `json:"countryCode"` // ISO alpha-2 小写国家码，用于自动国旗
}

// Stats 周期上报的实时指标，字段名与前端 LiveStats 对齐。
type Stats struct {
	CPU       float64 `json:"cpu"`
	MemUsed   uint64  `json:"memUsed"`
	SwapUsed  uint64  `json:"swapUsed"`
	DiskUsed  uint64  `json:"diskUsed"`
	NetUp     float64 `json:"netUp"`
	NetDown   float64 `json:"netDown"`
	TotalUp   uint64  `json:"totalUp"`
	TotalDown uint64  `json:"totalDown"`
	TCP       int     `json:"tcp"`
	Processes int     `json:"processes"`
	Load1     float64 `json:"load1"`
	Load5     float64 `json:"load5"`
	Load15    float64 `json:"load15"`
}

// PingTask 下发给 agent 的探测任务。
type PingTask struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"` // icmp / tcp / http
	Target   string `json:"target"`
	Interval int    `json:"interval"` // 秒
}

// 执行任务的传输约束。WS 单条消息上限 64KB（见 server/agent_ws.go 的 SetReadLimit），
// Data 经 JSON base64 编码后约膨胀 33%，故原始分片上限取 32KB 留足余量。
const (
	ExecChunkSize  = 32 << 10 // 单个分片的原始字节上限
	ExecOutputCap  = 1 << 20  // 单次执行回传的总输出上限，超出即截断
	ExecMaxTimeout = 30 * 60  // 单次执行允许的最长超时（秒）
)

// ExecTask server → agent 的命令执行任务。
type ExecTask struct {
	ID      string `json:"id"`      // 幂等 ID，agent 侧据此去重，重连后重复下发不会二次执行
	Cmd     string `json:"cmd"`     // 完整命令，由 agent 交给平台默认 shell 执行
	Dir     string `json:"dir"`     // 工作目录，留空则由 agent 取平台默认值
	Timeout int    `json:"timeout"` // 硬超时（秒），到点 agent 强杀整个进程树
}

// WriteTask server → agent 的文件写入任务。
//
// 不复用 ExecTask + heredoc：shell 转义是 AI 写配置文件时最高频的出错点，
// 且内容混在命令行里无法原样审计。独立任务类型让「写了什么」可以逐字节留痕。
type WriteTask struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	// Data 走 base64，二进制安全；上限见 WriteSizeCap。
	Data   []byte `json:"data"`
	Mode   uint32 `json:"mode,omitempty"`   // 0 表示用 0644
	Append bool   `json:"append,omitempty"` // 追加而非覆盖
	Mkdir  bool   `json:"mkdir,omitempty"`  // 自动创建父目录
}

// WriteSizeCap 单次写入的内容上限。
//
// WS 单条消息上限 64KB，base64 后膨胀约 33%，故内容取 48KB 上限一次装得下，
// 无需为写入再实现一套分片重组。nginx 配置、compose 文件、systemd 单元
// 都在几 KB 量级，48KB 覆盖绝大多数场景；更大的文件本就应当用 exec 去目标机拉取。
const WriteSizeCap = 48 << 10

// UpgradeTask server → agent 的自升级任务。
//
// 不复用 ExecTask 下发安装脚本，两条理由：
//   - 闸 2 硬拦「动 agent 二进制 / 配置 / systemd 单元」，走 exec 会被自己的闸拦下；
//   - 更要紧的是，能下发任意升级脚本等于开了一条绕过命令黑名单的通道。
//
// 升级逻辑内置在 agent 里，server 只给「升到哪个版本、从哪下载」，不给可执行内容。
type UpgradeTask struct {
	ID      string `json:"id"`      // 幂等 ID，agent 侧据此去重
	Version string `json:"version"` // 目标版本，带 v 前缀的 tag 名，如 v2.0.0-beta.2
	// BaseURL 是 release 的下载目录，文件名由 agent 按自身 runtime.GOOS/GOARCH 拼。
	//
	// 不由 server 拼好完整 URL：库里的 os 列存的是「Debian 13」这类人类可读的发行版名，
	// 靠字符串匹配反推平台既脆又容易在新系统上悄悄猜错，而 agent 自己一定知道
	// 它是什么平台什么架构。命名规则与 release.yml 的产物一致。
	BaseURL string `json:"baseUrl"`
	// GraceSec 回滚判据：重启后多少秒内没连回 server 就判定失败并回滚。
	// 0 表示用 UpgradeDefaultGrace。
	GraceSec int `json:"graceSec,omitempty"`
}

// 升级阶段。replaced 之后连接必然中断，界面据此显示「正在重启，等待重连」，
// 而不是把断连误报成升级失败。
const (
	UpgradeStageDownloading = "downloading"
	UpgradeStageVerified    = "verified"
	UpgradeStageReplaced    = "replaced"
	UpgradeStageRestarting  = "restarting"
)

// UpgradeDefaultGrace 新 agent 重启后允许的最长失联时间（秒）。
// 正常重连在数秒内完成，60s 给足富余；再长则机器失联时间不可接受。
const UpgradeDefaultGrace = 60

// UpgradeResult agent → server 的升级进度。
//
// 收不到最终成功回执是正常的：agent 在 restarting 阶段就会被重启掐断连接。
// 真正的成功判据是「agent 重新连上来且上报的版本号等于目标版本」，由 server 侧判定。
type UpgradeResult struct {
	ID    string `json:"id"`
	Stage string `json:"stage"`
	Error string `json:"error,omitempty"` // 非空表示升级在该阶段失败，此时不会重启
}

// ExecResult agent → server 的执行输出分片。
// 输出流式回传：同一个 ID 会产生多条 Seq 递增的分片，最后一条 Done=true 且携带退出码。
type ExecResult struct {
	ID     string `json:"id"`
	Seq    int    `json:"seq"`              // 分片序号，从 0 递增，供 server 侧排序重组
	Stream string `json:"stream,omitempty"` // stdout / stderr
	// Data 用 []byte 而非 string：命令输出可能含非 UTF-8 字节，
	// Go 的 encoding/json 会把无效 UTF-8 的 string 替换成 U+FFFD 从而损坏输出，
	// []byte 走 base64 可原样往返。
	Data []byte `json:"data,omitempty"`

	Done      bool   `json:"done"`                // true 表示本次执行结束，此后不再有分片
	ExitCode  int    `json:"exitCode,omitempty"`  // 仅 Done=true 且 Error 为空时有效
	Error     string `json:"error,omitempty"`     // 非空表示未能正常执行（拒绝/超时/启动失败）
	Truncated bool   `json:"truncated,omitempty"` // 输出触顶 ExecOutputCap 被截断
}

// AgentMsg agent → server。
type AgentMsg struct {
	Type      string     `json:"type"` // register / report / ping / exec_result / upgrade_result
	Info      *AgentInfo `json:"info,omitempty"`
	Stats     *Stats     `json:"stats,omitempty"`
	UptimeSec uint64     `json:"uptimeSec,omitempty"`
	TaskID    int64      `json:"taskId,omitempty"`
	Ms        int        `json:"ms"` // -1 表示探测失败/丢包；agent 将 <1ms 记为 1ms，故 0 不会出现

	Exec    *ExecResult    `json:"exec,omitempty"`
	Upgrade *UpgradeResult `json:"upgrade,omitempty"`
}

// ServerMsg server → agent。
type ServerMsg struct {
	Type     string     `json:"type"` // config / exec / write / upgrade
	Interval int        `json:"interval,omitempty"`
	Tasks    []PingTask `json:"tasks,omitempty"`

	Exec    *ExecTask    `json:"exec,omitempty"`
	Write   *WriteTask   `json:"write,omitempty"`
	Upgrade *UpgradeTask `json:"upgrade,omitempty"`
}
