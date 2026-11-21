package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/noir017/runabout/internal/config"
)

// fakeUpstream 是一个最小的 MCP Streamable HTTP 上游，用来验证客户端行为。
type fakeUpstream struct {
	mu              sync.Mutex
	tools           []Tool
	requireToken    string
	sse             bool
	protocol        string
	instructions    string
	notifyStatus    int
	badResult       bool
	paginateForever bool
	// pages 非空时按页返回 tools/list，最后一页不带 nextCursor。
	pages    [][]Tool
	listPage int
	// noSession 时 initialize 不返回 Mcp-Session-Id。
	noSession bool
	// rejectCalls 时 tools/call 一律回 404，用于验证重连重试有上限。
	rejectCalls bool

	session      string
	sessionSeq   int
	initCount    int
	calls        []fakeCall
	requests     []fakeRequest
	expired      map[string]bool
	expireOnCall bool

	// barrierSize > 0 时，tools/call 会等齐这么多请求后再一起处理，
	// 用来稳定复现"并发请求同时撞上会话过期"。
	barrierSize int
	barrierCh   chan struct{}
	barrierN    int
}

type fakeCall struct {
	Name    string
	Args    json.RawMessage
	Session string
}

type fakeRequest struct {
	Method  string
	Proto   string
	Session string
	Auth    string
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if f.requireToken != "" && r.Header.Get("Authorization") != "Bearer "+f.requireToken {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{
		Method:  req.Method,
		Proto:   r.Header.Get("MCP-Protocol-Version"),
		Session: r.Header.Get("Mcp-Session-Id"),
		Auth:    r.Header.Get("Authorization"),
	})
	f.mu.Unlock()

	switch req.Method {
	case "initialize":
		f.mu.Lock()
		f.sessionSeq++
		sid := ""
		if !f.noSession {
			f.session = fmt.Sprintf("sess-%d", f.sessionSeq)
			sid = f.session
		}
		f.initCount++
		proto := f.protocol
		if proto == "" {
			proto = "2025-06-18"
		}
		instructions := f.instructions
		f.mu.Unlock()
		if sid != "" {
			w.Header().Set("Mcp-Session-Id", sid)
		}
		f.respond(w, req.ID, map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			"instructions":    instructions,
		})
	case "notifications/initialized":
		f.mu.Lock()
		status := f.notifyStatus
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, "notify failed", status)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		if !f.sessionOK(r) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.mu.Lock()
		if f.pages != nil {
			idx := f.listPage
			f.listPage++
			var body map[string]any
			if idx < len(f.pages) {
				body = map[string]any{"tools": f.pages[idx]}
				if idx < len(f.pages)-1 {
					body["nextCursor"] = "more"
				}
			} else {
				body = map[string]any{"tools": []Tool{}}
			}
			f.mu.Unlock()
			f.respond(w, req.ID, body)
			return
		}
		paginate := f.paginateForever
		tools := append([]Tool(nil), f.tools...)
		f.mu.Unlock()
		if paginate {
			f.respond(w, req.ID, map[string]any{"tools": []any{}, "nextCursor": "more"})
			return
		}
		f.respond(w, req.ID, map[string]any{"tools": tools})
	case "tools/call":
		if f.barrierSize > 0 {
			f.mu.Lock()
			if f.barrierCh == nil {
				f.barrierCh = make(chan struct{})
			}
			f.barrierN++
			if f.barrierN == f.barrierSize {
				close(f.barrierCh)
			}
			ch := f.barrierCh
			f.mu.Unlock()
			<-ch
		}

		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)

		f.mu.Lock()
		reject := f.rejectCalls
		f.mu.Unlock()
		if reject {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		f.mu.Lock()
		if f.expireOnCall {
			f.expireOnCall = false
			f.expired[f.session] = true
			f.session = ""
		}
		valid := f.sessionOKLocked(r)
		badResult := f.badResult
		f.mu.Unlock()
		if !valid {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if badResult {
			f.respond(w, req.ID, []any{"not", "an", "object"})
			return
		}

		f.mu.Lock()
		f.calls = append(f.calls, fakeCall{Name: p.Name, Args: p.Arguments, Session: r.Header.Get("Mcp-Session-Id")})
		f.mu.Unlock()

		if p.Name != "echo" {
			f.respondError(w, req.ID, -32602, "unknown tool "+p.Name)
			return
		}
		f.respond(w, req.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "echo:" + string(p.Arguments)}},
			"custom":  map[string]any{"kept": true},
		})
	default:
		f.respondError(w, req.ID, -32601, "method not found")
	}
}

