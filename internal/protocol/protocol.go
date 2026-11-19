// Package protocol 定义 server 与 agent 之间的 WebSocket 消息格式。
package protocol

// AgentInfo 注册时上报的主机静态信息。
type AgentInfo struct {
	UUID           string `json:"uuid,omitempty"` // 节点固定硬件/持久化 UUID，用于重注册时稳定绑定 server_id
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
	ExecChunkSize            = 32 << 10     // 单个分片的原始字节上限
	ExecOutputCap            = 1 << 20      // 单次流式执行回传的总输出上限，超出即截断
	ExecMaxTimeout           = 30 * 60      // 普通执行允许的最长超时（30 分钟，秒）
	ExecDetachDefaultTimeout = 60 * 60      // detach 后台托管模式的默认超时（1 小时，秒）
	ExecDetachMaxTimeout     = 24 * 60 * 60 // detach 后台托管模式允许的最长超时（24 小时，秒）
)

// ExecTask server → agent 的命令执行任务。
type ExecTask struct {
	ID      string `json:"id"`               // 幂等 ID，agent 侧据此去重，重连后重复下发不会二次执行
	Cmd     string `json:"cmd"`              // 完整命令，由 agent 交给平台默认 shell 执行
	Dir     string `json:"dir"`              // 工作目录，留空则由 agent 取平台默认值
	Timeout int    `json:"timeout"`          // 硬超时（秒），普通模式到点强杀进程树，detach 模式走 systemd RuntimeMaxSec
	Detach  bool   `json:"detach,omitempty"` // true 表示后台托管模式（systemd-run --unit=moss-job-<id>，日志落盘无 1MB 上限）
}

// JobQuery server → agent 查询后台托管任务的运行状态与增量/尾部日志。
type JobQuery struct {
	ReqID  string `json:"reqId"`
	JobID  string `json:"jobId"`
	Offset int64  `json:"offset,omitempty"`
	Tail   int64  `json:"tail,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// JobCancel server → agent 终止正在执行的任务（含 detach 后台单元与普通在途进程树）。
type JobCancel struct {
	ReqID string `json:"reqId"`
	JobID string `json:"jobId"`
}

// JobStatus agent → server 的后台任务状态与日志切片回执（响应 JobQuery 或 JobCancel）。
type JobStatus struct {
	ReqID      string `json:"reqId"`
	JobID      string `json:"jobId"`
	Unit       string `json:"unit,omitempty"`
	Found      bool   `json:"found"`
	Running    bool   `json:"running"`
	Canceled   bool   `json:"canceled,omitempty"`
	ExitCode   int    `json:"exitCode"`
	Error      string `json:"error,omitempty"`
	Data       []byte `json:"data,omitempty"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	TotalBytes int64  `json:"totalBytes"`
}

// WriteTask server → agent 的文件写入任务。
type WriteTask struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Data   []byte `json:"data"`
	Mode   uint32 `json:"mode,omitempty"`
	Append bool   `json:"append,omitempty"`
	Mkdir  bool   `json:"mkdir,omitempty"`
}

const WriteSizeCap = 48 << 10

// ReadTask server → agent 的分块文件读取/下载任务。
type ReadTask struct {
	ReqID  string `json:"reqId"`
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Length int    `json:"length,omitempty"`
}

// ReadResult agent → server 的分块文件读取回执。
type ReadResult struct {
	ReqID      string `json:"reqId"`
	Path       string `json:"path,omitempty"`
	Data       []byte `json:"data,omitempty"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"nextOffset"`
	TotalBytes int64  `json:"totalBytes"`
	EOF        bool   `json:"eof"`
	Error      string `json:"error,omitempty"`
}

// UpgradeTask server → agent 的自升级任务。
type UpgradeTask struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	BaseURL  string `json:"baseUrl"`
	GraceSec int    `json:"graceSec,omitempty"`
}

const (
	UpgradeStageDownloading = "downloading"
	UpgradeStageVerified    = "verified"
	UpgradeStageReplaced    = "replaced"
	UpgradeStageRestarting  = "restarting"
)

const UpgradeDefaultGrace = 60

// UpgradeResult agent → server 的升级进度。
type UpgradeResult struct {
	ID    string `json:"id"`
	Stage string `json:"stage"`
	Error string `json:"error,omitempty"`
}

// ExecResult agent → server 的执行输出分片。
type ExecResult struct {
	ID     string `json:"id"`
	Seq    int    `json:"seq"`
	Stream string `json:"stream,omitempty"`
	Data   []byte `json:"data,omitempty"`

	Done      bool   `json:"done"`
	Detached  bool   `json:"detached,omitempty"`
	Unit      string `json:"unit,omitempty"`
	ExitCode  int    `json:"exitCode,omitempty"`
	Error     string `json:"error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// AgentMsg agent → server。
type AgentMsg struct {
	Type      string     `json:"type"` // register / report / ping / exec_result / upgrade_result / job_status / read_result
	Info      *AgentInfo `json:"info,omitempty"`
	Stats     *Stats     `json:"stats,omitempty"`
	UptimeSec uint64     `json:"uptimeSec,omitempty"`
	TaskID    int64      `json:"taskId,omitempty"`
	Ms        int        `json:"ms"`

	Exec      *ExecResult    `json:"exec,omitempty"`
	Upgrade   *UpgradeResult `json:"upgrade,omitempty"`
	JobStatus *JobStatus     `json:"jobStatus,omitempty"`
	Read      *ReadResult    `json:"read,omitempty"`
}

// ServerMsg server → agent。
type ServerMsg struct {
	Type     string     `json:"type"` // config / exec / write / read / upgrade / job_query / job_cancel
	Interval int        `json:"interval,omitempty"`
	Tasks    []PingTask `json:"tasks,omitempty"`

	Exec      *ExecTask    `json:"exec,omitempty"`
	Write     *WriteTask   `json:"write,omitempty"`
	Read      *ReadTask    `json:"read,omitempty"`
	Upgrade   *UpgradeTask `json:"upgrade,omitempty"`
	JobQuery  *JobQuery    `json:"jobQuery,omitempty"`
	JobCancel *JobCancel   `json:"jobCancel,omitempty"`
}
