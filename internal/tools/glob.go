package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/noir017/runabout/internal/fleet"
	"github.com/noir017/runabout/internal/globmatch"
	"github.com/noir017/runabout/internal/mcp"
)

type globTool struct{ d *Deps }

type globArgs struct {
	Target      string `json:"target"`
	Pattern     string `json:"pattern"`
	Path        string `json:"path"`
	SortBy      string `json:"sort_by"`
	MaxResults  int    `json:"max_results"`
	Hidden      bool   `json:"hidden"`
	IncludeDirs bool   `json:"include_dirs"`
}

func (t *globTool) Definition() mcp.ToolDef {
	remoteDoc := ""
	if t.d.fleetEnabled() {
		remoteDoc = "\n- 远端机器有 rg 就用 rg，没有就退回 find，并在结果里注明。"
	}
	return mcp.ToolDef{
		Name:  "glob",
		Title: "按文件名模式查找",
		Description: fmt.Sprintf(`按通配模式查找文件路径，默认按修改时间从新到旧排列（先看到最近改过的文件）。

- pattern 支持 * ? [] {a,b}，以及跨层级的 **，例如 "**/*.go"、"src/**/*.{ts,tsx}"、"Dockerfile*"。
- 模式不含 / 时会自动按任意层级匹配文件名（"*.go" 等价于 "**/*.go"）。
- path 指定搜索起点，默认当前工作目录。
- 默认最多返回 %d 条，跳过 .git、node_modules 等目录。
- 找内容用 search，找文件名用 glob。%s`, t.d.Cfg.Tools.Search.MaxGlobResult, remoteDoc),
		InputSchema: schema(t.d.withTarget(map[string]any{
			"pattern":      strProp("文件名通配模式，如 **/*.go"),
			"path":         strProp("搜索起点目录"),
			"sort_by":      enumProp("排序方式，默认 mtime", "mtime", "path", "size"),
			"max_results":  intProp("最多返回多少条", 1, 5000),
			"hidden":       boolProp("包含隐藏文件与常被忽略的目录"),
			"include_dirs": boolProp("结果中也包含目录"),
		}), "pattern"),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    mcp.Ptr(true),
			IdempotentHint:  mcp.Ptr(true),
			DestructiveHint: mcp.Ptr(false),
		},
	}
}

type globHit struct {
	Path  string
	Size  int64
	Mtime time.Time
	IsDir bool
}

