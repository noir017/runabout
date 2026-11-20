package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/noir017/runabout/internal/config"
)

// 测试二进制自己扮演 stdio 上游：TestMain 在 helper 环境变量下切到服务循环，
// 不再跑普通用例。
func TestMain(m *testing.M) {
	if os.Getenv("RB_STDIO_HELPER") == "1" {
		runStdioHelper()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// runStdioHelper 是一个按行分隔 JSON-RPC 的最小上游实现。
// RB_STDIO_MODE=crash 时，第一个进程在收到 tools/call 后直接退出，
// 用来验证崩溃后重启。
func runStdioHelper() {
	mode := os.Getenv("RB_STDIO_MODE")
	marker := os.Getenv("RB_STDIO_MARKER")
	crashOnCall := false
	if mode == "crash" && marker != "" {
		if _, err := os.Stat(marker); err != nil {
			_ = os.WriteFile(marker, []byte("1"), 0o600)
			crashOnCall = true
		}
	}
	if starts := os.Getenv("RB_STDIO_STARTS"); starts != "" {
		f, err := os.OpenFile(starts, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString("start\n")
			_ = f.Close()
		}
	}

	var writeMu sync.Mutex
	write := func(v any) {
		payload, err := json.Marshal(v)
		if err != nil {
			return
		}
		writeMu.Lock()
		_, _ = os.Stdout.Write(append(payload, '\n'))
		writeMu.Unlock()
	}
	respond := func(id any, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	respondErr := func(id any, code int, msg string) {
		write(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": code, "message": msg}})
	}

	var wg sync.WaitGroup
	var seq int64
	var seqMu sync.Mutex

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			continue
		}
		var id any
		if len(req.ID) > 0 {
			_ = json.Unmarshal(req.ID, &id)
		}
		switch req.Method {
		case "initialize":
			respond(id, map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "helper", "version": "1"},
				"instructions":    os.Getenv("RB_STDIO_INSTRUCTIONS"),
			})
		case "notifications/initialized":
			// 通知不需要回复。
		case "tools/list":
			respond(id, map[string]any{"tools": []any{
				map[string]any{"name": "echo", "description": "回显", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "slow", "description": "慢工具", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "secret", "description": "白名单外", "inputSchema": map[string]any{"type": "object"}},
			}})
			if mode == "close-stdout" {
				// 只关闭输出端，进程继续活着。
				_ = os.Stdout.Close()
				time.Sleep(24 * time.Hour)
			}
			if mode == "stuck" {
				// 不再读 stdin，让客户端的写操作把管道塞满。
				time.Sleep(24 * time.Hour)
			}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if crashOnCall {
				os.Exit(3)
			}
			if mode == "exit-after-call" && p.Name == "echo" {
				// 先写出响应再立刻退出，验证客户端不会丢掉管道里的最后一行。
				respond(id, map[string]any{
					"content": []any{map[string]any{"type": "text", "text": "echo:" + string(p.Arguments)}},
				})
				os.Exit(0)
			}
			wg.Add(1)
			go func(id any, name string, args json.RawMessage) {
				defer wg.Done()
				switch name {
				case "echo":
					// 故意让先到的请求后回，验证客户端按 id 匹配响应。
					seqMu.Lock()
					seq++
					n := seq
					seqMu.Unlock()
					time.Sleep(time.Duration(30-n%30) * 3 * time.Millisecond)
					respond(id, map[string]any{
						"content": []any{map[string]any{"type": "text", "text": "echo:" + string(args)}},
					})
				case "slow":
					ms := 3000
					if v := os.Getenv("RB_STDIO_SLOW_MS"); v != "" {
						_, _ = fmt.Sscanf(v, "%d", &ms)
					}
					time.Sleep(time.Duration(ms) * time.Millisecond)
					respond(id, map[string]any{
						"content": []any{map[string]any{"type": "text", "text": "slow done"}},
					})
				default:
					respondErr(id, -32602, "unknown tool "+name)
				}
			}(id, p.Name, p.Arguments)
		default:
			respondErr(id, -32601, "method not found")
		}
	}
	// 读到 EOF 后等在飞请求处理完再退出，模拟规范实现的优雅关停。
	wg.Wait()
	if f := os.Getenv("RB_STDIO_EXITFILE"); f != "" {
		_ = os.WriteFile(f, []byte("exited\n"), 0o600)
	}
}

func stdioConfig(t *testing.T, mode string, env map[string]string) config.Upstream {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Upstream{
		Name:            "helper",
		Command:         []string{exe},
		Prefix:          "hp",
		Tools:           []string{"echo", "slow"},
		Timeout:         5 * time.Second,
		RefreshInterval: 100 * time.Millisecond,
		Env: map[string]string{
			"RB_STDIO_HELPER": "1",
			"RB_STDIO_MODE":   mode,
		},
	}
	for k, v := range env {
		cfg.Env[k] = v
	}
	return cfg
}

