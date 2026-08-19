package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/noir017/agent-tools-mcp/internal/globmatch"
	"github.com/noir017/agent-tools-mcp/internal/mcp"
)

type searchTool struct{ d *Deps }

type searchArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	Literal         bool   `json:"literal"`
	CaseInsensitive bool   `json:"case_insensitive"`
	ContextLines    int    `json:"context_lines"`
	FilesOnly       bool   `json:"files_only"`
	MaxResults      int    `json:"max_results"`
	Hidden          bool   `json:"hidden"`
}

func (t *searchTool) Definition() mcp.ToolDef {
	engine := "内置 Go 实现"
	if t.ripgrep() != "" {
		engine = "ripgrep（尊重 .gitignore）"
	}
	return mcp.ToolDef{
		Name:  "search",
		Title: "按内容搜索",
		Description: fmt.Sprintf(`在文件内容里搜索正则（当前引擎：%s），输出 路径:行号:内容。

- pattern 默认按正则解释；要搜字面量（含 . ( ) 等符号）请设 literal=true。
- path 可以是目录或单个文件，默认当前工作目录。
- glob 用来限定文件范围，例如 "*.go"、"**/*.{ts,tsx}"、"!vendor/**"。
- files_only=true 只列出命中的文件名，适合先摸清范围。
- context_lines 输出命中行前后各 N 行上下文。
- 默认最多返回 %d 条，超出会提示收窄条件。
- 默认跳过 .git、node_modules、vendor 等目录；hidden=true 时也搜隐藏文件。`,
			engine, t.d.Cfg.Tools.Search.MaxResults),
		InputSchema: schema(map[string]any{
			"pattern":          strProp("正则表达式（或 literal=true 时的字面量）"),
			"path":             strProp("搜索起点，目录或文件"),
			"glob":             strProp("文件名过滤，如 *.go；! 开头表示排除"),
			"literal":          boolProp("把 pattern 当字面量而非正则"),
			"case_insensitive": boolProp("忽略大小写"),
			"context_lines":    intProp("命中行前后各输出多少行上下文", 0, 20),
			"files_only":       boolProp("只输出命中的文件路径"),
			"max_results":      intProp("最多返回多少条", 1, 5000),
			"hidden":           boolProp("包含隐藏文件与被忽略的目录"),
		}, "pattern"),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: mcp.Ptr(true)},
	}
}

func (t *searchTool) ripgrep() string {
	cfg := t.d.Cfg.Tools.Search
	if !cfg.UseRipgrep {
		return ""
	}
	if cfg.RipgrepPath != "" {
		return cfg.RipgrepPath
	}
	if p, err := exec.LookPath("rg"); err == nil {
		return p
	}
	return ""
}

func (t *searchTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a searchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, mcp.Errorf(mcp.CodeInvalidParams, "%s", err.Error())
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return mcp.ToolError("pattern 不能为空"), nil
	}
	cfg := t.d.Cfg.Tools.Search
	max := a.MaxResults
	if max <= 0 {
		max = cfg.MaxResults
	}

	root := a.Path
	if strings.TrimSpace(root) == "" {
		root = t.d.Cfg.Tools.Shell.DefaultWorkdir
	}
	abs, err := t.d.Paths.CheckRead(root)
	if err != nil {
		return mcp.ToolError("%s", err.Error()), nil
	}
	if _, err := os.Stat(abs); err != nil {
		return mcp.ToolError("搜索起点 %s 不可用: %v", abs, err), nil
	}

	ctx, cancel := context.WithTimeout(cc.Ctx, cfg.Timeout)
	defer cancel()

	if rg := t.ripgrep(); rg != "" {
		res, err := t.searchWithRipgrep(ctx, rg, a, abs, max)
		if err == nil {
			return res, nil
		}
		// rg 失败（未安装、参数不兼容）时静默回退，不让用户感知实现细节。
		t.d.Log.Warn("ripgrep 调用失败，回退内置搜索", "err", err)
	}
	return t.searchNative(ctx, a, abs, max)
}

func (t *searchTool) searchWithRipgrep(ctx context.Context, rg string, a searchArgs, root string, max int) (*mcp.CallToolResult, error) {
	args := []string{"--line-number", "--no-heading", "--color", "never", "--with-filename"}
	if a.Literal {
		args = append(args, "--fixed-strings")
	}
	if a.CaseInsensitive {
		args = append(args, "--ignore-case")
	}
	if a.Hidden {
		args = append(args, "--hidden", "--no-ignore")
	}
	if a.Glob != "" {
		args = append(args, "--glob", a.Glob)
	}
	if a.ContextLines > 0 {
		args = append(args, "--context", fmt.Sprint(a.ContextLines))
	}
	if a.FilesOnly {
		args = append(args, "--files-with-matches")
	}
	// 多要一条用于判断"是否还有更多结果"。
	args = append(args, "--max-count", fmt.Sprint(max+1), "--", a.Pattern, root)

	cmd := exec.CommandContext(ctx, rg, args...)
	out := newCapWriter(4 << 20)
	errOut := newCapWriter(64 << 10)
	cmd.Stdout, cmd.Stderr = out, errOut
	err := cmd.Run()

	var ee *exec.ExitError
	if err != nil && errors.As(err, &ee) && ee.ExitCode() == 1 {
		// rg 用退出码 1 表示"没有匹配"。
		return t.emptyResult(a, root), nil
	}
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, truncate(errOut.String(), 300))
	}
	return t.formatMatches(a, root, splitNonEmpty(out.String()), max, "ripgrep"), nil
}

