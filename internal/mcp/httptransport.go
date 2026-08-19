package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/noir017/agent-tools-mcp/internal/idgen"
)

const (
	headerSessionID = "Mcp-Session-Id"
	headerProtoVer  = "MCP-Protocol-Version"
	maxRequestBytes = 16 << 20
)

// HTTPOptions 配置 Streamable HTTP 传输。
type HTTPOptions struct {
	// BaseURL 是对外根地址，用于校验 Origin。
	BaseURL string
	// AllowedOrigins 为白名单；含 "*" 时不校验。
	AllowedOrigins []string
	SessionTTL     time.Duration
	// StrictSessions 为 true 时，非 initialize 请求必须带合法 Mcp-Session-Id。
	StrictSessions bool
	Log            *slog.Logger
}

type session struct {
	id        string
	proto     string
	createdAt time.Time
	lastSeen  time.Time
}

// HTTPTransport 把 Server 暴露为 MCP Streamable HTTP 端点。
// 同一个路径上：POST 提交消息，GET 打开 SSE 下行流，DELETE 结束会话。
type HTTPTransport struct {
	srv  *Server
	opt  HTTPOptions
	log  *slog.Logger
	host string

	mu       sync.Mutex
	sessions map[string]*session
}

func NewHTTPTransport(srv *Server, opt HTTPOptions) *HTTPTransport {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.SessionTTL <= 0 {
		opt.SessionTTL = 2 * time.Hour
	}
	host := ""
	if u, err := url.Parse(opt.BaseURL); err == nil {
		host = u.Host
	}
	t := &HTTPTransport{srv: srv, opt: opt, log: opt.Log, host: host, sessions: map[string]*session{}}
	go t.reap()
	return t
}

func (t *HTTPTransport) reap() {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for range tick.C {
		cutoff := time.Now().Add(-t.opt.SessionTTL)
		t.mu.Lock()
		for id, s := range t.sessions {
			if s.lastSeen.Before(cutoff) {
				delete(t.sessions, id)
			}
		}
		t.mu.Unlock()
	}
}

func (t *HTTPTransport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := t.checkOrigin(r); err != nil {
		httpError(w, http.StatusForbidden, err.Error())
		return
	}
	switch r.Method {
	case http.MethodPost:
		t.handlePost(w, r)
	case http.MethodGet:
		t.handleGet(w, r)
	case http.MethodDelete:
		t.handleDelete(w, r)
	case http.MethodOptions:
		t.cors(w, r)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE, OPTIONS")
		httpError(w, http.StatusMethodNotAllowed, "不支持的方法 "+r.Method)
	}
}

// checkOrigin 做 DNS rebinding 防护：浏览器发起的跨站请求会带 Origin，
// 非浏览器客户端（含 ChatGPT 服务端拉取）通常不带，放行。
func (t *HTTPTransport) checkOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	for _, a := range t.opt.AllowedOrigins {
		if a == "*" {
			return nil
		}
		if strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(origin, "/")) {
			return nil
		}
	}
	if u, err := url.Parse(origin); err == nil && t.host != "" && u.Host == t.host {
		return nil
	}
	return fmt.Errorf("Origin %q 不在白名单内（见 server.allowed_origins）", origin)
}

func (t *HTTPTransport) cors(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, "+headerSessionID+", "+headerProtoVer+", Accept, Last-Event-ID")
	w.Header().Set("Access-Control-Expose-Headers", headerSessionID+", "+headerProtoVer)
}

