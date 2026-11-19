// MCP 工具集：AI 能对集群做的事。
//
// 每个工具都要过两道闸：能力集（这把 Key 能不能做这类事）与机器白名单
// （这把 Key 能不能碰这台机器）。两者都在 apikey.go 定义，此处逐个工具执行。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"moss/internal/protocol"
)

// mcpTools 返回工具清单。编译期固定，故 capabilities 里不声明 listChanged。
func mcpTools() []mcpTool {
	return []mcpTool{
		{
			Name:  "list_servers",
			Title: "列出服务器",
			Description: "列出所有服务器及其在线状态、系统信息。返回的 id 是后续所有工具的目标标识，任何操作前都应先调用本工具确认 id。" +
				"agentVersion 是该机 agent 的版本：exec / write_file 需要 2.0.0 及以上，" +
				"低于此版本的 agent 会静默丢弃这两类任务，症状是干等到超时而非报错。" +
				"因此命令执行超时却查不出原因时，应先回来核对本字段，而不是反复重试。",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
			Annotations: &mcpToolAnnotations{
				Title:           "列出服务器",
				ReadOnlyHint:    boolPtr(true),
				DestructiveHint: boolPtr(false),
				IdempotentHint:  boolPtr(true),
				OpenWorldHint:   boolPtr(false),
			},
		},
		{
			Name:        "get_metrics",
			Title:       "读取实时指标",
			Description: "读取指定服务器的实时指标：CPU、内存、交换区、磁盘、网速、TCP 连接数、进程数、负载、运行时长。排查问题应先看本工具的结果，确认瓶颈在哪再决定执行什么命令；部署完成后也应调用本工具确认机器状态正常。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        "string",
						"description": "目标服务器 id，来自 list_servers",
					},
				},
				"required":             []string{"server_id"},
				"additionalProperties": false,
			},
			Annotations: &mcpToolAnnotations{
				Title:           "读取实时指标",
				ReadOnlyHint:    boolPtr(true),
				DestructiveHint: boolPtr(false),
				IdempotentHint:  boolPtr(true),
				OpenWorldHint:   boolPtr(false),
			},
		},
		{
			Name:  "get_history",
			Title: "读取历史曲线",
			Description: "读取指定服务器的历史指标曲线。被告警叫醒后的第一个问题永远是「从什么时候开始的、是突发还是渐变」，" +
				"实时指标答不了这个问题，应当用本工具。" +
				"返回降采样后的时间序列（点数上限 120）与每个指标的 min/max/avg/first/last/trend 摘要；" +
				"多数情况下只看 summary 就足以定位，不必逐点分析。" +
				"单位：cpu/mem/swap/disk 为百分比，load1 为 1 分钟负载，net_up/net_down 为字节每秒，tcp/processes 为计数。" +
				"时间戳为秒级 Unix 时间；序列与摘要的键是 camelCase（net_up 对应 netUp）。" +
				"trend 取首尾各 1/4 段的均值比较，相对变化不足 5% 记为 flat。" +
				"bucketSeconds 是实际聚合粒度，比它更短的抖动已被均值抹平，要看瞬时毛刺请缩小 hours。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        "string",
						"description": "目标服务器 id，来自 list_servers",
					},
					"hours": map[string]any{
						"type":        "integer",
						"description": "回看时长（小时），默认 1。超过历史保留期会被自动收敛，并在 note 中说明实际用了多久",
					},
					"metrics": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "string",
							"enum": []string{"cpu", "mem", "swap", "disk", "load1", "net_up", "net_down", "tcp", "processes"},
						},
						"description": "要返回的指标，留空返回 cpu/mem/disk。按需选取：每多要一项就多一条完整曲线，会挤占上下文",
					},
				},
				"required":             []string{"server_id"},
				"additionalProperties": false,
			},
			Annotations: &mcpToolAnnotations{
				Title:           "读取历史曲线",
				ReadOnlyHint:    boolPtr(true),
				DestructiveHint: boolPtr(false),
				IdempotentHint:  boolPtr(true),
				OpenWorldHint:   boolPtr(false),
			},
		},
		{
			Name:  "exec",
			Title: "执行命令",
			Description: "在指定服务器上执行 shell 命令。异步：立即返回 jobId，需用 get_result 轮询取回输出。" +
				"长耗时任务（系统大包更新、dpkg 解压、编译构建等）请设 detach=true：agent 将用 systemd-run --unit=moss-job-<id> 拉起独立后台服务并立即返回，" +
				"任务脱离 agent 运行，日志直接落盘（无 1MB 上限），超时由 RuntimeMaxSec 控制（最高 24 小时），退出码写入状态文件；Windows 暂不支持 detach。" +
				"破坏性命令（删根目录、格式化磁盘、关机、停用 moss agent 等）会被服务端拦截并记入审计。" +
				"执行前务必确认 server_id 是对的；生产环境的破坏性变更应先向人确认。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        "string",
						"description": "目标服务器 id，来自 list_servers",
					},
					"cmd": map[string]any{
						"type":        "string",
						"description": "完整的 shell 命令",
					},
					"dir": map[string]any{
						"type":        "string",
						"description": "工作目录，留空则用目标机默认值",
					},
					"timeout": map[string]any{
						"type":        "integer",
						"description": "硬超时（秒）。默认模式下默认 60、最大 1800，到点后 agent 强杀进程树；detach=true 时默认 3600（1 小时）、最大 86400（24 小时），由 systemd RuntimeMaxSec 控制",
					},
					"detach": map[string]any{
						"type":        "boolean",
						"description": "true 表示启用后台托管模式（systemd-run --unit=moss-job-<id>）：任务脱离 agent 独立运行，日志写磁盘无 1MB 上限，超时上限放宽到 24 小时（Windows 暂不支持）。默认 false",
					},
				},
				"required":             []string{"server_id", "cmd"},
				"additionalProperties": false,
			},
			Annotations: &mcpToolAnnotations{
				Title:           "执行命令",
				ReadOnlyHint:    boolPtr(false),
				DestructiveHint: boolPtr(true),
				IdempotentHint:  boolPtr(false),
				OpenWorldHint:   boolPtr(true),
			},
		},
		{
			Name:  "get_result",
			Title: "取回执行结果",
			Description: "用 exec 返回的 jobId 取回执行结果与日志。running 为 true 表示仍在执行，稍后重试。" +
				"支持 offset（按字节偏移增量追读新增日志，配合返回的 nextOffset 分段流式读取）和 tail（直接读取日志末尾 N KB）。" +
				"普通任务结果在内存保留 30 分钟（输出上限 1MB）；detach=true 的后台托管任务日志落盘无 1MB 上限，且元数据存数据库，不受 30 分钟 TTL 和服务重启影响。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "exec 返回的 jobId",
					},
					"offset": map[string]any{
						"type":        "integer",
						"description": "从指定字节偏移量开始读取增量日志（默认 0）。可传入上次 get_result 返回的 nextOffset 实现分段追读",
					},
					"tail": map[string]any{
						"type":        "integer",
						"description": "直接读取日志末尾的 N KB（例如 16 表示读取末尾 16KB）。设置大于 0 时优先于 offset",
					},
				},
				"required":             []string{"job_id"},
				"additionalProperties": false,
			},
			Annotations: &mcpToolAnnotations{
				Title:           "取回执行结果",
				ReadOnlyHint:    boolPtr(true),
				DestructiveHint: boolPtr(false),
				IdempotentHint:  boolPtr(true),
				OpenWorldHint:   boolPtr(false),
			},
		},
		{
			Name:  "cancel",
			Title: "取消执行任务",
			Description: "终止正在运行的命令任务（支持 detach 后台托管任务与普通异步任务），并将取消状态持久化到数据库。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "要取消的任务 jobId（来自 exec 或 list_jobs）",
					},
				},
				"required":             []string{"job_id"},
				"additionalProperties": false,
			},
			Annotations: &mcpToolAnnotations{
				Title:           "取消执行任务",
				ReadOnlyHint:    boolPtr(false),
				DestructiveHint: boolPtr(true),
				IdempotentHint:  boolPtr(true),
				OpenWorldHint:   boolPtr(false),
			},
		},
		{
			Name:  "list_jobs",
			Title: "列出执行任务",
			Description: "列出命令执行任务（含 detach 后台长任务与普通异步任务）。任务元数据存于数据库，不受 30 分钟内存 TTL 和服务端重启影响；跨会话或重启后可用本工具找回在途或已完成的任务。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        "string",
						"description": "按目标服务器 id 筛选；留空则列出当前 Key 有权访问的所有服务器上的任务",
					},
					"running_only": map[string]any{
						"type":        "boolean",
						"description": "true 表示仅列出仍在运行中的任务，默认 false",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "返回条数上限，默认 20，最大 100",
					},
				},
				"additionalProperties": false,
			},
			Annotations: &mcpToolAnnotations{
				Title:           "列出执行任务",
				ReadOnlyHint:    boolPtr(true),
				DestructiveHint: boolPtr(false),
				IdempotentHint:  boolPtr(true),
				OpenWorldHint:   boolPtr(false),
			},
		},
		{
			Name:  "write_file",
			Title: "写入文件",
			Description: "把内容写入指定服务器的文件。覆盖写是原子的（先写临时文件再顶替），不会留下半截文件。" +
				"写配置文件应当用本工具而不是 exec 配 heredoc——shell 转义容易出错，且本工具会把内容原样记入审计。" +
				"内容上限 48KB；更大的文件应当用 exec 在目标机上拉取。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        "string",
						"description": "目标服务器 id，来自 list_servers",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "目标文件的绝对路径",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "文件内容",
					},
					"mode": map[string]any{
						"type":        "string",
						"description": "八进制权限，如 \"644\"、\"600\"，默认 644",
					},
					"append": map[string]any{
						"type":        "boolean",
						"description": "true 表示追加到文件末尾而非覆盖，默认 false",
					},
					"mkdir": map[string]any{
						"type":        "boolean",
						"description": "true 表示自动创建父目录，默认 false",
					},
				},
				"required":             []string{"server_id", "path", "content"},
				"additionalProperties": false,
			},
			Annotations: &mcpToolAnnotations{
				Title:           "写入文件",
				ReadOnlyHint:    boolPtr(false),
				DestructiveHint: boolPtr(true),
				IdempotentHint:  boolPtr(true),
				OpenWorldHint:   boolPtr(false),
			},
		},
	}
}