func (f *fakeUpstream) sessionOK(r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionOKLocked(r)
}

func (f *fakeUpstream) sessionOKLocked(r *http.Request) bool {
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		return f.session == ""
	}
	if f.expired[sid] {
		return false
	}
	return f.session == "" || sid == f.session
}

func (f *fakeUpstream) respond(w http.ResponseWriter, id *int64, result any) {
	msg := map[string]any{"jsonrpc": "2.0", "id": idValue(id), "result": result}
	payload, _ := json.Marshal(msg)
	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

func (f *fakeUpstream) respondError(w http.ResponseWriter, id *int64, code int, msg string) {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": idValue(id),
		"error": map[string]any{"code": code, "message": msg},
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

func idValue(id *int64) any {
	if id == nil {
		return nil
	}
	return *id
}

func (f *fakeUpstream) stats() (initCount int, calls []fakeCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.initCount, append([]fakeCall(nil), f.calls...)
}

func (f *fakeUpstream) seenRequests() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

func newFake() *fakeUpstream {
	return &fakeUpstream{expired: map[string]bool{}}
}

func testConfig(url string) config.Upstream {
	return config.Upstream{
		Name: "fake", URL: url, Prefix: "fk", Tools: []string{"echo"},
		Timeout: 3 * time.Second, RefreshInterval: time.Hour,
	}
}

func TestClientListAndCall(t *testing.T) {
	fake := newFake()
	fake.requireToken = "s3cret"
	fake.tools = []Tool{{
		Name:        "echo",
		Description: "回显参数",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}}}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	t.Setenv("FAKE_MCP_TOKEN", "s3cret")
	cfg := testConfig(srv.URL)
	cfg.TokenEnv = "FAKE_MCP_TOKEN"
	c := NewClient(cfg)

	ctx := context.Background()
	list, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("拉取工具失败: %v", err)
	}
	tools := list.Tools
	if len(tools) != 1 || tools[0].Name != "echo" || tools[0].Description != "回显参数" {
		t.Fatalf("工具列表不对: %+v", tools)
	}

	raw, err := c.CallTool(ctx, "echo", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	content := res["content"].([]any)[0].(map[string]any)
	if content["text"] != `echo:{"msg":"hi"}` {
		t.Errorf("参数没有原样转发: %v", content)
	}
	if _, ok := res["custom"]; !ok {
		t.Error("客户端应保留上游结果里的未知字段")
	}

	initCount, calls := fake.stats()
	if initCount != 1 {
		t.Errorf("握手应只发生一次，实际 %d", initCount)
	}
	if len(calls) != 1 || calls[0].Session != "sess-1" {
		t.Errorf("调用没有带上会话 id: %+v", calls)
	}
}

// TestClientStoresInstructions 验证 initialize 里的 instructions 被保存下来。
func TestClientStoresInstructions(t *testing.T) {
	fake := newFake()
	fake.instructions = "先调用 list_items"
	fake.tools = []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	c := NewClient(testConfig(srv.URL))
	if got := c.Instructions(); got != "" {
		t.Errorf("握手前说明应为空，实际 %q", got)
	}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Instructions(); got != "先调用 list_items" {
		t.Errorf("说明应来自上游 initialize，实际 %q", got)
	}
}

func TestClientUsesServerProtocolVersion(t *testing.T) {
	fake := newFake()
	fake.protocol = "2025-03-26"
	fake.tools = []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	c := NewClient(testConfig(srv.URL))
	ctx := context.Background()
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CallTool(ctx, "echo", nil); err != nil {
		t.Fatal(err)
	}

	for _, req := range fake.seenRequests() {
		switch req.Method {
		case "initialize":
			if req.Proto != "" {
				t.Errorf("initialize 不应带 MCP-Protocol-Version，实际 %q", req.Proto)
			}
		default:
			if req.Proto != "2025-03-26" {
				t.Errorf("%s 应使用服务端返回的协议版本 2025-03-26，实际 %q", req.Method, req.Proto)
			}
		}
	}
}

func TestClientSSEResponse(t *testing.T) {
	fake := newFake()
	fake.sse = true
	fake.tools = []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	c := NewClient(testConfig(srv.URL))
	ctx := context.Background()
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatalf("SSE 模式下拉取工具失败: %v", err)
	}
	if _, err := c.CallTool(ctx, "echo", nil); err != nil {
		t.Fatalf("SSE 模式下调用失败: %v", err)
	}
}

