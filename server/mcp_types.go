// MCP（Model Context Protocol）协议类型定义。
//
// 版本选型：以 legacy 时代为基线（2025-11-25 为主，兼容 2025-06-18 / 2025-03-26）。
//
// MCP 在 2026-07-28 做了断代改动——废除协议级会话、废除 initialize 握手、
// 移除 GET 流，改为每请求自带 _meta 的无状态模型 + server/discover。
// 现网客户端生态（含官方 Go SDK 的版本协商逻辑）仍以 2025-11-25 为落点：
// 该 SDK 对任何非 2026-07-28 的客户端一律回落到 2025-11-25。
// 因此这里实现 legacy 语义；规范给出的 dual-era 做法是在同一端点上按
// 「body 里有 _meta.protocolVersion → 走无状态路径 / method 为 initialize → 走会话路径」
// 分流，将来升级时按此扩展，不必推翻现有代码。
package main

import (
	"encoding/json"
	"strings"
)

// 协议版本。
const (
	mcpVersionLatest = "2025-11-25" // 本服务端首选版本
	mcpVersion202506 = "2025-06-18"
	mcpVersion202503 = "2025-03-26" // 缺失 MCP-Protocol-Version 头时的规范兜底值
)

// mcpSupportedVersions 按优先级从高到低。客户端请求的版本不在此列时，
// 规范要求回以服务端支持的某个版本（SHOULD 为最新），由客户端决定是否断开。
var mcpSupportedVersions = []string{mcpVersionLatest, mcpVersion202506, mcpVersion202503}

func mcpVersionSupported(v string) bool {
	for _, s := range mcpSupportedVersions {
		if s == v {
			return true
		}
	}
	return false
}

// JSON-RPC 2.0 标准错误码。
const (
	jrpcParseError     = -32700
	jrpcInvalidRequest = -32600
	jrpcMethodNotFound = -32601
	jrpcInvalidParams  = -32602
	jrpcInternalError  = -32603
)

const jsonrpcVersion = "2.0"

// jsonrpcRequest 兼容 request 与 notification 两种形态：
// notification 无 id，据此区分——规范要求 notification 不得回响应。
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // string | number，规范禁止 null
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification 判断是否为通知。缺 id 或 id 为 null 都按通知处理
// （规范禁止 id 为 null，宽容接收避免因客户端小瑕疵整条链路失败）。
func (r *jsonrpcRequest) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// jsonrpcResponse 是一条 JSON-RPC 2.0 响应。
//
// ID 不能用 omitempty：规范规定响应的 id 是 REQUIRED，在无法确定请求 id 时
// （解析失败、非法请求）必须显式为 null。omitempty 会让整个键消失，
// 严格校验的客户端会判为非法响应。所以由 rpcResult / rpcError 保证 ID 非空——
// 传进来的 nil 一律补成 JSON null。
type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// jsonNull 是字面量 null，供 id 未知时使用。
var jsonNull = json.RawMessage("null")

// normalizeID 把缺失的 id 补成 JSON null。
func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return jsonNull
	}
	return id
}

func rpcResult(id json.RawMessage, result any) jsonrpcResponse {
	return jsonrpcResponse{JSONRPC: jsonrpcVersion, ID: normalizeID(id), Result: result}
}

func rpcError(id json.RawMessage, code int, msg string, data any) jsonrpcResponse {
	return jsonrpcResponse{
		JSONRPC: jsonrpcVersion,
		ID:      normalizeID(id),
		Error:   &jsonrpcError{Code: code, Message: msg, Data: data},
	}
}

/* ---------- 握手 ---------- */

// mcpImplementation 对应规范的 Implementation。
type mcpImplementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type mcpToolsCapability struct {
	// ListChanged 表示服务端会在工具列表变化时发通知。
	// moss 的工具集是编译期固定的，永远不变，故为 false（省略）。
	ListChanged bool `json:"listChanged,omitempty"`
}

type mcpServerCapabilities struct {
	Tools *mcpToolsCapability `json:"tools,omitempty"`
}

type mcpInitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	ClientInfo      *mcpImplementation `json:"clientInfo,omitempty"`
}

type mcpInitializeResult struct {
	ProtocolVersion string                `json:"protocolVersion"`
	Capabilities    mcpServerCapabilities `json:"capabilities"`
	ServerInfo      mcpImplementation     `json:"serverInfo"`
	Instructions    string                `json:"instructions,omitempty"`
}

/* ---------- 工具 ---------- */

// mcpToolAnnotations 全部字段都只是 hint，规范明确客户端不应据此做安全决策。
// 我们如实标注，方便客户端排序与提示，真正的边界在 Key 作用域与命令拦截。
type mcpToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

type mcpTool struct {
	Name        string              `json:"name"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	InputSchema map[string]any      `json:"inputSchema"`
	Annotations *mcpToolAnnotations `json:"annotations,omitempty"`
}

type mcpListToolsResult struct {
	Tools []mcpTool `json:"tools"`
	// 无分页：工具数固定为个位数，一次返回完，故不带 nextCursor。
}

type mcpCallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// mcpContent 目前只用 text 一种块类型。
type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// mcpCallToolResult 的 Content 必填，即使为空也要是空数组而非 null。
type mcpCallToolResult struct {
	Content           []mcpContent `json:"content"`
	StructuredContent any          `json:"structuredContent,omitempty"`
	IsError           bool         `json:"isError,omitempty"`
}

func mcpText(s string) []mcpContent { return []mcpContent{{Type: "text", Text: s}} }

// toolFailure 构造结构化的「通道 / 权限 / 参数失败」结果。
//
// 统一返回结构化 JSON（同时写入 Content[0].Text 与 StructuredContent）并附带 status 字段，
// 使调用端无论读 structuredContent 还是解析 content 文本，都能拿到一致的状态机字段，
// 避免节点离线/断连时退化为无结构的纯文本报错。
func toolFailure(msg string) mcpCallToolResult {
	status := "error"
	switch {
	case strings.Contains(msg, "离线") || strings.Contains(msg, "未连接"):
		status = "offline"
	case strings.Contains(msg, "掉线"):
		status = "disconnected"
	case strings.Contains(msg, "拦截"):
		status = "blocked"
	case strings.Contains(msg, "超时"):
		status = "timed_out"
	case strings.Contains(msg, "取消"):
		status = "canceled"
	}
	payload := map[string]any{
		"status":  status,
		"running": false,
		"error":   msg,
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return mcpCallToolResult{Content: mcpText(msg), IsError: true}
	}
	return mcpCallToolResult{
		Content:           mcpText(string(b)),
		StructuredContent: payload,
		IsError:           true,
	}
}

func boolPtr(b bool) *bool { return &b }