// mcpCallTool 分发工具调用。
func (s *App) mcpCallTool(r *http.Request, key *apiKey, id json.RawMessage, p mcpCallToolParams) jsonrpcResponse {
	switch p.Name {
	case "list_servers":
		return rpcResult(id, s.toolListServers(key))
	case "get_metrics":
		return rpcResult(id, s.toolGetMetrics(key, p.Arguments))
	case "get_history":
		return rpcResult(id, s.toolGetHistory(key, p.Arguments))
	case "exec":
		return rpcResult(id, s.toolExec(key, p.Arguments))
	case "get_result":
		return rpcResult(id, s.toolGetResult(key, p.Arguments))
	case "cancel":
		return rpcResult(id, s.toolCancel(key, p.Arguments))
	case "list_jobs":
		return rpcResult(id, s.toolListJobs(key, p.Arguments))
	case "write_file":
		return rpcResult(id, s.toolWriteFile(r, key, p.Arguments))
	default:
		return rpcError(id, jrpcInvalidParams, "未知的工具: "+p.Name, nil)
	}
}

func jsonResult(v any) mcpCallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolFailure("结果序列化失败: " + err.Error())
	}
	return mcpCallToolResult{Content: mcpText(string(b)), StructuredContent: v}
}

func requireCap(key *apiKey, cap string) *mcpCallToolResult {
	if !key.has(cap) {
		f := toolFailure(fmt.Sprintf("当前 Key 缺少 %q 能力，无法执行此操作。请让管理员在 moss 后台调整 Key 权限。", cap))
		return &f
	}
	return nil
}