func (t *HTTPTransport) handlePost(w http.ResponseWriter, r *http.Request) {
	t.cors(w, r)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		httpError(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	msgs, batch, err := parseMessages(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, newErrorResponse(nil,
			Errorf(CodeParseError, "JSON 解析失败: %v", err)))
		return
	}
	if len(msgs) == 0 {
		writeJSON(w, http.StatusBadRequest, newErrorResponse(nil,
			Errorf(CodeInvalidRequest, "请求体不含任何 JSON-RPC 消息")))
		return
	}

	isInit := false
	for _, m := range msgs {
		if m.Method == "initialize" {
			isInit = true
		}
	}

	sess, err := t.resolveSession(r, isInit, msgs)
	if err != nil {
		// 按规范返回 404，让客户端重新 initialize。
		writeJSON(w, http.StatusNotFound, newErrorResponse(msgs[0].ID,
			Errorf(CodeInvalidRequest, "%s", err.Error())))
		return
	}
	if isInit {
		w.Header().Set(headerSessionID, sess.id)
	}
	w.Header().Set(headerProtoVer, sess.proto)

	responses := make([]*Response, 0, len(msgs))
	for _, m := range msgs {
		if resp := t.srv.Dispatch(r.Context(), sess.id, sess.proto, m); resp != nil {
			responses = append(responses, resp)
		}
		if m.Method == "initialize" {
			t.mu.Lock()
			if s := t.sessions[sess.id]; s != nil {
				s.proto = negotiate(initProto(m))
				sess.proto = s.proto
			}
			t.mu.Unlock()
			w.Header().Set(headerProtoVer, sess.proto)
		}
	}

	if len(responses) == 0 {
		// 全是通知：规范要求 202 且无响应体。
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if batch {
		writeJSON(w, http.StatusOK, responses)
		return
	}
	writeJSON(w, http.StatusOK, responses[0])
}

func initProto(m *Message) string {
	var p InitializeParams
	_ = json.Unmarshal(m.Params, &p)
	return p.ProtocolVersion
}

// resolveSession 取出或新建会话。initialize 总是新建；其余请求按头查找，
// 宽松模式下找不到就补一个，以兼容不回传 Mcp-Session-Id 的客户端。
func (t *HTTPTransport) resolveSession(r *http.Request, isInit bool, msgs []*Message) (*session, error) {
	id := r.Header.Get(headerSessionID)
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	if isInit {
		s := &session{id: idgen.New("sess"), proto: ProtocolLatest, createdAt: now, lastSeen: now}
		t.sessions[s.id] = s
		return s, nil
	}
	if id != "" {
		if s, ok := t.sessions[id]; ok {
			s.lastSeen = now
			return s, nil
		}
		if t.opt.StrictSessions {
			return nil, errors.New("会话不存在或已过期，请重新 initialize")
		}
	} else if t.opt.StrictSessions {
		return nil, errors.New("缺少 " + headerSessionID + " 请求头")
	}
	// 宽松模式：为无状态客户端隐式建会话。
	s := &session{id: orDefault(id, idgen.New("sess")), proto: protoFromHeader(r), createdAt: now, lastSeen: now}
	t.sessions[s.id] = s
	return s, nil
}

func protoFromHeader(r *http.Request) string {
	return negotiate(r.Header.Get(headerProtoVer))
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// handleGet 打开 SSE 下行流。本服务不主动推送消息，流的作用是让客户端
// 保持长连接探活；心跳用 SSE 注释帧，不会被解析成消息。
func (t *HTTPTransport) handleGet(w http.ResponseWriter, r *http.Request) {
	t.cors(w, r)
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		httpError(w, http.StatusNotAcceptable, "GET 该端点需要 Accept: text/event-stream")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "当前服务端不支持流式响应")
		return
	}
	if id := r.Header.Get(headerSessionID); id != "" {
		t.mu.Lock()
		if s, ok := t.sessions[id]; ok {
			s.lastSeen = time.Now()
		}
		t.mu.Unlock()
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": stream open\n\n")
	flusher.Flush()

	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (t *HTTPTransport) handleDelete(w http.ResponseWriter, r *http.Request) {
	t.cors(w, r)
	id := r.Header.Get(headerSessionID)
	if id == "" {
		httpError(w, http.StatusBadRequest, "缺少 "+headerSessionID+" 请求头")
		return
	}
	t.mu.Lock()
	_, existed := t.sessions[id]
	delete(t.sessions, id)
	t.mu.Unlock()
	if !existed {
		httpError(w, http.StatusNotFound, "会话不存在")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseMessages 同时接受单条消息与 JSON-RPC 批量数组。
func parseMessages(body []byte) (msgs []*Message, batch bool, err error) {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	if strings.HasPrefix(trimmed, "[") {
		var arr []*Message
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, true, err
		}
		return arr, true, nil
	}
	var m Message
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false, err
	}
	return []*Message{&m}, false, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("写响应失败", "err", err)
	}
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