func TestStdioListAndCall(t *testing.T) {
	c := newStdioClient(stdioConfig(t, "echo", nil), discardLogger())
	defer c.Close()

	ctx := context.Background()
	list, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("拉取工具失败: %v", err)
	}
	if len(list.Tools) != 3 {
		t.Fatalf("应拿到 3 个工具，实际 %+v", list.Tools)
	}
	if list.Tools[0].Description != "回显" {
		t.Errorf("工具描述应来自上游: %+v", list.Tools[0])
	}

	raw, err := c.CallTool(ctx, "echo", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != `echo:{"msg":"hi"}` {
		t.Errorf("参数没有原样转发: %s", raw)
	}
}

// TestStdioConcurrentCalls 验证一条 stdio 上并发多路请求按 id 匹配，
// 即使上游乱序回复也不会串结果。
func TestStdioConcurrentCalls(t *testing.T) {
	c := newStdioClient(stdioConfig(t, "echo", nil), discardLogger())
	defer c.Close()

	const n = 20
	ctx := context.Background()
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	got := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw, err := c.CallTool(ctx, "echo", json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)))
			if err != nil {
				errs[i] = err
				return
			}
			var res struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				errs[i] = err
				return
			}
			got[i] = res.Content[0].Text
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("并发调用 %d 失败: %v", i, errs[i])
		}
		want := fmt.Sprintf(`echo:{"i":%d}`, i)
		if got[i] != want {
			t.Errorf("并发响应串了：调用 %d 得到 %q，期望 %q", i, got[i], want)
		}
	}
}

