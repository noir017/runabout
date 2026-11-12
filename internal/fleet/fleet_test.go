package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noir017/runabout/internal/config"
)

// stubSSH 是一个假 ssh：把最后一个参数当作远端命令，用本地 sh 执行。
func stubSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stub-ssh")
	script := `#!/bin/sh
if [ -n "$STUB_LOG" ]; then printf '%s\n' "$*" >> "$STUB_LOG"; fi
for last; do :; done
exec sh -c "$last"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestFleet(t *testing.T, hosts ...config.FleetHost) (*Fleet, string) {
	t.Helper()
	stub := stubSSH(t)
	cfg := config.Fleet{
		SSHCommand:   []string{stub},
		ProbeTimeout: 2 * time.Second,
		Hosts:        hosts,
	}
	return New(cfg, "/bin/bash", nil), stub
}

func TestBuildRemoteCommand(t *testing.T) {
	got, err := BuildRemoteCommand(Spec{
		Command: "echo hi",
		Workdir: "/srv/app data",
		Env:     map[string]string{"FOO": "a'b", "BAR": "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `export BAR='2' FOO='a'\''b'; cd -- '/srv/app data' && echo hi`
	if got != want {
		t.Errorf("远端命令拼装不对:\n得到 %s\n期望 %s", got, want)
	}
}

func TestBuildRemoteCommandExpandsHome(t *testing.T) {
	got, err := BuildRemoteCommand(Spec{Command: "pwd", Workdir: "~/work"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `cd -- "$HOME"/'work' &&`) {
		t.Errorf("~ 应交给远端 shell 展开: %s", got)
	}
	if _, err := BuildRemoteCommand(Spec{Command: "x", Env: map[string]string{"1BAD": "x"}}); err == nil {
		t.Error("非法环境变量名应报错")
	}
}

func TestLocalRun(t *testing.T) {
	f := New(config.Fleet{}, "/bin/bash", nil)
	dir := t.TempDir()
	res, err := f.Run(context.Background(), Spec{
		Target: "local", Command: "pwd; echo err >&2; exit 3", Workdir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Errorf("退出码应为 3，得到 %d", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, dir) || !strings.Contains(res.Stderr, "err") {
		t.Errorf("stdout/stderr 不对: %q / %q", res.Stdout, res.Stderr)
	}

	ctx := withTimeout(t, 200*time.Millisecond)
	res, err = f.Run(ctx, Spec{Target: "local", Command: "sleep 5", Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Errorf("超时标记缺失: %+v", res)
	}
}

func withTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestSSHStubRun(t *testing.T) {
	f, _ := newTestFleet(t, config.FleetHost{Name: "web1", SSH: "web1"})
	res, err := f.Run(context.Background(), Spec{
		Target: "web1", Command: "echo remote; echo $FOO", Env: map[string]string{"FOO": "bar"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "remote") || !strings.Contains(res.Stdout, "bar") {
		t.Errorf("远端执行结果不对: %+v", res)
	}
}

func TestSSHConnectError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad-ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'ssh: connect to host web1 port 22: Connection refused' >&2\nexit 255\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := New(config.Fleet{
		SSHCommand: []string{path},
		Hosts:      []config.FleetHost{{Name: "web1"}},
	}, "/bin/bash", nil)
	_, err := f.Run(context.Background(), Spec{Target: "web1", Command: "true"})
	var ce *ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("应识别为连接失败，得到 %v", err)
	}
	if !strings.Contains(ce.Detail, "Connection refused") {
		t.Errorf("连接错误详情不对: %q", ce.Detail)
	}
}

func TestRemoteFS(t *testing.T) {
	f, _ := newTestFleet(t, config.FleetHost{Name: "web1"})
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	inoBefore := before.Sys().(*syscall.Stat_t).Ino

	fsys := &RemoteFS{Runner: f, Target: "web1", MaxBytes: 1 << 20}
	st, err := fsys.Stat(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size != 4 || st.IsDir {
		t.Errorf("Stat 结果不对: %+v", st)
	}
	if err := fsys.WriteFile(context.Background(), path, []byte("new content\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if inoAfter := after.Sys().(*syscall.Stat_t).Ino; inoAfter != inoBefore {
		t.Errorf("远端写回必须保住 inode：%d -> %d", inoBefore, inoAfter)
	}
	data, err := fsys.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new content\n" {
		t.Errorf("读回内容不对: %q", data)
	}
	if got := after.Mode().Perm(); got != 0o640 {
		t.Errorf("cat > 应保留原权限，得到 %o", got)
	}

	newPath := filepath.Join(dir, "sub", "new.txt")
	if err := fsys.MkdirAll(context.Background(), filepath.Dir(newPath)); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile(context.Background(), newPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Remove(context.Background(), newPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Error("Remove 没删掉文件")
	}
}

func TestRemoteFSReadLimit(t *testing.T) {
	f, _ := newTestFleet(t, config.FleetHost{Name: "web1"})
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys := &RemoteFS{Runner: f, Target: "web1", MaxBytes: 1024}
	if _, err := fsys.ReadFile(context.Background(), path); err == nil {
		t.Error("超过 MaxBytes 应报错")
	}
}

func TestSSHPassesShellEnvToLocalProcess(t *testing.T) {
	stub := stubSSH(t)
	logFile := filepath.Join(t.TempDir(), "log")
	f := New(config.Fleet{
		SSHCommand: []string{stub},
		Hosts:      []config.FleetHost{{Name: "web1"}},
	}, "/bin/bash", map[string]string{"STUB_LOG": logFile})
	if _, err := f.Run(context.Background(), Spec{Target: "web1", Command: "true"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("tools.shell.env 没传给本地 ssh 进程: %v", err)
	}
	if !strings.Contains(string(data), "true") {
		t.Errorf("桩脚本没收到远端命令: %q", data)
	}
}

func TestApprovalCheck(t *testing.T) {
	dir := t.TempDir()
	flag := filepath.Join(dir, "ok")
	f := New(config.Fleet{
		ApprovalCheck: "test -f " + flag,
		Hosts:         []config.FleetHost{{Name: "nas", Approval: true}},
	}, "/bin/bash", nil)
	if f.ApprovalAllowed(context.Background(), "nas") {
		t.Error("标记文件不存在时不应视为已授权")
	}
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !f.ApprovalAllowed(context.Background(), "nas") {
		t.Error("标记文件存在时应视为已授权")
	}
}
