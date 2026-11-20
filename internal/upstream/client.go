// Package upstream 实现上游 MCP 服务（Streamable HTTP）的透传：
// 初始化握手、工具列表拉取与调用转发。runabout 不解释上游的业务语义，
// 只负责把白名单里的工具暴露出去并原样转发结果。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/mcp"
)

const (
	// maxResponseBytes 限制单次上游响应体大小，防止异常上游把内存打满。
	maxResponseBytes = 32 << 20
	// maxToolsListPageBytes 限制 tools/list 单页响应体大小，比通用响应上限更小。
	maxToolsListPageBytes = 4 << 20
	// maxToolsListBytes 限制一次 tools/list 所有分页响应体的累计大小。
	maxToolsListBytes = 32 << 20
	// maxToolsPerUpstream 限制一个上游返回的工具总数。
	maxToolsPerUpstream = 1000
	// maxToolNameBytes 限制单个工具名的大小；配置里的工具名也不超过这个长度。
	maxToolNameBytes = 128
	// maxToolTitleBytes 限制单个工具标题的大小。
	maxToolTitleBytes = 1 << 10
	// maxToolDescriptionBytes 限制单个工具描述的大小。
	maxToolDescriptionBytes = 64 << 10
	// maxToolSchemaBytes 限制单个工具 inputSchema 的大小。
	maxToolSchemaBytes = 256 << 10
	// maxToolPages 限制 tools/list 的分页页数，超过即报错，不静默截断。
	maxToolPages = 20
)

// Tool 是上游 tools/list 返回的工具定义（保留原始 schema 与 annotations）。
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