func (s *App) resolveTarget(key *apiKey, serverID string) (string, *mcpCallToolResult) {
	serverID = strings.TrimSpace(serverID)
	if serverID == "" {
		f := toolFailure("server_id 不能为空，请先调用 list_servers 获取")
		return "", &f
	}
	if !key.canAccess(serverID) {
		f := toolFailure("当前 Key 未被授权访问服务器 " + serverID + "。这是权限限制，重试不会成功；可调用 list_servers 查看有权访问的机器。")
		return "", &f
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM servers WHERE id = ?`, serverID).Scan(&n); err != nil || n == 0 {
		f := toolFailure("服务器不存在: " + serverID)
		return "", &f
	}
	return serverID, nil
}

/* ---------- list_servers ---------- */

type mcpServerInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Group        string `json:"group"`
	Online       bool   `json:"online"`
	OS           string `json:"os,omitempty"`
	Arch         string `json:"arch,omitempty"`
	CPUCores     int    `json:"cpuCores,omitempty"`
	MemTotal     uint64 `json:"memTotalBytes,omitempty"`
	DiskGB       uint64 `json:"diskTotalBytes,omitempty"`
	IP           string `json:"ip,omitempty"`
	Note         string `json:"note,omitempty"`
	AgentVersion string `json:"agentVersion,omitempty"`
}

func (s *App) toolListServers(key *apiKey) mcpCallToolResult {
	if f := requireCap(key, capRead); f != nil {
		return *f
	}
	rows, err := s.db.Query(`SELECT id, name, grp, os, arch, cpu_cores, mem_total, disk_total, ip, note, agent_version
	                         FROM servers ORDER BY sort, id`)
	if err != nil {
		return toolFailure("查询服务器列表失败: " + err.Error())
	}
	defer rows.Close()

	list := make([]mcpServerInfo, 0, 16)
	for rows.Next() {
		var it mcpServerInfo
		if err := rows.Scan(&it.ID, &it.Name, &it.Group, &it.OS, &it.Arch,
			&it.CPUCores, &it.MemTotal, &it.DiskGB, &it.IP, &it.Note, &it.AgentVersion); err != nil {
			return toolFailure("读取服务器列表失败: " + err.Error())
		}
		if !key.canAccess(it.ID) {
			continue
		}
		_, _, it.Online = s.hub.Snapshot(it.ID)
		list = append(list, it)
	}
	if err := rows.Err(); err != nil {
		return toolFailure("读取服务器列表失败: " + err.Error())
	}
	return jsonResult(map[string]any{"servers": list, "count": len(list)})
}

/* ---------- get_metrics ---------- */

func (s *App) toolGetMetrics(key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capRead); f != nil {
		return *f
	}
	var args struct {
		ServerID string `json:"server_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolFailure("参数解析失败: " + err.Error())
	}
	serverID, f := s.resolveTarget(key, args.ServerID)
	if f != nil {
		return *f
	}

	stats, uptime, online := s.hub.Snapshot(serverID)
	if !online {
		return toolFailure("服务器 " + serverID + " 当前离线，无法读取实时指标。可用 list_servers 确认状态。")
	}
	return jsonResult(map[string]any{
		"serverId":         serverID,
		"online":           true,
		"cpuPercent":       stats.CPU,
		"memUsedBytes":     stats.MemUsed,
		"swapUsedBytes":    stats.SwapUsed,
		"diskUsedBytes":    stats.DiskUsed,
		"netUpBytesPerS":   stats.NetUp,
		"netDownBytesPerS": stats.NetDown,
		"totalUpBytes":     stats.TotalUp,
		"totalDownBytes":   stats.TotalDown,
		"tcpConnections":   stats.TCP,
		"processes":        stats.Processes,
		"load1":            stats.Load1,
		"load5":            stats.Load5,
		"load15":           stats.Load15,
		"uptimeSeconds":    uptime,
	})
}

/* ---------- get_history ---------- */

type historyMetric struct {
	arg string
	col string
	key string
}

