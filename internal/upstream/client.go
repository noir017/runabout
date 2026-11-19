// Package upstream 实现上游 MCP 服务（Streamable HTTP）的透传：
// 初始化握手、工具列表拉取与调用转发。runabout 不解释上游的业务语义，
// 只负责把白名单里的工具暴露出去并原样转发结果。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/mcp"
)

// maxResponseBytes 限制单次上游响应体大小，防止异常上游把内存打满。
const maxResponseBytes = 32 << 20

// Tool 是上游 tools/list 返回的工具定义（保留原始 schema 与 annotations）。
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// Client 是一个上游 MCP 端点的最小客户端：JSON-RPC over Streamable HTTP。
type Client struct {
	url      string
	tokenEnv string
	hc       *http.Client

	mu        sync.Mutex
	sessionID string
	proto     string
	inited    bool

	nextID atomic.Int64
}

func NewClient(cfg config.Upstream) *Client {
	return &Client{
		url:      cfg.URL,
		tokenEnv: cfg.TokenEnv,
		hc:       &http.Client{},
	}
}

// ListTools 拉取上游全部工具，自动跟随 nextCursor 分页。
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if err := c.EnsureInitialized(ctx); err != nil {
		return nil, err
	}
	var all []Tool
	cursor := ""
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
	return all, nil
}

// CallTool 转发 tools/call，返回上游的原始 result 对象。
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if err := c.EnsureInitialized(ctx); err != nil {
		return nil, err
	}
	arguments := args
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	params := map[string]any{"name": name, "arguments": arguments}
	var raw json.RawMessage
	if err := c.call(ctx, "tools/call", params, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("上游返回了空结果")
	}
	return raw, nil
}

// EnsureInitialized 完成 initialize 握手并发送 initialized 通知。
// 并发调用安全，成功后只握手一次。
func (c *Client) EnsureInitialized(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inited {
		return nil
	}
	return c.initializeLocked(ctx)
}

func (c *Client) initializeLocked(ctx context.Context) error {
	id := c.nextID.Add(1)
	params := map[string]any{
		"protocolVersion": mcp.ProtocolLatest,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "runabout", "version": "1.0"},
	}
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: &id, Method: "initialize", Params: params})
	if err != nil {
		return err
	}
	body, status, hdr, err := c.send(ctx, payload, "", "")
	if err != nil {
		return fmt.Errorf("连接上游失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return httpStatusError(status, body)
	}
	if _, err := decodeResult(body, hdr.Get("Content-Type"), id); err != nil {
		return err
	}
	sessionID := hdr.Get("Mcp-Session-Id")
	proto := mcp.ProtocolLatest

	note, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	if err != nil {
		return err
	}
	if _, _, _, err := c.send(ctx, note, sessionID, proto); err != nil {
		return fmt.Errorf("发送 initialized 通知失败: %w", err)
	}

	c.sessionID = sessionID
	c.proto = proto
	c.inited = true
	return nil
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	return c.callRetry(ctx, method, params, out, true)
}

func (c *Client) callRetry(ctx context.Context, method string, params any, out any, mayRetry bool) error {
	sid, proto := c.session()
	id := c.nextID.Add(1)
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return err
	}
	body, status, hdr, err := c.send(ctx, payload, sid, proto)
	if err != nil {
		return fmt.Errorf("连接上游失败: %w", err)
	}
	// 会话过期时上游按规范回 404：重新握手后重试一次。
	if status == http.StatusNotFound && sid != "" && mayRetry {
		c.invalidateSession()
		if err := c.EnsureInitialized(ctx); err != nil {
			return err
		}
		return c.callRetry(ctx, method, params, out, false)
	}
	if status < 200 || status >= 300 {
		return httpStatusError(status, body)
	}
	raw, err := decodeResult(body, hdr.Get("Content-Type"), id)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("上游结果无法解析: %w", err)
	}
	return nil
}

func (c *Client) session() (sid, proto string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID, c.proto
}

func (c *Client) invalidateSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionID = ""
	c.proto = ""
	c.inited = false
}

// token 每次请求都从环境变量读取，避免把令牌存进配置或日志。
func (c *Client) token() (string, error) {
	if c.tokenEnv == "" {
		return "", nil
	}
	tok := strings.TrimSpace(os.Getenv(c.tokenEnv))
	if tok == "" {
		return "", fmt.Errorf("环境变量 %s 为空，无法向上游发送 Bearer 令牌", c.tokenEnv)
	}
	return tok, nil
}

func (c *Client) send(ctx context.Context, payload []byte, sid, proto string) ([]byte, int, http.Header, error) {
	tok, err := c.token()
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	if proto != "" {
		req.Header.Set("MCP-Protocol-Version", proto)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, 0, nil, err
	}
	return body, resp.StatusCode, resp.Header, nil
}

func decodeResult(body []byte, contentType string, id int64) (json.RawMessage, error) {
	resp, err := findResponse(body, contentType, id)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("上游返回错误 %d: %s", resp.Error.Code, resp.Error.Message)
	}
	if len(resp.Result) == 0 {
		return nil, fmt.Errorf("上游响应缺少 result")
	}
	return resp.Result, nil
}

// findResponse 同时支持普通 JSON 与 SSE 两种响应体（Streamable HTTP 允许后者）。
func findResponse(body []byte, contentType string, id int64) (*rpcResponse, error) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return findSSEResponse(body, id)
	}
	var resp rpcResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("上游响应不是合法 JSON: %w", err)
	}
	if !resp.matches(id) {
		return nil, fmt.Errorf("上游响应的 id 与请求不匹配")
	}
	return &resp, nil
}

func findSSEResponse(body []byte, id int64) (*rpcResponse, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64<<10), maxResponseBytes)
	var data []string
	flush := func() (*rpcResponse, bool) {
		if len(data) == 0 {
			return nil, false
		}
		payload := strings.Join(data, "\n")
		data = nil
		var resp rpcResponse
		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			return nil, false
		}
		if resp.matches(id) {
			return &resp, true
		}
		return nil, false
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if resp, ok := flush(); ok {
				return resp, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if resp, ok := flush(); ok {
		return resp, nil
	}
	return nil, fmt.Errorf("SSE 响应里没有找到对应的 JSON-RPC 结果")
}

func (r *rpcResponse) matches(id int64) bool {
	s := strings.Trim(strings.TrimSpace(string(r.ID)), `"`)
	return s == strconv.FormatInt(id, 10)
}

func httpStatusError(status int, body []byte) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("上游鉴权失败（HTTP %d），检查 token_env 指向的令牌", status)
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	if msg == "" {
		return fmt.Errorf("上游返回 HTTP %d", status)
	}
	return fmt.Errorf("上游返回 HTTP %d: %s", status, msg)
}
