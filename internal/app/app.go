// Package app 负责把各层装配成一个可运行的 HTTP 服务。
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/noir017/agent-tools-mcp/internal/audit"
	"github.com/noir017/agent-tools-mcp/internal/auth"
	"github.com/noir017/agent-tools-mcp/internal/config"
	"github.com/noir017/agent-tools-mcp/internal/mcp"
	"github.com/noir017/agent-tools-mcp/internal/policy"
	"github.com/noir017/agent-tools-mcp/internal/tools"
)

// Version 由构建时通过 -ldflags 注入。
var Version = "dev"

const serverInstructions = `这是一台 Linux 服务器上的基础工具集，你可以用它直接操作这台机器。

工具分工：
- shell：执行任意 bash 命令。长任务用 run_in_background=true，再配合 shell_output / shell_kill。
- read / write：读写文件。read 默认带行号输出。
- apply_patch：修改已有文件的首选方式，靠上下文定位，不会覆盖没读过的内容。
- search：按内容搜正则（相当于 rg）。glob：按文件名模式找文件。list_dir：看目录结构。

建议的工作方式：先用 list_dir / glob / search 摸清情况，用 read 确认要改的具体内容，
再用 apply_patch 落改动，最后用 shell 跑测试或重启服务验证。

关于安全拦截：绝大多数命令直接执行。少数破坏性操作会被拦下并返回 confirm_token，
这时应当先向用户说明这条命令会做什么、有什么后果，得到同意后再带 token 重发。
不要在用户没表态的情况下自己确认。`

// Built 是装配好的服务：一个 http.Handler 加上关停时要做的清理。
type Built struct {
	Handler http.Handler
	Tools   []string
	Close   func()
}

// Build 按配置装配整个服务，但不监听端口。抽出来是为了让集成测试能用
// httptest 跑完整的 OAuth + MCP 链路，而不必去猜端口。
func Build(cfg *config.Config, log *slog.Logger) (*Built, error) {
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	aud, err := audit.New(cfg.Audit, log)
	if err != nil {
		return nil, fmt.Errorf("初始化审计日志失败: %w", err)
	}

	store, err := auth.OpenStore(cfg.Server.DataDir)
	if err != nil {
		return nil, err
	}
	authSrv, err := auth.New(cfg, store, aud, log)
	if err != nil {
		return nil, err
	}

	shellGuard, err := policy.NewShellGuard(cfg.Policy, selfNames())
	if err != nil {
		return nil, fmt.Errorf("加载 shell 策略失败: %w", err)
	}
	deps := &tools.Deps{
		Cfg:     cfg,
		Paths:   policy.NewPathGuard(cfg.Policy, cfg.Tools.Shell.DefaultWorkdir),
		Shell:   shellGuard,
		Confirm: policy.NewConfirmStore(cfg.Policy.ConfirmTTL),
		Audit:   aud,
		Procs:   tools.NewProcManager(cfg.Tools.Shell.MaxBackground),
		Log:     log,
	}

	mcpSrv := mcp.NewServer(mcp.Implementation{
		Name: "agent-tools-mcp", Title: "服务器基础工具集", Version: Version,
	}, serverInstructions, log)
	mcpSrv.SetAuditor(aud)
	mcpSrv.Register(tools.All(deps)...)

	transport := mcp.NewHTTPTransport(mcpSrv, mcp.HTTPOptions{
		BaseURL:        cfg.Server.BaseURL,
		AllowedOrigins: cfg.Server.AllowedOrigins,
		SessionTTL:     cfg.Server.SessionTTL,
		StrictSessions: cfg.Server.StrictSessions,
		Log:            log,
	})

	mux := http.NewServeMux()
	authSrv.Routes(mux)
	mux.Handle("/mcp", authSrv.Protect(transport))
	// 有些客户端习惯在末尾带斜杠。
	mux.Handle("/mcp/", authSrv.Protect(transport))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"status": "ok", "version": Version,
			"license": "AGPL-3.0-or-later", "source": cfg.Server.SourceURL,
			"tools": mcpSrv.ToolNames(), "auth": cfg.Auth.Enabled,
			"pending_confirms": deps.Confirm.Pending(),
			"oauth":            store.Stats(),
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		renderLanding(w, cfg, mcpSrv.ToolNames())
	})

	return &Built{
		Handler: withRecover(withRequestLog(log, mux)),
		Tools:   mcpSrv.ToolNames(),
		Close: func() {
			deps.Procs.KillAll()
			_ = aud.Close()
		},
	}, nil
}

// Run 启动服务并阻塞直到收到退出信号。
func Run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	log := newLogger()

	built, err := Build(cfg, log)
	if err != nil {
		return err
	}
	defer built.Close()

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           built.Handler,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		// 后台任务与 SSE 需要长连接，空闲超时不能太短。
		IdleTimeout: 10 * time.Minute,
	}

	log.Info("agent-tools-mcp 启动",
		"version", Version, "listen", cfg.Server.Listen, "base_url", cfg.Server.BaseURL,
		"auth", cfg.Auth.Enabled, "tools", len(built.Tools))
	if !cfg.Auth.Enabled {
		log.Warn("鉴权已关闭：任何能访问该端口的人都可以在这台机器上执行命令，仅限本机调试使用")
	}
	if strings.HasPrefix(cfg.Server.BaseURL, "http://") &&
		!strings.Contains(cfg.Server.BaseURL, "127.0.0.1") && !strings.Contains(cfg.Server.BaseURL, "localhost") {
		log.Warn("base_url 不是 https，ChatGPT 无法连接；请在反代层配置 TLS", "base_url", cfg.Server.BaseURL)
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errCh:
		return fmt.Errorf("监听 %s 失败: %w", cfg.Server.Listen, err)
	case <-ctx.Done():
		log.Info("收到退出信号，正在关闭")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("ATM_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if strings.EqualFold(os.Getenv("ATM_LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

// selfNames 给 shell 策略提供"本服务自身"的名字，用于识别自杀式命令。
func selfNames() []string {
	names := []string{"agent-tools-mcp"}
	if exe, err := os.Executable(); err == nil {
		if base := baseName(exe); base != "" && base != "agent-tools-mcp" {
			names = append(names, base)
		}
	}
	return names
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// withRecover 兜住 handler 里的 panic，避免单个请求打崩进程。
func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("HTTP handler panic", "panic", rec, "path", r.URL.Path)
				http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush 透传给底层，SSE 依赖它。
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func withRequestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" {
			return
		}
		log.Debug("http", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur_ms", time.Since(start).Milliseconds())
	})
}
