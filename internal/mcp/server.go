package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

// Principal 描述一次调用背后的身份，由鉴权中间件注入请求上下文。
type Principal struct {
	Subject  string // 登录用户名，静态令牌时为 token 名
	ClientID string // OAuth 客户端 ID
	TokenID  string
	Method   string // oauth | static | disabled
}

type principalKey struct{}

// WithPrincipal 把身份写入上下文。
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// RequestWithPrincipal 返回携带身份的请求副本。
func RequestWithPrincipal(r *http.Request, p Principal) *http.Request {
	return r.WithContext(WithPrincipal(r.Context(), p))
}

// PrincipalFromContext 取回身份，不存在时返回零值。
func PrincipalFromContext(ctx context.Context) Principal {
	if p, ok := ctx.Value(principalKey{}).(Principal); ok {
		return p
	}
	return Principal{}
}

// CallContext 是工具执行时可见的调用环境。
type CallContext struct {
	Ctx             context.Context
	Principal       Principal
	SessionID       string
	ProtocolVersion string
}

// Handler 是一个可被 MCP 客户端调用的工具。
type Handler interface {
	Definition() ToolDef
	Call(cc *CallContext, args json.RawMessage) (*CallToolResult, error)
}

// Auditor 接收工具调用审计事件。
type Auditor interface {
	ToolCall(p Principal, session, tool string, args json.RawMessage, isError bool, errMsg string, dur time.Duration)
}

type nopAuditor struct{}

func (nopAuditor) ToolCall(Principal, string, string, json.RawMessage, bool, string, time.Duration) {}

// Server 持有工具注册表并负责 JSON-RPC 方法分发。
type Server struct {
	info         Implementation
	instructions string
	log          *slog.Logger
	auditor      Auditor

	mu    sync.RWMutex
	tools map[string]Handler
}

func NewServer(info Implementation, instructions string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		info:         info,
		instructions: instructions,
		log:          log,
		auditor:      nopAuditor{},
		tools:        map[string]Handler{},
	}
}

func (s *Server) SetAuditor(a Auditor) {
	if a != nil {
		s.auditor = a
	}
}

// Register 注册工具，重名会 panic（属于启动期编程错误）。
func (s *Server) Register(hs ...Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range hs {
		name := h.Definition().Name
		if _, dup := s.tools[name]; dup {
			panic("mcp: 工具重复注册: " + name)
		}
		s.tools[name] = h
	}
}

// ToolNames 返回已注册工具名（已排序）。
func (s *Server) ToolNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.tools))
	for n := range s.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Dispatch 处理单条消息；返回 nil 表示这是一条通知，无需回复。
func (s *Server) Dispatch(ctx context.Context, sessionID, protoVer string, msg *Message) *Response {
	if msg.JSONRPC != "" && msg.JSONRPC != jsonRPCVersion {
		return newErrorResponse(msg.ID, Errorf(CodeInvalidRequest, "jsonrpc 必须为 %q", jsonRPCVersion))
	}
	if msg.Method == "" {
		// 这是客户端对服务端请求的回复；本服务不发起请求，直接忽略。
		return nil
	}

	notify := msg.IsNotification()
	result, rpcErr := s.call(ctx, sessionID, protoVer, msg)
	if notify {
		if rpcErr != nil {
			s.log.Warn("通知处理失败", "method", msg.Method, "err", rpcErr.Message)
		}
		return nil
	}
	if rpcErr != nil {
		return newErrorResponse(msg.ID, rpcErr)
	}
	return newResponse(msg.ID, result)
}

func (s *Server) call(ctx context.Context, sessionID, protoVer string, msg *Message) (any, *RPCError) {
	switch msg.Method {
	case "initialize":
		return s.handleInitialize(msg.Params)

	case "notifications/initialized", "notifications/cancelled", "notifications/progress",
		"notifications/roots/list_changed":
		return map[string]any{}, nil

	case "ping":
		return map[string]any{}, nil

	case "logging/setLevel":
		return map[string]any{}, nil

	case "tools/list":
		return s.handleListTools(), nil

	case "tools/call":
		return s.handleCallTool(ctx, sessionID, protoVer, msg.Params)

	// 未声明能力但部分客户端仍会探测，返回空列表比报错更省事。
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, nil

	default:
		return nil, Errorf(CodeMethodNotFound, "未知方法 %q", msg.Method)
	}
}

