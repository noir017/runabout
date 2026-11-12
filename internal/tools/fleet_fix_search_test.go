package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noir017/runabout/internal/config"
)

func TestRemoteSearchExpandsHome(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, "a.txt"), "needle\n")
	d, _ := newFleetDepsEnv(t, config.Fleet{}, map[string]string{"HOME": home})
	out, isErr := call(t, &searchTool{d: d}, map[string]any{
		"target": "web1", "pattern": "needle", "path": "~",
	})
	if isErr || !strings.Contains(out, "a.txt") {
		t.Errorf("search 应展开 ~ 并命中: %s", out)
	}
}

func TestRemoteGlobExpandsHomeAndUsesExpandedRoot(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, "sub", "a.txt"), "x\n")
	d, _ := newFleetDepsEnv(t, config.Fleet{}, map[string]string{"HOME": home})
	out, isErr := call(t, &globTool{d: d}, map[string]any{
		"target": "web1", "pattern": "**/*.txt", "path": "~",
	})
	if isErr || !strings.Contains(out, "a.txt") {
		t.Errorf("glob 应展开 ~ 并命中: %s", out)
	}
	if !strings.Contains(out, home) {
		t.Errorf("glob 的相对路径应基于展开后的根 %s:\n%s", home, out)
	}
}

// ---------- 4. 远端 search/glob 超时 ----------

func TestRemoteSearchAndGlobUseTimeout(t *testing.T) {
	slow := filepath.Join(t.TempDir(), "slow-ssh")
	script := "#!/bin/sh\nsleep 0.5\nfor last; do :; done\nexec sh -c \"$last\"\n"
	if err := os.WriteFile(slow, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d, _ := newFleetDepsWithSSH(t, config.Fleet{}, nil, slow)
	d.Cfg.Tools.Search.Timeout = 100 * time.Millisecond
	dir := t.TempDir()

	out, isErr := call(t, &searchTool{d: d}, map[string]any{
		"target": "web1", "pattern": "x", "path": dir,
	})
	if !isErr || !strings.Contains(out, "超时") {
		t.Errorf("远端 search 应套用 search.timeout: %s", out)
	}
	out, isErr = call(t, &globTool{d: d}, map[string]any{
		"target": "web1", "pattern": "*.go", "path": dir,
	})
	if !isErr || !strings.Contains(out, "超时") {
		t.Errorf("远端 glob 应套用 search.timeout: %s", out)
	}
}

// ---------- 5. 远端 glob 目录不存在 ----------

func TestRemoteGlobMissingPathErrors(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	missing := filepath.Join(t.TempDir(), "missing")
	out, isErr := call(t, &globTool{d: d}, map[string]any{
		"target": "web1", "pattern": "*.go", "path": missing,
	})
	if !isErr {
		t.Fatalf("目录不存在应报错，不能返回没有匹配: %s", out)
	}
	if strings.Contains(out, "没有匹配") {
		t.Errorf("错误结果里不该出现没有匹配: %s", out)
	}
}

// ---------- 7. 单机模式与 main 一致 ----------

func TestRemoteGlobRGExitOneMeansNoMatch(t *testing.T) {
	fakeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeDir, "rg"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, _ := newFleetDepsEnv(t, config.Fleet{}, map[string]string{
		"PATH": fakeDir + ":/usr/bin:/bin",
	})
	out, isErr := call(t, &globTool{d: d}, map[string]any{
		"target": "web1", "pattern": "*.go", "path": t.TempDir(),
	})
	if isErr || !strings.Contains(out, "没有匹配") {
		t.Errorf("rg 退出 1 应视为没有匹配: %s", out)
	}
}

func TestRemoteGlobNewlineFilenameDoesNotError(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad\nname.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := call(t, &globTool{d: d}, map[string]any{
		"target": "web1", "pattern": "*.txt", "path": dir,
	})
	if isErr {
		t.Fatalf("换行文件名不应让 glob 报错: %s", out)
	}
}

func TestRemoteGlobIgnoresRootErrorSubstring(t *testing.T) {
	noisy := filepath.Join(t.TempDir(), "noisy-ssh")
	script := "#!/bin/sh\necho 'prefix root-error' >&2\nfor last; do :; done\nexec sh -c \"$last\"\n"
	if err := os.WriteFile(noisy, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d, _ := newFleetDepsWithSSH(t, config.Fleet{}, nil, noisy)
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "x\n")
	out, isErr := call(t, &globTool{d: d}, map[string]any{
		"target": "web1", "pattern": "*.txt", "path": dir,
	})
	if isErr {
		t.Fatalf("stderr 里的 root-error 子串不应误判: %s", out)
	}
	if !strings.Contains(out, "a.txt") {
		t.Errorf("正常 glob 结果缺失: %s", out)
	}
}

func TestRemoteSearchAndGlobDefaultToHostWorkdir(t *testing.T) {
	hostDir := t.TempDir()
	mustWrite(t, filepath.Join(hostDir, "a.txt"), "needle\n")
	d, _ := newFleetDeps(t, config.Fleet{Hosts: []config.FleetHost{
		{Name: "web1", Workdir: hostDir},
	}})
	out, isErr := call(t, &searchTool{d: d}, map[string]any{
		"target": "web1", "pattern": "needle",
	})
	if isErr || !strings.Contains(out, "a.txt") {
		t.Errorf("search 不传 path 时应使用机器 workdir: %s", out)
	}
	out, isErr = call(t, &globTool{d: d}, map[string]any{
		"target": "web1", "pattern": "*.txt",
	})
	if isErr || !strings.Contains(out, "a.txt") {
		t.Errorf("glob 不传 path 时应使用机器 workdir: %s", out)
	}
}
