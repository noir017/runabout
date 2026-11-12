package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/mcp"
)

func callResult(t *testing.T, h mcp.Handler, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Call(&mcp.CallContext{
		Ctx: context.Background(), Principal: mcp.Principal{Subject: "tester"},
	}, raw)
	if err != nil {
		t.Fatalf("工具返回协议级错误: %v", err)
	}
	return res
}

func resultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

// ---------- 1. ~/.. 穿越 ----------

func TestRemoteJoinAnchorsHome(t *testing.T) {
	cases := map[string]string{
		"~/../../etc/shadow": "~/etc/shadow",
		"~/../.ssh/id_rsa":   "~/.ssh/id_rsa",
		"~/a/../../b":        "~/b",
		"~":                  "~",
	}
	for in, want := range cases {
		if got := remoteJoin("/workspace", in); got != want {
			t.Errorf("remoteJoin(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestRemoteTraversalResolvedInsideHome(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	tgt, err := d.resolveTarget("web1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveReadPath(d, tgt, d.Paths, "~/../../etc/shadow")
	if err != nil {
		t.Fatal(err)
	}
	if got != "~/etc/shadow" {
		t.Errorf("~/.. 应锚定在 home 内，得到 %q", got)
	}
}

func TestRemoteWriteTraversalResolvedInsideHome(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	tgt, err := d.resolveTarget("web1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveWritePath(d, tgt, d.Paths, "~/../../etc/sudoers")
	if err != nil {
		t.Fatal(err)
	}
	if got != "~/etc/sudoers" {
		t.Errorf("~/.. 写路径应锚定在 home 内，得到 %q", got)
	}
}

func TestRemoteTraversalCleanHitsBlacklist(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	tgt, err := d.resolveTarget("web1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveReadPath(d, tgt, d.Paths, "/tmp/../../etc/shadow"); err == nil ||
		!strings.Contains(err.Error(), "敏感路径") {
		t.Errorf("Clean 后应命中黑名单，得到 %v", err)
	}
}

func TestRemoteTraversalReadDenied(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	out, isErr := call(t, &readTool{d: d}, map[string]any{
		"target": "web1", "path": "~/../.ssh/id_rsa",
	})
	if !isErr || !strings.Contains(out, "敏感路径") {
		t.Errorf("~/.. 穿越应命中读黑名单: %s", out)
	}
}

// ---------- 2. 远端默认 workdir ----------

func TestRemoteShellNoWorkdirStaysInHome(t *testing.T) {
	home := t.TempDir()
	d, defaultDir := newFleetDepsEnv(t, config.Fleet{}, map[string]string{"HOME": home})
	out, isErr := call(t, &shellTool{d: d}, map[string]any{"target": "web1", "command": "pwd"})
	if isErr {
		t.Fatalf("pwd 失败: %s", out)
	}
	if !strings.Contains(out, home) {
		t.Errorf("没传 workdir 时应留在远端家目录 %s:\n%s", home, out)
	}
	if strings.Contains(out, defaultDir) {
		t.Errorf("不该 cd 到本机 default_workdir %s:\n%s", defaultDir, out)
	}
}

func TestRemoteShellUsesHostWorkdir(t *testing.T) {
	hostDir := t.TempDir()
	d, _ := newFleetDeps(t, config.Fleet{Hosts: []config.FleetHost{
		{Name: "web1", Workdir: hostDir},
	}})
	out, isErr := call(t, &shellTool{d: d}, map[string]any{"target": "web1", "command": "pwd"})
	if isErr || !strings.Contains(out, hostDir) {
		t.Errorf("应使用机器的 workdir %s:\n%s", hostDir, out)
	}
}

func TestRemoteFileRelativeUsesHostWorkdir(t *testing.T) {
	hostDir := t.TempDir()
	mustWrite(t, filepath.Join(hostDir, "rel.txt"), "host-workdir\n")
	d, _ := newFleetDeps(t, config.Fleet{Hosts: []config.FleetHost{
		{Name: "web1", Workdir: hostDir},
	}})
	out, isErr := call(t, &readTool{d: d}, map[string]any{"target": "web1", "path": "rel.txt"})
	if isErr || !strings.Contains(out, "host-workdir") {
		t.Errorf("相对路径应按机器 workdir 解析: %s", out)
	}
}

func TestRemoteFileRelativeDefaultsToHome(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, "rel.txt"), "home\n")
	d, _ := newFleetDepsEnv(t, config.Fleet{}, map[string]string{"HOME": home})
	out, isErr := call(t, &readTool{d: d}, map[string]any{"target": "web1", "path": "rel.txt"})
	if isErr || !strings.Contains(out, "home") {
		t.Errorf("没配 workdir 时相对路径应相对家目录: %s", out)
	}
}

func TestRemoteSearchAndGlobRelativeUseHostWorkdir(t *testing.T) {
	hostDir := t.TempDir()
	mustWrite(t, filepath.Join(hostDir, "a.txt"), "needle\n")
	d, _ := newFleetDeps(t, config.Fleet{Hosts: []config.FleetHost{
		{Name: "web1", Workdir: hostDir},
	}})
	out, isErr := call(t, &searchTool{d: d}, map[string]any{
		"target": "web1", "pattern": "needle", "path": ".",
	})
	if isErr || !strings.Contains(out, "a.txt") {
		t.Errorf("search 相对路径应按机器 workdir 解析: %s", out)
	}
	out, isErr = call(t, &globTool{d: d}, map[string]any{
		"target": "web1", "pattern": "*.txt", "path": ".",
	})
	if isErr || !strings.Contains(out, "a.txt") {
		t.Errorf("glob 相对路径应按机器 workdir 解析: %s", out)
	}
}

// ---------- 3. ~ 展开 ----------