// TestClientSSEStreamsWithoutClosing 验证客户端读到匹配事件后立即返回，
// 不会一直等到上游关闭连接。
func TestClientSSEStreamsWithoutClosing(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": *req.ID,
				"result": map[string]any{"protocolVersion": "2025-06-18"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\n\n", *req.ID)
			flusher.Flush()
			// 发完事件故意不关连接。
			<-release
		}
	}))
	defer srv.Close()
	defer close(release)

	c := NewClient(testConfig(srv.URL))
	done := make(chan error, 1)
	go func() {
		_, err := c.CallTool(context.Background(), "echo", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SSE 调用失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("客户端应在读到匹配事件后立即返回，而不是等待连接关闭")
	}
}

// TestClientConcurrentSessionExpiry 验证多个请求同时撞上会话过期时，
// 只重建一次会话，且不会互相清掉对方新建的会话。
func TestClientConcurrentSessionExpiry(t *testing.T) {
	fake := newFake()
	fake.tools = []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	fake.expireOnCall = true
	fake.barrierSize = 8
	srv := httptest.NewServer(fake)
	defer srv.Close()

	c := NewClient(testConfig(srv.URL))
	ctx := context.Background()
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatal(err)
	}

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := c.CallTool(ctx, "echo", nil)
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("并发重连后调用应成功: %v", err)
		}
	}
	initCount, calls := fake.stats()
	if initCount != 2 {
		t.Errorf("并发过期只应重连一次（共 2 次握手），实际 %d 次", initCount)
	}
	if len(calls) != n {
		t.Errorf("应有 %d 次成功调用，实际 %d", n, len(calls))
	}
}

// TestCallToolRetriesWhenSessionEmpty 复现旧实现的窄窗口：初始化已完成、
// 但会话被另一个 goroutine 清空，请求带着空会话发出去收到 404。
// 修复后应重连一次并成功，而不是直接报"会话不可用"。
func TestCallToolRetriesWhenSessionEmpty(t *testing.T) {
	fake := newFake()
	fake.tools = []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	c := NewClient(testConfig(srv.URL))
	ctx := context.Background()
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatal(err)
	}

	// 保留 inited，只清会话，等价于并发 invalidateSession 恰好插在
	// EnsureInitialized 与 session() 之间留下的状态。
	c.mu.Lock()
	c.sessionID = ""
	c.mu.Unlock()

	if _, err := c.CallTool(ctx, "echo", nil); err != nil {
		t.Fatalf("空会话收到 404 后应重连重试: %v", err)
	}
	initCount, calls := fake.stats()
	if initCount != 2 {
		t.Errorf("应重连一次（共 2 次握手），实际 %d 次", initCount)
	}
	if len(calls) != 1 || calls[0].Session != "sess-2" {
		t.Errorf("重试应使用新会话，实际 %+v", calls)
	}
}

// TestCallToolSessionInvalidatedBeforeSnapshot 用钩子把并发失效固定在
// "取会话快照之前"，验证请求不会带着空会话发出。
func TestCallToolSessionInvalidatedBeforeSnapshot(t *testing.T) {
	fake := newFake()
	fake.tools = []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	c := NewClient(testConfig(srv.URL))
	ctx := context.Background()
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatal(err)
	}

	var once sync.Once
	c.beforeSession = func() {
		once.Do(func() {
			c.mu.Lock()
			sid := c.sessionID
			c.mu.Unlock()
			c.invalidateSession(sid)
		})
	}
	if _, err := c.CallTool(ctx, "echo", nil); err != nil {
		t.Fatalf("会话在快照前失效时应重新握手: %v", err)
	}
	initCount, calls := fake.stats()
	if initCount != 2 {
		t.Errorf("应重新握手一次，实际 %d 次", initCount)
	}
	if len(calls) != 1 || calls[0].Session != "sess-2" {
		t.Errorf("调用应使用新会话，实际 %+v", calls)
	}
}

