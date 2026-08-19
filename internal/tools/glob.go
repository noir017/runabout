package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/noir017/agent-tools-mcp/internal/globmatch"
	"github.com/noir017/agent-tools-mcp/internal/mcp"
)

type globTool struct{ d *Deps }

type globArgs struct {
	Pattern     string `json:"pattern"`
	Path        string `json:"path"`
	SortBy      string `json:"sort_by"`
	MaxResults  int    `json:"max_results"`
	Hidden      bool   `json:"hidden"`
	IncludeDirs bool   `json:"include_dirs"`
}

func (t *globTool) Definition() mcp.ToolDef {
	return mcp.ToolDef{
		Name:  "glob",
		Title: "按文件名模式查找",
		Description: fmt.Sprintf(`按通配模式查找文件路径，默认按修改时间从新到旧排列（先看到最近改过的文件）。

- pattern 支持 * ? [] {a,b}，以及跨层级的 **，例如 "**/*.go"、"src/**/*.{ts,tsx}"、"Dockerfile*"。
- 模式不含 / 时会自动按任意层级匹配文件名（"*.go" 等价于 "**/*.go"）。
- path 指定搜索起点，默认当前工作目录。
- 默认最多返回 %d 条，跳过 .git、node_modules 等目录。
- 找内容用 search，找文件名用 glob。`, t.d.Cfg.Tools.Search.MaxGlobResult),
		InputSchema: schema(map[string]any{
			"pattern":      strProp("文件名通配模式，如 **/*.go"),
			"path":         strProp("搜索起点目录"),
			"sort_by":      enumProp("排序方式，默认 mtime", "mtime", "path", "size"),
			"max_results":  intProp("最多返回多少条", 1, 5000),
			"hidden":       boolProp("包含隐藏文件与常被忽略的目录"),
			"include_dirs": boolProp("结果中也包含目录"),
		}, "pattern"),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: mcp.Ptr(true)},
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
	cfg := t.d.Cfg.Tools.Search
	max := a.MaxResults
	if max <= 0 {
		max = cfg.MaxGlobResult
	}
	root := a.Path
	if strings.TrimSpace(root) == "" {
		root = t.d.Cfg.Tools.Shell.DefaultWorkdir
	}
	abs, err := t.d.Paths.CheckRead(root)
	if err != nil {
		return mcp.ToolError("%s", err.Error()), nil
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
		return mcp.Textf("在 %s 下没有匹配 %q 的路径。\n\n"+
			"提示：跨层级要用 **（如 **/*.go）；目标可能是隐藏文件或在 .gitignore 里，可试 hidden=true。",
			abs, a.Pattern).WithStructured(map[string]any{"files": []any{}, "count": 0}), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "在 %s 下匹配 %q 的 %d 个路径", abs, a.Pattern, len(hits))
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
		"files": items, "count": len(items), "truncated": truncated, "root": abs,
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
