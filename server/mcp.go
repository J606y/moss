// MCP Streamable HTTP 传输层与方法分发。
//
// 单端点 POST /mcp，Bearer Key 鉴权（复用 apikey.go 的 Key 体系）。
// 实现 legacy 语义（见 mcp_types.go 的版本选型说明）。
package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

// mcpMaxBody 请求体上限。write_file 的内容上限 48KB，base64 后约 64KB，
// 留足余量到 1MB，同时挡住把 server 内存打爆的畸形请求。
const mcpMaxBody = 1 << 20

// handleMCP 是 MCP 端点。
//
// 无会话设计：不实现 Mcp-Session-Id。规范里会话是 MAY 而非 MUST，
// 而 moss 的工具集与鉴权完全由 Key 决定、不随连接变化，没有需要跨请求保持的状态。
// 这也顺带对齐了 2026-07-28 的无状态方向，将来升级不必拆会话层。
// 对客户端发来的 Mcp-Session-Id 一律忽略（规范允许），GET / DELETE 回 405。
func (s *App) handleMCP(w http.ResponseWriter, r *http.Request) {
	// Origin 校验防 DNS rebinding：规范要求 Origin 存在且非法时回 403。
	// 浏览器之外的客户端（OpenClaw、Claude Code）不带 Origin，直接放行。
	if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin) {
		writeJSON(w, http.StatusForbidden, rpcError(nil, jrpcInvalidRequest, "Origin 不被允许", nil))
		return
	}

	if r.Method != http.MethodPost {
		// 不提供 GET SSE 流（无服务端主动消息）也不提供 DELETE（无会话）。
		// 规范明确允许以 405 表达「本端点不提供该能力」。
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 协议版本头：缺失时按规范兜底为 2025-03-26，不当作错误。
	ver := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version"))
	if ver == "" {
		ver = mcpVersion202503
	}
	if !mcpVersionSupported(ver) {
		// 规范：收到无效或不支持的版本必须回 400。
		writeJSON(w, http.StatusBadRequest, rpcError(nil, jrpcInvalidParams,
			"不支持的协议版本", map[string]any{"supported": mcpSupportedVersions, "requested": ver}))
		return
	}

	// 鉴权。此处不要求具体能力，逐个工具再按需校验——
	// 只有 read 能力的 Key 也应当能完成握手并看到工具列表。
	key, err := s.authKey(r, "")
	if err != nil {
		if errors.Is(err, errKeyMissing) || errors.Is(err, errKeyInvalid) || errors.Is(err, errKeyExpired) {
			// 规范要求 401 带 WWW-Authenticate 挑战。
			w.Header().Set("WWW-Authenticate", `Bearer realm="moss"`)
		}
		writeJSON(w, keyErrStatus(err), rpcError(nil, jrpcInvalidRequest, err.Error(), nil))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpMaxBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, rpcError(nil, jrpcParseError, "请求体读取失败", nil))
		return
	}

	var req jsonrpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// 批量请求自 2025-06-18 起被移除；若收到数组，解析到此失败并回 Parse error。
		writeJSON(w, http.StatusBadRequest, rpcError(nil, jrpcParseError, "JSON 解析失败", nil))
		return
	}
	if req.JSONRPC != jsonrpcVersion || req.Method == "" {
		writeJSON(w, http.StatusBadRequest, rpcError(req.ID, jrpcInvalidRequest, "非法的 JSON-RPC 请求", nil))
		return
	}

	// 通知无需响应，规范要求受理后回 202 且无 body。
	if req.isNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := s.mcpDispatch(r, key, ver, &req)
	// 应用层错误一律以 HTTP 200 承载：JSON-RPC 的错误在 body 里表达，
	// 用 HTTP 状态码表达会让客户端的传输层与协议层错误处理混在一起。
	writeJSON(w, http.StatusOK, resp)
}

