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

// AgentMsg agent → server。
type AgentMsg struct {
	Type      string     `json:"type"` // register / report / ping
	Info      *AgentInfo `json:"info,omitempty"`
	Stats     *Stats     `json:"stats,omitempty"`
	UptimeSec uint64     `json:"uptimeSec,omitempty"`
	TaskID    int64      `json:"taskId,omitempty"`
	Ms        int        `json:"ms"` // -1 表示探测失败/丢包；agent 将 <1ms 记为 1ms，故 0 不会出现
}

// ServerMsg server → agent。
type ServerMsg struct {
	Type     string     `json:"type"` // config
	Interval int        `json:"interval,omitempty"`
	Tasks    []PingTask `json:"tasks,omitempty"`
}
