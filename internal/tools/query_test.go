package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noir017/runabout/internal/mcp"
)

func TestQueryAllowed(t *testing.T) {
	d, dir := newTestDeps(t)
	target := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(target, []byte("第一行\n第二行\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := call(t, &queryTool{d: d}, map[string]any{"command": "cat notes.txt"})
	if isErr {
		t.Fatalf("只读命令应能执行: %s", out)
	}
	if !strings.Contains(out, "第一行") {
		t.Errorf("应返回命令输出:\n%s", out)
	}

	def := (&queryTool{d: d}).Definition()
	a := def.Annotations
	if a == nil || a.ReadOnlyHint == nil || !*a.ReadOnlyHint ||
		a.IdempotentHint == nil || !*a.IdempotentHint ||
		a.DestructiveHint == nil || *a.DestructiveHint {
		t.Errorf("annotations 必须是 readOnly/idempotent 且非 destructive: %+v", a)
	}
}

func TestQueryRejectsWrite(t *testing.T) {
	d, dir := newTestDeps(t)
	out, isErr := call(t, &queryTool{d: d}, map[string]any{"command": "echo hi > out.txt"})
	if !isErr {
		t.Fatalf("写重定向应被拒绝: %s", out)
	}
	if !strings.Contains(out, "改用 shell") {
		t.Errorf("应提示改用 shell:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Error("被拒绝的命令不应产生文件")
	}
}

func TestQueryRejectsNonWhitelist(t *testing.T) {
	d, _ := newTestDeps(t)
	out, isErr := call(t, &queryTool{d: d}, map[string]any{"command": "rm -rf /tmp/nope"})
	if !isErr || !strings.Contains(out, "只读") {
		t.Errorf("白名单外的命令应被拒绝:\n%s", out)
	}
}

func TestQueryTimeout(t *testing.T) {
	d, _ := newTestDeps(t)
	out, isErr := call(t, &queryTool{d: d}, map[string]any{
		"command": "tail -f /dev/null", "timeout_ms": 200,
	})
	if !isErr || !strings.Contains(out, "超时") {
		t.Errorf("应超时并明确标注:\n%s", out)
	}
}

func TestQueryRegistration(t *testing.T) {
	d, _ := newTestDeps(t)
	d.Cfg.Tools.Query.Enabled = false
	if hasTool(All(d), "query") {
		t.Error("默认不应注册 query")
	}
	d.Cfg.Tools.Query.Enabled = true
	if !hasTool(All(d), "query") {
		t.Error("打开 tools.query.enabled 后应注册 query")
	}
}

func hasTool(hs []mcp.Handler, name string) bool {
	for _, h := range hs {
		if h.Definition().Name == name {
			return true
		}
	}
	return false
}