// TestCallToolRetryIsBounded 验证空会话下的 404 重试有上限，不会对错误地址无限重连。
func TestCallToolRetryIsBounded(t *testing.T) {
	fake := newFake()
	fake.noSession = true
	fake.rejectCalls = true
	fake.tools = []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	_, err := NewClient(testConfig(srv.URL)).CallTool(context.Background(), "echo", nil)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("持续 404 应返回会话不可用，实际: %v", err)
	}
	initCount, _ := fake.stats()
	if initCount != 2 {
		t.Errorf("最多重连一次（共 2 次握手），实际 %d 次", initCount)
	}
	calls := 0
	for _, req := range fake.seenRequests() {
		if req.Method == "tools/call" {
			calls++
		}
	}
	if calls != 2 {
		t.Errorf("tools/call 最多尝试 2 次，实际 %d 次", calls)
	}
}

func TestClientMissingTokenEnv(t *testing.T) {
	srv := httptest.NewServer(newFake())
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.TokenEnv = "FAKE_MCP_TOKEN_DEFINITELY_UNSET"
	c := NewClient(cfg)
	_, err := c.ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), cfg.TokenEnv) {
		t.Fatalf("环境变量缺失时应给出明确错误，实际: %v", err)
	}
}

func TestClientAuthFailure(t *testing.T) {
	fake := newFake()
	fake.requireToken = "right"
	srv := httptest.NewServer(fake)
	defer srv.Close()

	t.Setenv("FAKE_MCP_TOKEN", "wrong")
	cfg := testConfig(srv.URL)
	cfg.TokenEnv = "FAKE_MCP_TOKEN"
	_, err := NewClient(cfg).ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "鉴权失败") {
		t.Fatalf("401 应报鉴权失败，实际: %v", err)
	}
}

func TestClientInitializedNotificationFailure(t *testing.T) {
	fake := newFake()
	fake.notifyStatus = http.StatusInternalServerError
	srv := httptest.NewServer(fake)
	defer srv.Close()

	_, err := NewClient(testConfig(srv.URL)).ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "initialized") {
		t.Fatalf("initialized 通知失败应报错，实际: %v", err)
	}
}

func TestClientRejectsNonObjectResult(t *testing.T) {
	fake := newFake()
	fake.tools = []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	fake.badResult = true
	srv := httptest.NewServer(fake)
	defer srv.Close()

	_, err := NewClient(testConfig(srv.URL)).CallTool(context.Background(), "echo", nil)
	if err == nil || !strings.Contains(err.Error(), "JSON 对象") {
		t.Fatalf("非对象 result 应报错，实际: %v", err)
	}
}

func TestClientRejectsTooManyTools(t *testing.T) {
	fake := newFake()
	for i := 0; i <= maxToolsPerUpstream; i++ {
		fake.tools = append(fake.tools, Tool{Name: fmt.Sprintf("t%d", i), InputSchema: json.RawMessage(`{"type":"object"}`)})
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	_, err := NewClient(testConfig(srv.URL)).ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("工具数超限应报错，实际: %v", err)
	}
}

func TestClientRejectsEndlessPagination(t *testing.T) {
	fake := newFake()
	fake.paginateForever = true
	srv := httptest.NewServer(fake)
	defer srv.Close()

	_, err := NewClient(testConfig(srv.URL)).ListTools(context.Background())
	if err == nil || !strings.Contains(err.Error(), "分页") {
		t.Fatalf("无限分页应报错，实际: %v", err)
	}
}

