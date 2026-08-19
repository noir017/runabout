package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/noir017/agent-tools-mcp/internal/mcp"
)

type listDirTool struct{ d *Deps }

type listDirArgs struct {
	Path       string `json:"path"`
	Depth      int    `json:"depth"`
	Hidden     bool   `json:"hidden"`
	MaxEntries int    `json:"max_entries"`
	SortBy     string `json:"sort_by"`
}

func (t *listDirTool) Definition() mcp.ToolDef {
	return mcp.ToolDef{
		Name:  "list_dir",
		Title: "列出目录内容",
		Description: `列出目录内容，输出类型、大小、修改时间。

- depth=1（默认）只看当前层；depth>1 以树状展开，方便快速了解项目结构。
- 默认不显示隐藏项，hidden=true 可显示。
- 展开多层时会跳过 .git、node_modules 等目录，避免刷屏。`,
		InputSchema: schema(map[string]any{
			"path":        strProp("目录路径，默认当前工作目录"),
			"depth":       intProp("展开层数，默认 1", 1, 8),
			"hidden":      boolProp("显示隐藏文件"),
			"max_entries": intProp("最多列出多少项，默认 500", 1, 5000),
			"sort_by":     enumProp("排序方式，默认 name", "name", "mtime", "size"),
		}),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: mcp.Ptr(true)},
	}
}

func (t *listDirTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a listDirArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, mcp.Errorf(mcp.CodeInvalidParams, "%s", err.Error())
	}
	path := a.Path
	if strings.TrimSpace(path) == "" {
		path = t.d.Cfg.Tools.Shell.DefaultWorkdir
	}
	abs, err := t.d.Paths.CheckRead(path)
	if err != nil {
		return mcp.ToolError("%s", err.Error()), nil
	}
	st, err := os.Stat(abs)
	if err != nil {
		return mcp.ToolError("无法访问 %s: %v", abs, err), nil
	}
	if !st.IsDir() {
		return mcp.ToolError("%s 不是目录，用 read 读文件", abs), nil
	}

	depth := a.Depth
	if depth <= 0 {
		depth = 1
	}
	maxEntries := a.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 500
	}
	ignore := map[string]bool{}
	for _, d := range t.d.Cfg.Tools.Search.IgnoreDirs {
		ignore[d] = true
	}

	var sb strings.Builder
	items := []map[string]any{}
	count := 0
	truncated := false

	var walk func(dir string, level int, prefix string) error
	walk = func(dir string, level int, prefix string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(&sb, "%s(无法读取: %v)\n", prefix, err)
			return nil
		}
		entries = filterEntries(entries, a.Hidden)
		sortEntries(entries, a.SortBy)
		for _, e := range entries {
			if count >= maxEntries {
				truncated = true
				return nil
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			count++
			full := filepath.Join(dir, e.Name())
			kind := "file"
			display := e.Name()
			switch {
			case e.IsDir():
				kind = "dir"
				display += "/"
			case info.Mode()&os.ModeSymlink != 0:
				kind = "symlink"
				if target, err := os.Readlink(full); err == nil {
					display += " -> " + target
				}
			}
			size := humanBytes(info.Size())
			if e.IsDir() {
				size = "-"
			}
			fmt.Fprintf(&sb, "%s%-40s %10s  %s\n", prefix, display, size,
				info.ModTime().Format("2006-01-02 15:04"))
			items = append(items, map[string]any{
				"path": full, "name": e.Name(), "type": kind,
				"size": info.Size(), "mode": info.Mode().String(),
				"mtime": info.ModTime().Format(time.RFC3339),
			})
			if e.IsDir() && level < depth {
				if ignore[e.Name()] {
					fmt.Fprintf(&sb, "%s  …（%s 已跳过）\n", prefix, e.Name())
					continue
				}
				if err := walk(full, level+1, prefix+"  "); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(abs, 1, ""); err != nil {
		return mcp.ToolError("列目录失败: %v", err), nil
	}

	header := fmt.Sprintf("%s（%d 项", abs, count)
	if truncated {
		header += fmt.Sprintf("，已截断到 %d", maxEntries)
	}
	header += "）\n\n"
	if count == 0 {
		return mcp.Textf("%s 是空目录。", abs), nil
	}
	return mcp.Text(header + strings.TrimRight(sb.String(), "\n")).WithStructured(map[string]any{
		"path": abs, "entries": items, "count": count, "truncated": truncated,
	}), nil
}

func filterEntries(entries []os.DirEntry, hidden bool) []os.DirEntry {
	if hidden {
		return entries
	}
	out := entries[:0]
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func sortEntries(entries []os.DirEntry, by string) {
	switch by {
	case "mtime":
		sort.Slice(entries, func(i, j int) bool {
			a, errA := entries[i].Info()
			b, errB := entries[j].Info()
			if errA != nil || errB != nil {
				return entries[i].Name() < entries[j].Name()
			}
			return a.ModTime().After(b.ModTime())
		})
	case "size":
		sort.Slice(entries, func(i, j int) bool {
			a, errA := entries[i].Info()
			b, errB := entries[j].Info()
			if errA != nil || errB != nil {
				return entries[i].Name() < entries[j].Name()
			}
			return a.Size() > b.Size()
		})
	default:
		// 目录优先，再按名字排，读起来最顺。
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].IsDir() != entries[j].IsDir() {
				return entries[i].IsDir()
			}
			return entries[i].Name() < entries[j].Name()
		})
	}
}
