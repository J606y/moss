// MCP Streamable HTTP 传输层与方法分发。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

const mcpMaxBody = 1 << 20

func (s *App) handleMCP(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !s.originAllowed(origin) {
		writeJSON(w, http.StatusForbidden, rpcError(nil, jrpcInvalidRequest, "Origin 不被允许", nil))
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ver := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version"))
	if ver == "" {
		ver = mcpVersion202503
	}
	if !mcpVersionSupported(ver) {
		writeJSON(w, http.StatusBadRequest, rpcError(nil, jrpcInvalidParams,
			"不支持的协议版本", map[string]any{"supported": mcpSupportedVersions, "requested": ver}))
		return
	}

	key, err := s.authKey(r, "")
	if err != nil {
		if errors.Is(err, errKeyMissing) || errors.Is(err, errKeyInvalid) || errors.Is(err, errKeyExpired) {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="moss", resource_metadata="%s/.well-known/oauth-protected-resource"`, s.baseURL(r)))
		}
		writeJSON(w, keyErrStatus(err), rpcError(nil, jrpcInvalidRequest, err.Error(), nil))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpMaxBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, rpcError(nil, jrpcParseError, "请求体读取失败", nil))
		return
	}

	if isJSONArray(body) {
		writeJSON(w, http.StatusBadRequest, rpcError(nil, jrpcInvalidRequest,
			"本端点不支持批量请求，请逐条发送", nil))
		return
	}

	var req jsonrpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcError(nil, jrpcParseError, "JSON 解析失败", nil))
		return
	}
	if req.JSONRPC != jsonrpcVersion || req.Method == "" {
		writeJSON(w, http.StatusBadRequest, rpcError(req.ID, jrpcInvalidRequest, "非法的 JSON-RPC 请求", nil))
		return
	}

	if req.isNotification() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := s.mcpDispatch(r, key, ver, &req)
	writeJSON(w, http.StatusOK, resp)
}

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
		return rpcResult(req.ID, map[string]any{})

	case "tools/list":
		return rpcResult(req.ID, mcpListToolsResult{Tools: mcpTools()})

	case "tools/call":
		var p mcpCallToolParams
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return rpcError(req.ID, jrpcInvalidParams, "tools/call 参数非法", nil)
		}
		return s.mcpCallTool(r, key, req.ID, p)

	default:
		return rpcError(req.ID, jrpcMethodNotFound, "未实现的方法: "+req.Method, nil)
	}
}

func (s *App) mcpInitialize(headerVer string, req *jsonrpcRequest) mcpInitializeResult {
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
const mcpInstructions = `Moss 是服务器集群的监控与运维中枢。你可以查看机器状态、执行命令、传输文件、写入配置。

工作方式：
- 先用 list_servers 拿到机器 ID，后续所有操作都用这个 ID 指定目标。
- 排查问题时先看 get_metrics，确认是 CPU、内存还是磁盘的问题，再决定执行什么命令。
- 集群巡检或多机探针优先用 batch_exec：一次并发下发到多台（或全部在线）服务器并在服务端聚合返回结果。
- 单机命令用 exec（异步返回 jobId）配 get_result 轮询。get_result 统一提供 status 状态机字段与合并后的 output 字段，且通道异常与命令退出码解耦（命令非零 exitCode 不会触发 MCP isError）。
- 系统大包更新、dpkg 解压、编译构建等长耗时命令请给 exec 传 detach=true：agent 会用 systemd-run --unit=moss-job-<id> 拉起独立后台服务（超时上限 24 小时，日志落盘无 1MB 上限），可用 get_result 的 offset/limit/tail 分页追读、用 list_jobs 查列表、用 cancel 终止任务。
- 拉取远端文件（含大文件或二进制 deb/tar 包）用 read_file 按 offset + length 分块读取；写配置文件用 write_file。

限制：
- 破坏性命令会被服务端拦截（删根目录、格式化磁盘、关机、停用 moss agent 等），不要尝试绕过。
- 你的 Key 可能只被授权访问部分机器；越权访问会被拒绝。
- 每次操作都有完整审计记录，包括被拦截的尝试。

执行命令前先确认目标机器是对的。生产环境的破坏性变更应当先向人确认。`

func isJSONArray(body []byte) bool {
	for _, b := range body {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '[':
			return true
		default:
			return false
		}
	}
	return false
}