var historyMetricList = []historyMetric{
	{"cpu", "cpu", "cpu"},
	{"mem", "mem", "mem"},
	{"swap", "swap", "swap"},
	{"disk", "disk", "disk"},
	{"load1", "load1", "load1"},
	{"net_up", "net_up", "netUp"},
	{"net_down", "net_down", "netDown"},
	{"tcp", "tcp", "tcp"},
	{"processes", "processes", "processes"},
}

var historyDefaultMetrics = []string{"cpu", "mem", "disk"}

const historyMaxPoints = 120

type historySummary struct {
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Avg   float64 `json:"avg"`
	First float64 `json:"first"`
	Last  float64 `json:"last"`
	Trend string  `json:"trend"`
}

func (s *App) toolGetHistory(key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capRead); f != nil {
		return *f
	}
	var args struct {
		ServerID string   `json:"server_id"`
		Hours    int      `json:"hours"`
		Metrics  []string `json:"metrics"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolFailure("参数解析失败: " + err.Error())
	}
	sel, err := pickHistoryMetrics(args.Metrics)
	if err != nil {
		return toolFailure(err.Error())
	}
	serverID, f := s.resolveTarget(key, args.ServerID)
	if f != nil {
		return *f
	}

	var notes []string
	hours := args.Hours
	if hours < 1 {
		hours = 1
	}
	maxHours := getSettingInt(s.db, keyHistoryDays, 7) * 24
	if hours > maxHours {
		notes = append(notes, fmt.Sprintf("hours 超出历史保留期，已收敛为 %d 小时（history_days=%d 天）。",
			maxHours, maxHours/24))
		hours = maxHours
	}

	untilMs := time.Now().UnixMilli()
	sinceMs := untilMs - int64(hours)*3600*1000
	bucketSec := historyBucketSeconds(int64(hours) * 3600)

	times, series, err := s.queryHistory(serverID, sel, sinceMs, untilMs, bucketSec*1000)
	if err != nil {
		return toolFailure("查询历史数据失败: " + err.Error() + "。这是服务端故障，稍后可重试。")
	}

	keys := make([]string, len(sel))
	seriesOut := make(map[string][]float64, len(sel))
	summary := make(map[string]historySummary, len(sel))
	for i, m := range sel {
		keys[i] = m.key
		seriesOut[m.key] = series[i]
		if len(series[i]) > 0 {
			summary[m.key] = summarize(series[i])
		}
	}
	if len(times) == 0 {
		notes = append(notes, "该时间窗内没有历史数据。可能是机器刚接入、长时间离线，或数据已超出保留期；换更大的 hours 再看一次即可判断，不必重复用相同参数重试。")
	}

	res := map[string]any{
		"serverId":      serverID,
		"hours":         hours,
		"fromTime":      sinceMs / 1000,
		"toTime":        untilMs / 1000,
		"bucketSeconds": bucketSec,
		"points":        len(times),
		"metrics":       keys,
		"times":         times,
		"series":        seriesOut,
		"summary":       summary,
	}
	if len(notes) > 0 {
		res["note"] = strings.Join(notes, " ")
	}
	return jsonResult(res)
}

func pickHistoryMetrics(want []string) ([]historyMetric, error) {
	if len(want) == 0 {
		want = historyDefaultMetrics
	}
	picked := make(map[string]bool, len(want))
	for _, w := range want {
		w = strings.ToLower(strings.TrimSpace(w))
		ok := false
		for _, m := range historyMetricList {
			if m.arg == w {
				picked[m.arg] = true
				ok = true
				break
			}
		}
		if !ok {
			names := make([]string, len(historyMetricList))
			for i, m := range historyMetricList {
				names[i] = m.arg
			}
			return nil, errors.New("不支持的指标 " + strconv.Quote(w) + "，可选：" + strings.Join(names, "、"))
		}
	}
	sel := make([]historyMetric, 0, len(picked))
	for _, m := range historyMetricList {
		if picked[m.arg] {
			sel = append(sel, m)
		}
	}
	return sel, nil
}

func historyBucketSeconds(rangeSec int64) int64 {
	for _, b := range []int64{10, 15, 20, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 21600, 43200, 86400} {
		if b*historyMaxPoints > rangeSec {
			return b
		}
	}
	return rangeSec/historyMaxPoints + 1
}

func (s *App) queryHistory(serverID string, sel []historyMetric, sinceMs, untilMs, bucketMs int64) ([]int64, [][]float64, error) {
	cols := make([]string, len(sel))
	for i, m := range sel {
		cols[i] = "AVG(" + m.col + ")"
	}
	rows, err := s.db.Query(
		`SELECT CAST(AVG(time) AS INTEGER), `+strings.Join(cols, ", ")+
			` FROM history WHERE server_id = ? AND time >= ? AND time <= ?
			  GROUP BY (time - ?) / ? ORDER BY 1`,
		serverID, sinceMs, untilMs, sinceMs, bucketMs)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	times := []int64{}
	series := make([][]float64, len(sel))
	for i := range series {
		series[i] = []float64{}
	}
	vals := make([]float64, len(sel))
	dest := make([]any, len(sel)+1)
	var ts int64
	dest[0] = &ts
	for i := range vals {
		dest[i+1] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, err
		}
		times = append(times, ts/1000)
		for i, v := range vals {
			series[i] = append(series[i], round2(v))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return times, series, nil
}

func summarize(v []float64) historySummary {
	out := historySummary{Min: v[0], Max: v[0], First: v[0], Last: v[len(v)-1]}
	sum := 0.0
	for _, x := range v {
		if x < out.Min {
			out.Min = x
		}
		if x > out.Max {
			out.Max = x
		}
		sum += x
	}
	out.Avg = round2(sum / float64(len(v)))
	out.Trend = trendOf(v)
	return out
}

func trendOf(v []float64) string {
	q := len(v) / 4
	if q < 1 {
		q = 1
	}
	first, last := mean(v[:q]), mean(v[len(v)-q:])
	base := math.Max(math.Abs(first), math.Abs(last))
	if base < 1e-9 || math.Abs(last-first)/base < 0.05 {
		return "flat"
	}
	if last > first {
		return "rising"
	}
	return "falling"
}

func mean(v []float64) float64 {
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

/* ---------- exec ---------- */

func (s *App) toolExec(key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capExec); f != nil {
		return *f
	}
	var args struct {
		ServerID string `json:"server_id"`
		Cmd      string `json:"cmd"`
		Dir      string `json:"dir"`
		Timeout  int    `json:"timeout"`
		Detach   bool   `json:"detach"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolFailure("参数解析失败: " + err.Error())
	}
	if strings.TrimSpace(args.Cmd) == "" {
		return toolFailure("cmd 不能为空")
	}
	serverID, f := s.resolveTarget(key, args.ServerID)
	if f != nil {
		return *f
	}

	task := protocol.ExecTask{
		Cmd:     args.Cmd,
		Dir:     args.Dir,
		Timeout: args.Timeout,
		Detach:  args.Detach,
	}
	normalizeTaskTimeout(&task)

	jobID, err := s.exec.Start(s.hub, serverID, s.callerLabel(key), task)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, errExecBlocked) {
			if out, _, _, ok := s.exec.Result(jobID); ok && out.Error != "" {
				msg = out.Error
			}
			msg += "。这是服务端的安全拦截，改写命令绕过是不允许的；如确有必要请让人工上机操作。"
		}
		return toolFailure(msg)
	}
	res := map[string]any{
		"jobId":   jobID,
		"status":  "running",
		"timeout": task.Timeout,
		"hint":    "用 get_result 并传入此 jobId 取回输出；命令仍在执行时 running 为 true，稍后重试即可。",
	}
	if args.Detach {
		res["detached"] = true
		res["unit"] = sanitizeUnitName(jobID)
		res["hint"] = "任务已通过 systemd-run 在后台托管运行；可用 get_result（支持 offset 增量追读或 tail 查看末尾 N KB）轮询输出，或用 cancel 终止任务。"
	}
	return jsonResult(res)
}