// ToolList 是一次 tools/list 的结果。
// Rejected 记录白名单里因超过单工具上限而被丢弃的工具名与原因；
// 白名单之外的工具超限时直接丢弃，不在这里记录。
type ToolList struct {
	Tools    []Tool
	Rejected map[string]string
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
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// client 是 Manager 使用的最小上游客户端接口。HTTP 与 stdio 各实现一份，
// Manager 不关心底层传输方式。
type client interface {
	ListTools(ctx context.Context) (ToolList, error)
	CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
	// Instructions 返回上游 initialize 里带的说明；还没握手成功时为空。
	Instructions() string
	// Close 释放传输资源；对 HTTP 客户端是空操作。
	Close()
}

// Client 是一个上游 MCP 端点的最小客户端：JSON-RPC over Streamable HTTP。
type Client struct {
	url      string
	safeURL  string
	tokenEnv string
	hc       *http.Client

	// allowed 是配置里的工具白名单，用于判断超限工具是否需要上报给 Manager。
	allowed map[string]bool
	// listPageLimit / listTotalLimit 是 tools/list 单页与累计响应体上限，
	// 默认取常量，测试可以调小。
	listPageLimit  int64
	listTotalLimit int64
	// beforeSession 仅用于测试：在取会话快照前调用，用来固定并发失效的时序。
	beforeSession func()

	// mu 同时保护会话状态与握手过程：同一时刻只有一个调用在握手，
	// 其余调用等锁后直接复用结果，等价于一次 singleflight 重连。
	mu        sync.Mutex
	sessionID string
	proto     string
	inited    bool
	// instructions 是最近一次 initialize 返回的说明。单独用原子值保存，
	// 避免握手期间取说明被 mu 阻塞。
	instructions atomic.Value // string

	nextID atomic.Int64
}

func NewClient(cfg config.Upstream) *Client {
	allowed := make(map[string]bool, len(cfg.Tools))
	for _, name := range cfg.Tools {
		allowed[name] = true
	}
	return &Client{
		url:            cfg.URL,
		safeURL:        redactURL(cfg.URL),
		tokenEnv:       cfg.TokenEnv,
		hc:             &http.Client{},
		allowed:        allowed,
		listPageLimit:  maxToolsListPageBytes,
		listTotalLimit: maxToolsListBytes,
	}
}

// ListTools 拉取上游全部工具，自动跟随 nextCursor 分页。
// 每页响应体有单独上限，所有页的累计大小也有上限；工具数与分页数超过上限时报错。
// 单个工具超过长度上限时只丢弃该工具：白名单内的记录到 Rejected，白名单外的静默丢弃。
func (c *Client) ListTools(ctx context.Context) (ToolList, error) {
	return listTools(ctx, c.allowed, c.listPageLimit, c.listTotalLimit,
		func(ctx context.Context, params map[string]any, limit int64) (json.RawMessage, int64, error) {
			rep, err := c.callRetry(ctx, "tools/list", params, limit, true)
			return rep.raw, rep.size, err
		})
}

// Instructions 返回上游 initialize 里带的说明。
func (c *Client) Instructions() string {
	s, _ := c.instructions.Load().(string)
	return s
}

// Close 对 HTTP 客户端是空操作，保留接口一致性。
func (c *Client) Close() {}

// listTools 按 nextCursor 拉取分页工具列表。call 返回一页的原始 result 与字节数，
// HTTP 与 stdio 客户端共用这套分页、体积与单工具上限逻辑。
func listTools(ctx context.Context, allowed map[string]bool, pageLimit, totalLimit int64,
	call func(context.Context, map[string]any, int64) (json.RawMessage, int64, error)) (ToolList, error) {
	out := ToolList{Rejected: map[string]string{}}
	cursor := ""
	var total int64
	for page := 0; ; page++ {
		if page >= maxToolPages {
			return ToolList{}, fmt.Errorf("工具列表分页超过 %d 页上限", maxToolPages)
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, size, err := call(ctx, params, pageLimit)
		if err != nil {
			return ToolList{}, err
		}
		total += size
		if total > totalLimit {
			return ToolList{}, fmt.Errorf("tools/list 响应累计超过 %d 字节上限", totalLimit)
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return ToolList{}, fmt.Errorf("上游结果无法解析: %w", err)
		}
		for _, t := range res.Tools {
			// 解码一页就立刻检查，避免把超限工具累积到内存里。
			if reason := toolLimitReason(t); reason != "" {
				if allowed[t.Name] {
					out.Rejected[t.Name] = reason
				}
				continue
			}
			if len(out.Tools) >= maxToolsPerUpstream {
				return ToolList{}, fmt.Errorf("上游工具数超过 %d 上限", maxToolsPerUpstream)
			}
			out.Tools = append(out.Tools, t)
		}
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

// toolLimitReason 返回工具定义超限的原因；未超限时返回空字符串。
func toolLimitReason(t Tool) string {
	switch {
	case len(t.Name) > maxToolNameBytes:
		return fmt.Sprintf("name 超过 %d 字节上限", maxToolNameBytes)
	case len(t.Title) > maxToolTitleBytes:
		return fmt.Sprintf("title 超过 %d 字节上限", maxToolTitleBytes)
	case len(t.Description) > maxToolDescriptionBytes:
		return fmt.Sprintf("description 超过 %d 字节上限", maxToolDescriptionBytes)
	case len(t.InputSchema) > maxToolSchemaBytes:
		return fmt.Sprintf("inputSchema 超过 %d 字节上限", maxToolSchemaBytes)
	}
	return ""
}

// CallTool 转发 tools/call，返回上游的原始 result 对象。
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
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

// sessionForCall 在一次加锁内完成"确保已初始化"与"取会话快照"。
// 两步分开做时，并发 invalidateSession 可能正好插在中间，导致请求不带
// Mcp-Session-Id 发出去；合并后这个窗口不存在。
func (c *Client) sessionForCall(ctx context.Context) (sid, proto string, err error) {
	if c.beforeSession != nil {
		c.beforeSession()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.inited {
		if err := c.initializeLocked(ctx); err != nil {
			return "", "", err
		}
	}
	return c.sessionID, c.proto, nil
}

func (c *Client) initializeLocked(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": mcp.ProtocolLatest,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "runabout", "version": "1.0"},
	}
	rep, err := c.request(ctx, "initialize", params, "", "", false, maxResponseBytes)
	if err != nil {
		return err
	}
	if rep.status < 200 || rep.status >= 300 {
		return fmt.Errorf("initialize 返回 HTTP %d", rep.status)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		Instructions    string `json:"instructions"`
	}
	if err := json.Unmarshal(rep.raw, &res); err != nil {
		return fmt.Errorf("initialize 结果无法解析: %w", err)
	}
	// 以服务端返回的版本为准，后续请求与 initialized 通知都带这个版本。
	proto := strings.TrimSpace(res.ProtocolVersion)
	if proto == "" {
		proto = mcp.ProtocolLatest
	}
	sid := rep.header.Get("Mcp-Session-Id")

	if _, err := c.request(ctx, "notifications/initialized", nil, sid, proto, true, maxResponseBytes); err != nil {
		return fmt.Errorf("发送 initialized 通知失败: %w", err)
	}

	c.sessionID = sid
	c.proto = proto
	c.inited = true
	c.instructions.Store(res.Instructions)
	return nil
}

// rpcReply 是一次 JSON-RPC 请求的原始结果与响应元信息。
type rpcReply struct {
	raw    json.RawMessage
	header http.Header
	status int
	size   int64 // 响应体字节数，用于 tools/list 的累计上限
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	rep, err := c.callRetry(ctx, method, params, maxResponseBytes, true)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(rep.raw, out); err != nil {
		return fmt.Errorf("上游结果无法解析: %w", err)
	}
	return nil
}

func (c *Client) callRetry(ctx context.Context, method string, params any, limit int64, mayRetry bool) (rpcReply, error) {
	sid, proto, err := c.sessionForCall(ctx)
	if err != nil {
		return rpcReply{}, err
	}
	rep, err := c.request(ctx, method, params, sid, proto, false, limit)
	if err != nil {
		return rpcReply{}, err
	}
	// 会话过期时上游按规范回 404：只清掉自己用过的那个会话，重新握手后重试一次。
	// 即使快照里的会话为空也重试一次（可能是并发失效刚清空的），重试次数由
	// mayRetry 限制，不会对错误地址无限重连。
	if rep.status == http.StatusNotFound {
		if !mayRetry {
			return rpcReply{}, fmt.Errorf("上游会话不可用（HTTP 404）")
		}
		c.invalidateSession(sid)
		return c.callRetry(ctx, method, params, limit, false)
	}
	return rep, nil
}

// invalidateSession 只在当前会话仍是 stale 时才清空。
// 并发请求会各自收到旧会话的 404，但只有第一个生效，避免把别人刚建好的新会话清掉。
func (c *Client) invalidateSession(stale string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionID != stale {
		return
	}
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

func (c *Client) post(ctx context.Context, payload []byte, sid, proto string) (*http.Response, error) {
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构造上游请求失败: %v", stripURLError(err))
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
		// 标准库的 *url.Error 会把完整 URL 拼进错误；这里只保留安全的地址。
		return nil, fmt.Errorf("连接上游 %s 失败: %v", c.safeURL, stripURLError(err))
	}
	return resp, nil
}

// request 发送一条 JSON-RPC 请求并返回原始 result。
// notify=true 时不解析响应体，只检查 HTTP 状态码。
// limit 是响应体字节上限（notify 固定为 4KB）。
// SSE 响应按流逐行读取，拿到匹配 id 的事件立即返回并关闭 body。
func (c *Client) request(ctx context.Context, method string, params any, sid, proto string, notify bool, limit int64) (rpcReply, error) {
	var idPtr *int64
	if !notify {
		id := c.nextID.Add(1)
		idPtr = &id
	}
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: idPtr, Method: method, Params: params})
	if err != nil {
		return rpcReply{}, err
	}
	resp, err := c.post(ctx, payload, sid, proto)
	if err != nil {
		return rpcReply{}, err
	}
	defer resp.Body.Close()

	if notify {
		body, _ := readLimited(resp.Body, 4<<10)
		rep := rpcReply{header: resp.Header, status: resp.StatusCode, size: int64(len(body))}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return rep, httpStatusError(resp.StatusCode, body)
		}
		return rep, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return rpcReply{header: resp.Header, status: resp.StatusCode, size: int64(len(body))}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := readLimited(resp.Body, 64<<10)
		return rpcReply{header: resp.Header, status: resp.StatusCode, size: int64(len(body))},
			httpStatusError(resp.StatusCode, body)
	}

	var raw json.RawMessage
	var size int64
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		raw, size, err = readSSE(resp.Body, *idPtr, limit)
	} else {
		var body []byte
		body, err = readLimited(resp.Body, limit)
		size = int64(len(body))
		if err == nil {
			raw, err = decodeJSONResult(body, *idPtr)
		}
	}
	rep := rpcReply{raw: raw, header: resp.Header, status: resp.StatusCode, size: size}
	if err != nil {
		return rep, err
	}
	if !isJSONObject(raw) {
		return rep, fmt.Errorf("上游 result 不是 JSON 对象")
	}
	return rep, nil
}

