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
	mu           sync.Mutex
	tools        []Tool
	requireToken string
	sse          bool
	session      string
	sessionSeq   int
	initCount    int
	calls        []fakeCall
	expireOnCall bool
}

type fakeCall struct {
	Name    string
	Args    json.RawMessage
	Session string
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
	switch req.Method {
	case "initialize":
		f.mu.Lock()
		f.sessionSeq++
		f.session = fmt.Sprintf("sess-%d", f.sessionSeq)
		sid := f.session
		f.initCount++
		f.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", sid)
		f.respond(w, req.ID, map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "fake", "version": "1"},
		})
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		f.mu.Lock()
		valid := f.session == "" || r.Header.Get("Mcp-Session-Id") == f.session
		tools := append([]Tool(nil), f.tools...)
		f.mu.Unlock()
		if !valid {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.respond(w, req.ID, map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)

		f.mu.Lock()
		expire := f.expireOnCall
		if expire {
			f.expireOnCall = false
			f.session = ""
		}
		valid := f.session == "" || r.Header.Get("Mcp-Session-Id") == f.session
		f.mu.Unlock()
		if expire || !valid {
			w.WriteHeader(http.StatusNotFound)
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

func testConfig(url string) config.Upstream {
	return config.Upstream{
		Name: "fake", URL: url, Prefix: "fk", Tools: []string{"echo"},
		Timeout: 3 * time.Second, RefreshInterval: time.Hour,
	}
}

func TestClientListAndCall(t *testing.T) {
	fake := &fakeUpstream{
		requireToken: "s3cret",
		tools: []Tool{{
			Name:        "echo",
			Description: "回显参数",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}}}`),
		}},
	}
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

func TestClientSSEResponse(t *testing.T) {
	fake := &fakeUpstream{
		sse:   true,
		tools: []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
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

func TestClientReinitializesOnSessionExpiry(t *testing.T) {
	fake := &fakeUpstream{
		tools:        []Tool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		expireOnCall: true,
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	c := NewClient(testConfig(srv.URL))
	ctx := context.Background()
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CallTool(ctx, "echo", nil); err != nil {
		t.Fatalf("会话过期后应自动重新握手并重试: %v", err)
	}
	initCount, calls := fake.stats()
	if initCount != 2 {
		t.Errorf("应重新握手一次，实际握手 %d 次", initCount)
	}
	if len(calls) != 1 || calls[0].Session != "sess-2" {
		t.Errorf("重试应使用新会话: %+v", calls)
	}
}

func TestClientMissingTokenEnv(t *testing.T) {
	srv := httptest.NewServer(&fakeUpstream{})
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
	fake := &fakeUpstream{requireToken: "right"}
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

func TestManagerDefinitionsAndWhitelist(t *testing.T) {
	fake := &fakeUpstream{
		tools: []Tool{
			{
				Name:        "echo",
				Title:       "回显",
				Description: "上游描述",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}},"required":["msg"]}`),
				Annotations: json.RawMessage(`{"readOnlyHint":true}`),
			},
			{Name: "hidden", Description: "不在白名单里"},
		},
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
	fake := &fakeUpstream{
		tools: []Tool{{Name: "echo", Description: "第一版", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
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

func TestManagerStartAndClose(t *testing.T) {
	fake := &fakeUpstream{tools: []Tool{{Name: "echo", Description: "ready", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
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
