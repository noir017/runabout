package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubSlowTool struct {
	delay time.Duration
}

func (s stubSlowTool) Definition() ToolDef {
	return ToolDef{
		Name:        "slow_echo",
		Description: "test tool",
		InputSchema: map[string]any{"type": "object"},
	}
}

func (s stubSlowTool) Call(cc *CallContext, _ json.RawMessage) (*CallToolResult, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-cc.Ctx.Done():
			return nil, cc.Ctx.Err()
		}
	}
	return &CallToolResult{Content: []Content{{Type: "text", Text: "ok"}}}, nil
}

func newTestTransport(t *testing.T, delay, interval time.Duration, toolDelay time.Duration) *httptest.Server {
	t.Helper()
	srv := NewServer(Implementation{Name: "test", Version: "0.0.1"}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.Register(stubSlowTool{delay: toolDelay})
	tr := NewHTTPTransport(srv, HTTPOptions{
		SSEKeepaliveDelay:    delay,
		SSEKeepaliveInterval: interval,
		Log:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(tr)
	t.Cleanup(ts.Close)
	return ts
}

func TestHandlePostFastReturnsJSON(t *testing.T) {
	ts := newTestTransport(t, 200*time.Millisecond, 50*time.Millisecond, 0)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow_echo","arguments":{}}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("期望快速请求返回 application/json，实际 %q", ct)
	}
	var rpcResp Response
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("解码 JSON 响应失败: %v", err)
	}
	if rpcResp.Error != nil {
		t.Fatalf("不应返回错误: %v", rpcResp.Error)
	}
}

func TestHandlePostSlowUpgradesToSSEKeepalive(t *testing.T) {
	ts := newTestTransport(t, 30*time.Millisecond, 25*time.Millisecond, 120*time.Millisecond)
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow_echo","arguments":{}}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("期望慢请求切为 text/event-stream，实际 %q", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, ": keepalive\n\n") {
		t.Fatalf("期望包含 SSE 心跳注释帧，实际:\n%s", text)
	}
	idx := strings.Index(text, "event: message\ndata: ")
	if idx < 0 {
		t.Fatalf("期望包含 event: message 帧，实际:\n%s", text)
	}
	dataLine := strings.TrimPrefix(text[idx:], "event: message\ndata: ")
	dataLine = strings.SplitN(dataLine, "\n", 2)[0]
	var rpcResp Response
	if err := json.Unmarshal([]byte(dataLine), &rpcResp); err != nil {
		t.Fatalf("解析 SSE data JSON 失败: %v\nraw=%s", err, dataLine)
	}
	if string(rpcResp.ID) != "2" || rpcResp.Error != nil {
		t.Fatalf("意外响应: id=%s err=%v", rpcResp.ID, rpcResp.Error)
	}
}

func TestHandlePostSlowWithoutAcceptSSEStaysJSON(t *testing.T) {
	ts := newTestTransport(t, 20*time.Millisecond, 20*time.Millisecond, 70*time.Millisecond)
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"slow_echo","arguments":{}}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("客户端未声明接受 SSE 时应保持 application/json，实际 %q", ct)
	}
}

func TestHandlePostStreamingClientCancel(t *testing.T) {
	ts := newTestTransport(t, 20*time.Millisecond, 20*time.Millisecond, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	body := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"slow_echo","arguments":{}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
}
