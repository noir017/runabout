// Package mcp 实现 Model Context Protocol 的服务端：JSON-RPC 消息层、
// 工具注册表，以及 Streamable HTTP 传输。
package mcp

import "encoding/json"

// ProtocolLatest 是本服务优先声明的协议版本。
const ProtocolLatest = "2025-06-18"

// SupportedProtocols 按新到旧列出可协商的版本。
var SupportedProtocols = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// ---------- JSON-RPC 2.0 ----------

const jsonRPCVersion = "2.0"

// JSON-RPC 标准错误码。
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// IsNotification 报告消息是否为无需回复的通知。
func (m *Message) IsNotification() bool { return len(m.ID) == 0 || string(m.ID) == "null" }

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

func Errorf(code int, format string, args ...any) *RPCError {
	return &RPCError{Code: code, Message: sprintf(format, args...)}
}

func newResponse(id json.RawMessage, result any) *Response {
	return &Response{JSONRPC: jsonRPCVersion, ID: id, Result: result}
}

func newErrorResponse(id json.RawMessage, err *RPCError) *Response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &Response{JSONRPC: jsonRPCVersion, ID: id, Error: err}
}

// ---------- initialize ----------

type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type InitializeParams struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities,omitempty"`
	ClientInfo      Implementation  `json:"clientInfo"`
}

type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type ServerCapabilities struct {
	Tools   *ToolsCapability `json:"tools,omitempty"`
	Logging *struct{}        `json:"logging,omitempty"`
}

type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// ---------- tools ----------

type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

type ToolDef struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description"`
	InputSchema any              `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

type ListToolsResult struct {
	Tools []ToolDef `json:"tools"`
}

type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Content 是工具返回的内容块，支持 text 与 image。
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

func TextContent(s string) Content { return Content{Type: "text", Text: s} }

func ImageContent(b64, mime string) Content {
	return Content{Type: "image", Data: b64, MimeType: mime}
}

type CallToolResult struct {
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// Text 构造一个成功结果。
func Text(s string) *CallToolResult {
	return &CallToolResult{Content: []Content{TextContent(s)}}
}

// Textf 构造一个成功结果（带格式化）。
func Textf(format string, args ...any) *CallToolResult {
	return Text(sprintf(format, args...))
}

// ToolError 构造一个"工具执行失败"结果。这类失败按 MCP 约定走 isError 而不是
// JSON-RPC error，好让模型看到原因并自行修正。
func ToolError(format string, args ...any) *CallToolResult {
	return &CallToolResult{Content: []Content{TextContent(sprintf(format, args...))}, IsError: true}
}

// WithStructured 附加结构化输出，便于客户端程序化消费。
func (r *CallToolResult) WithStructured(v any) *CallToolResult {
	r.StructuredContent = v
	return r
}

// Ptr 返回指向 v 的指针，用于填充 *bool 这类可选提示字段。
func Ptr[T any](v T) *T { return &v }