func (t *globTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a globArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, mcp.Errorf(mcp.CodeInvalidParams, "%s", err.Error())
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return mcp.ToolError("pattern 不能为空"), nil
	}
	target, err := t.d.resolveTarget(a.Target)
	if err != nil {
		return mcp.ToolError("%s", err.Error()), nil
	}
	cfg := t.d.Cfg.Tools.Search
	max := a.MaxResults
	if max <= 0 {
		max = cfg.MaxGlobResult
	}
	root := a.Path
	if strings.TrimSpace(root) == "" {
		root = t.d.Cfg.Tools.Shell.DefaultWorkdir
	}
	_, guard := t.d.fsFor(target)
	abs, err := resolveReadPath(t.d, target, guard, root)
	if err != nil {
		return mcp.ToolError("%s", err.Error()), nil
	}
	if target.remote {
		return t.globRemote(cc.Ctx, target, a, abs, max)
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return mcp.ToolError("%s 不是可访问的目录", abs), nil
	}

	// 绝对路径模式（/etc/*.conf）直接以模式自身为准，不再拼接 root。
	pattern := a.Pattern
	absolute := strings.HasPrefix(pattern, "/")

	ctx, cancel := context.WithTimeout(cc.Ctx, cfg.Timeout)
	defer cancel()

	ignore := map[string]bool{}
	for _, d := range cfg.IgnoreDirs {
		ignore[d] = true
	}

	var hits []globHit
	truncated := false
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		name := d.Name()
		if d.IsDir() {
			if path != abs && !a.Hidden && (ignore[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			if !a.IncludeDirs {
				return nil
			}
		} else if !a.Hidden && strings.HasPrefix(name, ".") {
			return nil
		}
		if path == abs {
			return nil
		}

		subject := path
		if !absolute {
			rel, relErr := filepath.Rel(abs, path)
			if relErr != nil {
				return nil
			}
			subject = rel
		}
		if !globMatchLoose(pattern, subject, name) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		hits = append(hits, globHit{Path: path, Size: info.Size(), Mtime: info.ModTime(), IsDir: d.IsDir()})
		if len(hits) > max {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !strings.Contains(err.Error(), "skip") {
		return mcp.ToolError("遍历 %s 失败: %v", abs, err), nil
	}

	switch a.SortBy {
	case "path":
		sort.Slice(hits, func(i, j int) bool { return hits[i].Path < hits[j].Path })
	case "size":
		sort.Slice(hits, func(i, j int) bool { return hits[i].Size > hits[j].Size })
	default:
		sort.Slice(hits, func(i, j int) bool { return hits[i].Mtime.After(hits[j].Mtime) })
	}
	if len(hits) > max {
		hits, truncated = hits[:max], true
	}

	if len(hits) == 0 {
		return mcp.Textf("%s在 %s 下没有匹配 %q 的路径。\n\n"+
			"提示：跨层级要用 **（如 **/*.go）；目标可能是隐藏文件或在 .gitignore 里，可试 hidden=true。",
			label(target), abs, a.Pattern).WithStructured(map[string]any{
			"target": target.name, "files": []any{}, "count": 0,
		}), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s在 %s 下匹配 %q 的 %d 个路径", label(target), abs, a.Pattern, len(hits))
	if truncated {
		fmt.Fprintf(&sb, "（已截断到 %d 条）", max)
	}
	sb.WriteString("：\n")
	items := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		mark := ""
		if h.IsDir {
			mark = "/"
		}
		fmt.Fprintf(&sb, "\n%s%s\t%s\t%s", h.Path, mark, humanBytes(h.Size),
			h.Mtime.Format("2006-01-02 15:04"))
		items = append(items, map[string]any{
			"path": h.Path, "size": h.Size, "is_dir": h.IsDir,
			"mtime": h.Mtime.Format(time.RFC3339),
		})
	}
	return mcp.Text(sb.String()).WithStructured(map[string]any{
		"target": target.name, "files": items, "count": len(items),
		"truncated": truncated, "root": abs,
	}), nil
}

// globMatchLoose 在标准匹配之外补两条便利规则：模式不含 / 时按任意层级匹配
// 文件名；以 / 结尾的模式当作目录前缀。
func globMatchLoose(pattern, rel, name string) bool {
	if globmatch.Match(pattern, rel) {
		return true
	}
	if !strings.Contains(pattern, "/") {
		return globmatch.Match(pattern, name)
	}
	return false
}

func (t *globTool) globRemote(ctx context.Context, target targetInfo, a globArgs, root string, max int) (*mcp.CallToolResult, error) {
	script := t.remoteGlobScript(a, root, max)
	res, err := t.d.fleetOrLocal().Run(ctx, fleet.Spec{
		Target: target.name, Command: script, MaxOutputBytes: 4 << 20,
	})
	if err != nil {
		return mcp.ToolError("%s", targetError(target.name, err)), nil
	}
	engine := "find（远端回退）"
	filter := true
	if strings.Contains(res.Stderr, "engine=rg") {
		engine, filter = "rg --files（远端）", false
	}
	if res.ExitCode != 0 {
		msg := firstLine(res.Stderr)
		if msg == "" {
			msg = "exit=" + strconv.Itoa(res.ExitCode)
		}
		return mcp.ToolError("%s查找失败：%s", label(target), msg), nil
	}

	ignore := map[string]bool{}
	for _, d := range t.d.Cfg.Tools.Search.IgnoreDirs {
		ignore[d] = true
	}
	hits := make([]globHit, 0, max)
	truncated := false
	for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		size, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			continue
		}
		mt, _ := strconv.ParseInt(parts[1], 10, 64)
		raw, err := strconv.ParseUint(parts[2], 16, 32)
		if err != nil {
			continue
		}
		p := parts[3]
		if filter {
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				continue
			}
			if hiddenOrIgnored(rel, a.Hidden, ignore) {
				continue
			}
			subject := rel
			if strings.HasPrefix(a.Pattern, "/") {
				subject = p
			}
			if !globMatchLoose(a.Pattern, subject, path.Base(p)) {
				continue
			}
		}
		hits = append(hits, globHit{
			Path: p, Size: size, Mtime: time.Unix(mt, 0),
			IsDir: raw&0o170000 == 0o040000,
		})
		if len(hits) > max {
			truncated = true
			break
		}
	}
	if len(hits) > max {
		hits, truncated = hits[:max], true
	}

	switch a.SortBy {
	case "path":
		sort.Slice(hits, func(i, j int) bool { return hits[i].Path < hits[j].Path })
	case "size":
		sort.Slice(hits, func(i, j int) bool { return hits[i].Size > hits[j].Size })
	default:
		sort.Slice(hits, func(i, j int) bool { return hits[i].Mtime.After(hits[j].Mtime) })
	}

	if len(hits) == 0 {
		return mcp.Textf("%s在 %s 下没有匹配 %q 的路径（%s）。\n\n"+
			"提示：跨层级要用 **（如 **/*.go）；目标可能是隐藏文件，可试 hidden=true。",
			label(target), root, a.Pattern, engine).WithStructured(map[string]any{
			"target": target.name, "files": []any{}, "count": 0, "engine": engine,
		}), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s在 %s 下匹配 %q 的 %d 个路径（%s）", label(target), root, a.Pattern, len(hits), engine)
	if strings.Contains(engine, "回退") {
		sb.WriteString("；回退模式下部分参数可能不生效")
	}
	if truncated {
		fmt.Fprintf(&sb, "（已截断到 %d 条）", max)
	}
	sb.WriteString("：\n")
	items := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		mark := ""
		if h.IsDir {
			mark = "/"
		}
		fmt.Fprintf(&sb, "\n%s%s\t%s\t%s", h.Path, mark, humanBytes(h.Size), h.Mtime.Format("2006-01-02 15:04"))
		items = append(items, map[string]any{
			"path": h.Path, "size": h.Size, "is_dir": h.IsDir, "mtime": h.Mtime.Format(time.RFC3339),
		})
	}
	return mcp.Text(sb.String()).WithStructured(map[string]any{
		"target": target.name, "files": items, "count": len(items),
		"truncated": truncated, "root": root, "engine": engine,
	}), nil
}

