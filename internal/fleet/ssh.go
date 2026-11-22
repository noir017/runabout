package fleet

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// defaultKeyRaceBackoffs 定义 OpenSSH 握手期遭遇公私钥瞬时不匹配时的退避序列，
// 长度即最大自动重试次数。
var defaultKeyRaceBackoffs = []time.Duration{
	1500 * time.Millisecond,
	2 * time.Second,
}

// SSH 通过本地 ssh 进程执行远端命令。审批不在 runabout 里实现：
// 对这里来说，需要审批的机器只是"一条会阻塞一会儿的 ssh"。
type SSH struct {
	prefix          []string
	baseEnv         map[string]string
	keyRaceBackoffs []time.Duration
}

func NewSSH(prefix []string, shellEnv map[string]string) *SSH {
	if len(prefix) == 0 {
		prefix = []string{"ssh", "-o", "BatchMode=yes"}
	}
	base := make(map[string]string, len(defaultEnv)+len(shellEnv))
	for k, v := range defaultEnv {
		base[k] = v
	}
	// tools.shell.env 要传给本地 ssh 进程，审批脚本从环境变量读等待时长。
	for k, v := range shellEnv {
		base[k] = v
	}
	return &SSH{
		prefix:          prefix,
		baseEnv:         base,
		keyRaceBackoffs: defaultKeyRaceBackoffs,
	}
}

func (s *SSH) command(ctx context.Context, spec Spec, merge bool) (*exec.Cmd, *CapWriter, *CapWriter, error) {
	remote, err := BuildRemoteCommand(spec)
	if err != nil {
		return nil, nil, nil, err
	}
	args := make([]string, 0, len(s.prefix)+3)
	args = append(args, s.prefix[1:]...)
	args = append(args, "--", spec.SSH, remote)
	cmd := exec.CommandContext(ctx, s.prefix[0], args...)
	// 远端 export 走命令字符串；本地只带 tools.shell.env，不把调用方的 env 泄漏给 ssh 本身。
	cmd.Env = mergeEnv(s.baseEnv, nil)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if spec.Stdin != "" {
		cmd.Stdin = strings.NewReader(spec.Stdin)
	}
	limit := spec.MaxOutputBytes
	out := newCapWriter(limit, spec.Binary)
	var errOut *CapWriter
	if merge {
		errOut = out
	} else {
		errOut = newCapWriter(limit, spec.Binary)
	}
	cmd.Stdout, cmd.Stderr = out, errOut
	return cmd, out, errOut, nil
}

func (s *SSH) Run(ctx context.Context, spec Spec) (Result, error) {
	totalStart := time.Now()
	for attempt := 0; ; attempt++ {
		res, err := s.runOnce(ctx, spec)
		res.Duration = time.Since(totalStart)
		if !shouldRetryKeyRace(ctx, res, err) || attempt >= len(s.keyRaceBackoffs) {
			return res, err
		}
		if !sleepContext(ctx, s.keyRaceBackoffs[attempt]) {
			res.Duration = time.Since(totalStart)
			res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
			return res, err
		}
	}
}

func (s *SSH) runOnce(ctx context.Context, spec Spec) (Result, error) {
	cmd, out, errOut, err := s.command(ctx, spec, false)
	if err != nil {
		return Result{}, err
	}
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 3 * time.Second

	start := time.Now()
	runErr := cmd.Run()
	res := Result{
		Stdout:    out.String(),
		Stderr:    errOut.String(),
		Duration:  time.Since(start),
		TimedOut:  errors.Is(ctx.Err(), context.DeadlineExceeded),
		Truncated: out.Truncated() || errOut.Truncated(),
	}
	if runErr == nil {
		return res, nil
	}
	var ee *exec.ExitError
	switch {
	case errors.As(runErr, &ee):
		res.ExitCode = exitCode(ee)
	case ctx.Err() != nil:
		res.ExitCode = 124
		res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	default:
		return res, runErr
	}
	// ssh 自己失败时退出码是 255，和远端命令的退出码区分开。
	if res.ExitCode == 255 && looksLikeConnectError(res.Stderr) {
		return res, &ConnectError{Host: spec.SSH, Detail: strings.TrimSpace(res.Stderr)}
	}
	return res, nil
}

func (s *SSH) Start(ctx context.Context, spec Spec) (Process, error) {
	cmd, out, _, err := s.command(ctx, spec, spec.MergeStderr)
	if err != nil {
		return nil, err
	}
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &process{cmd: cmd, out: out}, nil
}

func looksLikeConnectError(stderr string) bool {
	for _, k := range []string{
		"ssh:", "Connection ", "Connection refused", "Permission denied",
		"Host key verification failed", "Could not resolve hostname",
	} {
		if strings.Contains(stderr, k) {
			return true
		}
	}
	return false
}

// shouldRetryKeyRace 仅在确定尚未执行远端命令的握手阶段瞬时密钥竞态时允许重试：
// 必须同时满足未超时/取消、识别为 ConnectError、退出码 255、无任何 stdout，
// 且 stderr 明确包含 OpenSSH identity_sign 公私钥内容不匹配错误。
func shouldRetryKeyRace(ctx context.Context, res Result, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var ce *ConnectError
	if !errors.As(err, &ce) {
		return false
	}
	return res.ExitCode == 255 && res.Stdout == "" && looksLikeTransientKeyRace(res.Stderr)
}

func looksLikeTransientKeyRace(stderr string) bool {
	return strings.Contains(stderr, "identity_sign:") &&
		strings.Contains(stderr, "contents do not match public")
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
