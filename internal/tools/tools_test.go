package tools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/noir017/runabout/internal/audit"
	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/mcp"
	"github.com/noir017/runabout/internal/policy"
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

// only_new 的语义是"自上一次 only_new 读取之后的新增"，游标必须在锁内推进：
// 轮询长任务时多个 shell_output 会并发落到同一个 Proc 上。
func TestProcTakeNewIsIncrementalAndRaceFree(t *testing.T) {
	p := &Proc{ID: "proc_test", out: newCapWriter(1 << 20)}

	if got := p.TakeNew(); got != "" {
		t.Fatalf("空进程首次 TakeNew 应为空，得到 %q", got)
	}
	p.out.Write([]byte("line 1\nline 2\n"))
	if got := p.TakeNew(); got != "line 1\nline 2\n" {
		t.Fatalf("首次增量不对: %q", got)
	}
	if got := p.TakeNew(); got != "" {
		t.Fatalf("没有新输出时应为空，得到 %q", got)
	}
	p.out.Write([]byte("line 3\n"))
	if got := p.TakeNew(); got != "line 3\n" {
		t.Fatalf("第二次增量应只含新行，得到 %q", got)
	}

	// 并发读写：-race 下能抓到游标未加锁的情况；同时验证每段输出只被交付一次。
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.out.Write([]byte("x\n"))
			n := len(p.TakeNew())
			mu.Lock()
			seen += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	seen += len(p.TakeNew())
	if seen != 16 {
		t.Fatalf("8 个 goroutine 各写 2 字节，累计交付应为 16，得到 %d", seen)
	}
}

// 输出超过上限被头尾裁剪后，only_new 仍要拿到全部新内容。
// 游标必须用绝对写入偏移，不能用快照字符串的下标。
func TestProcTakeNewKeepsNewOutputAfterTruncation(t *testing.T) {
	p := &Proc{ID: "proc_trunc", out: newCapWriter(20)}

	p.out.Write([]byte(strings.Repeat("a", 20)))
	if got, want := p.TakeNew(), strings.Repeat("a", 20); got != want {
		t.Fatalf("首段应完整返回: got %q", got)
	}
	p.out.Write([]byte(strings.Repeat("b", 10)))
	if got, want := p.TakeNew(), strings.Repeat("b", 10); got != want {
		t.Fatalf("截断后新写入的 b 不能丢: got %q", got)
	}
	p.out.Write([]byte(strings.Repeat("c", 10)))
	if got, want := p.TakeNew(), strings.Repeat("c", 10); got != want {
		t.Fatalf("再次截断后新写入的 c 不能丢: got %q", got)
	}
	if got := p.TakeNew(); got != "" {
		t.Fatalf("没有新输出时应为空: %q", got)
	}
}

// 新内容已被裁掉一部分时不能静默丢弃：必须说明丢了多少字节，
// 并返回仍然保留的那部分。
func TestProcTakeNewReportsLostBytesAfterTruncation(t *testing.T) {
	p := &Proc{ID: "proc_trunc_loss", out: newCapWriter(20)}

	p.out.Write([]byte(strings.Repeat("a", 20)))
	p.TakeNew() // 游标推进到 20
	// total=40：保留 head=[0,10)、tail=[30,40)，游标 20 落在缺口里。
	p.out.Write([]byte(strings.Repeat("b", 20)))
	got := p.TakeNew()
	if !strings.Contains(got, "…（中间省略 10 字节，共 40 字节）…") {
		t.Fatalf("应完整写出丢失的字节数: %q", got)
	}
	if !strings.HasSuffix(got, strings.Repeat("b", 10)) {
		t.Fatalf("应返回仍然保留的新内容: %q", got)
	}
}