/* ---------- get_result ---------- */

// sliceTextByOffsetTail 对内存/审计中的文本按 offset 或 tailKB 切片，保持与 detach 磁盘切片一致的语义。
func sliceTextByOffsetTail(s string, offset, tailKB int64) (sliced string, actualOffset, nextOffset, totalBytes int64) {
	totalBytes = int64(len(s))
	start := offset
	if tailKB > 0 {
		start = totalBytes - tailKB*1024
	}
	if start < 0 {
		start = 0
	}
	if start > totalBytes {
		start = totalBytes
	}
	return s[start:], start, totalBytes, totalBytes
}

// getResultFromAudit 在内存里查不到 jobID 时回退查数据库（支持 detach 后台任务的实时日志拉取）。
func (s *App) getResultFromAudit(key *apiKey, jobID string, offset, tailKB int64) mcpCallToolResult {
	meta, err := s.exec.lookupAuditJob(jobID)
	if err != nil {
		return toolFailure("找不到任务 " + jobID + "：jobId 有误，或记录已被审计清理策略删除。")
	}
	if _, f := s.resolveTarget(key, meta.ServerID); f != nil {
		return *f
	}

	if meta.Detach {
		return s.getDetachedResult(meta, offset, tailKB)
	}

	if meta.FinishedAt == 0 {
		return toolFailure("任务 " + jobID + " 的结果已随服务端重启丢失，且它当时尚未执行完。" +
			"命令可能仍在目标机上运行过——重跑前请先确认它是否幂等。")
	}
	slicedOut, actualOffset, nextOffset, totalBytes := sliceTextByOffsetTail(meta.Stdout, offset, tailKB)
	res := map[string]any{
		"jobId":      jobID,
		"running":    false,
		"exitCode":   meta.ExitCode,
		"stdout":     slicedOut,
		"stderr":     meta.Stderr,
		"offset":     actualOffset,
		"nextOffset": nextOffset,
		"totalBytes": totalBytes,
		"durationMs": meta.FinishedAt - meta.StartedAt,
		"source":     "audit",
		"sourceNote": "内存结果已随服务端重启丢失，这份取自审计表：输出被截断到 32KB，可能不完整。",
	}
	if meta.Truncated {
		res["truncated"] = true
	}
	if meta.Error != "" {
		res["error"] = meta.Error
	}
	return jsonResult(res)
}

