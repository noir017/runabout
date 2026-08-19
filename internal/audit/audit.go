// Package audit 把所有工具调用与策略判定写成 JSONL 审计日志。
//
// 这个服务本质上是"把一台机器的 shell 交给远端模型"，所以留痕不是可选项：
// 出了问题要能回答"谁、什么时候、通过哪个客户端、执行了什么"。
package audit

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/mcp"
)

// Record 是一条审计记录。
type Record struct {
	Time     string          `json:"ts"`
	Event    string          `json:"event"`
	User     string          `json:"user,omitempty"`
	ClientID string          `json:"client_id,omitempty"`
	AuthMode string          `json:"auth,omitempty"`
	Session  string          `json:"session,omitempty"`
	Tool     string          `json:"tool,omitempty"`
	Args     json.RawMessage `json:"args,omitempty"`
	ArgsLen  int             `json:"args_len,omitempty"`
	Decision string          `json:"decision,omitempty"`
	RuleID   string          `json:"rule_id,omitempty"`
	Command  string          `json:"command,omitempty"`
	Workdir  string          `json:"workdir,omitempty"`
	ExitCode *int            `json:"exit_code,omitempty"`
	IsError  bool            `json:"is_error,omitempty"`
	Message  string          `json:"message,omitempty"`
	DurMS    int64           `json:"dur_ms,omitempty"`
}

// Logger 是审计日志写入器，并发安全。
type Logger struct {
	cfg config.Audit
	log *slog.Logger

	mu sync.Mutex
	w  io.WriteCloser
}

// New 打开审计日志文件。Enabled 为 false 时返回一个只走 slog 的实例。
func New(cfg config.Audit, log *slog.Logger) (*Logger, error) {
	l := &Logger{cfg: cfg, log: log}
	if !cfg.Enabled {
		return l, nil
	}
	if err := os.MkdirAll(filepath.Dir(cfg.File), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(cfg.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	l.w = f
	return l, nil
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w != nil {
		return l.w.Close()
	}
	return nil
}

// Write 落一条记录。写失败只记 warn，绝不影响正常调用。
func (l *Logger) Write(r Record) {
	if r.Time == "" {
		r.Time = time.Now().Format(time.RFC3339Nano)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.w == nil {
		return
	}
	b, err := json.Marshal(r)
	if err != nil {
		l.log.Warn("审计记录序列化失败", "err", err)
		return
	}
	if _, err := l.w.Write(append(b, '\n')); err != nil {
		l.log.Warn("审计日志写入失败", "err", err)
	}
}

// ToolCall 实现 mcp.Auditor。
func (l *Logger) ToolCall(p mcp.Principal, session, tool string, args json.RawMessage,
	isError bool, errMsg string, dur time.Duration) {
	r := Record{
		Event: "tool_call", User: p.Subject, ClientID: p.ClientID, AuthMode: p.Method,
		Session: session, Tool: tool, ArgsLen: len(args),
		IsError: isError, Message: errMsg, DurMS: dur.Milliseconds(),
	}
	if l.cfg.LogArgs && len(args) > 0 {
		if l.cfg.MaxArgs > 0 && len(args) > l.cfg.MaxArgs {
			r.Args = json.RawMessage(`"<参数过长，已省略>"`)
		} else {
			r.Args = args
		}
	}
	l.Write(r)
}

// Policy 记录一次策略判定（拒绝、要求确认、确认通过）。
func (l *Logger) Policy(p mcp.Principal, session, tool, decision, ruleID, command, workdir, msg string) {
	l.Write(Record{
		Event: "policy", User: p.Subject, ClientID: p.ClientID, AuthMode: p.Method,
		Session: session, Tool: tool, Decision: decision, RuleID: ruleID,
		Command: command, Workdir: workdir, Message: msg,
	})
	l.log.Warn("策略判定", "tool", tool, "decision", decision, "rule", ruleID,
		"user", p.Subject, "command", truncate(command, 200))
}

// Auth 记录鉴权相关事件（登录成功/失败、发令牌、动态注册）。
func (l *Logger) Auth(event, user, clientID, msg string) {
	l.Write(Record{Event: event, User: user, ClientID: clientID, Message: msg})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
