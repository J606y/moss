// MCP 工具集：AI 能对集群做的事（工具定义、分发与基础监控/单机命令执行）。
package main

import (
	"database/sql"
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
			Name:  "list_servers",
			Title: "列出服务器",
			Description: "列出所有服务器及其在线状态、固定 UUID、系统信息与 Agent 版本。返回的 id（或固定的 uuid）均可作为后续所有工具的 server_id 目标标识。" +
				"agentVersion 是该机 agent 的版本：exec / write_file / read_file 需要 2.0.0 及以上，也可调用 upgrade_agent 核对版本或触发受控升级。",
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
						"description": "目标服务器 id 或固定 uuid，来自 list_servers",
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
						"description": "目标服务器 id 或固定 uuid，来自 list_servers",
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
						"description": "目标服务器 id 或固定 uuid，来自 list_servers",
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
			Name:  "batch_exec",
			Title: "批量并发执行命令",
			Description: "在多台（或全部在线）服务器上并发执行同一条 shell 命令，并在服务端统一等待、聚合各节点结果后一次性返回。" +
				"适合全集群巡检、CVE 排查与版本核对，省去逐台调 exec 再逐个轮询 get_result 的往返开销。" +
				"命令退出码（exitCode）与通道异常已解耦：单个节点离线或命令返回非零退出码不会中断其余节点的结果汇聚。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd": map[string]any{
						"type":        "string",
						"description": "要在目标节点上并发执行的 shell 命令",
					},
					"server_ids": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "目标服务器 id（或 uuid）列表；留空则自动选中当前 Key 有权访问的所有在线服务器",
					},
					"group": map[string]any{
						"type":        "string",
						"description": "按服务器分组名筛选（如 \"CDN\"、\"PROXY\"、\"默认\"）；留空表示不限分组",
					},
					"dir": map[string]any{
						"type":        "string",
						"description": "工作目录，留空则用各目标机默认值",
					},
					"timeout": map[string]any{
						"type":        "integer",
						"description": "单节点硬超时（秒），默认 60，最大 1800",
					},
					"include_offline": map[string]any{
						"type":        "boolean",
						"description": "未指定 server_ids 时，是否在返回结果中一并列出当前离线的节点（status 为 \"offline\"），默认 false",
					},
				},
				"required":             []string{"cmd"},
				"additionalProperties": false,
			},
			Annotations: &mcpToolAnnotations{
				Title:           "批量并发执行命令",
				ReadOnlyHint:    boolPtr(false),
				DestructiveHint: boolPtr(true),
				IdempotentHint:  boolPtr(false),
				OpenWorldHint:   boolPtr(true),
			},
		},
		{
			Name:  "get_result",
			Title: "取回执行结果",
			Description: "用 exec 返回的 jobId 取回执行结果与日志。统一返回 status 状态机字段（\"running\" / \"completed\" / \"timed_out\" / \"canceled\" / \"disconnected\" / \"blocked\" / \"offline\" / \"lost\"）与 running 布尔值，" +
				"并同时提供 stdout、stderr 与合并后的 output 字段（content JSON 文本与 structuredContent 完全一致）。" +
				"通道异常与命令退出码解耦：命令正常执行完毕（无论 exitCode 是否为 0）均以 isError=false 返回，仅当发生超时、断连、拦截等通道或调度异常时才置 isError=true。" +
				"支持 offset + limit（按字节偏移量分页读取输出，配合返回的 nextOffset 顺序翻页）和 tail（直接读取末尾 N KB）。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"job_id": map[string]any{
						"type":        "string",
						"description": "exec 返回的 jobId",
					},
					"offset": map[string]any{
						"type":        "integer",
						"description": "从指定字节偏移量开始读取日志（默认 0）。传入上次 get_result 返回的 nextOffset 可分页或增量追读",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "本次分页读取的最大字节数（默认 65536 即 64KB，最大 262144 即 256KB）",
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
			Name:        "cancel",
			Title:       "取消执行任务",
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
			Name:        "list_jobs",
			Title:       "列出执行任务",
			Description: "列出命令执行任务（含 detach 后台长任务与普通异步任务）。任务元数据存于数据库，不受 30 分钟内存 TTL 和服务端重启影响；跨会话或重启后可用本工具找回在途或已完成的任务。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        "string",
						"description": "按目标服务器 id（或 uuid）筛选；留空则列出当前 Key 有权访问的所有服务器上的任务",
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
			Name:  "read_file",
			Title: "分块读取/下载文件",
			Description: "从指定服务器按字节偏移量（offset + length）原生分块读取或下载文件，支持文本（utf8）与二进制（base64）模式，并返回 nextOffset、totalBytes、eof 与分块 sha256。" +
				"拉取编译产物（如内核 deb 包、归档文件）或大日志时，可直接用 offset=nextOffset 循环分块拉取，无需再用 dd + base64 包装脚本。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        "string",
						"description": "目标服务器 id 或固定 uuid，来自 list_servers",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "目标文件的绝对路径",
					},
					"offset": map[string]any{
						"type":        "integer",
						"description": "读取起始字节偏移量（默认 0）。配合返回的 nextOffset 可顺序分块下载大文件",
					},
					"length": map[string]any{
						"type":        "integer",
						"description": "本次读取的字节数（默认 65536 即 64KB，最大 262144 即 256KB）",
					},
					"encoding": map[string]any{
						"type":        "string",
						"enum":        []string{"auto", "utf8", "base64"},
						"description": "返回内容的编码方式：auto（默认，合法文本走 utf8，二进制自动走 base64）、utf8 或 base64",
					},
				},
				"required":             []string{"server_id", "path"},
				"additionalProperties": false,
			},
			Annotations: &mcpToolAnnotations{
				Title:           "分块读取/下载文件",
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
						"description": "目标服务器 id 或固定 uuid，来自 list_servers",
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
		{
			Name:  "upgrade_agent",
			Title: "Agent 版本查询与受控升级",
			Description: "查询指定服务器 moss-agent 的当前版本、服务端目标版本、可升级性与在途升级进度（action=\"check\" 或 \"status\"），" +
				"或触发带 SHA256 校验与 60 秒失联自动回滚守护的受控自升级（action=\"upgrade\"，仅升级至当前服务端官方版本并记入审计）。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"server_id": map[string]any{
						"type":        "string",
						"description": "目标服务器 id 或固定 uuid，来自 list_servers",
					},
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"check", "status", "upgrade"},
						"description": "操作类型：check / status（默认，只读查询版本与升级状态）或 upgrade（下发受控自升级指令）",
					},
				},
				"required":             []string{"server_id"},
				"additionalProperties": false,
			},
			Annotations: &mcpToolAnnotations{
				Title:           "Agent 版本查询与受控升级",
				ReadOnlyHint:    boolPtr(false),
				DestructiveHint: boolPtr(false),
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
	case "batch_exec":
		return rpcResult(id, s.toolBatchExec(r, key, p.Arguments))
	case "get_result":
		return rpcResult(id, s.toolGetResult(key, p.Arguments))
	case "cancel":
		return rpcResult(id, s.toolCancel(key, p.Arguments))
	case "list_jobs":
		return rpcResult(id, s.toolListJobs(key, p.Arguments))
	case "read_file":
		return rpcResult(id, s.toolReadFile(key, p.Arguments))
	case "write_file":
		return rpcResult(id, s.toolWriteFile(r, key, p.Arguments))
	case "upgrade_agent":
		return rpcResult(id, s.toolUpgradeAgent(key, p.Arguments))
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

// resolveTarget 校验机器作用域并确认服务器存在（支持传入 id 或节点固定 uuid）。
func (s *App) resolveTarget(key *apiKey, serverID string) (string, *mcpCallToolResult) {
	serverID = strings.TrimSpace(serverID)
	if serverID == "" {
		f := toolFailure("server_id 不能为空，请先调用 list_servers 获取")
		return "", &f
	}
	// 先尝试按 id 或固定 uuid 解析出规范的 server_id
	var canonicalID string
	err := s.db.QueryRow(
		`SELECT id FROM servers WHERE id = ? OR (uuid != '' AND LOWER(uuid) = LOWER(?)) ORDER BY CASE WHEN id = ? THEN 0 ELSE 1 END LIMIT 1`,
		serverID, serverID, serverID,
	).Scan(&canonicalID)
	if errors.Is(err, sql.ErrNoRows) || canonicalID == "" {
		// 若库里不存在，但当前 Key 本身有白名单限制且传入的 ID 不在白名单内，优先报权限错误（与既有单测保持一致）
		if !key.canAccess(serverID) {
			// 措辞明确告诉模型这是权限问题而非 ID 写错，避免它反复重试。
			f := toolFailure("当前 Key 未被授权访问服务器 " + serverID + "。这是权限限制，重试不会成功；可调用 list_servers 查看有权访问的机器。")
			return "", &f
		}
		f := toolFailure("服务器不存在: " + serverID)
		return "", &f
	}
	if !key.canAccess(canonicalID) && !key.canAccess(serverID) {
		f := toolFailure("当前 Key 未被授权访问服务器 " + serverID + "。这是权限限制，重试不会成功；可调用 list_servers 查看有权访问的机器。")
		return "", &f
	}
	return canonicalID, nil
}

/* ---------- list_servers ---------- */

type mcpServerInfo struct {
	ID           string `json:"id"`
	UUID         string `json:"uuid,omitempty"`
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
	rows, err := s.db.Query(`SELECT id, uuid, name, grp, os, arch, cpu_cores, mem_total, disk_total, ip, note, agent_version
	                         FROM servers ORDER BY sort, id`)
	if err != nil {
		return toolFailure("查询服务器列表失败: " + err.Error())
	}
	defer rows.Close()

	list := make([]mcpServerInfo, 0, 16)
	for rows.Next() {
		var it mcpServerInfo
		if err := rows.Scan(&it.ID, &it.UUID, &it.Name, &it.Group, &it.OS, &it.Arch,
			&it.CPUCores, &it.MemTotal, &it.DiskGB, &it.IP, &it.Note, &it.AgentVersion); err != nil {
			return toolFailure("读取服务器列表失败: " + err.Error())
		}
		// 只返回这把 Key 有权访问的机器：让模型看见它碰不到的机器毫无意义，
		// 只会诱导它去尝试越权操作。
		if !key.canAccess(it.ID) && (it.UUID == "" || !key.canAccess(it.UUID)) {
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

/* ---------- 状态机与输出聚合辅助函数 ---------- */

func combineOutput(stdout, stderr string) string {
	if stdout != "" && stderr != "" {
		if strings.HasSuffix(stdout, "\n") {
			return stdout + stderr
		}
		return stdout + "\n" + stderr
	}
	if stdout != "" {
		return stdout
	}
	return stderr
}

func classifyExecStatus(canceled bool, errMsg string) string {
	if canceled || errMsg == "任务已取消" {
		return "canceled"
	}
	if errMsg == "" {
		return "completed"
	}
	switch {
	case strings.HasPrefix(errMsg, execBlockedPrefix) || strings.Contains(errMsg, "拦截"):
		return "blocked"
	case strings.Contains(errMsg, "超时"):
		return "timed_out"
	case strings.Contains(errMsg, "掉线"):
		return "disconnected"
	case strings.Contains(errMsg, "未连接") || strings.Contains(errMsg, "离线"):
		return "offline"
	default:
		return "failed"
	}
}

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
		// 拦截与掉线都在这里落地。带上 jobId：调用方仍可用 get_result 看到完整原因。
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
		"jobId":    jobID,
		"serverId": serverID,
		"running":  true,
		"status":   "running",
		"timeout":  task.Timeout,
		"hint":     "用 get_result 并传入此 jobId 取回输出；命令仍在执行时 running 为 true（status 为 \"running\"），稍后重试即可。",
	}
	if args.Detach {
		res["detached"] = true
		res["unit"] = sanitizeUnitName(jobID)
		res["hint"] = "任务已通过 systemd-run 在后台托管运行；可用 get_result（支持 offset/limit 分页追读或 tail 查看末尾 N KB）轮询输出，或用 cancel 终止任务。"
	}
	return jsonResult(res)
}

/* ---------- get_result ---------- */

// getResultFromAudit 在内存里查不到 jobID 时回退查审计表。
//
// jobs 与 finished 都是纯内存，服务端一重启就全部归零；而 exec_audit 里明明
// 有这条记录。此前一律回「jobId 有误，或结果已超过 30 分钟保留期」，
// 等于把「服务端重启过」说成「你参数写错了」，会让模型往完全错误的方向排查，
// 甚至把一条非幂等的命令重跑一遍。
//
// 审计里的输出被截到 32KB（内存留全量、审计留摘要），所以要明确告知这是
// 降级结果，别让调用方把截断后的日志当成完整输出。
func sliceTextByOffsetTailLimit(s string, offset, tailKB, limitBytes int64) (sliced string, actualOffset, nextOffset, totalBytes int64) {
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
	end := totalBytes
	if limitBytes > 0 && tailKB <= 0 && start+limitBytes < end {
		end = start + limitBytes
	}
	return s[start:end], start, end, totalBytes
}

func (s *App) getResultFromAudit(key *apiKey, jobID string, offset, tailKB, limitBytes int64) mcpCallToolResult {
	meta, err := s.exec.lookupAuditJob(jobID)
	if err != nil {
		return toolFailure("找不到任务 " + jobID + "：jobId 有误，或记录已被审计清理策略删除。")
	}
	// 审计表这条路同样要过机器作用域闸，不能因为走了降级分支就放行。
	if _, f := s.resolveTarget(key, meta.ServerID); f != nil {
		return *f
	}

	if meta.Detach {
		return s.getDetachedResult(meta, offset, tailKB, limitBytes)
	}

	if meta.FinishedAt == 0 {
		return toolFailure("任务 " + jobID + " 的结果已随服务端重启丢失，且它当时尚未执行完。" +
			"命令可能仍在目标机上运行过——重跑前请先确认它是否幂等。")
	}
	slicedOut, actualOffset, nextOffset, totalBytes := sliceTextByOffsetTailLimit(meta.Stdout, offset, tailKB, limitBytes)
	st := classifyExecStatus(meta.Canceled, meta.Error)
	res := map[string]any{
		"jobId":      jobID,
		"serverId":   meta.ServerID,
		"running":    false,
		"status":     st,
		"ok":         meta.Error == "" && meta.ExitCode == 0,
		"exitCode":   meta.ExitCode,
		"stdout":     slicedOut,
		"stderr":     meta.Stderr,
		"output":     combineOutput(slicedOut, meta.Stderr),
		"offset":     actualOffset,
		"nextOffset": nextOffset,
		"totalBytes": totalBytes,
		"hasMore":    nextOffset < totalBytes,
		"eof":        nextOffset >= totalBytes,
		"durationMs": meta.FinishedAt - meta.StartedAt,
		"source":     "audit",
		"sourceNote": "内存结果已随服务端重启丢失，这份取自审计表：输出被截断到 32KB，可能不完整。",
	}
	if meta.Truncated {
		res["truncated"] = true
	}
	if meta.Error != "" {
		res["error"] = meta.Error
		return mcpCallToolResult{
			Content:           mcpText(mustJSON(res)),
			StructuredContent: res,
			IsError:           true,
		}
	}
	return jsonResult(res)
}

func (s *App) getDetachedResult(meta *auditJobMeta, offset, tailKB, limitBytes int64) mcpCallToolResult {
	if meta.FinishedAt > 0 && meta.Error != "" && meta.LogBytes == 0 && meta.Stdout == "" {
		st := classifyExecStatus(meta.Canceled, meta.Error)
		res := map[string]any{
			"jobId":      meta.JobID,
			"serverId":   meta.ServerID,
			"running":    false,
			"status":     st,
			"ok":         false,
			"detached":   true,
			"exitCode":   meta.ExitCode,
			"error":      meta.Error,
			"stdout":     "",
			"stderr":     "",
			"output":     "",
			"offset":     int64(0),
			"nextOffset": int64(0),
			"totalBytes": int64(0),
			"hasMore":    false,
			"eof":        true,
			"durationMs": meta.FinishedAt - meta.StartedAt,
		}
		return mcpCallToolResult{
			Content:           mcpText(mustJSON(res)),
			StructuredContent: res,
			IsError:           true,
		}
	}

	st, err := s.exec.QueryDetached(s.hub, meta.ServerID, meta.JobID, offset, tailKB, limitBytes)
	if err != nil {
		if meta.FinishedAt > 0 {
			slicedOut, actualOffset, nextOffset, totalBytes := sliceTextByOffsetTailLimit(meta.Stdout, offset, tailKB, limitBytes)
			status := classifyExecStatus(meta.Canceled, meta.Error)
			res := map[string]any{
				"jobId":      meta.JobID,
				"serverId":   meta.ServerID,
				"running":    false,
				"status":     status,
				"ok":         meta.Error == "" && meta.ExitCode == 0,
				"detached":   true,
				"unit":       meta.Unit,
				"exitCode":   meta.ExitCode,
				"stdout":     slicedOut,
				"stderr":     meta.Stderr,
				"output":     combineOutput(slicedOut, meta.Stderr),
				"offset":     actualOffset,
				"nextOffset": nextOffset,
				"totalBytes": totalBytes,
				"hasMore":    nextOffset < totalBytes,
				"eof":        nextOffset >= totalBytes,
				"durationMs": meta.FinishedAt - meta.StartedAt,
				"source":     "audit",
				"sourceNote": "目标机当前离线，无法读取完整磁盘日志，这份取自数据库审计摘要。",
			}
			if meta.Error != "" {
				res["error"] = meta.Error
				return mcpCallToolResult{
					Content:           mcpText(mustJSON(res)),
					StructuredContent: res,
					IsError:           true,
				}
			}
			return jsonResult(res)
		}
		return toolFailure(fmt.Sprintf("读取后台任务 %s 状态失败: %v（后台任务仍在目标机 systemd 中独立运行，待 agent 在线后可继续查询）", meta.JobID, err))
	}

	if !st.Found {
		if meta.FinishedAt > 0 {
			slicedOut, actualOffset, nextOffset, totalBytes := sliceTextByOffsetTailLimit(meta.Stdout, offset, tailKB, limitBytes)
			status := classifyExecStatus(meta.Canceled, meta.Error)
			res := map[string]any{
				"jobId":      meta.JobID,
				"serverId":   meta.ServerID,
				"running":    false,
				"status":     status,
				"ok":         meta.Error == "" && meta.ExitCode == 0,
				"detached":   true,
				"unit":       meta.Unit,
				"exitCode":   meta.ExitCode,
				"stdout":     slicedOut,
				"stderr":     meta.Stderr,
				"output":     combineOutput(slicedOut, meta.Stderr),
				"offset":     actualOffset,
				"nextOffset": nextOffset,
				"totalBytes": totalBytes,
				"hasMore":    nextOffset < totalBytes,
				"eof":        nextOffset >= totalBytes,
				"durationMs": meta.FinishedAt - meta.StartedAt,
				"source":     "audit",
			}
			if meta.Error != "" {
				res["error"] = meta.Error
				return mcpCallToolResult{
					Content:           mcpText(mustJSON(res)),
					StructuredContent: res,
					IsError:           true,
				}
			}
			return jsonResult(res)
		}
		return toolFailure("目标机上找不到后台任务 " + meta.JobID + " 的磁盘日志记录。")
	}

	endMs := time.Now().UnixMilli()
	if !st.Running && meta.FinishedAt > 0 {
		endMs = meta.FinishedAt
	}
	stdoutStr := string(st.Data)
	statusStr := "running"
	if !st.Running {
		statusStr = classifyExecStatus(st.Canceled, st.Error)
	}
	res := map[string]any{
		"jobId":      meta.JobID,
		"serverId":   meta.ServerID,
		"running":    st.Running,
		"status":     statusStr,
		"ok":         !st.Running && st.Error == "" && st.ExitCode == 0,
		"detached":   true,
		"unit":       st.Unit,
		"exitCode":   st.ExitCode,
		"stdout":     stdoutStr,
		"stderr":     "",
		"output":     stdoutStr,
		"offset":     st.Offset,
		"nextOffset": st.NextOffset,
		"totalBytes": st.TotalBytes,
		"hasMore":    st.NextOffset < st.TotalBytes,
		"eof":        !st.Running && st.NextOffset >= st.TotalBytes,
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
	return jsonResult(res)
}

func (s *App) toolGetResult(key *apiKey, raw json.RawMessage) mcpCallToolResult {
	if f := requireCap(key, capExec); f != nil {
		return *f
	}
	var args struct {
		JobID  string `json:"job_id"`
		Offset int64  `json:"offset"`
		Limit  int64  `json:"limit"`
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
		return s.getDetachedResult(meta, args.Offset, args.Tail, args.Limit)
	}

	out, serverID, running, found := s.exec.Result(args.JobID)
	if !found {
		// 查不到时回退查审计表，把两种完全不同的情况区分开。
		//
		// jobs/finished 都是纯内存：服务端一重启，在途任务与已完成结果一并归零，
		// 而 exec_audit 里明明有这条记录（含 32KB 截断输出）。此前一律回
		// 「jobId 有误」，等于把「服务端重启过」说成「你参数写错了」，
		// 会让模型往完全错误的方向排查。
		return s.getResultFromAudit(key, args.JobID, args.Offset, args.Tail, args.Limit)
	}
	// 闸 1 的后半：机器作用域。get_result 曾是六个工具里唯一漏掉这道闸的，
	// 任何持 exec 能力的 Key 都能读到别的机器上的完整 stdout。
	if _, f := s.resolveTarget(key, serverID); f != nil {
		return *f
	}
	slicedOut, actualOffset, nextOffset, totalBytes := sliceTextByOffsetTailLimit(out.Stdout, args.Offset, args.Tail, args.Limit)
	if running {
		res := map[string]any{
			"jobId":      args.JobID,
			"serverId":   serverID,
			"running":    true,
			"status":     "running",
			"stdout":     slicedOut,
			"stderr":     out.Stderr,
			"output":     combineOutput(slicedOut, out.Stderr),
			"offset":     actualOffset,
			"nextOffset": nextOffset,
			"totalBytes": totalBytes,
			"hasMore":    nextOffset < totalBytes,
			"eof":        false,
			"hint":       "命令仍在执行，稍后再次调用本工具（可传入 offset=nextOffset 增量读取）。",
		}
		return jsonResult(res)
	}
	statusStr := classifyExecStatus(false, out.Error)
	res := map[string]any{
		"jobId":      out.JobID,
		"serverId":   serverID,
		"running":    false,
		"status":     statusStr,
		"ok":         out.Error == "" && out.ExitCode == 0,
		"exitCode":   out.ExitCode,
		"stdout":     slicedOut,
		"stderr":     out.Stderr,
		"output":     combineOutput(slicedOut, out.Stderr),
		"offset":     actualOffset,
		"nextOffset": nextOffset,
		"totalBytes": totalBytes,
		"hasMore":    nextOffset < totalBytes,
		"eof":        nextOffset >= totalBytes,
		"durationMs": out.DurationMs,
	}
	if out.Truncated {
		res["truncated"] = true
		res["truncatedNote"] = "输出触顶 1MB 已截断，如需完整内容请改用 detach=true 后台托管模式或 read_file 分块拉取。"
	}
	if out.Error != "" {
		res["error"] = out.Error
		return mcpCallToolResult{
			Content:           mcpText(mustJSON(res)),
			StructuredContent: res,
			IsError:           true,
		}
	}
	return jsonResult(res)
}

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
