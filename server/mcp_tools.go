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
//
// 说明文字是写给模型看的，不是写给人看的：要讲清楚什么时候用、
// 返回什么、有什么坑，模型才不会拿错工具或反复试错。
func mcpTools() []mcpTool {
	return []mcpTool{
		{
			Name:        "list_servers",
			Title:       "列出服务器",
			Description: "列出所有服务器及其在线状态、系统信息。返回的 id 是后续所有工具的目标标识，任何操作前都应先调用本工具确认 id。",
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
						"description": "硬超时（秒），默认 60，最大 1800。到点后 agent 强杀整个进程树",
					},
				},
				"required":             []string{"server_id", "cmd"},
				"additionalProperties": false,
			},
			Annotations: &mcpToolAnnotations{
				Title: "执行命令",
				// 任意命令，可能造成破坏且不幂等，如实标注让客户端有机会提示用户。
				ReadOnlyHint:    boolPtr(false),
				DestructiveHint: boolPtr(true),
				IdempotentHint:  boolPtr(false),
				OpenWorldHint:   boolPtr(true),
			},
		},
		{
			Name:  "get_result",
			Title: "取回执行结果",
			Description: "用 exec 返回的 jobId 取回执行结果。running 为 true 表示仍在执行，稍后重试。" +
				"结果保留 30 分钟。输出超过 1MB 会被截断并标注。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "exec 返回的 jobId",
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
				Title: "写入文件",
				// 覆盖既有文件属于破坏性操作；同样的内容重复写结果一致，故幂等。
				ReadOnlyHint:    boolPtr(false),
				DestructiveHint: boolPtr(true),
				IdempotentHint:  boolPtr(true),
				OpenWorldHint:   boolPtr(false),
			},
		},
	}
}

// mcpCallTool 分发工具调用。
//
// 返回值一律是 jsonrpcResponse 包着 mcpCallToolResult：
// 工具层面的失败（越权、拦截、掉线、非零退出码）走 isError=true 让模型看见并自我纠正，
// 只有「工具不存在」才用 JSON-RPC error——这是规范的明确要求。
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
	case "write_file":
		return rpcResult(id, s.toolWriteFile(r, key, p.Arguments))
	default:
		return rpcError(id, jrpcInvalidParams, "未知的工具: "+p.Name, nil)
	}
}

// jsonResult 把结构化数据同时放进 structuredContent 与 text 块。
// 规范建议：返回结构化内容时也应把序列化后的 JSON 放进一个 text 块，
// 以兼容尚未支持 structuredContent 的客户端。
func jsonResult(v any) mcpCallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolFailure("结果序列化失败: " + err.Error())
	}
	return mcpCallToolResult{Content: mcpText(string(b)), StructuredContent: v}
}

// requireCap 校验能力集（闸 1 的前半）。
func requireCap(key *apiKey, cap string) *mcpCallToolResult {
	if !key.has(cap) {
		f := toolFailure(fmt.Sprintf("当前 Key 缺少 %q 能力，无法执行此操作。请让管理员在 moss 后台调整 Key 权限。", cap))
		return &f
	}
	return nil
}

