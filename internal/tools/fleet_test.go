package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noir017/runabout/internal/audit"
	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/fleet"
	"github.com/noir017/runabout/internal/mcp"
	"github.com/noir017/runabout/internal/policy"
)

func stubSSH(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stub-ssh")
	script := `#!/bin/sh
if [ -n "$STUB_LOG" ]; then printf '%s\n' "$*" >> "$STUB_LOG"; fi
if [ -n "$HOME" ]; then cd "$HOME" 2>/dev/null || true; fi
for last; do :; done
exec sh -c "$last"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// newFleetDeps 在临时目录上叠一个假 ssh，远端命令其实在本机执行。
func newFleetDeps(t *testing.T, cfgFleet config.Fleet) (*Deps, string) {
	return newFleetDepsEnv(t, cfgFleet, nil)
}

func newFleetDepsEnv(t *testing.T, cfgFleet config.Fleet, env map[string]string) (*Deps, string) {
	return newFleetDepsWithSSH(t, cfgFleet, env, "")
}

func newFleetDepsWithSSH(t *testing.T, cfgFleet config.Fleet, env map[string]string, sshPath string) (*Deps, string) {
	t.Helper()
	d, dir := newTestDeps(t)
	if env != nil {
		d.Cfg.Tools.Shell.Env = env
	}
	if sshPath == "" {
		sshPath = stubSSH(t)
	}
	if cfgFleet.SSHCommand == nil {
		cfgFleet.SSHCommand = []string{sshPath}
	}
	if len(cfgFleet.Hosts) == 0 {
		cfgFleet.Hosts = []config.FleetHost{{Name: "web1", Desc: "测试机", SSH: "web1"}}
	}
	if cfgFleet.ProbeTimeout == 0 {
		cfgFleet.ProbeTimeout = 3 * time.Second
	}
	d.Fleet = fleet.New(cfgFleet, d.Cfg.Tools.Shell.Path, d.Cfg.Tools.Shell.Env)
	return d, dir
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Sys().(*syscall.Stat_t).Ino
}

func TestShellRemoteTarget(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	out, isErr := call(t, &shellTool{d: d}, map[string]any{
		"target": "web1", "command": "echo remote-ok; pwd",
	})
	if isErr {
		t.Fatalf("远端命令不应失败: %s", out)
	}
	if !strings.Contains(out, "[web1] exit=0") {
		t.Errorf("结果头应带机器名: %s", out)
	}
	if !strings.Contains(out, "remote-ok") {
		t.Errorf("远端输出缺失: %s", out)
	}
}

func TestShellUnknownTarget(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	out, isErr := call(t, &shellTool{d: d}, map[string]any{"target": "db1", "command": "true"})
	if !isErr || !strings.Contains(out, "未知的 target") {
		t.Errorf("未知机器应给出可选列表: %s", out)
	}
}

// 本机签发的令牌不能拿到另一台机器上用。
func TestConfirmTokenBoundToTarget(t *testing.T) {
	d, dir := newFleetDeps(t, config.Fleet{})
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := call(t, &shellTool{d: d}, map[string]any{"target": "local", "command": "rm -rf *"})
	if !isErr {
		t.Fatalf("应先要求确认: %s", out)
	}
	m := regexp.MustCompile(`confirm_token = (\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("没有下发令牌: %s", out)
	}
	out, isErr = call(t, &shellTool{d: d}, map[string]any{
		"target": "web1", "command": "rm -rf *", "confirm_token": m[1],
	})
	if !isErr || !strings.Contains(out, "不匹配") {
		t.Errorf("跨机器使用令牌应失败: %s", out)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Error("被拦下的命令绝不能执行")
	}

	// 重新签发一个本机令牌，证明失败原因是 target 而不是令牌本身坏了。
	out, _ = call(t, &shellTool{d: d}, map[string]any{"target": "local", "command": "rm -rf *"})
	m = regexp.MustCompile(`confirm_token = (\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("第二次没有下发令牌: %s", out)
	}
	out, isErr = call(t, &shellTool{d: d}, map[string]any{
		"target": "local", "command": "rm -rf *", "confirm_token": m[1],
	})
	if isErr {
		t.Fatalf("本机使用本机令牌应成功: %s", out)
	}
}

func TestShellRemoteBackground(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	out, isErr := call(t, &shellTool{d: d}, map[string]any{
		"target": "web1", "command": "for i in 1 2 3; do echo tick$i; sleep 0.05; done",
		"run_in_background": true, "label": "远端计数",
	})
	if isErr {
		t.Fatalf("远端后台启动失败: %s", out)
	}
	if !strings.Contains(out, "[web1]") {
		t.Errorf("后台任务应标明机器: %s", out)
	}
	var procID string
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, "proc_") {
			procID = field
		}
	}
	if procID == "" {
		t.Fatalf("没有拿到后台进程 id: %s", out)
	}
	out, isErr = call(t, &shellOutputTool{d: d}, map[string]any{"process_id": procID, "wait_ms": 3000})
	if isErr {
		t.Fatalf("读取后台输出失败: %s", out)
	}
	if !strings.Contains(out, "tick3") || !strings.Contains(out, "[web1]") {
		t.Errorf("远端后台输出不完整: %s", out)
	}
}

func TestReadRemote(t *testing.T) {
	d, dir := newFleetDeps(t, config.Fleet{})
	f := filepath.Join(dir, "remote.txt")
	if err := os.WriteFile(f, []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := call(t, &readTool{d: d}, map[string]any{"target": "web1", "path": f})
	if isErr {
		t.Fatalf("远端读取失败: %s", out)
	}
	if !strings.Contains(out, "[web1]") || !strings.Contains(out, "hello") {
		t.Errorf("远端读取结果不对: %s", out)
	}
}

func TestReadRemoteImage(t *testing.T) {
	d, dir := newFleetDeps(t, config.Fleet{})
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	pixelPath := filepath.Join(dir, "pixel.png")
	if err := os.WriteFile(pixelPath, png, 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"target": "web1", "path": pixelPath})
	res, err := (&readTool{d: d}).Call(&mcp.CallContext{
		Ctx: context.Background(), Principal: mcp.Principal{Subject: "tester"},
	}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 2 || res.Content[1].Type != "image" || res.Content[1].MimeType != "image/png" {
		t.Fatalf("远端图片应返回 image 块: %+v", res.Content)
	}
	if !strings.Contains(res.Content[0].Text, "[web1]") {
		t.Errorf("图片说明应带机器名: %s", res.Content[0].Text)
	}
}

func TestApplyPatchRemoteKeepsInode(t *testing.T) {
	d, dir := newFleetDeps(t, config.Fleet{})
	f := filepath.Join(dir, "app.conf")
	if err := os.WriteFile(f, []byte("port=80\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	before := inode(t, f)
	patch := "*** Begin Patch\n*** Update File: " + f + "\n@@\n-port=80\n+port=8080\n*** End Patch"
	out, isErr := call(t, &applyPatchTool{d: d}, map[string]any{"target": "web1", "patch": patch})
	if isErr {
		t.Fatalf("远端补丁失败: %s", out)
	}
	if got := readFile(t, f); got != "port=8080\n" {
		t.Errorf("内容不对: %q", got)
	}
	if after := inode(t, f); after != before {
		t.Errorf("远端写回必须原地覆盖：inode %d -> %d", before, after)
	}
}

func TestApplyPatchRemotePartialWriteListsFiles(t *testing.T) {
	d, dir := newFleetDeps(t, config.Fleet{})
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "a.txt")
	bad := filepath.Join(dir, "blocker", "b.txt")
	patch := "*** Begin Patch\n*** Add File: " + good + "\n+a\n*** Add File: " + bad + "\n+b\n*** End Patch"
	out, isErr := call(t, &applyPatchTool{d: d}, map[string]any{"target": "web1", "patch": patch})
	if !isErr {
		t.Fatalf("第二个文件写不进去时应失败: %s", out)
	}
	if !strings.Contains(out, "写入失败（可能已部分写入）") || !strings.Contains(out, bad) {
		t.Errorf("失败的文件应单独标注: %s", out)
	}
	writtenPart, rest, ok := strings.Cut(out, "未写入")
	if !ok || !strings.Contains(writtenPart, "已写入") || !strings.Contains(writtenPart, good) {
		t.Errorf("已写入部分应包含第一个文件: %s", out)
	}
	if strings.Contains(rest, bad) {
		t.Errorf("失败的文件不应再列进未写入: %s", out)
	}
	if _, err := os.Stat(good); err != nil {
		t.Error("第一个文件应已写入")
	}
}

func TestApplyPatchRemoteMoveReportsTarget(t *testing.T) {
	d, dir := newFleetDeps(t, config.Fleet{})
	src := filepath.Join(dir, "a.txt")
	dst := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(src, []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "blocker", "c.txt")
	patch := "*** Begin Patch\n*** Update File: " + src + "\n*** Move to: " + dst +
		"\n@@\n-line1\n+line2\n*** Add File: " + bad + "\n+x\n*** End Patch"
	out, isErr := call(t, &applyPatchTool{d: d}, map[string]any{"target": "web1", "patch": patch})
	if !isErr {
		t.Fatalf("第二个文件应失败: %s", out)
	}
	writtenPart, _, _ := strings.Cut(out, "未写入")
	if !strings.Contains(writtenPart, dst) {
		t.Errorf("Move 成功后应记录目标路径 %s: %s", dst, out)
	}
	if strings.Contains(writtenPart, src) {
		t.Errorf("Move 成功后不应记录旧路径 %s: %s", src, out)
	}
	if got := readFile(t, dst); got != "line2\n" {
		t.Errorf("Move 目标内容不对: %q", got)
	}
}

func TestServersOverviewSkipsApprovalHost(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "stub.log")
	d, _ := newFleetDeps(t, config.Fleet{
		Hosts: []config.FleetHost{
			{Name: "web1", Desc: "测试机"},
			{Name: "nas", Desc: "存储", Approval: true},
		},
	})
	d.Cfg.Tools.Shell.Env = map[string]string{"STUB_LOG": logFile}
	d.Fleet = fleet.New(config.Fleet{
		SSHCommand: []string{stubSSH(t)},
		Hosts: []config.FleetHost{
			{Name: "web1", Desc: "测试机"},
			{Name: "nas", Desc: "存储", Approval: true},
		},
	}, d.Cfg.Tools.Shell.Path, d.Cfg.Tools.Shell.Env)

	out, isErr := call(t, &serversTool{d: d}, nil)
	if isErr {
		t.Fatalf("servers 失败: %s", out)
	}
	if !strings.Contains(out, "[web1] 在线") {
		t.Errorf("普通机器应被探测: %s", out)
	}
	if !strings.Contains(out, "[nas] 需审批") {
		t.Errorf("需审批的机器应显示需审批: %s", out)
	}
	if !strings.Contains(out, "负载") || !strings.Contains(out, "根分区") {
		t.Errorf("总览应包含负载和根分区: %s", out)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("桩脚本没有日志: %v", err)
	}
	if strings.Contains(string(data), "nas") {
		t.Errorf("没配 approval_check 时不应探测需审批的机器:\n%s", data)
	}
	out, isErr = call(t, &serversTool{d: d}, map[string]any{"target": "nas"})
	if !isErr || !strings.Contains(out, "需审批") {
		t.Errorf("详情也应拒绝未授权的机器: %s", out)
	}
}

func TestServersApprovalCheckAllowsProbe(t *testing.T) {
	flagDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(flagDir, "nas"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "stub.log")
	d, _ := newFleetDeps(t, config.Fleet{
		ApprovalCheck: "test -f " + filepath.Join(flagDir, "{host}"),
		Hosts:         []config.FleetHost{{Name: "nas", Desc: "存储", Approval: true}},
	})
	d.Cfg.Tools.Shell.Env = map[string]string{"STUB_LOG": logFile}
	d.Fleet = fleet.New(config.Fleet{
		SSHCommand:    []string{stubSSH(t)},
		ApprovalCheck: "test -f " + filepath.Join(flagDir, "{host}"),
		Hosts:         []config.FleetHost{{Name: "nas", Desc: "存储", Approval: true}},
	}, d.Cfg.Tools.Shell.Path, d.Cfg.Tools.Shell.Env)

	out, isErr := call(t, &serversTool{d: d}, nil)
	if isErr {
		t.Fatalf("servers 失败: %s", out)
	}
	if !strings.Contains(out, "[nas] 在线") {
		t.Errorf("已授权时应探测: %s", out)
	}
	out, isErr = call(t, &serversTool{d: d}, map[string]any{"target": "nas"})
	if isErr {
		t.Fatalf("详情失败: %s", out)
	}
	if !strings.Contains(out, "## 内存") || !strings.Contains(out, "## 容器") {
		t.Errorf("详情应包含内存与容器: %s", out)
	}
}

func TestServersCache(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	now := time.Now()
	tool := &serversTool{d: d, now: func() time.Time { return now }}
	if out, _ := call(t, tool, nil); strings.Contains(out, "复用") {
		t.Errorf("第一次不应命中缓存: %s", out)
	}
	if out, _ := call(t, tool, nil); !strings.Contains(out, "30 秒内复用") {
		t.Errorf("第二次应命中缓存: %s", out)
	}
	now = now.Add(31 * time.Second)
	if out, _ := call(t, tool, nil); strings.Contains(out, "复用") {
		t.Errorf("超过 30 秒应重新探测: %s", out)
	}
}

func TestSearchRemote(t *testing.T) {
	d, dir := newFleetDeps(t, config.Fleet{})
	mustWrite(t, filepath.Join(dir, "a.go"), "package main\n// TODO: 修\n")
	mustWrite(t, filepath.Join(dir, "sub", "b.go"), "// TODO: 也要修\n")
	mustWrite(t, filepath.Join(dir, "node_modules", "c.go"), "// TODO: 不该搜到\n")

	out, isErr := call(t, &searchTool{d: d}, map[string]any{
		"target": "web1", "pattern": "TODO", "path": dir,
	})
	if isErr {
		t.Fatalf("远端搜索失败: %s", out)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Errorf("应命中 a.go 与 b.go: %s", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Errorf("远端搜索也应跳过 node_modules: %s", out)
	}
	if !strings.Contains(out, "ripgrep（远端）") {
		t.Errorf("应注明用了远端 rg: %s", out)
	}

	// 回退分支至少要出现在脚本里。
	script := (&searchTool{d: d}).remoteSearchScript(searchArgs{Pattern: "x"}, "/tmp", 10)
	if !strings.Contains(script, "command -v rg") || !strings.Contains(script, "grep") {
		t.Errorf("回退脚本缺失: %s", script)
	}
}

func TestGlobRemote(t *testing.T) {
	d, dir := newFleetDeps(t, config.Fleet{})
	mustWrite(t, filepath.Join(dir, "a.go"), "x\n")
	mustWrite(t, filepath.Join(dir, "sub", "b.go"), "x\n")
	mustWrite(t, filepath.Join(dir, "node_modules", "c.go"), "x\n")

	out, isErr := call(t, &globTool{d: d}, map[string]any{
		"target": "web1", "pattern": "**/*.go", "path": dir,
	})
	if isErr {
		t.Fatalf("远端 glob 失败: %s", out)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Errorf("应命中两层 .go: %s", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Errorf("远端 glob 也应跳过 node_modules: %s", out)
	}
	script := (&globTool{d: d}).remoteGlobScript(globArgs{Pattern: "**/*.go"}, "/tmp", 10)
	if !strings.Contains(script, "find") {
		t.Errorf("回退脚本应包含 find: %s", script)
	}
}

func TestRemotePathDenyIsLiteral(t *testing.T) {
	d, dir := newFleetDeps(t, config.Fleet{})
	mustWrite(t, filepath.Join(dir, "ok.txt"), "fine\n")
	for _, p := range []string{"~/.ssh/id_rsa", "/home/someone/.ssh/id_ed25519", "/etc/shadow"} {
		out, isErr := call(t, &readTool{d: d}, map[string]any{"target": "web1", "path": p})
		if !isErr || !strings.Contains(out, "敏感路径") {
			t.Errorf("远端读取 %s 应被拒绝: %s", p, out)
		}
	}
	out, isErr := call(t, &readTool{d: d}, map[string]any{
		"target": "web1", "path": filepath.Join(dir, "ok.txt"),
	})
	if isErr {
		t.Errorf("非凭据文件不该被拒绝: %s", out)
	}
}

func TestSingleMachineHasNoTargetParam(t *testing.T) {
	d, _ := newTestDeps(t)
	def := (&shellTool{d: d}).Definition()
	props := def.InputSchema.(map[string]any)["properties"].(map[string]any)
	if _, ok := props["target"]; ok {
		t.Error("没配 fleet.hosts 时不应出现 target 参数")
	}
}

func TestTargetParamIsSimpleEnum(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	def := (&shellTool{d: d}).Definition()
	props := def.InputSchema.(map[string]any)["properties"].(map[string]any)
	tp, ok := props["target"].(map[string]any)
	if !ok {
		t.Fatal("配置机群后应出现 target 参数")
	}
	if tp["type"] != "string" {
		t.Errorf("target 应为 string: %v", tp)
	}
	enum, ok := tp["enum"].([]string)
	if !ok || len(enum) != 2 || enum[0] != "local" || enum[1] != "web1" {
		t.Errorf("target enum 不对: %v", tp["enum"])
	}
	if _, bad := tp["anyOf"]; bad {
		t.Error("不能用 anyOf，Gemini 只支持简单 schema")
	}
	if _, bad := tp["oneOf"]; bad {
		t.Error("不能用 oneOf")
	}
	if !strings.Contains(tp["description"].(string), "local：") {
		t.Errorf("参数描述应每台机器一行: %v", tp["description"])
	}
}

func TestAuditRecordsTarget(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Auth.Enabled = false
	cfg.Server.DataDir = filepath.Join(dir, ".data")
	cfg.Tools.Shell.DefaultWorkdir = dir
	cfg.Audit.File = filepath.Join(dir, "audit.jsonl")
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	aud, err := audit.New(cfg.Audit, log)
	if err != nil {
		t.Fatal(err)
	}
	defer aud.Close()
	guard, err := policy.NewShellGuard(cfg.Policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &Deps{
		Cfg: cfg, Paths: policy.NewPathGuard(cfg.Policy, dir), Shell: guard,
		Confirm: policy.NewConfirmStore(cfg.Policy.ConfirmTTL), Audit: aud,
		Procs: NewProcManager(4), Log: log,
		Fleet: fleet.New(config.Fleet{
			SSHCommand: []string{stubSSH(t)},
			Hosts:      []config.FleetHost{{Name: "web1"}},
		}, cfg.Tools.Shell.Path, cfg.Tools.Shell.Env),
	}
	if out, isErr := call(t, &shellTool{d: d}, map[string]any{"target": "web1", "command": "rm -rf /"}); !isErr {
		t.Fatalf("拒绝级命令应失败: %s", out)
	}
	aud.ToolCall(mcp.Principal{Subject: "tester"}, "sess", "shell", json.RawMessage(`{"target":"web1"}`), false, "", time.Millisecond)
	data, err := os.ReadFile(cfg.Audit.File)
	if err != nil {
		t.Fatal(err)
	}
	var policyOK, callOK bool
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["event"] == "policy" && rec["tool"] == "shell" && rec["target"] == "web1" {
			policyOK = true
		}
		if rec["event"] == "tool_call" && rec["tool"] == "shell" && rec["target"] == "web1" {
			callOK = true
		}
	}
	if !policyOK || !callOK {
		t.Errorf("审计里应记录 target=web1（policy=%v tool_call=%v）:\n%s", policyOK, callOK, data)
	}
}
