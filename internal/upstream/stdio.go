package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/mcp"
)

const (
	// defaultShutdownWait 是关停时等待子进程处理完在飞请求的时间。
	defaultShutdownWait = 5 * time.Second
	// maxStderrLineBytes 限制单条 stderr 日志的长度，避免上游刷屏。
	maxStderrLineBytes = 4 << 10
)

// stdioClient 是一个 stdio 上游的最小客户端：按行分隔的 JSON-RPC，
// 所有请求共用一条 stdin/stdout，并发多路复用并按 id 匹配响应。
type stdioClient struct {
	cfg     config.Upstream
	log     *slog.Logger
	allowed map[string]bool

	// startMu 串行化"启动进程 + initialize"，保证同一时刻只有一个进程在握手。
	startMu sync.Mutex

	mu           sync.Mutex
	proc         *stdioProc
	instructions string
	// retryAt 是下一次允许重启进程的时间，按 retryDelay 退避。
	retryAt   time.Time
	failCount int
	closed    bool

	shutdownWait time.Duration
}

func newStdioClient(cfg config.Upstream, log *slog.Logger) *stdioClient {
	if log == nil {
		log = slog.Default()
	}
	allowed := make(map[string]bool, len(cfg.Tools))
	for _, name := range cfg.Tools {
		allowed[name] = true
	}
	return &stdioClient{
		cfg:          cfg,
		log:          log,
		allowed:      allowed,
		shutdownWait: defaultShutdownWait,
	}
}

func (c *stdioClient) ListTools(ctx context.Context) (ToolList, error) {
	return listTools(ctx, c.allowed, maxToolsListPageBytes, maxToolsListBytes,
		func(ctx context.Context, params map[string]any, limit int64) (json.RawMessage, int64, error) {
			p, err := c.session(ctx)
			if err != nil {
				return nil, 0, err
			}
			return p.request(ctx, "tools/list", params, limit)
		})
}

func (c *stdioClient) CallTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	arguments := args
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	p, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	params := map[string]any{"name": name, "arguments": arguments}
	raw, _, err := p.request(ctx, "tools/call", params, maxResponseBytes)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("上游返回了空结果")
	}
	if !isJSONObject(raw) {
		return nil, fmt.Errorf("上游 result 不是 JSON 对象")
	}
	return raw, nil
}

func (c *stdioClient) Instructions() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.instructions
}

// Close 关停子进程：先关 stdin 让它处理完在飞请求，超时再杀进程组。
func (c *stdioClient) Close() {
	// 与启动流程互斥，避免关停的同时又拉起一个新进程。
	c.startMu.Lock()
	defer c.startMu.Unlock()

	c.mu.Lock()
	p := c.proc
	c.proc = nil
	c.closed = true
	c.mu.Unlock()
	if p != nil {
		p.shutdown(c.shutdownWait)
	}
}

// session 返回可用的子进程；进程不在或刚崩溃时按退避策略重启并重新 initialize。
func (c *stdioClient) session(ctx context.Context) (*stdioProc, error) {
	c.startMu.Lock()
	defer c.startMu.Unlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("上游 %s 已关闭", c.cfg.Name)
	}
	p := c.proc
	retryAt := c.retryAt
	c.mu.Unlock()

	if p != nil && !p.isDead() {
		return p, nil
	}
	if p != nil && retryAt.IsZero() {
		// 第一次观察到进程退出：从退出时刻起按 retryDelay 退避后再重启。
		delay := retryDelay(c.failCount, c.cfg.RefreshInterval)
		retryAt = p.diedAtTime().Add(delay)
		c.failCount++
		c.mu.Lock()
		c.retryAt = retryAt
		c.mu.Unlock()
		c.log.Warn("上游进程退出", "upstream", c.cfg.Name, "err", p.err(), "retry_in", delay)
	}
	if wait := time.Until(retryAt); wait > 0 {
		return nil, fmt.Errorf("上游 %s 的进程已退出，%s 后重试", c.cfg.Name, wait.Round(time.Millisecond))
	}

	p, err := c.start(ctx)
	if err != nil {
		delay := retryDelay(c.failCount, c.cfg.RefreshInterval)
		c.failCount++
		c.mu.Lock()
		c.retryAt = time.Now().Add(delay)
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Lock()
	c.proc = p
	c.retryAt = time.Time{}
	c.failCount = 0
	c.instructions = p.instructions()
	c.mu.Unlock()
	return p, nil
}