// remoteGlobScript 有 rg 用 rg --files，否则用 find；引擎标记写 stderr，
// 结果统一交给 while 循环用 stat 补上大小和 mtime。
func (t *globTool) remoteGlobScript(a globArgs, root string, max int) string {
	var b strings.Builder
	absolute := strings.HasPrefix(a.Pattern, "/")
	if !a.IncludeDirs && !absolute {
		b.WriteString("if command -v rg >/dev/null 2>&1; then\nprintf 'engine=rg\\n' >&2\nrg")
		args := []string{"--files"}
		if a.Hidden {
			args = append(args, "--hidden", "--no-ignore")
		}
		args = append(args, "--glob", a.Pattern)
		if !a.Hidden {
			for _, d := range t.d.Cfg.Tools.Search.IgnoreDirs {
				args = append(args, "--glob", "!"+d+"/**", "--glob", "!**/"+d+"/**")
			}
		}
		args = append(args, "--", root)
		for _, arg := range args {
			b.WriteString(" ")
			b.WriteString(fleet.Quote(arg))
		}
		b.WriteString("\nelse\n")
	}
	b.WriteString("printf 'engine=find\\n' >&2\nfind ")
	findArgs := []string{root}
	if a.IncludeDirs {
		findArgs = append(findArgs, "(", "-type", "f", "-o", "-type", "d", ")")
	} else {
		findArgs = append(findArgs, "-type", "f")
	}
	base := path.Base(a.Pattern)
	if base != "" && base != "." && base != "/" && base != "**" && !strings.ContainsAny(base, "{}") {
		findArgs = append(findArgs, "-name", base)
	}
	for i, arg := range findArgs {
		if i == 0 {
			b.WriteString(fleet.Quote(arg))
			continue
		}
		b.WriteString(" ")
		b.WriteString(fleet.Quote(arg))
	}
	if !a.IncludeDirs && !absolute {
		b.WriteString("\nfi")
	}
	limit := max*4 + 100
	if limit < 500 {
		limit = 500
	}
	fmt.Fprintf(&b, " | head -n %d | while IFS= read -r f; do stat -c '%%s|%%Y|%%f|%%n' -- \"$f\" 2>/dev/null; done", limit)
	return b.String()
}

func hiddenOrIgnored(rel string, hidden bool, ignore map[string]bool) bool {
	if hidden {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.HasPrefix(seg, ".") || ignore[seg] {
			return true
		}
	}
	return false
}