// 截断点落在多字节字符中间时，输出必须是合法 UTF-8，不能出现半个汉字。
func TestProcTakeNewTruncationOnRuneBoundary(t *testing.T) {
	// 上限 20：head 取前 10 字节，切在"界"（第 9-11 字节）中间。
	p := &Proc{ID: "proc_utf8_head", out: newCapWriter(20)}
	p.out.Write([]byte("你好世界" + strings.Repeat("x", 20)))
	got := p.TakeNew()
	if !utf8.ValidString(got) {
		t.Fatalf("输出必须是合法 UTF-8: %q", got)
	}
	if !strings.HasPrefix(got, "你好世") {
		t.Fatalf("head 末尾的半个字符应整段丢掉: %q", got)
	}
	if !strings.Contains(got, "…（中间省略 13 字节，共 32 字节）…") {
		t.Fatalf("省略标记应完整且字节数正确: %q", got)
	}

	// 上限 20：tail 从 [17,27) 开始，起点切在"世"（第 16-18 字节）中间。
	p2 := &Proc{ID: "proc_utf8_tail", out: newCapWriter(20)}
	p2.out.Write([]byte(strings.Repeat("a", 10) + "你好世界" + strings.Repeat("b", 5)))
	got = p2.TakeNew()
	if !utf8.ValidString(got) {
		t.Fatalf("输出必须是合法 UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "界"+strings.Repeat("b", 5)) {
		t.Fatalf("tail 开头不完整的字符应丢掉: %q", got)
	}
}

// Write 把汉字切成两次写入时游标会落在字符中间：TakeNew 必须跳到下一个
// 完整字符，跳过的续字节要写进省略提示，不能返回非法 UTF-8。
func TestProcTakeNewCursorInsideRune(t *testing.T) {
	p := &Proc{ID: "proc_utf8_cursor", out: newCapWriter(1 << 10)}
	// "你" = E4 BD A0，先写前两个字节，游标随之停在这两个字节之后。
	p.out.Write([]byte{0xE4, 0xBD})
	p.TakeNew()
	// 再写最后一个字节和后续内容，此时游标正好落在续字节上。
	p.out.Write([]byte{0xA0, 'o', 'k'})
	got := p.TakeNew()
	if !utf8.ValidString(got) {
		t.Fatalf("输出必须是合法 UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "ok") {
		t.Fatalf("跳过的续字节之后的内容应完整返回: %q", got)
	}
	if !strings.Contains(got, "…（中间省略 1 字节，共 5 字节）…") {
		t.Fatalf("跳过的续字节必须写进省略提示: %q", got)
	}
}

// 并发写入且输出已被截断：游标必须单调推进，每个字节要么交付要么计入
// 省略，-race 下不能报数据竞争。读者先启动、写者每写一块让出调度，
// 保证读写真正交错；旧实现（字符串下标当游标）只能读到一两次，
// 会卡在非空读取次数上。
func TestProcTakeNewConcurrentWithTruncation(t *testing.T) {
	const (
		writers  = 4
		chunks   = 100
		chunk    = "abcdefghij"
		minReads = 50
	)
	p := &Proc{ID: "proc_trunc_race", out: newCapWriter(16)}
	// 先写到超过 cap，保证后续每次读取都跨在缺口上。
	p.out.Write([]byte(strings.Repeat(chunk, 2)))
	prefill := 2 * len(chunk)

	writersDone := make(chan struct{})
	readerReady := make(chan struct{})
	reads := make(chan string, 1<<16)
	go func() {
		defer close(reads)
		ready := false
		for {
			if out := p.TakeNew(); out != "" {
				reads <- out
				if !ready {
					close(readerReady)
					ready = true
				}
				continue
			}
			select {
			case <-writersDone:
				// 写完后再读一次，把最后一段收干净。
				if out := p.TakeNew(); out != "" {
					reads <- out
					continue
				}
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	// 等读者先拿到预填内容，再开始写，避免第一次读已经在全部写完之后。
	<-readerReady

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < chunks; j++ {
				p.out.Write([]byte(chunk))
				// 让读者有机会在两次写入之间读到增量；只用 Gosched
				// 时写者仍可能在读者被调度前跑完。
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()
	close(writersDone)

	marker := regexp.MustCompile(`\n?…（中间省略 (\d+) 字节，共 \d+ 字节）…\n`)
	delivered, skipped, nonEmpty := 0, 0, 0
	for out := range reads {
		nonEmpty++
		for _, m := range marker.FindAllStringSubmatch(out, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("无法解析省略字节数: %q", out)
			}
			skipped += n
		}
		rest := marker.ReplaceAllString(out, "")
		if strings.IndexFunc(rest, func(r rune) bool { return r < 'a' || r > 'j' }) >= 0 {
			t.Fatalf("正文只能来自写入的字符，标记必须完整: %q", out)
		}
		delivered += len(rest)
	}
	if nonEmpty < minReads {
		t.Fatalf("非空读取只有 %d 次（< %d），读写没有真正交错", nonEmpty, minReads)
	}
	if want := prefill + writers*chunks*len(chunk); delivered+skipped != want {
		t.Fatalf("交付 %d + 跳过 %d != 写入 %d", delivered, skipped, want)
	}
	if !p.out.Truncated() || skipped == 0 {
		t.Fatalf("用例应触发截断并跳过字节: truncated=%v skipped=%d", p.out.Truncated(), skipped)
	}
}