// TestClientDropsOversizedToolDefinition 验证单个工具超限时只丢弃该工具：
// 白名单内的记入 Rejected，同页的正常工具仍然返回。
func TestClientDropsOversizedToolDefinition(t *testing.T) {
	cases := []struct {
		name string
		tool Tool
	}{
		{"name", Tool{Name: strings.Repeat("n", maxToolNameBytes+1)}},
		{"title", Tool{Name: "bad", Title: strings.Repeat("t", maxToolTitleBytes+1)}},
		{"description", Tool{Name: "bad", Description: strings.Repeat("d", maxToolDescriptionBytes+1)}},
		{"schema", Tool{Name: "bad", InputSchema: json.RawMessage(`"` + strings.Repeat("s", maxToolSchemaBytes+1) + `"`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake()
			fake.tools = []Tool{
				tc.tool,
				{Name: "echo", Description: "正常", InputSchema: json.RawMessage(`{"type":"object"}`)},
			}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			cfg := testConfig(srv.URL)
			cfg.Tools = []string{"echo", tc.tool.Name}
			list, err := NewClient(cfg).ListTools(context.Background())
			if err != nil {
				t.Fatalf("单个工具超限不应让整次拉取失败: %v", err)
			}
			if len(list.Tools) != 1 || list.Tools[0].Name != "echo" || list.Tools[0].Description != "正常" {
				t.Fatalf("正常工具应保留，实际 %+v", list.Tools)
			}
			reason, ok := list.Rejected[tc.tool.Name]
			if !ok || !strings.Contains(reason, "上限") {
				t.Fatalf("超限工具应记入 Rejected，实际 %+v", list.Rejected)
			}
		})
	}
}

// TestClientDropsOversizedNonWhitelistedTool 验证白名单外的超限工具直接丢弃，不上报。
func TestClientDropsOversizedNonWhitelistedTool(t *testing.T) {
	fake := newFake()
	fake.tools = []Tool{
		{Name: "echo", Description: "正常", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "hidden", Description: strings.Repeat("d", maxToolDescriptionBytes+1)},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	list, err := NewClient(testConfig(srv.URL)).ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "echo" {
		t.Fatalf("白名单外的超限工具应被丢弃，实际 %+v", list.Tools)
	}
	if len(list.Rejected) != 0 {
		t.Fatalf("白名单外的超限工具不应记入 Rejected，实际 %+v", list.Rejected)
	}
}

// TestClientRejectsOversizedToolsListPage 验证 tools/list 单页有独立的上限，
// JSON 与 SSE 两种响应都算体积。
func TestClientRejectsOversizedToolsListPage(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			fake := newFake()
			fake.sse = sse
			fake.tools = []Tool{{
				Name:        "echo",
				Description: strings.Repeat("x", 2<<10),
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			c := NewClient(testConfig(srv.URL))
			c.listPageLimit = 1 << 10
			_, err := c.ListTools(context.Background())
			if err == nil || !strings.Contains(err.Error(), "上限") {
				t.Fatalf("单页超限应报错，实际: %v", err)
			}
		})
	}
}

// TestClientRejectsCumulativeToolsListBytes 验证分页累计超过总上限时报错，
// JSON 与 SSE 两种响应都算体积。
func TestClientRejectsCumulativeToolsListBytes(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprintf("sse=%v", sse), func(t *testing.T) {
			fake := newFake()
			fake.sse = sse
			for i := 0; i < 3; i++ {
				fake.pages = append(fake.pages, []Tool{{
					Name:        "echo",
					Description: strings.Repeat("x", 2<<10),
					InputSchema: json.RawMessage(`{"type":"object"}`),
				}})
			}
			srv := httptest.NewServer(fake)
			defer srv.Close()

			c := NewClient(testConfig(srv.URL))
			c.listPageLimit = 4 << 10
			c.listTotalLimit = 5 << 10
			_, err := c.ListTools(context.Background())
			if err == nil || !strings.Contains(err.Error(), "累计") {
				t.Fatalf("累计超限应报错，实际: %v", err)
			}
		})
	}
}

func TestReadLimitedRejectsOversize(t *testing.T) {
	if _, err := readLimited(strings.NewReader("01234567890"), 10); err == nil {
		t.Fatal("超过上限应报错")
	}
	got, err := readLimited(strings.NewReader("0123456789"), 10)
	if err != nil || string(got) != "0123456789" {
		t.Fatalf("恰好等于上限应通过: %q %v", got, err)
	}
}

func TestRedactURL(t *testing.T) {
	got := redactURL("https://user:pass@example.com:8443/mcp?token=secret#frag")
	if got != "https://example.com:8443/mcp" {
		t.Errorf("URL 应只保留 scheme+host+path，实际 %q", got)
	}
}