func (c *stdioClient) start(ctx context.Context) (*stdioProc, error) {
	cmd := exec.Command(c.cfg.Command[0], c.cfg.Command[1:]...)
	cmd.Dir = c.cfg.Workdir
	cmd.Env = childEnv(c.cfg.Env)
	// 独立进程组：关停时可以连同子进程派生的进程一起杀掉。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("创建上游 stdin 失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("创建上游 stdout 失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("创建上游 stderr 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动上游进程 %s 失败: %w", c.cfg.Name, err)
	}

	p := &stdioProc{
		name:    c.cfg.Name,
		log:     c.log,
		cmd:     cmd,
		stdin:   stdin,
		pending: map[int64]chan rpcResult{},
		exited:  make(chan struct{}),
	}
	go p.waitLoop()
	go p.readLoop(stdout)
	go p.logStderr(stderr)

	if err := p.initialize(ctx); err != nil {
		p.kill()
		p.waitExit(defaultShutdownWait)
		return nil, err
	}
	return p, nil
}

// childEnv 构造子进程环境：只保留 PATH、HOME、LANG，再叠加配置里的字面值。
func childEnv(extra map[string]string) []string {
	env := make(map[string]string, len(extra)+3)
	for _, name := range []string{"PATH", "HOME", "LANG"} {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	for name, value := range extra {
		env[name] = value
	}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+env[name])
	}
	return out
}

// rpcResult 是一条响应或一次失败。
type rpcResult struct {
	raw json.RawMessage
	err error
}

// stdioProc 是一个运行中的上游子进程。
type stdioProc struct {
	name  string
	log   *slog.Logger
	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu sync.Mutex

	mu       sync.Mutex
	pending  map[int64]chan rpcResult
	dead     bool
	deathErr error
	diedAt   time.Time
	instr    string

	failOnce sync.Once
	// exited 在进程被回收后关闭。
	exited chan struct{}
	nextID atomic.Int64
}

func (p *stdioProc) isDead() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dead
}

func (p *stdioProc) err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deathErr
}

func (p *stdioProc) diedAtTime() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.diedAt
}

func (p *stdioProc) instructions() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.instr
}

// fail 把进程标记为已退出，并让所有在飞请求立即失败。
func (p *stdioProc) fail(err error) {
	p.failOnce.Do(func() {
		p.mu.Lock()
		p.dead = true
		p.deathErr = err
		p.diedAt = time.Now()
		pending := p.pending
		p.pending = nil
		p.mu.Unlock()
		for _, ch := range pending {
			ch <- rpcResult{err: err}
		}
	})
}

func (p *stdioProc) waitLoop() {
	err := p.cmd.Wait()
	reason := "exit 0"
	if err != nil {
		reason = err.Error()
	}
	p.fail(fmt.Errorf("上游进程已退出（%s）", reason))
	close(p.exited)
}

func (p *stdioProc) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxResponseBytes)
	for scanner.Scan() {
		p.handleLine(scanner.Bytes())
	}
	if err := scanner.Err(); err != nil {
		p.log.Warn("读取上游输出失败", "upstream", p.name, "err", err)
		// 行过长或读错误说明帧格式已不可恢复，直接杀掉，让上层按退避重启。
		p.kill()
	}
}

func (p *stdioProc) handleLine(line []byte) {
	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		p.log.Debug("忽略无法解析的上游输出", "upstream", p.name, "err", err)
		return
	}
	// 上游发来的通知或服务端请求，本客户端不处理。
	if len(resp.ID) == 0 || resp.Method != "" {
		return
	}
	id, ok := parseRPCID(resp.ID)
	if !ok {
		return
	}
	p.mu.Lock()
	ch := p.pending[id]
	delete(p.pending, id)
	p.mu.Unlock()
	if ch == nil {
		return
	}
	raw, err := resp.result()
	ch <- rpcResult{raw: raw, err: err}
}

