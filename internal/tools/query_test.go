package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noir017/runabout/internal/config"
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

func TestQueryRemoteAllowed(t *testing.T) {
	d, dir := newFleetDeps(t, config.Fleet{})
	target := filepath.Join(dir, "remote-notes.txt")
	if err := os.WriteFile(target, []byte("远端内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"target": "web1", "command": "cat " + target})
	res, err := (&queryTool{d: d}).Call(&mcp.CallContext{
		Ctx: context.Background(), Principal: mcp.Principal{Subject: "tester"},
	}, raw)
	if err != nil {
		t.Fatal(err)
	}
	text := textOf(res)
	if res.IsError || !strings.Contains(text, "[web1] exit=0") || !strings.Contains(text, "远端内容") {
		t.Errorf("远端只读命令应成功并带机器名: %s", text)
	}
	meta, _ := res.StructuredContent.(map[string]any)
	if meta["target"] != "web1" {
		t.Errorf("structuredContent 应带 target: %+v", res.StructuredContent)
	}
}

func TestQueryRemoteRejectsWrite(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	out, isErr := call(t, &queryTool{d: d}, map[string]any{
		"target": "web1", "command": "echo hi > pwn.txt",
	})
	if !isErr || !strings.Contains(out, "改用 shell") {
		t.Errorf("远端写命令应被拒绝: %s", out)
	}
}

func TestQuerySingleMachineNoTarget(t *testing.T) {
	d, _ := newTestDeps(t)
	props := (&queryTool{d: d}).Definition().InputSchema.(map[string]any)["properties"].(map[string]any)
	if _, ok := props["target"]; ok {
		t.Error("单机模式不应出现 target 参数")
	}
}

func TestQueryFleetHasTarget(t *testing.T) {
	d, _ := newFleetDeps(t, config.Fleet{})
	props := (&queryTool{d: d}).Definition().InputSchema.(map[string]any)["properties"].(map[string]any)
	tp, ok := props["target"].(map[string]any)
	if !ok {
		t.Fatal("配置机群后应出现 target 参数")
	}
	enum, _ := tp["enum"].([]string)
	if len(enum) != 2 || enum[0] != "local" || enum[1] != "web1" {
		t.Errorf("target enum 不对: %v", tp["enum"])
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
