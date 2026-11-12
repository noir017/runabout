package fleet

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// defaultEnv 屏蔽交互式提示，避免命令在无人值守下卡死。
var defaultEnv = map[string]string{
	"TERM":                          "dumb",
	"NO_COLOR":                      "1",
	"CLICOLOR":                      "0",
	"PAGER":                         "cat",
	"GIT_PAGER":                     "cat",
	"GIT_TERMINAL_PROMPT":           "0",
	"DEBIAN_FRONTEND":               "noninteractive",
	"PYTHONUNBUFFERED":              "1",
	"PIP_DISABLE_PIP_VERSION_CHECK": "1",
}

// Local 在本机用配置的 shell 执行命令，沿用独立进程组与超时处理。
type Local struct {
	shell   string
	baseEnv map[string]string
}

func NewLocal(shellPath string, shellEnv map[string]string) *Local {
	if shellPath == "" {
		shellPath = "/bin/bash"
	}
	base := make(map[string]string, len(defaultEnv)+len(shellEnv))
	for k, v := range defaultEnv {
		base[k] = v
	}
	for k, v := range shellEnv {
		base[k] = v
	}
	return &Local{shell: shellPath, baseEnv: base}
}

func (l *Local) command(ctx context.Context, spec Spec, merge bool) (*exec.Cmd, *CapWriter, *CapWriter) {
	cmd := exec.CommandContext(ctx, l.shell, "-c", spec.Command)
	if spec.Workdir != "" {
		cmd.Dir = spec.Workdir
	}
	cmd.Env = mergeEnv(l.baseEnv, spec.Env)
	// 独立进程组：超时或 kill 时能连同 fork 出来的子进程一起收拾干净。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	limit := spec.MaxOutputBytes
	out := NewCapWriter(limit)
	var errOut *CapWriter
	if merge {
		errOut = out
	} else {
		errOut = NewCapWriter(limit)
	}
	cmd.Stdout, cmd.Stderr = out, errOut
	return cmd, out, errOut
}

func (l *Local) Run(ctx context.Context, spec Spec) (Result, error) {
	cmd, out, errOut := l.command(ctx, spec, false)
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 3 * time.Second

	start := time.Now()
	err := cmd.Run()
	res := Result{
		Stdout:    out.String(),
		Stderr:    errOut.String(),
		Duration:  time.Since(start),
		TimedOut:  errors.Is(ctx.Err(), context.DeadlineExceeded),
		Truncated: out.Truncated() || errOut.Truncated(),
	}
	if err == nil {
		return res, nil
	}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		res.ExitCode = exitCode(ee)
	case ctx.Err() != nil:
		// Cancel 没来得及生效时 Run 直接返回 context 错误，按超时处理。
		res.ExitCode = 124
		res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	default:
		return res, err
	}
	return res, nil
}

func (l *Local) Start(ctx context.Context, spec Spec) (Process, error) {
	cmd, out, _ := l.command(ctx, spec, spec.MergeStderr)
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &process{cmd: cmd, out: out}, nil
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	return nil
}

func exitCode(ee *exec.ExitError) int {
	code := ee.ExitCode()
	if code < 0 {
		return 128
	}
	return code
}

type process struct {
	cmd *exec.Cmd
	out *CapWriter
}

func (p *process) Pid() int { return p.cmd.Process.Pid }

func (p *process) Out() *CapWriter { return p.out }

func (p *process) Wait() (int, error) {
	err := p.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return exitCode(ee), err
	}
	return -1, err
}

func (p *process) Signal(sig syscall.Signal) error {
	if p.cmd.Process == nil {
		return fmt.Errorf("进程尚未启动")
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, sig); err != nil {
		return p.cmd.Process.Signal(sig)
	}
	return nil
}
