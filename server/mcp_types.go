// MCP（Model Context Protocol）协议类型定义。
package main

import (
	"encoding/json"
	"strings"
)

const (
	mcpVersionLatest = "2025-11-25"
	mcpVersion202506 = "2025-06-18"
	mcpVersion202503 = "2025-03-26"
)

var mcpSupportedVersions = []string{mcpVersionLatest, mcpVersion202506, mcpVersion202503}

func mcpVersionSupported(v string) bool {
	for _, s := range mcpSupportedVersions {
		if s == v {
			return true
		}
	}
	return false
}

const (
	jrpcParseError     = -32700
	jrpcInvalidRequest = -32600
	jrpcMethodNotFound = -32601
	jrpcInvalidParams  = -32602
	jrpcInternalError  = -32603
)

const jsonrpcVersion = "2.0"

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r *jsonrpcRequest) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

var jsonNull = json.RawMessage("null")

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

type mcpImplementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type mcpToolsCapability struct {
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
}

type mcpCallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

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