func (s *Server) handleInitialize(raw json.RawMessage) (any, *RPCError) {
	var p InitializeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, Errorf(CodeInvalidParams, "initialize 参数无法解析: %v", err)
		}
	}
	version := negotiate(p.ProtocolVersion)
	s.log.Info("MCP 初始化", "client", p.ClientInfo.Name, "client_version", p.ClientInfo.Version,
		"requested", p.ProtocolVersion, "negotiated", version)
	return InitializeResult{
		ProtocolVersion: version,
		Capabilities: ServerCapabilities{
			Tools:   &ToolsCapability{},
			Logging: &struct{}{},
		},
		ServerInfo:   s.info,
		Instructions: s.instructions,
	}, nil
}

// negotiate 选出双方都支持的协议版本；客户端请求未知版本时回落到最新支持版本。
func negotiate(requested string) string {
	for _, v := range SupportedProtocols {
		if v == requested {
			return v
		}
	}
	return ProtocolLatest
}

func (s *Server) handleListTools() ListToolsResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	defs := make([]ToolDef, 0, len(s.tools))
	for _, h := range s.tools {
		defs = append(defs, h.Definition())
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return ListToolsResult{Tools: defs}
}

func (s *Server) handleCallTool(ctx context.Context, sessionID, protoVer string, raw json.RawMessage) (any, *RPCError) {
	var p CallToolParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, Errorf(CodeInvalidParams, "tools/call 参数无法解析: %v", err)
	}
	s.mu.RLock()
	h, ok := s.tools[p.Name]
	s.mu.RUnlock()
	if !ok {
		return nil, Errorf(CodeMethodNotFound, "未知工具 %q，可用工具: %s",
			p.Name, strings.Join(s.ToolNames(), ", "))
	}

	cc := &CallContext{
		Ctx:             ctx,
		Principal:       PrincipalFromContext(ctx),
		SessionID:       sessionID,
		ProtocolVersion: protoVer,
	}
	args := p.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}

	start := time.Now()
	res, err := s.safeCall(h, cc, args)
	dur := time.Since(start)

	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	} else if res != nil && res.IsError && len(res.Content) > 0 {
		errMsg = truncate(res.Content[0].Text, 300)
	}
	isErr := err != nil || (res != nil && res.IsError)
	s.auditor.ToolCall(cc.Principal, sessionID, p.Name, args, isErr, errMsg, dur)

	if err != nil {
		// 协议层错误（参数非法等）用 JSON-RPC error；其余按 isError 回给模型。
		var rpcErr *RPCError
		if asRPCError(err, &rpcErr) {
			return nil, rpcErr
		}
		s.log.Warn("工具执行出错", "tool", p.Name, "err", err, "dur", dur)
		return ToolError("%s 执行失败: %v", p.Name, err), nil
	}
	if res == nil {
		return ToolError("%s 未返回任何结果", p.Name), nil
	}
	s.log.Info("工具调用", "tool", p.Name, "user", cc.Principal.Subject,
		"dur_ms", dur.Milliseconds(), "is_error", res.IsError)
	return res, nil
}

// safeCall 拦截工具里的 panic，避免一个坏工具带崩整个服务。
func (s *Server) safeCall(h Handler, cc *CallContext, args json.RawMessage) (res *CallToolResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("工具 panic", "tool", h.Definition().Name, "panic", r,
				"stack", string(debug.Stack()))
			res, err = nil, fmt.Errorf("内部错误: %v", r)
		}
	}()
	return h.Call(cc, args)
}

func asRPCError(err error, out **RPCError) bool {
	if e, ok := err.(*RPCError); ok {
		*out = e
		return true
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