func (s *App) getDetachedResult(meta *auditJobMeta, offset, tailKB int64) mcpCallToolResult {
	if meta.FinishedAt > 0 && meta.Error != "" && meta.LogBytes == 0 && meta.Stdout == "" {
		res := map[string]any{
			"jobId":      meta.JobID,
			"running":    false,
			"detached":   true,
			"exitCode":   meta.ExitCode,
			"error":      meta.Error,
			"stdout":     "",
			"stderr":     "",
			"offset":     int64(0),
			"nextOffset": int64(0),
			"totalBytes": int64(0),
			"durationMs": meta.FinishedAt - meta.StartedAt,
		}
		return mcpCallToolResult{
			Content:           mcpText(mustJSON(res)),
			StructuredContent: res,
			IsError:           true,
		}
	}

	st, err := s.exec.QueryDetached(s.hub, meta.ServerID, meta.JobID, offset, tailKB)
	if err != nil {
		if meta.FinishedAt > 0 {
			slicedOut, actualOffset, nextOffset, totalBytes := sliceTextByOffsetTail(meta.Stdout, offset, tailKB)
			res := map[string]any{
				"jobId":      meta.JobID,
				"running":    false,
				"detached":   true,
				"unit":       meta.Unit,
				"exitCode":   meta.ExitCode,
				"stdout":     slicedOut,
				"stderr":     meta.Stderr,
				"offset":     actualOffset,
				"nextOffset": nextOffset,
				"totalBytes": totalBytes,
				"durationMs": meta.FinishedAt - meta.StartedAt,
				"source":     "audit",
				"sourceNote": "目标机当前离线，无法读取完整磁盘日志，这份取自数据库审计摘要。",
			}
			if meta.Error != "" {
				res["error"] = meta.Error
			}
			return jsonResult(res)
		}
		return toolFailure(fmt.Sprintf("读取后台任务 %s 状态失败: %v（后台任务仍在目标机 systemd 中独立运行，待 agent 在线后可继续查询）", meta.JobID, err))
	}

	if !st.Found {
		if meta.FinishedAt > 0 {
			slicedOut, actualOffset, nextOffset, totalBytes := sliceTextByOffsetTail(meta.Stdout, offset, tailKB)
			res := map[string]any{
				"jobId":      meta.JobID,
				"running":    false,
				"detached":   true,
				"unit":       meta.Unit,
				"exitCode":   meta.ExitCode,
				"stdout":     slicedOut,
				"stderr":     meta.Stderr,
				"offset":     actualOffset,
				"nextOffset": nextOffset,
				"totalBytes": totalBytes,
				"durationMs": meta.FinishedAt - meta.StartedAt,
				"source":     "audit",
			}
			if meta.Error != "" {
				res["error"] = meta.Error
			}
			return jsonResult(res)
		}
		return toolFailure("目标机上找不到后台任务 " + meta.JobID + " 的磁盘日志记录。")
	}

	endMs := time.Now().UnixMilli()
	if !st.Running && meta.FinishedAt > 0 {
		endMs = meta.FinishedAt
	}
	res := map[string]any{
		"jobId":      meta.JobID,
		"running":    st.Running,
		"detached":   true,
		"unit":       st.Unit,
		"exitCode":   st.ExitCode,
		"stdout":     string(st.Data),
		"stderr":     "",
		"offset":     st.Offset,
		"nextOffset": st.NextOffset,
		"totalBytes": st.TotalBytes,
		"hasMore":    st.NextOffset < st.TotalBytes,
		"durationMs": endMs - meta.StartedAt,
	}
	if st.Canceled {
		res["canceled"] = true
	}
	if st.Running {
		res["hint"] = "后台任务仍在运行；下次调用 get_result 传入 offset=nextOffset 可继续追读新增日志，或传 tail 查看末尾 N KB。"
		return jsonResult(res)
	}
	if st.Error != "" {
		res["error"] = st.Error
		return mcpCallToolResult{
			Content:           mcpText(mustJSON(res)),
			StructuredContent: res,
			IsError:           true,
		}
	}
	if st.ExitCode != 0 {
		return mcpCallToolResult{
			Content:           mcpText(mustJSON(res)),
			StructuredContent: res,
			IsError:           true,
		}
	}
	return jsonResult(res)
}