// originAllowed 判断 Origin 是否可信。
// moss 的 MCP 端点面向 AI 客户端（非浏览器），同源的前端并不调用它，
// 因此只放行本机来源——这足以阻断 DNS rebinding，又不影响正常接入。
func (s *App) originAllowed(origin string) bool {
	low := strings.ToLower(origin)
	for _, p := range []string{"http://localhost", "https://localhost", "http://127.0.0.1", "https://127.0.0.1", "http://[::1]", "https://[::1]"} {
		if low == p || strings.HasPrefix(low, p+":") {
			return true
		}
	}
	return false
}

func (s *App) mcpDispatch(r *http.Request, key *apiKey, ver string, req *jsonrpcRequest) jsonrpcResponse {
	switch req.Method {
	case "initialize":
		return rpcResult(req.ID, s.mcpInitialize(ver, req))

	case "ping":
		// 规范要求 ping 回空结果对象。
		return rpcResult(req.ID, map[string]any{})

	case "tools/list":
		return rpcResult(req.ID, mcpListToolsResult{Tools: mcpTools()})

	case "tools/call":
		var p mcpCallToolParams
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			// 请求结构畸形 → 协议错误（而非 isError），规范明确如此归类。
			return rpcError(req.ID, jrpcInvalidParams, "tools/call 参数非法", nil)
		}
		return s.mcpCallTool(r, key, req.ID, p)

	default:
		return rpcError(req.ID, jrpcMethodNotFound, "未实现的方法: "+req.Method, nil)
	}
}

func (s *App) mcpInitialize(headerVer string, req *jsonrpcRequest) mcpInitializeResult {
	// 版本协商：客户端在 body 里声明的版本优先（握手时还没有版本头的约束）。
	// 支持则回同一版本，否则回服务端最新版，由客户端决定是否断开。
	negotiated := headerVer
	var p mcpInitializeParams
	if err := json.Unmarshal(req.Params, &p); err == nil && p.ProtocolVersion != "" {
		if mcpVersionSupported(p.ProtocolVersion) {
			negotiated = p.ProtocolVersion
		} else {
			negotiated = mcpVersionLatest
		}
		if p.ClientInfo != nil {
			log.Printf("MCP 客户端接入: %s %s（协议 %s）", p.ClientInfo.Name, p.ClientInfo.Version, negotiated)
		}
	}
	return mcpInitializeResult{
		ProtocolVersion: negotiated,
		Capabilities:    mcpServerCapabilities{Tools: &mcpToolsCapability{}},
		ServerInfo: mcpImplementation{
			Name:    "moss",
			Title:   "Moss 服务器控制中枢",
			Version: serverVersion,
		},
		Instructions: mcpInstructions,
	}
}

// mcpInstructions 是给 AI 的使用说明，随握手一次性下发。
// 写清楚边界比事后让模型试错便宜：它读到这段就知道哪些事做不到，不会反复撞墙。
const mcpInstructions = `Moss 是服务器集群的监控与运维中枢。你可以查看机器状态、执行命令、写入配置文件。

工作方式：
- 先用 list_servers 拿到机器 ID，后续所有操作都用这个 ID 指定目标。
- 排查问题时先看 get_metrics，确认是 CPU、内存还是磁盘的问题，再决定执行什么命令。
- exec 是异步的：提交后立刻返回 jobId，用 get_result 轮询取回输出。部署这类长命令必须这样用。
- 写配置文件用 write_file，不要用 exec 配 heredoc——转义容易出错，且内容无法原样审计。

限制：
- 破坏性命令会被服务端拦截（删根目录、格式化磁盘、关机、停用 moss agent 等），不要尝试绕过。
- 你的 Key 可能只被授权访问部分机器；越权访问会被拒绝。
- 每次操作都有完整审计记录，包括被拦截的尝试。

执行命令前先确认目标机器是对的。生产环境的破坏性变更应当先向人确认。`
