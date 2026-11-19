package upstream

import (
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

	"github.com/noir017/runabout/internal/config"
)

// fakeUpstream 是一个最小的 MCP Streamable HTTP 上游，用来验证客户端行为。
type fakeUpstream struct {
	mu              sync.Mutex
	tools           []Tool
	requireToken    string
	sse             bool
	protocol        string
	notifyStatus    int
	badResult       bool
	paginateForever bool

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
		f.session = fmt.Sprintf("sess-%d", f.sessionSeq)
		sid := f.session
		f.initCount++
		proto := f.protocol
		if proto == "" {
			proto = "2025-06-18"
		}
		f.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", sid)
		f.respond(w, req.ID, map[string]any{
			"protocolVersion": proto,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fake", "version": "1"},
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
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("拉取工具失败: %v", err)
	}
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

	def := m.Definition("fake", "echo")
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
	if hidden := m.Definition("fake", "hidden"); hidden.Name != "fk_hidden" || !strings.Contains(hidden.Description, "没有") {
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

func TestManagerUnreachableUpstream(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1/mcp")
	cfg.Timeout = 300 * time.Millisecond
	m := New([]config.Upstream{cfg}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.RefreshAll(context.Background()); err == nil {
		t.Fatal("不可达的上游应返回错误")
	}

	def := m.Definition("fake", "echo")
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
	if got := m.Definition("fake", "echo").Description; got != "第一版" {
		t.Fatalf("初始描述不对: %q", got)
	}

	fake.mu.Lock()
	fake.tools[0].Description = "第二版"
	fake.mu.Unlock()
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.Definition("fake", "echo").Description; got != "第二版" {
		t.Errorf("刷新后描述应更新，实际 %q", got)
	}
}

// TestManagerRejectsOversizedDefinition 验证超限时整次刷新失败，
// 保留上一版定义而不是截断。
func TestManagerRejectsOversizedDefinition(t *testing.T) {
	fake := newFake()
	fake.tools = []Tool{{Name: "echo", Description: "正常", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	m := New([]config.Upstream{testConfig(srv.URL)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	fake.tools[0].Description = strings.Repeat("x", maxToolDescriptionBytes+1)
	fake.mu.Unlock()
	if err := m.RefreshAll(context.Background()); err == nil {
		t.Fatal("description 超限应报错")
	}
	if got := m.Definition("fake", "echo").Description; got != "正常" {
		t.Errorf("刷新失败时应保留上一版定义，实际 %q", got)
	}

	fake.mu.Lock()
	fake.tools[0].Description = "正常"
	fake.tools[0].InputSchema = json.RawMessage(`"` + strings.Repeat("x", maxToolSchemaBytes+1) + `"`)
	fake.mu.Unlock()
	if err := m.RefreshAll(context.Background()); err == nil || !strings.Contains(err.Error(), "inputSchema") {
		t.Errorf("inputSchema 超限应报错，实际: %v", err)
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
		if m.Definition("fake", "echo").Description == "ready" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("后台刷新没有在启动后拉到工具定义")
}