func TestClientErrorHidesQuery(t *testing.T) {
	c := NewClient(config.Upstream{URL: "http://127.0.0.1:1/mcp?token=secret", Timeout: 300 * time.Millisecond})
	_, err := c.ListTools(context.Background())
	if err == nil {
		t.Fatal("连接失败应报错")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("错误信息不应带 query: %v", err)
	}
	if !strings.Contains(err.Error(), "http://127.0.0.1:1/mcp") {
		t.Errorf("错误信息应保留 scheme+host+path: %v", err)
	}
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		attempt  int
		interval time.Duration
		want     time.Duration
	}{
		{0, time.Hour, 5 * time.Second},
		{1, time.Hour, 10 * time.Second},
		{3, time.Hour, 40 * time.Second},
		{10, time.Hour, time.Minute},
		{0, 2 * time.Second, 2 * time.Second},
	}
	for _, c := range cases {
		if got := retryDelay(c.attempt, c.interval); got != c.want {
			t.Errorf("retryDelay(%d, %v) = %v，期望 %v", c.attempt, c.interval, got, c.want)
		}
	}
}

func TestManagerDefinitionsAndWhitelist(t *testing.T) {
	fake := newFake()
	fake.tools = []Tool{
		{
			Name:        "echo",
			Title:       "回显",
			Description: "上游描述",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`),
			Annotations: json.RawMessage(`{"readOnlyHint":true}`),
		},
		{Name: "hidden", Description: "不在白名单里"},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	m := New([]config.Upstream{testConfig(srv.URL)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	def := m.Definition("fake", "fk", "echo")
	if def.Name != "fk_echo" || def.Description != "上游描述" || def.Title != "回显" {
		t.Errorf("定义应来自上游: %+v", def)
	}
	schema, ok := def.InputSchema.(json.RawMessage)
	if !ok || !strings.Contains(string(schema), `"msg"`) {
		t.Errorf("inputSchema 应保留上游原文: %#v", def.InputSchema)
	}
	if def.Annotations == nil || def.Annotations.ReadOnlyHint == nil || !*def.Annotations.ReadOnlyHint {
		t.Errorf("annotations 应透传: %+v", def.Annotations)
	}

	// 白名单之外的工具即使上游存在也不会被注册；管理器只按白名单回答。
	if hidden := m.Definition("fake", "fk", "hidden"); hidden.Name != "fk_hidden" || !strings.Contains(hidden.Description, "没有") {
		t.Errorf("非白名单工具应给出占位定义: %+v", hidden)
	}
	if _, err := m.Call(context.Background(), "fake", "hidden", nil); err == nil || !strings.Contains(err.Error(), "白名单") {
		t.Errorf("非白名单工具应拒绝调用，实际: %v", err)
	}

	res, err := m.Call(context.Background(), "fake", "echo", json.RawMessage(`{"msg":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != `echo:{"msg":"x"}` || res.IsError {
		t.Errorf("调用结果不对: %+v", res)
	}
	// 原始 JSON 必须保留未知字段。
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"custom"`) {
		t.Errorf("序列化结果应保留上游原始字段: %s", out)
	}
}

func TestManagerToolAnnotationsOverride(t *testing.T) {
	fake := newFake()
	fake.tools = []Tool{
		{
			Name:        "plain",
			Description: "无 annotations 的只读工具",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		{
			Name:        "partial",
			Description: "带部分 annotations 的工具",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Annotations: json.RawMessage(`{"title":"上游标题","readOnlyHint":false,"openWorldHint":true}`),
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	tr, fa := true, false
	cfg := config.Upstream{
		Name:   "fake",
		URL:    srv.URL,
		Prefix: "fk",
		Tools:  []string{"plain", "partial"},
		ToolAnnotations: map[string]config.UpstreamToolAnnotations{
			"plain": {
				ReadOnlyHint:    &tr,
				DestructiveHint: &fa,
				IdempotentHint:  &tr,
			},
			"partial": {
				ReadOnlyHint:    &tr,
				DestructiveHint: &fa,
			},
		},
		Timeout: 2 * time.Second,
	}
	m := New([]config.Upstream{cfg}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// 刷新前 fallbackDef 也应携带配置的 annotations，避免重启首次 tools/list 被当写工具缓存。
	pre := m.Definition("fake", "fk", "plain")
	if pre.Annotations == nil || pre.Annotations.ReadOnlyHint == nil || !*pre.Annotations.ReadOnlyHint ||
		pre.Annotations.DestructiveHint == nil || *pre.Annotations.DestructiveHint {
		t.Fatalf("刷新前占位定义应套用 tool_annotations，实际: %+v", pre.Annotations)
	}

	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	plain := m.Definition("fake", "fk", "plain")
	if plain.Annotations == nil || plain.Annotations.ReadOnlyHint == nil || !*plain.Annotations.ReadOnlyHint ||
		plain.Annotations.DestructiveHint == nil || *plain.Annotations.DestructiveHint ||
		plain.Annotations.IdempotentHint == nil || !*plain.Annotations.IdempotentHint {
		t.Errorf("plain 应补全配置的 annotations，实际: %+v", plain.Annotations)
	}

	// partial 配置只覆写 read_only 与 destructive，上游原有的 title 与 openWorldHint 应保留。
	partial := m.Definition("fake", "fk", "partial")
	if partial.Annotations == nil || partial.Annotations.Title != "上游标题" ||
		partial.Annotations.ReadOnlyHint == nil || !*partial.Annotations.ReadOnlyHint ||
		partial.Annotations.DestructiveHint == nil || *partial.Annotations.DestructiveHint ||
		partial.Annotations.OpenWorldHint == nil || !*partial.Annotations.OpenWorldHint {
		t.Errorf("partial 应按字段合并 annotations，实际: %+v", partial.Annotations)
	}
}

func TestManagerUnreachableUpstream(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1/mcp")
	cfg.Timeout = 300 * time.Millisecond
	m := New([]config.Upstream{cfg}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.RefreshAll(context.Background()); err == nil {
		t.Fatal("不可达的上游应返回错误")
	}

	def := m.Definition("fake", "fk", "echo")
	if def.Name != "fk_echo" || !strings.Contains(def.Description, "尚未取到") {
		t.Errorf("拉取失败时应给出占位定义: %+v", def)
	}
	if _, err := m.Call(context.Background(), "fake", "echo", nil); err == nil {
		t.Error("上游不可达时调用应报错")
	}
}

func TestManagerRefreshUpdatesDefinitions(t *testing.T) {
	fake := newFake()
	fake.tools = []Tool{{Name: "echo", Description: "第一版", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	m := New([]config.Upstream{testConfig(srv.URL)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.Definition("fake", "fk", "echo").Description; got != "第一版" {
		t.Fatalf("初始描述不对: %q", got)
	}

	fake.mu.Lock()
	fake.tools[0].Description = "第二版"
	fake.mu.Unlock()
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.Definition("fake", "fk", "echo").Description; got != "第二版" {
		t.Errorf("刷新后描述应更新，实际 %q", got)
	}
}

// TestManagerOversizedToolKeepsPreviousDefinition 验证单个工具超限只影响它自己：
// 其余工具照常更新，超限工具沿用上一版定义，且每个坏工具只 warn 一次。
func TestManagerOversizedToolKeepsPreviousDefinition(t *testing.T) {
	fake := newFake()
	fake.tools = []Tool{
		{Name: "echo", Description: "正常", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "other", Description: "其他", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.Tools = []string{"echo", "other"}
	var logs bytes.Buffer
	m := New([]config.Upstream{cfg}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	fake.tools[0].Description = strings.Repeat("x", maxToolDescriptionBytes+1)
	fake.tools[1].Description = "其他第二版"
	fake.mu.Unlock()

	// 超限只跳过坏工具，刷新本身不失败。
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatalf("单个工具超限不应让整次刷新失败: %v", err)
	}
	if got := m.Definition("fake", "fk", "echo").Description; got != "正常" {
		t.Errorf("超限工具应保留上一版定义，实际 %q", got)
	}
	if got := m.Definition("fake", "fk", "other").Description; got != "其他第二版" {
		t.Errorf("其他工具应照常更新，实际 %q", got)
	}

	// 再刷新一次，坏工具仍然超限，不应重复 warn。
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "定义超限"); n != 1 {
		t.Errorf("每个坏工具只应 warn 一次，实际 %d 次:\n%s", n, logs.String())
	}

	// 修复后应恢复上游定义；再次超限时应重新 warn。
	fake.mu.Lock()
	fake.tools[0].Description = "正常"
	fake.mu.Unlock()
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.Definition("fake", "fk", "echo").Description; got != "正常" {
		t.Errorf("修复后应恢复上游定义，实际 %q", got)
	}

	fake.mu.Lock()
	fake.tools[0].Description = strings.Repeat("x", maxToolDescriptionBytes+1)
	fake.mu.Unlock()
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "定义超限"); n != 2 {
		t.Errorf("修复后再次超限应重新 warn，实际 %d 次:\n%s", n, logs.String())
	}
}

// TestManagerOversizedToolWithoutPreviousUsesPlaceholder 验证没有上一版定义时
// 用专门的占位描述说明定义超限，而不是说工具不在上游列表里。
func TestManagerOversizedToolWithoutPreviousUsesPlaceholder(t *testing.T) {
	fake := newFake()
	fake.tools = []Tool{{
		Name:        "echo",
		Title:       strings.Repeat("t", maxToolTitleBytes+1),
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	m := New([]config.Upstream{testConfig(srv.URL)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatalf("单个工具超限不应让整次刷新失败: %v", err)
	}
	def := m.Definition("fake", "fk", "echo")
	if def.Name != "fk_echo" {
		t.Errorf("占位定义应带前缀，实际 %q", def.Name)
	}
	if !strings.Contains(def.Description, "超过大小上限") || !strings.Contains(def.Description, "原样转发") {
		t.Errorf("占位描述应说明定义超限且调用仍会转发，实际 %q", def.Description)
	}
	if strings.Contains(def.Description, "没有") {
		t.Errorf("占位描述不应说工具不在列表里，实际 %q", def.Description)
	}
}

// TestManagerUnknownUpstreamPlaceholderKeepsPrefix 验证未知上游的占位名也带前缀。
func TestManagerUnknownUpstreamPlaceholderKeepsPrefix(t *testing.T) {
	m := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	def := m.Definition("missing", "pre", "echo")
	if def.Name != "pre_echo" {
		t.Errorf("未知上游的占位名应带前缀，实际 %q", def.Name)
	}
}

func TestManagerStartAndClose(t *testing.T) {
	fake := newFake()
	fake.tools = []Tool{{Name: "echo", Description: "ready", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	m := New([]config.Upstream{testConfig(srv.URL)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Start()
	defer m.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.Definition("fake", "fk", "echo").Description == "ready" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("后台刷新没有在启动后拉到工具定义")
}

// TestManagerInstructions 验证开启 instructions 的上游在握手后追加说明段落，
// 段头带上游名与工具前缀；未开启或还没连上的上游不出现。
func TestManagerInstructions(t *testing.T) {
	fake := newFake()
	fake.instructions = "上游使用说明"
	fake.tools = []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.Instructions = true
	var logs bytes.Buffer
	m := New([]config.Upstream{cfg}, slog.New(slog.NewTextHandler(&logs, nil)))
	defer m.Close()

	if got := m.Instructions(); got != "" {
		t.Errorf("上游还没连上时应跳过，实际 %q", got)
	}
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := m.Instructions()
	for _, want := range []string{
		"## 上游 fake 的说明（工具名前缀 fk_）",
		"上游使用说明",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("说明应包含 %q，实际:\n%s", want, got)
		}
	}

	// 关闭开关后不再追加。
	cfg.Instructions = false
	m2 := New([]config.Upstream{cfg}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m2.Close()
	if err := m2.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m2.Instructions(); got != "" {
		t.Errorf("instructions 关闭时不应追加，实际 %q", got)
	}
}

// TestManagerInstructionsTruncated 验证单段说明超过上限时截断并记日志，
// 且截断不破坏 UTF-8。
func TestManagerInstructionsTruncated(t *testing.T) {
	fake := newFake()
	fake.instructions = strings.Repeat("长", maxInstructionsPerUpstream)
	fake.tools = []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.Instructions = true
	var logs bytes.Buffer
	m := New([]config.Upstream{cfg}, slog.New(slog.NewTextHandler(&logs, nil)))
	defer m.Close()
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := m.Instructions()
	if !strings.Contains(got, "…") {
		t.Error("超长说明应带截断标记")
	}
	if !utf8.ValidString(got) {
		t.Error("截断后的说明必须是合法 UTF-8")
	}
	if len(got) > maxInstructionsPerUpstream+200 {
		t.Errorf("截断后仍过长: %d 字节", len(got))
	}
	if !strings.Contains(logs.String(), "已截断") {
		t.Errorf("截断应记日志:\n%s", logs.String())
	}
}
