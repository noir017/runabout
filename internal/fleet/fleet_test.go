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

// probeRunner 记录每次执行的 Spec，不真正跑命令。
type probeRunner struct{ specs []Spec }

func (p *probeRunner) Run(_ context.Context, spec Spec) (Result, error) {
	p.specs = append(p.specs, spec)
	return Result{}, nil
}

func (p *probeRunner) Start(context.Context, Spec) (Process, error) {
	return nil, errors.New("unused")
}

// 读文件内容必须走二进制模式，不能依赖文本模式的 UTF-8 对齐。
func TestRemoteFSReadsInBinaryMode(t *testing.T) {
	p := &probeRunner{}
	fsys := &RemoteFS{Runner: p, Target: "web1", MaxBytes: 1 << 20}
	if _, err := fsys.ReadFile(context.Background(), "/tmp/data.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadHead(context.Background(), "/tmp/data.bin", 512); err != nil {
		t.Fatal(err)
	}
	if len(p.specs) != 2 {
		t.Fatalf("应发起两次读取，得到 %d", len(p.specs))
	}
	for i, spec := range p.specs {
		if !spec.Binary {
			t.Errorf("第 %d 次读取没有使用二进制模式: %+v", i+1, spec)
		}
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
	if !strings.Contains(string(data), "-- web1") {
		t.Errorf("ssh 参数应在主机名前加 --: %q", data)
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

func TestFleetExposeLocalFalse(t *testing.T) {
	noLocal := false

	t.Run("单远端机且无 default_target", func(t *testing.T) {
		f := New(config.Fleet{
			ExposeLocal: &noLocal,
			Hosts:       []config.FleetHost{{Name: "web1"}},
		}, "/bin/bash", nil)
		if !f.Enabled() {
			t.Fatal("只有一台远端机且关闭 local 时，Enabled 仍应为 true")
		}
		if f.ExposeLocal() {
			t.Fatal("ExposeLocal 应为 false")
		}
		if f.DefaultTarget() != "" {
			t.Fatalf("DefaultTarget 应为空，实际 %q", f.DefaultTarget())
		}
		if len(f.Hosts()) != 1 || f.Hosts()[0].Name != "web1" {
			t.Fatalf("Hosts 不应包含 local: %+v", f.Hosts())
		}
		if _, ok := f.Lookup("local"); ok {
			t.Error("关闭 local 后不应查到 local")
		}
		if _, ok := f.Lookup(""); ok {
			t.Error("无 default_target 时空 target 不应命中")
		}
	})

	t.Run("带 default_target 缺省命中远端", func(t *testing.T) {
		f := New(config.Fleet{
			ExposeLocal:   &noLocal,
			DefaultTarget: "web1",
			Hosts:         []config.FleetHost{{Name: "web1"}, {Name: "web2"}},
		}, "/bin/bash", nil)
		h, ok := f.Lookup("")
		if !ok || h.Name != "web1" {
			t.Fatalf("空 target 应命中默认远端 web1，得到 ok=%v host=%+v", ok, h)
		}
	})
}

// writeCountingSSH 写一个记录调用次数的假 ssh 脚本，供重试边界单测使用。
func writeCountingSSH(t *testing.T, body string) (scriptPath, countFile string) {
	t.Helper()
	dir := t.TempDir()
	countFile = filepath.Join(dir, "attempts")
	scriptPath = filepath.Join(dir, "fake-ssh")
	script := "#!/bin/sh\n" +
		"n=0\n" +
		"if [ -f " + Quote(countFile) + " ]; then n=$(cat " + Quote(countFile) + "); fi\n" +
		"n=$((n + 1))\n" +
		"printf '%d' \"$n\" > " + Quote(countFile) + "\n" +
		body + "\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return scriptPath, countFile
}

func readAttemptCount(t *testing.T, countFile string) int {
	t.Helper()
	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("读取调用次数失败: %v", err)
	}
	var n int
	for _, b := range strings.TrimSpace(string(data)) {
		n = n*10 + int(b-'0')
	}
	return n
}

func TestSSHRetryTransientKeyRaceSucceeds(t *testing.T) {
	script, countFile := writeCountingSSH(t, `
if [ "$n" -eq 1 ]; then
  echo 'identity_sign: private key /tmp/test-key contents do not match public' >&2
  echo 'user@web1: Permission denied (publickey).' >&2
  exit 255
fi
cat
`)
	s := NewSSH([]string{script}, nil)
	s.keyRaceBackoffs = []time.Duration{5 * time.Millisecond, 10 * time.Millisecond}

	res, err := s.Run(context.Background(), Spec{
		SSH:     "web1",
		Command: "cat",
		Stdin:   "payload-preserved",
	})
	if err != nil {
		t.Fatalf("第 2 次应重试成功，得到错误: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "payload-preserved" {
		t.Errorf("重试后结果异常: %+v", res)
	}
	if got := readAttemptCount(t, countFile); got != 2 {
		t.Errorf("应调用 2 次，实际 %d 次", got)
	}
}

func TestSSHRetryTransientKeyRaceExhausted(t *testing.T) {
	script, countFile := writeCountingSSH(t, `
echo 'identity_sign: private key /tmp/test-key contents do not match public' >&2
echo 'user@web1: Permission denied (publickey).' >&2
exit 255
`)
	s := NewSSH([]string{script}, nil)
	s.keyRaceBackoffs = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond}

	_, err := s.Run(context.Background(), Spec{SSH: "web1", Command: "true"})
	var ce *ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("重试耗尽后应返回 ConnectError，得到: %v", err)
	}
	if got := readAttemptCount(t, countFile); got != 3 {
		t.Errorf("首次 + 2 次重试共应调用 3 次，实际 %d 次", got)
	}
}

func TestSSHRetryBoundaries(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantConnect bool
	}{
		{
			name:        "普通 Permission denied 不重试",
			body:        `echo 'user@web1: Permission denied (publickey).' >&2; exit 255`,
			wantConnect: true,
		},
		{
			name:        "连接被拒不重试",
			body:        `echo 'ssh: connect to host web1 port 22: Connection refused' >&2; exit 255`,
			wantConnect: true,
		},
		{
			name:        "已有 stdout 输出后中断严禁重试",
			body:        `echo 'already-started'; echo 'identity_sign: private key /tmp/test-key contents do not match public' >&2; echo 'user@web1: Permission denied (publickey).' >&2; exit 255`,
			wantConnect: true,
		},
		{
			name:        "未报认证失败的远端 255 退出码严禁重试",
			body:        `echo 'identity_sign: private key /tmp/test-key contents do not match public' >&2; exit 255`,
			wantConnect: false,
		},
		{
			name:        "远端非 255 退出码严禁重试",
			body:        `echo 'identity_sign: private key /tmp/test-key contents do not match public' >&2; echo 'user@web1: Permission denied (publickey).' >&2; exit 1`,
			wantConnect: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script, countFile := writeCountingSSH(t, tc.body)
			s := NewSSH([]string{script}, nil)
			s.keyRaceBackoffs = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond}

			_, err := s.Run(context.Background(), Spec{SSH: "web1", Command: "non-idempotent-cmd"})
			var ce *ConnectError
			gotConnect := errors.As(err, &ce)
			if gotConnect != tc.wantConnect {
				t.Errorf("ConnectError 判定不符: got=%v want=%v (err=%v)", gotConnect, tc.wantConnect, err)
			}
			if got := readAttemptCount(t, countFile); got != 1 {
				t.Errorf("该场景严禁重试，期望调用 1 次，实际 %d 次", got)
			}
		})
	}
}

func TestSSHRetryStopsOnContextCancel(t *testing.T) {
	script, countFile := writeCountingSSH(t, `
echo 'identity_sign: private key /tmp/test-key contents do not match public' >&2
echo 'user@web1: Permission denied (publickey).' >&2
exit 255
`)
	s := NewSSH([]string{script}, nil)
	s.keyRaceBackoffs = []time.Duration{5 * time.Second, 5 * time.Second}

	ctx := withTimeout(t, 80*time.Millisecond)
	start := time.Now()
	res, err := s.Run(ctx, Spec{SSH: "web1", Command: "true"})
	elapsed := time.Since(start)

	var ce *ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("退避期超时应返回 ConnectError，得到: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("退避期超时应设置 TimedOut=true: %+v", res)
	}
	if elapsed >= time.Second {
		t.Errorf("退避期应随 ctx 立即中止，实际耗时 %v", elapsed)
	}
	if got := readAttemptCount(t, countFile); got != 1 {
		t.Errorf("退避期被取消不应发起第 2 次调用，实际 %d 次", got)
	}
}
