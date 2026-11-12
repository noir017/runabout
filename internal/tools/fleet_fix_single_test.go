package tools

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSingleMachineShellHeaderMatchesMain(t *testing.T) {
	d, _ := newTestDeps(t)
	res := callResult(t, &shellTool{d: d}, map[string]any{"command": "exit 3"})
	text := resultText(res)
	if strings.Contains(text, "[local]") {
		t.Errorf("单机模式不应出现 [local]:\n%s", text)
	}
	if !strings.HasPrefix(text, "exit_code: 3") {
		t.Errorf("单机模式应使用旧头部 exit_code: 3:\n%s", text)
	}
	meta, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatal("structuredContent 缺失")
	}
	if _, has := meta["target"]; has {
		t.Errorf("单机模式 structuredContent 不应带 target: %v", meta)
	}
}

func TestSingleMachineDoesNotRegisterServers(t *testing.T) {
	d, _ := newTestDeps(t)
	for _, h := range All(d) {
		if h.Definition().Name == "servers" {
			t.Fatal("单机模式不应注册 servers")
		}
	}
}

func TestSingleMachineReadStructuredNoTarget(t *testing.T) {
	d, dir := newTestDeps(t)
	mustWrite(t, filepath.Join(dir, "x.txt"), "x\n")
	res := callResult(t, &readTool{d: d}, map[string]any{"path": "x.txt"})
	meta, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatal("structuredContent 缺失")
	}
	if _, has := meta["target"]; has {
		t.Errorf("单机模式 read 的 structuredContent 不应带 target: %v", meta)
	}
}