// resolveTarget 校验机器作用域（闸 1 的后半）并确认服务器存在。
func (s *App) resolveTarget(key *apiKey, serverID string) (string, *mcpCallToolResult) {
	serverID = strings.TrimSpace(serverID)
	if serverID == "" {
		f := toolFailure("server_id 不能为空，请先调用 list_servers 获取")
		return "", &f
	}
	if !key.canAccess(serverID) {
		// 措辞明确告诉模型这是权限问题而非 ID 写错，避免它反复重试。
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
	ID       string `json:"id"`
	Name     string `json:"name"`
	Group    string `json:"group"`
	Online   bool   `json:"online"`
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
	CPUCores int    `json:"cpuCores,omitempty"`
	MemTotal uint64 `json:"memTotalBytes,omitempty"`
	DiskGB   uint64 `json:"diskTotalBytes,omitempty"`
	IP       string `json:"ip,omitempty"`
	Note     string `json:"note,omitempty"`
}

func (s *App) toolListServers(key *apiKey) mcpCallToolResult {
	if f := requireCap(key, capRead); f != nil {
		return *f
	}
	rows, err := s.db.Query(`SELECT id, name, grp, os, arch, cpu_cores, mem_total, disk_total, ip, note
	                         FROM servers ORDER BY sort, id`)
	if err != nil {
		return toolFailure("查询服务器列表失败: " + err.Error())
	}
	defer rows.Close()

	list := make([]mcpServerInfo, 0, 16)
	for rows.Next() {
		var it mcpServerInfo
		if err := rows.Scan(&it.ID, &it.Name, &it.Group, &it.OS, &it.Arch,
			&it.CPUCores, &it.MemTotal, &it.DiskGB, &it.IP, &it.Note); err != nil {
			return toolFailure("读取服务器列表失败: " + err.Error())
		}
		// 只返回这把 Key 有权访问的机器：让模型看见它碰不到的机器毫无意义，
		// 只会诱导它去尝试越权操作。
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

// historyMetric 把一个指标的三种写法钉在一起：入参名沿用数据库列的 snake_case
// （模型看 schema 就能猜对），输出键统一 camelCase 以对齐项目其余 JSON。
type historyMetric struct {
	arg string
	col string
	key string
}

// 顺序即输出顺序，也是校验入参用的白名单——col 直接拼进 SQL，
// 只能取自这张表，绝不能让入参字符串落到 SQL 里。
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

// 默认只给这三条：模型要的是「哪台机器什么时候开始变坏」，
// 一次把九条曲线全塞回去只会把它真正需要的上下文挤掉。
var historyDefaultMetrics = []string{"cpu", "mem", "disk"}

// historyMaxPoints 是单条曲线的返回点数上限。
// 采样间隔默认 10 秒，一小时就是 360 点、一天 8640 点，不降采样必然撑爆模型上下文。
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
	// 保留期之外的数据早被 cleanupLoop 删了，放行只会让模型误以为「那段真的没事」。
	// 收敛而不是报错：它要的是曲线，给到手里能拿到的最长一段更有用。
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
		// 空窗口不是错误：机器刚接入、长期离线、或数据已过保留期都会走到这里，
		// 报 isError 会诱导模型反复重试同一个必然为空的查询。
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

// pickHistoryMetrics 把入参指标名解析成白名单条目，顺序按 historyMetricList 归一化。
// 未知指标直接报错而不是静默忽略：静默会让模型以为自己拿到了那条曲线。
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

// historyBucketSeconds 选出能把点数压进上限的最小「整」粒度。
//
// 取整值（30s/60s/5min…）而不是 跨度÷120 的余数粒度，是为了让模型一眼看懂分辨率。
// 判据用严格大于：桶宽恰好等于 跨度÷120 时，落在区间末端的那一行会算出第 120 个桶号，
// 点数就会变成 121 而破掉上限。
func historyBucketSeconds(rangeSec int64) int64 {
	for _, b := range []int64{10, 15, 20, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 21600, 43200, 86400} {
		if b*historyMaxPoints > rangeSec {
			return b
		}
	}
	return rangeSec/historyMaxPoints + 1 // 跨度超过 120 天时的兜底，同样保证严格大于
}

// queryHistory 在 SQL 里按等距时间桶聚合，避免把上万原始行捞进内存再算。
// 桶号以 sinceMs 为原点而非 epoch：epoch 对齐会让首尾各多出半个桶，点数不可控。
func (s *App) queryHistory(serverID string, sel []historyMetric, sinceMs, untilMs, bucketMs int64) ([]int64, [][]float64, error) {
	cols := make([]string, len(sel))
	for i, m := range sel {
		cols[i] = "AVG(" + m.col + ")"
	}
	// time <= untilMs 不能省：agent 时钟快了会写入未来时间戳，那种行的桶号会越界，
	// 一行脏数据就能把返回点数顶破上限。
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

// summarize 出摘要。模型多数时候看完摘要就能下判断，逐点读曲线是浪费 token。
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

// trendOf 用首尾各 1/4 段的均值比较判断走向。
// 取段均值而不是首尾两个点：单点受毛刺影响太大，一次采样抖动就能翻转结论。
// 阈值取相对值而非绝对值：绝对差 5 对百分比是明显变化，对字节每秒则毫无意义。
// 基准取首尾两段中较大的一个，避免起点为 0（机器刚起来）时除零。
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

	jobID, err := s.exec.Start(s.hub, serverID, s.callerLabel(key), protocol.ExecTask{
		Cmd:     args.Cmd,
		Dir:     args.Dir,
		Timeout: args.Timeout,
	})
	if err != nil {
		// 拦截与掉线都在这里落地。带上 jobId：调用方仍可用 get_result 看到完整原因。
		msg := err.Error()
		if errors.Is(err, errExecBlocked) {
			if out, _, ok := s.exec.Result(jobID); ok && out.Error != "" {
				msg = out.Error
			}
			msg += "。这是服务端的安全拦截，改写命令绕过是不允许的；如确有必要请让人工上机操作。"
		}
		return toolFailure(msg)
	}
	return jsonResult(map[string]any{
		"jobId":  jobID,
		"status": "running",
		"hint":   "用 get_result 并传入此 jobId 取回输出；命令仍在执行时 running 为 true，稍后重试即可。",
	})
}

/* ---------- get_result ---------- */

func (s *App) toolGetResult(key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capExec); f != nil {
		return *f
	}
	var args struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolFailure("参数解析失败: " + err.Error())
	}
	if strings.TrimSpace(args.JobID) == "" {
		return toolFailure("job_id 不能为空")
	}

	out, running, found := s.exec.Result(args.JobID)
	if !found {
		return toolFailure("找不到任务 " + args.JobID + "：jobId 有误，或结果已超过 30 分钟保留期。")
	}
	if running {
		return jsonResult(map[string]any{
			"jobId":   args.JobID,
			"running": true,
			"hint":    "命令仍在执行，稍后再次调用本工具。",
		})
	}
	res := map[string]any{
		"jobId":      out.JobID,
		"running":    false,
		"exitCode":   out.ExitCode,
		"stdout":     out.Stdout,
		"stderr":     out.Stderr,
		"durationMs": out.DurationMs,
	}
	if out.Truncated {
		res["truncated"] = true
		res["truncatedNote"] = "输出触顶 1MB 已截断，如需完整内容请缩小命令的输出范围后重跑。"
	}
	if out.Error != "" {
		res["error"] = out.Error
		return mcpCallToolResult{
			Content:           mcpText(mustJSON(res)),
			StructuredContent: res,
			IsError:           true,
		}
	}
	// 退出码非零不算工具故障，但要让模型明确看到失败，否则它会以为命令成功了。
	if out.ExitCode != 0 {
		return mcpCallToolResult{
			Content:           mcpText(mustJSON(res)),
			StructuredContent: res,
			IsError:           true,
		}
	}
	return jsonResult(res)
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
	// 路径拦截不在此处判断——交给 SubmitWrite，那里能一并落审计并推送告警。
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
			// 明确告诉模型这是安全拦截、重试无用，否则它会不断变换写法尝试绕过
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

// isWindowsAbs 判断形如 C:\ 的 Windows 绝对路径。
func isWindowsAbs(p string) bool {
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// parseFileMode 解析八进制权限字符串。
func parseFileMode(s string) (uint32, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil // 交给 agent 用默认 0644
	}
	var mode uint32
	if _, err := fmt.Sscanf(s, "%o", &mode); err != nil || mode > 0o7777 {
		return 0, errors.New("mode 必须是合法的八进制权限，如 \"644\"")
	}
	return mode, nil
}

// callerLabel 生成审计里的调用方标识。
// 带上 Key 名与 ID：同一把 Key 可能被改名，ID 才是稳定锚点。
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