func parseRPCID(raw json.RawMessage) (int64, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil
}

// request 发送一条 JSON-RPC 请求并等待匹配 id 的响应。
// limit 是单条响应的大小上限，超限按错误处理。
func (p *stdioProc) request(ctx context.Context, method string, params any, limit int64) (json.RawMessage, int64, error) {
	id := p.nextID.Add(1)
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return nil, 0, err
	}

	ch := make(chan rpcResult, 1)
	p.mu.Lock()
	if p.dead {
		err := p.deathErr
		p.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("上游进程已退出")
		}
		return nil, 0, err
	}
	p.pending[id] = ch
	p.mu.Unlock()

	if err := p.write(payload); err != nil {
		p.removePending(id)
		err = fmt.Errorf("写入上游进程失败: %w", err)
		p.fail(err)
		return nil, 0, err
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return nil, 0, res.err
		}
		size := int64(len(res.raw))
		if size > limit {
			return nil, 0, fmt.Errorf("上游响应超过 %d 字节上限", limit)
		}
		return res.raw, size, nil
	case <-ctx.Done():
		p.removePending(id)
		return nil, 0, fmt.Errorf("等待上游 %s 响应超时: %w", method, ctx.Err())
	}
}

func (p *stdioProc) removePending(id int64) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

func (p *stdioProc) write(payload []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	buf := make([]byte, 0, len(payload)+1)
	buf = append(buf, payload...)
	buf = append(buf, '\n')
	_, err := p.stdin.Write(buf)
	return err
}

func (p *stdioProc) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": mcp.ProtocolLatest,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "runabout", "version": "1.0"},
	}
	raw, _, err := p.request(ctx, "initialize", params, maxResponseBytes)
	if err != nil {
		return fmt.Errorf("initialize 失败: %w", err)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		Instructions    string `json:"instructions"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("initialize 结果无法解析: %w", err)
	}
	payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	if err != nil {
		return err
	}
	if err := p.write(payload); err != nil {
		return fmt.Errorf("发送 initialized 通知失败: %w", err)
	}
	p.mu.Lock()
	p.instr = res.Instructions
	p.mu.Unlock()
	proto := strings.TrimSpace(res.ProtocolVersion)
	if proto == "" {
		proto = mcp.ProtocolLatest
	}
	p.log.Debug("上游进程已初始化", "upstream", p.name, "protocol", proto)
	return nil
}

// logStderr 把子进程 stderr 按行写进日志，单行超长时截断。
func (p *stdioProc) logStderr(r io.Reader) {
	br := bufio.NewReaderSize(r, 32<<10)
	var buf []byte
	truncated := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			room := maxStderrLineBytes - len(buf)
			if room > 0 {
				piece := chunk
				if len(piece) > room {
					piece = piece[:room]
					truncated = true
				}
				buf = append(buf, piece...)
			} else {
				truncated = true
			}
			if err == nil {
				p.logStderrLine(buf, truncated)
				buf, truncated = buf[:0], false
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if len(buf) > 0 {
				p.logStderrLine(buf, truncated)
			}
			return
		}
	}
}

func (p *stdioProc) logStderrLine(line []byte, truncated bool) {
	s := strings.TrimRight(string(line), "\r\n")
	if strings.TrimSpace(s) == "" {
		return
	}
	if truncated {
		s += "…（已截断）"
	}
	p.log.Info("上游 stderr", "upstream", p.name, "line", s)
}

// kill 杀掉整个进程组；进程组不可用时退回单进程 kill。
func (p *stdioProc) kill() {
	if p.cmd.Process == nil {
		return
	}
	pid := p.cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		_ = p.cmd.Process.Kill()
	}
}

func (p *stdioProc) waitExit(d time.Duration) {
	select {
	case <-p.exited:
	case <-time.After(d):
		p.kill()
		select {
		case <-p.exited:
		case <-time.After(d):
		}
	}
}

// shutdown 先关 stdin 让子进程处理完在飞请求，超过 wait 再杀进程组。
func (p *stdioProc) shutdown(wait time.Duration) {
	_ = p.stdin.Close()
	p.waitExit(wait)
}
