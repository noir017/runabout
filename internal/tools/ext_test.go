package tools

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noir017/runabout/internal/audit"
	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/fleet"
	"github.com/noir017/runabout/internal/mcp"
	"github.com/noir017/runabout/internal/policy"
)

// ---------- Step 4: apply_patch 报错回显与 Create File ----------

func TestApplyPatchMismatchEchoesClosestContext(t *testing.T) {
	d, dir := newTestDeps(t)
	p := filepath.Join(dir, "sample.go")
	content := strings.Join([]string{
		"package demo",
		"",
		"func Alpha() int {",
		"\treturn 1",
		"}",
		"",
		"func Beta() int {",
		"\tx := 42",
		"\treturn x",
		"}",
		"",
	}, "\n")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	patch := "*** Begin Patch\n*** Update File: " + p + "\n@@ func Beta(\n func Beta() int {\n-\tx := 99\n+\tx := 100\n \treturn x\n*** End Patch"
	out, isErr := call(t, &applyPatchTool{d: d}, map[string]any{"patch": patch})
	if !isErr {
		t.Fatalf("上下文不匹配应失败，实际成功: %s", out)
	}
	if !strings.Contains(out, "文件最接近位置的实际内容") {
		t.Errorf("错误信息应附带最接近位置的实际上下文:\n%s", out)
	}
	if !strings.Contains(out, "8\t\tx := 42") {
		t.Errorf("实际上下文应包含带行号的真实内容（第 8 行 x := 42）:\n%s", out)
	}
}