func (t *searchTool) searchNative(ctx context.Context, a searchArgs, root string, max int) (*mcp.CallToolResult, error) {
	expr := a.Pattern
	if a.Literal {
		expr = regexp.QuoteMeta(expr)
	}
	if a.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return mcp.ToolError("正则 %q 无法编译: %v（如果本来就是要搜字面量，请设 literal=true）",
			a.Pattern, err), nil
	}
	cfg := t.d.Cfg.Tools.Search
	ignore := map[string]bool{}
	for _, d := range cfg.IgnoreDirs {
		ignore[d] = true
	}

	var lines []string
	seenFiles := map[string]bool{}
	stopped := false

	walkFn := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 权限不足之类跳过即可
		}
		if ctx.Err() != nil {
			stopped = true
			return filepath.SkipAll
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (ignore[name] && !a.Hidden || (!a.Hidden && strings.HasPrefix(name, "."))) {
				return filepath.SkipDir
			}
			return nil
		}
		if !a.Hidden && strings.HasPrefix(name, ".") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if a.Glob != "" && !matchGlobFilter(a.Glob, rel, name) {
			return nil
		}
		if info, err := d.Info(); err == nil && cfg.MaxFileBytes > 0 && info.Size() > cfg.MaxFileBytes {
			return nil
		}
		hits, err := grepFile(path, re, a.ContextLines, max-len(lines))
		if err != nil {
			return nil
		}
		for _, h := range hits {
			if a.FilesOnly {
				if !seenFiles[path] {
					seenFiles[path] = true
					lines = append(lines, path)
				}
				break
			}
			lines = append(lines, h)
		}
		if len(lines) > max {
			stopped = true
			return filepath.SkipAll
		}
		return nil
	}

	st, _ := os.Stat(root)
	if st != nil && !st.IsDir() {
		hits, err := grepFile(root, re, a.ContextLines, max+1)
		if err != nil {
			return mcp.ToolError("读取 %s 失败: %v", root, err), nil
		}
		lines = hits
	} else if err := filepath.WalkDir(root, walkFn); err != nil && !errors.Is(err, filepath.SkipAll) {
		return mcp.ToolError("遍历 %s 失败: %v", root, err), nil
	}

	if len(lines) == 0 {
		return t.emptyResult(a, root), nil
	}
	res := t.formatMatches(a, root, lines, max, "内置引擎")
	if stopped && ctx.Err() != nil {
		res.Content = append(res.Content, mcp.TextContent("\n⚠️ 搜索超时，结果可能不完整；请缩小 path 范围或加上 glob 过滤"))
	}
	return res, nil
}

// grepFile 在单个文件里找匹配行，返回 "path:line:content" 形式。
func grepFile(path string, re *regexp.Regexp, contextLines, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// 先探一下是不是二进制，免得把乱码塞给模型。
	probe := make([]byte, 1024)
	n, _ := f.Read(probe)
	if isBinary(probe[:n]) {
		return nil, nil
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}

	var out []string
	var ring []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	pendingAfter := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		switch {
		case re.MatchString(line):
			if contextLines > 0 {
				for i, c := range ring {
					out = append(out, fmt.Sprintf("%s-%d-%s", path, lineNo-len(ring)+i, c))
				}
				ring = ring[:0]
			}
			out = append(out, fmt.Sprintf("%s:%d:%s", path, lineNo, line))
			pendingAfter = contextLines
		case pendingAfter > 0:
			out = append(out, fmt.Sprintf("%s-%d-%s", path, lineNo, line))
			pendingAfter--
		case contextLines > 0:
			ring = append(ring, line)
			if len(ring) > contextLines {
				ring = ring[1:]
			}
		}
		if len(out) >= limit {
			break
		}
	}
	return out, sc.Err()
}

// matchGlobFilter 支持 "!" 前缀表示排除，并且同时按相对路径和文件名匹配。
func matchGlobFilter(pattern, rel, name string) bool {
	negate := strings.HasPrefix(pattern, "!")
	pattern = strings.TrimPrefix(pattern, "!")
	hit := globmatch.Match(pattern, rel) || globmatch.Match(pattern, name)
	if !hit && !strings.Contains(pattern, "/") {
		hit = globmatch.Match("**/"+pattern, rel)
	}
	if negate {
		return !hit
	}
	return hit
}

func (t *searchTool) emptyResult(a searchArgs, root string) *mcp.CallToolResult {
	return mcp.Textf("在 %s 下没有匹配 %q 的内容。\n\n"+
		"若结果意外为空，可以试试：literal=true（pattern 含正则符号时）、"+
		"case_insensitive=true、hidden=true（目标在 .gitignore 里或是隐藏文件）、放宽 glob。",
		root, a.Pattern).WithStructured(map[string]any{"matches": []any{}, "count": 0})
}

func (t *searchTool) formatMatches(a searchArgs, root string, lines []string, max int, engine string) *mcp.CallToolResult {
	truncated := len(lines) > max
	if truncated {
		lines = lines[:max]
	}
	var sb strings.Builder
	label := "条匹配"
	if a.FilesOnly {
		label = "个文件命中"
	}
	fmt.Fprintf(&sb, "在 %s 下找到 %d %s（%s）", root, len(lines), label, engine)
	if truncated {
		fmt.Fprintf(&sb, "，已截断到前 %d 条；请收窄 pattern / glob / path 再搜", max)
	}
	sb.WriteString("\n\n")
	sb.WriteString(strings.Join(lines, "\n"))

	return mcp.Text(sb.String()).WithStructured(map[string]any{
		"matches": lines, "count": len(lines), "truncated": truncated, "root": root,
	})
}

func splitNonEmpty(s string) []string {
	raw := strings.Split(strings.TrimRight(s, "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

var _ = time.Second