// readLimited 读取至多 limit 字节；超出时返回明确错误，而不是悄悄截断。
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("上游响应超过 %d 字节上限", limit)
	}
	return body, nil
}

func decodeJSONResult(body []byte, id int64) (json.RawMessage, error) {
	var resp rpcResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("上游响应不是合法 JSON: %w", err)
	}
	if !resp.matches(id) {
		return nil, fmt.Errorf("上游响应的 id 与请求不匹配")
	}
	return resp.result()
}

// readSSE 逐行解析 SSE，读到匹配 id 的事件就返回，不等待连接关闭。
// 返回已读取的字节数，用于 tools/list 的累计上限。
func readSSE(r io.Reader, id int64, limit int64) (json.RawMessage, int64, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), int(limit))
	var data []string
	var total int64
	for scanner.Scan() {
		line := scanner.Text()
		total += int64(len(line)) + 1
		if total > limit {
			return nil, total, fmt.Errorf("SSE 响应超过 %d 字节上限", limit)
		}
		if line == "" {
			if raw, ok, err := matchEvent(data, id); err != nil {
				return nil, total, err
			} else if ok {
				return raw, total, nil
			}
			data = nil
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, total, fmt.Errorf("读取 SSE 响应失败: %w", err)
	}
	if raw, ok, err := matchEvent(data, id); err != nil {
		return nil, total, err
	} else if ok {
		return raw, total, nil
	}
	return nil, total, fmt.Errorf("SSE 响应里没有找到对应的 JSON-RPC 结果")
}

func matchEvent(data []string, id int64) (json.RawMessage, bool, error) {
	if len(data) == 0 {
		return nil, false, nil
	}
	var resp rpcResponse
	if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &resp); err != nil {
		return nil, false, nil
	}
	if !resp.matches(id) {
		return nil, false, nil
	}
	raw, err := resp.result()
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func (r *rpcResponse) result() (json.RawMessage, error) {
	if r.Error != nil {
		return nil, fmt.Errorf("上游返回错误 %d: %s", r.Error.Code, r.Error.Message)
	}
	if len(r.Result) == 0 {
		return nil, fmt.Errorf("上游响应缺少 result")
	}
	return r.Result, nil
}

func (r *rpcResponse) matches(id int64) bool {
	s := strings.Trim(strings.TrimSpace(string(r.ID)), `"`)
	return s == strconv.FormatInt(id, 10)
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
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

// stripURLError 去掉 *url.Error 里拼接的完整 URL，避免把 query 等敏感信息带进日志。
func stripURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// redactURL 只保留 scheme、host 与 path，用于日志与错误信息。
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid url)"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	u.ForceQuery = false
	return u.String()
}