func TestApplyPatchCreateFilePrefixFreeAndNoOverwrite(t *testing.T) {
	d, dir := newTestDeps(t)
	p := filepath.Join(dir, "docs", "new.md")
	patch := "*** Begin Patch\n*** Create File: " + p + "\n# 标题\n+ 列表项（保留前导加号）\n- 另一项\n普通正文\n*** End Patch"
	out, isErr := call(t, &applyPatchTool{d: d}, map[string]any{"patch": patch})
	if isErr {
		t.Fatalf("Create File 新建文件失败: %s", out)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := "# 标题\n+ 列表项（保留前导加号）\n- 另一项\n普通正文\n"
	if string(got) != want {
		t.Errorf("Create File 内容不符:\ngot:  %q\nwant: %q", string(got), want)
	}

	// 目标文件已存在时必须拒绝，避免误覆盖。
	patch2 := "*** Begin Patch\n*** Create File: " + p + "\n覆盖尝试\n*** End Patch"
	out2, isErr2 := call(t, &applyPatchTool{d: d}, map[string]any{"patch": patch2})
	if !isErr2 || !strings.Contains(out2, "已存在") {
		t.Fatalf("Create File 对已存在文件应拒绝: isErr=%v out=%s", isErr2, out2)
	}
	gotAfter, _ := os.ReadFile(p)
	if string(gotAfter) != want {
		t.Errorf("被拒绝的 Create File 不应改动原文件")
	}
}

// ---------- Step 5: 可选扩展工具默认关闭 + skill / fleetherd_task ----------

func TestExtensionsDisabledByDefault(t *testing.T) {
	d, _ := newTestDeps(t)
	for _, h := range All(d) {
		name := h.Definition().Name
		if name == "skill" || name == "fleetherd_task" {
			t.Errorf("未启用 extensions 配置时不应注册扩展工具 %q", name)
		}
	}
}

func writeStubFleetherd(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "fake-fleetherd")
	script := `#!/bin/sh
cmd="$1"
shift
case "$cmd" in
  skills)
    sub="$1"
    shift
    printf 'SKILLS:%s|%s\n' "$sub" "$*"
    ;;
  topic-new)
    printf '{"parent_topic":"top-coord-001"}\n'
    ;;
  task-agents)
    printf 'AGENTS:sock=%s|tok_set=%s|args=%s\n' "$FLEETHERD_SOCKET" "$([ -n "$FLEETHERD_TURN_TOKEN" ] && echo yes || echo no)" "$*"
    ;;
  task-new)
    # 故意把 token 回显出来以验证 runabout 会脱敏
    printf 'topic_id=child-topic-777\nargs=%s\nleak=%s\n' "$*" "$FLEETHERD_TURN_TOKEN"
    ;;
  task-send|task-read|tasks|task-status|task-stop)
    printf 'CMD:%s|args=%s\n' "$cmd" "$*"
    ;;
  *)
    printf 'UNKNOWN:%s\n' "$cmd" >&2
    exit 2
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestSkillExtensionTool(t *testing.T) {
	d, dir := newTestDeps(t)
	bin := writeStubFleetherd(t, dir)
	d.Cfg.Extensions.Skill = config.SkillExtCfg{
		Enabled:        true,
		Target:         "local",
		Binary:         bin,
		Timeout:        5 * time.Second,
		MaxOutputBytes: 64 << 10,
	}

	var found bool
	for _, h := range All(d) {
		if h.Definition().Name == "skill" {
			found = true
		}
	}
	if !found {
		t.Fatal("启用 extensions.skill 后应注册 skill 工具")
	}

	st := &skillTool{d: d}
	if out, isErr := call(t, st, map[string]any{"action": "list"}); isErr || !strings.Contains(out, "SKILLS:list|") {
		t.Fatalf("skill list 失败: isErr=%v out=%s", isErr, out)
	}
	if out, isErr := call(t, st, map[string]any{"action": "search", "query": "deploy docker"}); isErr || !strings.Contains(out, "SKILLS:search|deploy docker") {
		t.Fatalf("skill search 失败: isErr=%v out=%s", isErr, out)
	}
	if out, isErr := call(t, st, map[string]any{"action": "read", "name": "runabout-deploy", "file": "references/guide.md"}); isErr || !strings.Contains(out, "SKILLS:show|--again runabout-deploy references/guide.md") {
		t.Fatalf("skill read 失败: isErr=%v out=%s", isErr, out)
	}
	// 路径穿越与非法参数必须拒绝。
	for _, bad := range []map[string]any{
		{"action": "read", "name": "../etc/passwd"},
		{"action": "read", "name": "ok-skill", "file": "../../etc/shadow"},
		{"action": "read", "name": "ok-skill", "file": "/etc/passwd"},
		{"action": "search", "query": ""},
		{"action": "delete", "name": "foo"},
	} {
		if out, isErr := call(t, st, bad); !isErr {
			t.Errorf("非法调用 %v 应被拒绝，实际成功: %s", bad, out)
		}
	}
}

func TestFleetherdTaskExtensionTool(t *testing.T) {
	dir := t.TempDir()
	bin := writeStubFleetherd(t, dir)
	bridgeDir := filepath.Join(dir, "web-bridge")
	if err := os.MkdirAll(bridgeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secretToken := "super-secret-turn-token-999999"
	bridgeJSON := map[string]string{
		"session_id": "acp-session-1",
		"socket":     "/tmp/fake-daemon.sock",
		"token":      secretToken,
		"agent_id":   "gemini-task",
		"created_at": "2026-11-19T00:00:00Z",
	}
	rawBridge, _ := json.Marshal(bridgeJSON)
	if err := os.WriteFile(filepath.Join(bridgeDir, "sess_1234abcd.json"), rawBridge, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Auth.Enabled = false
	cfg.Server.DataDir = filepath.Join(dir, ".data")
	cfg.Tools.Shell.DefaultWorkdir = dir
	cfg.Audit.File = filepath.Join(dir, "audit.jsonl")
	cfg.Extensions.FleetherdTask = config.FleetherdTaskExtCfg{
		Enabled:           true,
		Target:            "local",
		Binary:            bin,
		WebBridgeDir:      bridgeDir,
		WebUIBaseURL:      "https://fleet.example",
		EmptyTopicCommand: []string{bin, "topic-new", "--empty", "--title", "{title}"},
		Timeout:           5 * time.Second,
		MaxOutputBytes:    64 << 10,
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	aud, err := audit.New(cfg.Audit, log)
	if err != nil {
		t.Fatal(err)
	}
	defer aud.Close()
	sg, err := policy.NewShellGuard(cfg.Policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &Deps{
		Cfg: cfg, Paths: policy.NewPathGuard(cfg.Policy, dir), Shell: sg,
		Confirm: policy.NewConfirmStore(cfg.Policy.ConfirmTTL), Audit: aud,
		Procs: NewProcManager(4), Log: log,
		Fleet: fleet.New(cfg.Fleet, cfg.Tools.Shell.Path, cfg.Tools.Shell.Env),
	}

	// 1. 启用 fleetherd_task 后，read 工具绝不能读取 web_bridge_dir 里的映射文件。
	if out, isErr := call(t, &readTool{d: d, sem: make(chan struct{}, 1)}, map[string]any{
		"path": filepath.Join(bridgeDir, "sess_1234abcd.json"),
	}); !isErr {
		t.Fatalf("read 工具不应能读取 web-bridge 文件: %s", out)
	}

	ft := &fleetherdTaskTool{d: d}

	// 2. 带 session_ref 调用：注入 socket 与 token，但绝不把明文 token 返回给模型。
	out, isErr := call(t, ft, map[string]any{
		"action":      "new",
		"session_ref": "sess_1234abcd",
		"agent":       "codex",
		"prompt":      "hello 'world' $(rm -rf /)",
	})
	if isErr {
		t.Fatalf("带 session_ref 的 task-new 失败: %s", out)
	}
	if strings.Contains(out, secretToken) {
		t.Fatalf("返回给模型的输出绝不能包含明文 FLEETHERD_TURN_TOKEN:\n%s", out)
	}
	if !strings.Contains(out, "[REDACTED]") || !strings.Contains(out, "https://fleet.example/topics/child-topic-777") {
		t.Errorf("应脱敏 token 并附带 Web UI 链接:\n%s", out)
	}

	// 审计日志中也绝不能出现明文 token。
	aud.ToolCall(mcp.Principal{Subject: "tester"}, "s", "fleetherd_task", json.RawMessage(`{"action":"new","session_ref":"sess_1234abcd"}`), false, "", time.Millisecond)
	auditRaw, _ := os.ReadFile(cfg.Audit.File)
	if strings.Contains(string(auditRaw), secretToken) {
		t.Fatalf("审计日志绝不能记录明文 FLEETHERD_TURN_TOKEN")
	}

	// 3. 无 session_ref 且未传 parent_topic 时，自动调用 empty_topic_command 创建空白顶层话题并挂子任务。
	out, isErr = call(t, ft, map[string]any{
		"action": "new",
		"title":  "重构鉴权",
		"prompt": "梳理 OAuth 流程",
	})
	if isErr {
		t.Fatalf("无 session_ref 自动建空白顶层话题失败: %s", out)
	}
	if !strings.Contains(out, "parent_topic: top-coord-001") || !strings.Contains(out, "-c top-coord-001") {
		t.Errorf("应返回 parent_topic 并通过 -c 挂载子任务:\n%s", out)
	}

	// 4. stop 转发 task-stop，topic 必填且沿用同一套校验。
	out, isErr = call(t, ft, map[string]any{"action": "stop", "topic": "child-topic-777"})
	if isErr {
		t.Fatalf("action=stop 失败: %s", out)
	}
	if !strings.Contains(out, "CMD:task-stop|args=-- child-topic-777") {
		t.Errorf("action=stop 应转发 task-stop:\n%s", out)
	}
	for _, bad := range []map[string]any{
		{"action": "stop"},
		{"action": "stop", "topic": "../bad"},
	} {
		if out, isErr := call(t, ft, bad); !isErr {
			t.Errorf("非法 stop 调用 %v 应被拒绝，实际成功: %s", bad, out)
		}
	}

	// 5. 白名单校验与路径穿越防护。
	for _, bad := range []map[string]any{
		{"action": "reload"},
		{"action": "config"},
		{"action": "agents", "session_ref": "../evil"},
		{"action": "agents", "session_ref": "short"},
		{"action": "send", "topic": "../bad", "prompt": "hi"},
	} {
		if out, isErr := call(t, ft, bad); !isErr {
			t.Errorf("非法调用 %v 应被拒绝，实际成功: %s", bad, out)
		}
	}
}
