package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noir017/agent-tools-mcp/internal/audit"
	"github.com/noir017/agent-tools-mcp/internal/config"
	"github.com/noir017/agent-tools-mcp/internal/mcp"
	"github.com/noir017/agent-tools-mcp/internal/policy"
)

func newTestDeps(t *testing.T) (*Deps, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Auth.Enabled = false
	cfg.Server.DataDir = filepath.Join(dir, ".data")
	cfg.Tools.Shell.DefaultWorkdir = dir
	cfg.Audit.Enabled = false
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	aud, err := audit.New(cfg.Audit, log)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := policy.NewShellGuard(cfg.Policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Deps{
		Cfg: cfg, Paths: policy.NewPathGuard(cfg.Policy, dir), Shell: guard,
		Confirm: policy.NewConfirmStore(cfg.Policy.ConfirmTTL), Audit: aud,
		Procs: NewProcManager(4), Log: log,
	}, dir
}

func call(t *testing.T, h mcp.Handler, args map[string]any) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.Call(&mcp.CallContext{Ctx: context.Background(),
		Principal: mcp.Principal{Subject: "tester"}}, raw)
	if err != nil {
		t.Fatalf("工具返回协议级错误: %v", err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		sb.WriteString(c.Text)
	}
	return sb.String(), res.IsError
}

// ---------- apply_patch ----------

func TestApplyPatchUpdate(t *testing.T) {
	d, dir := newTestDeps(t)
	tool := &applyPatchTool{d: d}
	target := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc main() {\n\tprintln(\"old\")\n}\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	patch := `*** Begin Patch
*** Update File: ` + target + `
@@ func main() {
-	println("old")
+	println("new")
+	println("另一行")
*** End Patch`
	out, isErr := call(t, tool, map[string]any{"patch": patch})
	if isErr {
		t.Fatalf("补丁应当成功: %s", out)
	}
	got := readFile(t, target)
	want := "package main\n\nfunc main() {\n\tprintln(\"new\")\n\tprintln(\"另一行\")\n}\n"
	if got != want {
		t.Errorf("内容不对:\n得到 %q\n期望 %q", got, want)
	}
	if !strings.Contains(out, "+2/-1") {
		t.Errorf("摘要里应有增删行数统计: %s", out)
	}
}

func TestApplyPatchAddDeleteMove(t *testing.T) {
	d, dir := newTestDeps(t)
	tool := &applyPatchTool{d: d}
	old := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(old, []byte("删我\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	moveSrc := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(moveSrc, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newFile := filepath.Join(dir, "sub", "new.md")
	moveDst := filepath.Join(dir, "b.txt")

	patch := `*** Begin Patch
*** Add File: ` + newFile + `
+# 标题
+正文
*** Delete File: ` + old + `
*** Update File: ` + moveSrc + `
*** Move to: ` + moveDst + `
@@
 line1
-line2
+line2 改过了
*** End Patch`
	out, isErr := call(t, tool, map[string]any{"patch": patch})
	if isErr {
		t.Fatalf("补丁应当成功: %s", out)
	}
	if got := readFile(t, newFile); got != "# 标题\n正文\n" {
		t.Errorf("新建文件内容不对: %q", got)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old.txt 应已被删除")
	}
	if _, err := os.Stat(moveSrc); !os.IsNotExist(err) {
		t.Error("重命名后源文件应消失")
	}
	if got := readFile(t, moveDst); got != "line1\nline2 改过了\n" {
		t.Errorf("重命名后内容不对: %q", got)
	}
}

func TestApplyPatchAtomicOnFailure(t *testing.T) {
	d, dir := newTestDeps(t)
	tool := &applyPatchTool{d: d}
	good := filepath.Join(dir, "good.txt")
	if err := os.WriteFile(good, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(bad, []byte("完全不同的内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	patch := `*** Begin Patch
*** Update File: ` + good + `
@@
-keep
+改了
*** Update File: ` + bad + `
@@
-这行文件里没有
+新内容
*** End Patch`
	out, isErr := call(t, tool, map[string]any{"patch": patch})
	if !isErr {
		t.Fatal("第二个文件对不上，整个补丁应当失败")
	}
	if !strings.Contains(out, "没有任何文件被改动") {
		t.Errorf("失败信息应说明没有落盘: %s", out)
	}
	if got := readFile(t, good); got != "keep\n" {
		t.Errorf("第一个文件不该被改动，实际 %q", got)
	}
}

func TestApplyPatchDryRun(t *testing.T) {
	d, dir := newTestDeps(t)
	tool := &applyPatchTool{d: d}
	f := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(f, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "*** Begin Patch\n*** Update File: " + f + "\n@@\n-b\n+c\n*** End Patch"
	out, isErr := call(t, tool, map[string]any{"patch": patch, "dry_run": true})
	if isErr {
		t.Fatalf("dry_run 应当成功: %s", out)
	}
	if got := readFile(t, f); got != "a\nb\n" {
		t.Errorf("dry_run 不应写盘，实际 %q", got)
	}
}

func TestApplyPatchWhitespaceTolerance(t *testing.T) {
	// 模型常把行尾空白吃掉；上下文能对上就应该允许，同时给出提示。
	d, dir := newTestDeps(t)
	tool := &applyPatchTool{d: d}
	f := filepath.Join(dir, "ws.txt")
	if err := os.WriteFile(f, []byte("foo   \nbar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "*** Begin Patch\n*** Update File: " + f + "\n@@\n foo\n-bar\n+baz\n*** End Patch"
	out, isErr := call(t, tool, map[string]any{"patch": patch})
	if isErr {
		t.Fatalf("应当容忍行尾空白差异: %s", out)
	}
	if !strings.Contains(readFile(t, f), "baz") {
		t.Error("改动没生效")
	}
}

func TestApplyPatchErrorsAreActionable(t *testing.T) {
	d, dir := newTestDeps(t)
	tool := &applyPatchTool{d: d}
	f := filepath.Join(dir, "y.txt")
	if err := os.WriteFile(f, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 上下文对不上时要告诉模型"先 read 再重试"
	patch := "*** Begin Patch\n*** Update File: " + f + "\n@@\n-不存在的行\n+x\n*** End Patch"
	out, _ := call(t, tool, map[string]any{"patch": patch})
	if !strings.Contains(out, "read") {
		t.Errorf("错误信息应引导模型先 read 确认内容: %s", out)
	}
	// Add 一个已存在的文件要提示改用 Update / write
	patch = "*** Begin Patch\n*** Add File: " + f + "\n+x\n*** End Patch"
	out, isErr := call(t, tool, map[string]any{"patch": patch})
	if !isErr || !strings.Contains(out, "Update File") {
		t.Errorf("对已存在文件用 Add 应给出明确指引: %s", out)
	}
}

// ---------- read / write ----------

func TestReadWithLineNumbersAndOffset(t *testing.T) {
	d, dir := newTestDeps(t)
	f := filepath.Join(dir, "lines.txt")
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		sb.WriteString("line" + string(rune('0'+i%10)) + "\n")
	}
	if err := os.WriteFile(f, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := call(t, &readTool{d: d}, map[string]any{"path": f, "offset": 3, "limit": 2})
	if isErr {
		t.Fatalf("读取失败: %s", out)
	}
	if !strings.Contains(out, "     3\t") || !strings.Contains(out, "     4\t") {
		t.Errorf("应带行号输出第 3-4 行:\n%s", out)
	}
	if strings.Contains(out, "     5\t") {
		t.Errorf("limit 应限制到 2 行:\n%s", out)
	}
	if !strings.Contains(out, "共 10 行") {
		t.Errorf("应说明总行数:\n%s", out)
	}
}

func TestReadRejectsBinary(t *testing.T) {
	d, dir := newTestDeps(t)
	f := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(f, []byte{0x00, 0x01, 0x02, 0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := call(t, &readTool{d: d}, map[string]any{"path": f})
	if !isErr || !strings.Contains(out, "二进制") {
		t.Errorf("二进制文件应被拒绝并给出替代方案: %s", out)
	}
}

func TestWriteAtomicAndAppend(t *testing.T) {
	d, dir := newTestDeps(t)
	tool := &writeTool{d: d}
	f := filepath.Join(dir, "deep", "nested", "out.txt")
	if out, isErr := call(t, tool, map[string]any{"path": f, "content": "第一版\n"}); isErr {
		t.Fatalf("写入失败: %s", out)
	}
	if got := readFile(t, f); got != "第一版\n" {
		t.Errorf("内容不对: %q", got)
	}
	if out, isErr := call(t, tool, map[string]any{"path": f, "content": "追加\n", "append": true}); isErr {
		t.Fatalf("追加失败: %s", out)
	}
	if got := readFile(t, f); got != "第一版\n追加\n" {
		t.Errorf("追加结果不对: %q", got)
	}
	out, _ := call(t, tool, map[string]any{"path": f, "content": "覆盖\n"})
	if !strings.Contains(out, "原文件") {
		t.Errorf("覆盖时应说明原文件被替换: %s", out)
	}
}

// ---------- search / glob / list_dir ----------

func TestSearchAndGlob(t *testing.T) {
	d, dir := newTestDeps(t)
	mustWrite(t, filepath.Join(dir, "a.go"), "package main\n// TODO: 修一下\nfunc main(){}\n")
	mustWrite(t, filepath.Join(dir, "sub", "b.go"), "package sub\n// TODO: 也要修\n")
	mustWrite(t, filepath.Join(dir, "sub", "c.txt"), "无关内容\n")
	mustWrite(t, filepath.Join(dir, "node_modules", "d.go"), "// TODO: 不该被搜到\n")

	out, isErr := call(t, &searchTool{d: d}, map[string]any{"pattern": "TODO", "path": dir})
	if isErr {
		t.Fatalf("搜索失败: %s", out)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Errorf("应命中 a.go 与 b.go:\n%s", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Errorf("默认不该搜 node_modules:\n%s", out)
	}

	out, isErr = call(t, &searchTool{d: d}, map[string]any{
		"pattern": "TODO", "path": dir, "glob": "*.txt"})
	if isErr {
		t.Fatalf("带 glob 的搜索失败: %s", out)
	}
	if strings.Contains(out, "a.go") {
		t.Errorf("glob=*.txt 时不该命中 .go 文件:\n%s", out)
	}

	out, isErr = call(t, &globTool{d: d}, map[string]any{"pattern": "**/*.go", "path": dir})
	if isErr {
		t.Fatalf("glob 失败: %s", out)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Errorf("**/*.go 应同时命中两层:\n%s", out)
	}

	out, isErr = call(t, &listDirTool{d: d}, map[string]any{"path": dir, "depth": 2})
	if isErr {
		t.Fatalf("list_dir 失败: %s", out)
	}
	if !strings.Contains(out, "sub/") || !strings.Contains(out, "c.txt") {
		t.Errorf("depth=2 应展开子目录:\n%s", out)
	}
}

// ---------- shell ----------

func TestShellForegroundAndBackground(t *testing.T) {
	d, dir := newTestDeps(t)
	sh := &shellTool{d: d}

	out, isErr := call(t, sh, map[string]any{"command": "echo 标准输出; echo 标准错误 >&2; exit 0"})
	if isErr {
		t.Fatalf("命令应成功: %s", out)
	}
	if !strings.Contains(out, "标准输出") || !strings.Contains(out, "标准错误") {
		t.Errorf("stdout 与 stderr 都应展示:\n%s", out)
	}

	out, isErr = call(t, sh, map[string]any{"command": "sleep 5", "timeout_ms": 300})
	if !isErr || !strings.Contains(out, "超时") {
		t.Errorf("超时应被明确标注:\n%s", out)
	}

	out, isErr = call(t, sh, map[string]any{"command": "cat", "stdin": "喂进去的内容"})
	if isErr {
		t.Fatalf("stdin 传递失败: %s", out)
	}
	if !strings.Contains(out, "喂进去的内容") {
		t.Errorf("stdin 应被命令读到:\n%s", out)
	}

	out, isErr = call(t, sh, map[string]any{"command": "pwd", "workdir": dir})
	if isErr || !strings.Contains(out, dir) {
		t.Errorf("workdir 未生效:\n%s", out)
	}

	// 后台任务：启动 → 拉输出 → 结束
	out, isErr = call(t, sh, map[string]any{
		"command":           "for i in 1 2 3; do echo tick$i; sleep 0.1; done",
		"run_in_background": true, "label": "计数",
	})
	if isErr {
		t.Fatalf("后台启动失败: %s", out)
	}
	var procID string
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, "proc_") {
			procID = field
		}
	}
	if procID == "" {
		t.Fatalf("没有拿到后台进程 id:\n%s", out)
	}
	out, isErr = call(t, &shellOutputTool{d: d}, map[string]any{"process_id": procID, "wait_ms": 3000})
	if isErr {
		t.Fatalf("读取后台输出失败: %s", out)
	}
	if !strings.Contains(out, "tick3") {
		t.Errorf("后台输出不完整:\n%s", out)
	}
	if out, _ := call(t, &shellListTool{d: d}, nil); !strings.Contains(out, "计数") {
		t.Errorf("shell_list 应显示 label:\n%s", out)
	}
}

func TestShellDangerousCommandBlocked(t *testing.T) {
	d, _ := newTestDeps(t)
	out, isErr := call(t, &shellTool{d: d}, map[string]any{"command": "rm -rf /"})
	if !isErr {
		t.Fatal("rm -rf / 必须被拦下")
	}
	if !strings.Contains(out, "rm-recursive") {
		t.Errorf("应说明命中了哪条规则:\n%s", out)
	}
}

// ---------- 辅助 ----------

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return string(b)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRelativePathsUseDefaultWorkdir 守住一致性：文件工具和 shell 必须以
// 同一个目录为相对路径基准，否则 "src/main.go" 在两个工具里指向不同文件。
func TestRelativePathsUseDefaultWorkdir(t *testing.T) {
	d, dir := newTestDeps(t)
	if out, isErr := call(t, &writeTool{d: d}, map[string]any{
		"path": "sub/rel.txt", "content": "内容\n"}); isErr {
		t.Fatalf("写入失败: %s", out)
	}
	want := filepath.Join(dir, "sub", "rel.txt")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("相对路径应落在默认工作目录下（期望 %s）: %v", want, err)
	}
	out, isErr := call(t, &readTool{d: d}, map[string]any{"path": "sub/rel.txt"})
	if isErr || !strings.Contains(out, "内容") {
		t.Errorf("读同一个相对路径应拿到刚写的内容:\n%s", out)
	}
	// shell 的 pwd 必须是同一个目录
	out, _ = call(t, &shellTool{d: d}, map[string]any{"command": "cat sub/rel.txt"})
	if !strings.Contains(out, "内容") {
		t.Errorf("shell 的相对路径基准应与文件工具一致:\n%s", out)
	}
}