func (s *App) toolGetResult(key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capExec); f != nil {
		return *f
	}
	var args struct {
		JobID  string `json:"job_id"`
		Offset int64  `json:"offset"`
		Tail   int64  `json:"tail"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolFailure("参数解析失败: " + err.Error())
	}
	if strings.TrimSpace(args.JobID) == "" {
		return toolFailure("job_id 不能为空")
	}

	if meta, err := s.exec.lookupAuditJob(args.JobID); err == nil && meta.Detach {
		if _, f := s.resolveTarget(key, meta.ServerID); f != nil {
			return *f
		}
		return s.getDetachedResult(meta, args.Offset, args.Tail)
	}

	out, serverID, running, found := s.exec.Result(args.JobID)
	if !found {
		return s.getResultFromAudit(key, args.JobID, args.Offset, args.Tail)
	}
	if _, f := s.resolveTarget(key, serverID); f != nil {
		return *f
	}
	slicedOut, actualOffset, nextOffset, totalBytes := sliceTextByOffsetTail(out.Stdout, args.Offset, args.Tail)
	if running {
		res := map[string]any{
			"jobId":      args.JobID,
			"running":    true,
			"stdout":     slicedOut,
			"offset":     actualOffset,
			"nextOffset": nextOffset,
			"totalBytes": totalBytes,
			"hint":       "命令仍在执行，稍后再次调用本工具（可传入 offset=nextOffset 增量读取）。",
		}
		return jsonResult(res)
	}
	res := map[string]any{
		"jobId":      out.JobID,
		"running":    false,
		"exitCode":   out.ExitCode,
		"stdout":     slicedOut,
		"stderr":     out.Stderr,
		"offset":     actualOffset,
		"nextOffset": nextOffset,
		"totalBytes": totalBytes,
		"durationMs": out.DurationMs,
	}
	if out.Truncated {
		res["truncated"] = true
		res["truncatedNote"] = "输出触顶 1MB 已截断，如需完整内容请改用 detach=true 后台托管模式或缩小输出范围。"
	}
	if out.Error != "" {
		res["error"] = out.Error
		return mcpCallToolResult{
			Content:           mcpText(mustJSON(res)),
			StructuredContent: res,
			IsError:           true,
		}
	}
	if out.ExitCode != 0 {
		return mcpCallToolResult{
			Content:           mcpText(mustJSON(res)),
			StructuredContent: res,
			IsError:           true,
		}
	}
	return jsonResult(res)
}

/* ---------- cancel ---------- */

func (s *App) toolCancel(key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capExec); f != nil {
		return *f
	}
	var args struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolFailure("参数解析失败: " + err.Error())
	}
	args.JobID = strings.TrimSpace(args.JobID)
	if args.JobID == "" {
		return toolFailure("job_id 不能为空")
	}

	meta, err := s.exec.lookupAuditJob(args.JobID)
	if err != nil {
		if out, serverID, running, found := s.exec.Result(args.JobID); found {
			if _, f := s.resolveTarget(key, serverID); f != nil {
				return *f
			}
			if !running {
				return jsonResult(map[string]any{
					"jobId":    args.JobID,
					"serverId": serverID,
					"running":  false,
					"canceled": false,
					"status":   "already_finished",
					"exitCode": out.ExitCode,
					"note":     "任务已执行结束，无需取消。",
				})
			}
		}
		return toolFailure("找不到任务 " + args.JobID + "：jobId 有误，或记录已被清理。")
	}

	if _, f := s.resolveTarget(key, meta.ServerID); f != nil {
		return *f
	}

	alreadyFinished, cancelErr := s.exec.Cancel(s.hub, meta)
	if cancelErr != nil {
		return toolFailure("取消任务失败: " + cancelErr.Error())
	}
	if alreadyFinished {
		if latest, err := s.exec.lookupAuditJob(args.JobID); err == nil {
			meta = latest
		}
		return jsonResult(map[string]any{
			"jobId":    meta.JobID,
			"serverId": meta.ServerID,
			"running":  false,
			"canceled": meta.Canceled,
			"status":   "already_finished",
			"exitCode": meta.ExitCode,
			"note":     "任务此前已执行结束，无需取消。",
		})
	}
	return jsonResult(map[string]any{
		"jobId":    meta.JobID,
		"serverId": meta.ServerID,
		"detached": meta.Detach,
		"unit":     meta.Unit,
		"running":  false,
		"canceled": true,
		"status":   "canceled",
	})
}

/* ---------- list_jobs ---------- */

type mcpJobItem struct {
	JobID      string `json:"jobId"`
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName,omitempty"`
	Cmd        string `json:"cmd"`
	Dir        string `json:"dir,omitempty"`
	Detach     bool   `json:"detach,omitempty"`
	Unit       string `json:"unit,omitempty"`
	Timeout    int    `json:"timeout"`
	Running    bool   `json:"running"`
	Status     string `json:"status"`
	Canceled   bool   `json:"canceled,omitempty"`
	ExitCode   int    `json:"exitCode"`
	Error      string `json:"error,omitempty"`
	LogBytes   int64  `json:"logBytes,omitempty"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

func (s *App) toolListJobs(key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capExec); f != nil {
		return *f
	}
	var args struct {
		ServerID    string `json:"server_id"`
		RunningOnly bool   `json:"running_only"`
		Limit       int    `json:"limit"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return toolFailure("参数解析失败: " + err.Error())
		}
	}
	args.ServerID = strings.TrimSpace(args.ServerID)
	if args.ServerID != "" {
		sid, f := s.resolveTarget(key, args.ServerID)
		if f != nil {
			return *f
		}
		args.ServerID = sid
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	query := `
		SELECT a.job_id, a.server_id, COALESCE(s.name, ''), a.cmd, a.dir, a.timeout,
		       a.detach, a.unit, a.started_at, a.finished_at, a.exit_code, a.error,
		       a.canceled, a.log_bytes
		  FROM exec_audit a LEFT JOIN servers s ON s.id = a.server_id
		 WHERE a.cmd NOT LIKE '[write_file] %'`
	qArgs := []any{}
	if args.ServerID != "" {
		query += ` AND a.server_id = ?`
		qArgs = append(qArgs, args.ServerID)
	}
	query += ` ORDER BY a.started_at DESC, a.id DESC LIMIT 200`

	rows, err := s.db.Query(query, qArgs...)
	if err != nil {
		return toolFailure("查询任务列表失败: " + err.Error())
	}
	var candidates []mcpJobItem
	for rows.Next() {
		var (
			it               mcpJobItem
			detach, canceled int
		)
		if err := rows.Scan(
			&it.JobID, &it.ServerID, &it.ServerName, &it.Cmd, &it.Dir, &it.Timeout,
			&detach, &it.Unit, &it.StartedAt, &it.FinishedAt, &it.ExitCode, &it.Error,
			&canceled, &it.LogBytes,
		); err != nil {
			rows.Close()
			return toolFailure("读取任务列表失败: " + err.Error())
		}
		if !key.canAccess(it.ServerID) {
			continue
		}
		it.Detach = detach == 1
		it.Canceled = canceled == 1
		candidates = append(candidates, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return toolFailure("读取任务列表失败: " + err.Error())
	}
	rows.Close()

	nowMs := time.Now().UnixMilli()
	list := make([]mcpJobItem, 0, limit)
	for _, it := range candidates {
		if it.FinishedAt == 0 {
			if it.Detach {
				reqID := "l_" + randString(16)
				if st, err := s.exec.requestJobStatus(s.hub, it.ServerID, protocol.ServerMsg{
					Type: "job_query",
					JobQuery: &protocol.JobQuery{
						ReqID: reqID,
						JobID: it.JobID,
						Limit: -1,
					},
				}, reqID, 2*time.Second); err == nil && st.Found {
					s.exec.syncDetachedAudit(it.JobID, st)
					it.LogBytes = st.TotalBytes
					if st.Unit != "" {
						it.Unit = st.Unit
					}
					if st.Running {
						it.Running = true
						it.Status = "running"
						it.DurationMs = nowMs - it.StartedAt
					} else {
						it.Running = false
						it.FinishedAt = nowMs
						it.ExitCode = st.ExitCode
						it.Error = st.Error
						it.Canceled = st.Canceled
						it.DurationMs = it.FinishedAt - it.StartedAt
						it.Status = classifyFinishedStatus(it.Canceled, it.ExitCode, it.Error)
					}
				} else {
					it.Running = true
					it.Status = "running"
					it.DurationMs = nowMs - it.StartedAt
				}
			} else if s.exec.isRunningInMemory(it.JobID) {
				it.Running = true
				it.Status = "running"
				it.DurationMs = nowMs - it.StartedAt
			} else {
				it.Running = false
				it.Status = "lost"
				if it.Error == "" {
					it.Error = "服务端曾重启，非 detach 任务连接已断开"
				}
			}
		} else {
			it.Running = false
			it.DurationMs = it.FinishedAt - it.StartedAt
			it.Status = classifyFinishedStatus(it.Canceled, it.ExitCode, it.Error)
		}

		if args.RunningOnly && !it.Running {
			continue
		}
		list = append(list, it)
		if len(list) >= limit {
			break
		}
	}

	return jsonResult(map[string]any{
		"jobs":  list,
		"count": len(list),
	})
}

func classifyFinishedStatus(canceled bool, exitCode int, errMsg string) string {
	if canceled || errMsg == "任务已取消" {
		return "canceled"
	}
	if strings.HasPrefix(errMsg, execBlockedPrefix) {
		return "blocked"
	}
	if errMsg != "" || exitCode != 0 {
		return "failed"
	}
	return "succeeded"
}

/* ---------- write_file ---------- */

func (s *App) toolWriteFile(r *http.Request, key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capWrite); f != nil {
		return *f
	}
	var args struct {
		ServerID string `json:"server_id"`
		Path     string `json:"path"`
		Content  string `json:"content"`
		Mode     string `json:"mode"`
		Append   bool   `json:"append"`
		Mkdir    bool   `json:"mkdir"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolFailure("参数解析失败: " + err.Error())
	}
	if strings.TrimSpace(args.Path) == "" {
		return toolFailure("path 不能为空")
	}
	if !strings.HasPrefix(args.Path, "/") && !isWindowsAbs(args.Path) {
		return toolFailure("path 必须是绝对路径")
	}
	if len(args.Content) > protocol.WriteSizeCap {
		return toolFailure(fmt.Sprintf("内容 %d 字节超过上限 %d，请改用 exec 在目标机上拉取大文件",
			len(args.Content), protocol.WriteSizeCap))
	}
	mode, err := parseFileMode(args.Mode)
	if err != nil {
		return toolFailure(err.Error())
	}
	serverID, f := s.resolveTarget(key, args.ServerID)
	if f != nil {
		return *f
	}

	out, err := s.exec.SubmitWrite(r.Context(), s.hub, serverID, s.callerLabel(key), protocol.WriteTask{
		Path:   args.Path,
		Data:   []byte(args.Content),
		Mode:   mode,
		Append: args.Append,
		Mkdir:  args.Mkdir,
	})
	if err != nil || out.Error != "" {
		msg := out.Error
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if errors.Is(err, errExecBlocked) {
			msg += "。这是服务端的安全拦截，请让人工上机操作。"
		}
		return toolFailure("写入失败: " + msg)
	}
	return jsonResult(map[string]any{
		"serverId": serverID,
		"path":     args.Path,
		"bytes":    len(args.Content),
		"mode":     fmt.Sprintf("%o", mode),
		"appended": args.Append,
	})
}

func isWindowsAbs(p string) bool {
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

func parseFileMode(s string) (uint32, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	var mode uint32
	if _, err := fmt.Sscanf(s, "%o", &mode); err != nil || mode > 0o7777 {
		return 0, errors.New("mode 必须是合法的八进制权限，如 \"644\"")
	}
	return mode, nil
}

func (s *App) callerLabel(key *apiKey) string {
	return fmt.Sprintf("key:%d(%s)", key.ID, key.Name)
}

func mustJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