// TestStdioCrashRestart 验证子进程崩溃时在飞请求立即失败，退避后自动重启并重新 initialize。
func TestStdioCrashRestart(t *testing.T) {
	dir := t.TempDir()
	cfg := stdioConfig(t, "crash", map[string]string{
		"RB_STDIO_MARKER": filepath.Join(dir, "crashed"),
		"RB_STDIO_STARTS": filepath.Join(dir, "starts"),
	})
	c := newStdioClient(cfg, discardLogger())
	defer c.Close()

	ctx := context.Background()
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := c.CallTool(ctx, "echo", nil); err == nil {
		t.Fatal("进程崩溃时调用应立即失败")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("在飞请求应立即失败，实际等了 %v", d)
	}

	// 退避窗口内重启会被拒绝，窗口过后再调应该重新拉起并重新 initialize。
	time.Sleep(250 * time.Millisecond)
	raw, err := c.CallTool(ctx, "echo", json.RawMessage(`{"after":"restart"}`))
	if err != nil {
		t.Fatalf("重启后调用应成功: %v", err)
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != `echo:{"after":"restart"}` {
		t.Errorf("重启后的响应不对: %s", raw)
	}

	starts, err := os.ReadFile(filepath.Join(dir, "starts"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(starts), "start"); n != 2 {
		t.Errorf("崩溃后应重启一次（共 2 次启动），实际 %d 次", n)
	}
}

// TestStdioShutdownWaitsForInflight 验证关停先关 stdin，让在飞请求完成后再退出。
func TestStdioShutdownWaitsForInflight(t *testing.T) {
	exitFile := filepath.Join(t.TempDir(), "exited")
	cfg := stdioConfig(t, "echo", map[string]string{
		"RB_STDIO_EXITFILE": exitFile,
		"RB_STDIO_SLOW_MS":  "300",
	})
	c := newStdioClient(cfg, discardLogger())
	c.shutdownWait = 3 * time.Second
	defer c.Close()

	ctx := context.Background()
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.CallTool(ctx, "slow", nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)

	c.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("关停时应让在飞请求完成: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("在飞请求没有在关停窗口内完成")
	}
	if _, err := os.Stat(exitFile); err != nil {
		t.Errorf("子进程应在 stdin 关闭后正常退出: %v", err)
	}
}

// TestStdioCallTimeout 验证单次调用超时只影响该次调用。
func TestStdioCallTimeout(t *testing.T) {
	cfg := stdioConfig(t, "echo", map[string]string{"RB_STDIO_SLOW_MS": "2000"})
	c := newStdioClient(cfg, discardLogger())
	c.shutdownWait = 100 * time.Millisecond
	defer c.Close()

	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.CallTool(ctx, "slow", nil)
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("超时应返回明确错误，实际: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("超时应立即返回，实际等了 %v", d)
	}

	// 超时不应带崩客户端，后续调用仍然可用。
	if _, err := c.CallTool(context.Background(), "echo", nil); err != nil {
		t.Errorf("超时后其他调用应正常: %v", err)
	}
}

// TestStdioResponseBeforeExit 验证子进程写完响应就退出时，最后一行不会被丢掉。
func TestStdioResponseBeforeExit(t *testing.T) {
	c := newStdioClient(stdioConfig(t, "exit-after-call", nil), discardLogger())
	defer c.Close()

	ctx := context.Background()
	if _, err := c.ListTools(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := c.CallTool(ctx, "echo", json.RawMessage(`{"msg":"last"}`))
	if err != nil {
		t.Fatalf("子进程退出前写出的响应不应丢失: %v", err)
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != `echo:{"msg":"last"}` {
		t.Errorf("响应内容不对: %s", raw)
	}
}

// TestStdioStdoutClosedFailsInflight 验证 stdout 被关闭时，
// 在飞请求立即失败，而不是一直等到 context 超时。
func TestStdioStdoutClosedFailsInflight(t *testing.T) {
	c := newStdioClient(stdioConfig(t, "close-stdout", nil), discardLogger())
	c.shutdownWait = 100 * time.Millisecond
	defer c.Close()

	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := c.CallTool(ctx, "echo", nil); err == nil {
		t.Fatal("stdout 关闭后调用应失败")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("stdout 关闭应让在飞请求立即失败，实际等了 %v", d)
	}
}

// TestStdioWriteStuckProcess 验证子进程不读 stdin 时，写操作不会无限阻塞：
// 请求超时后判定失联并 kill，writeMu 不会被永久占住。
func TestStdioWriteStuckProcess(t *testing.T) {
	c := newStdioClient(stdioConfig(t, "stuck", nil), discardLogger())
	c.shutdownWait = 100 * time.Millisecond
	defer c.Close()

	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	big := json.RawMessage(fmt.Sprintf(`{"blob":%q}`, strings.Repeat("x", 256<<10)))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.CallTool(ctx, "echo", big)
	if err == nil {
		t.Fatal("子进程不读 stdin 时调用应超时失败")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("错误应说明写入超时，实际: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("写卡住不应无限阻塞，实际等了 %v", d)
	}
}

// TestChildEnvIsolated 验证子进程只拿到 PATH/HOME/LANG 与配置里的字面值。
func TestChildEnvIsolated(t *testing.T) {
	t.Setenv("RB_TEST_SECRET_LEAK", "must-not-leak")
	env := childEnv(map[string]string{"EXAMPLE_TOKEN": "literal"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "RB_TEST_SECRET_LEAK") || strings.Contains(joined, "must-not-leak") {
		t.Errorf("子进程不应继承 runabout 的其他环境变量:\n%s", joined)
	}
	if !strings.Contains(joined, "EXAMPLE_TOKEN=literal") {
		t.Errorf("配置里的 env 应注入子进程:\n%s", joined)
	}
	for _, name := range []string{"PATH=", "HOME=", "LANG="} {
		if !strings.Contains(joined, name) {
			t.Errorf("子进程应保留 %s:\n%s", name, joined)
		}
	}
}

// TestStdioStderrTruncation 验证 stderr 按行进日志，超长行截断。
func TestStdioStderrTruncation(t *testing.T) {
	var buf bytes.Buffer
	p := &stdioProc{name: "helper", log: slog.New(slog.NewTextHandler(&buf, nil))}
	p.readers.Add(1)
	long := strings.Repeat("x", maxStderrLineBytes+100)
	p.logStderr(strings.NewReader(long + "\n第二行\n"))
	out := buf.String()
	if !strings.Contains(out, "上游 stderr") {
		t.Fatalf("stderr 应写进日志:\n%s", out)
	}
	if !strings.Contains(out, "已截断") {
		t.Errorf("超长行应标记截断:\n%s", out)
	}
	if !strings.Contains(out, "第二行") {
		t.Errorf("后续行应继续记录:\n%s", out)
	}
	if strings.Contains(out, long) {
		t.Error("日志不应包含完整超长行")
	}
}

func TestManagerStdioUpstream(t *testing.T) {
	cfg := stdioConfig(t, "echo", nil)
	cfg.Tools = []string{"echo"}
	m := New([]config.Upstream{cfg}, discardLogger())
	defer m.Close()

	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	def := m.Definition("helper", "hp", "echo")
	if def.Name != "hp_echo" || def.Description != "回显" {
		t.Errorf("stdio 上游定义应透传: %+v", def)
	}
	if hidden := m.Definition("helper", "hp", "secret"); !strings.Contains(hidden.Description, "没有") {
		t.Errorf("白名单外的工具应给出占位定义: %+v", hidden)
	}

	res, err := m.Call(context.Background(), "helper", "echo", json.RawMessage(`{"msg":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != `echo:{"msg":"x"}` {
		t.Errorf("调用结果不对: %+v", res)
	}
}

// TestManagerStdioInstructions 验证 stdio 上游的 instructions 也会被转发。
func TestManagerStdioInstructions(t *testing.T) {
	cfg := stdioConfig(t, "echo", map[string]string{"RB_STDIO_INSTRUCTIONS": "stdio 上游说明"})
	cfg.Tools = []string{"echo"}
	cfg.Instructions = true
	m := New([]config.Upstream{cfg}, discardLogger())
	defer m.Close()

	if err := m.RefreshAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := m.Instructions()
	if !strings.Contains(got, "## 上游 helper 的说明（工具名前缀 hp_）") {
		t.Errorf("段头应写明上游名与前缀:\n%s", got)
	}
	if !strings.Contains(got, "stdio 上游说明") {
		t.Errorf("应追加 stdio 上游说明:\n%s", got)
	}
}
