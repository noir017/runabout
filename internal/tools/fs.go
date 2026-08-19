package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/noir017/agent-tools-mcp/internal/mcp"
)

// ---------- read ----------

type readTool struct{ d *Deps }

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
	Raw    bool   `json:"raw"`
}

func (t *readTool) Definition() mcp.ToolDef {
	fs := t.d.Cfg.Tools.FS
	return mcp.ToolDef{
		Name:  "read",
		Title: "读取文件",
		Description: fmt.Sprintf(`读取文件内容，默认带行号输出（形如 "  12\t代码"），便于随后用 apply_patch 精确改动。

- 默认从第 1 行起最多 %d 行；大文件请用 offset/limit 分段读。
- 单行超过 %d 字符会被截断。
- raw=true 时不加行号，适合需要原样内容的场景。
- 图片（png/jpg/gif/webp）会以图像形式返回；其他二进制文件会被拒绝，请改用 shell 里的 xxd/file。
- 少数凭据类文件（私钥、云凭据等）被策略禁止读取，会返回明确错误。`,
			fs.DefaultLines, fs.MaxLineChars),
		InputSchema: schema(map[string]any{
			"path":   strProp("文件路径，绝对或相对（相对于服务默认工作目录）"),
			"offset": intProp("起始行号，从 1 开始", 1, 0),
			"limit":  intProp("最多读取多少行", 1, 0),
			"raw":    boolProp("不输出行号"),
		}, "path"),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: mcp.Ptr(true)},
	}
}

func (t *readTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a readArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, mcp.Errorf(mcp.CodeInvalidParams, "%s", err.Error())
	}
	abs, err := t.d.Paths.CheckRead(a.Path)
	if err != nil {
		t.d.Audit.Policy(cc.Principal, cc.SessionID, "read", "denied", "path-deny", a.Path, "", err.Error())
		return mcp.ToolError("%s", err.Error()), nil
	}
	st, err := os.Stat(abs)
	if err != nil {
		return mcp.ToolError("无法访问 %s: %v", abs, err), nil
	}
	if st.IsDir() {
		return mcp.ToolError("%s 是目录，请用 list_dir", abs), nil
	}
	fs := t.d.Cfg.Tools.FS
	if st.Size() > int64(fs.MaxReadBytes) {
		return mcp.ToolError("文件 %s 有 %s，超过单次读取上限 %s；请用 offset/limit 分段读，"+
			"或用 shell 里的 sed -n / rg 定位后再读",
			abs, humanBytes(st.Size()), humanBytes(int64(fs.MaxReadBytes))), nil
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return mcp.ToolError("读取 %s 失败: %v", abs, err), nil
	}

	if mime, isImg := imageMime(abs); isImg {
		if len(data) > fs.MaxImageBytes {
			return mcp.ToolError("图片 %s 有 %s，超过上限 %s", abs, humanBytes(int64(len(data))),
				humanBytes(int64(fs.MaxImageBytes))), nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{
			mcp.TextContent(fmt.Sprintf("图片 %s（%s，%s）", abs, mime, humanBytes(int64(len(data))))),
			mcp.ImageContent(base64.StdEncoding.EncodeToString(data), mime),
		}}, nil
	}
	if isBinary(data) {
		return mcp.ToolError("%s 看起来是二进制文件（%s）。如需查看请用 shell：file %s / xxd %s | head",
			abs, humanBytes(st.Size()), abs, abs), nil
	}

	offset := a.Offset
	if offset <= 0 {
		offset = 1
	}
	limit := a.Limit
	if limit <= 0 {
		limit = fs.DefaultLines
	}

	text, meta := formatLines(string(data), offset, limit, fs.MaxLineChars, a.Raw)
	meta["path"] = abs
	meta["size_bytes"] = st.Size()

	var header strings.Builder
	fmt.Fprintf(&header, "%s（%s，共 %d 行）", abs, humanBytes(st.Size()), meta["total_lines"])
	if meta["truncated_lines"] == true {
		fmt.Fprintf(&header, "\n显示第 %v-%v 行；还有内容未显示，可继续用 offset=%v 读取",
			meta["start_line"], meta["end_line"], meta["next_offset"])
	}
	return mcp.Text(header.String() + "\n\n" + text).WithStructured(meta), nil
}

// formatLines 按行切片并可选加行号。
func formatLines(content string, offset, limit, maxLineChars int, raw bool) (string, map[string]any) {
	lines := strings.Split(content, "\n")
	// 末尾换行会产生一个空元素，不算作一行。
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	total := len(lines)
	start := offset - 1
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	selected := lines[start:end]

	var sb strings.Builder
	for i, ln := range selected {
		if maxLineChars > 0 && len(ln) > maxLineChars {
			ln = truncate(ln, maxLineChars) + fmt.Sprintf("[本行共 %d 字符，已截断]", len(ln))
		}
		if raw {
			sb.WriteString(ln)
		} else {
			fmt.Fprintf(&sb, "%6d\t%s", start+i+1, ln)
		}
		sb.WriteString("\n")
	}
	meta := map[string]any{
		"total_lines":     total,
		"start_line":      start + 1,
		"end_line":        end,
		"truncated_lines": end < total,
		"next_offset":     end + 1,
	}
	if total == 0 {
		return "(空文件)", meta
	}
	return strings.TrimRight(sb.String(), "\n"), meta
}

func imageMime(path string) (string, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".gif":
		return "image/gif", true
	case ".webp":
		return "image/webp", true
	case ".bmp":
		return "image/bmp", true
	}
	return "", false
}

// isBinary 用"含 NUL 字节或大量非法 UTF-8"来判断二进制。
func isBinary(data []byte) bool {
	probe := data
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return true
	}
	invalid := 0
	for i := 0; i < len(probe); {
		r, size := utf8.DecodeRune(probe[i:])
		if r == utf8.RuneError && size <= 1 {
			invalid++
		}
		i += size
	}
	return len(probe) > 0 && invalid*100/len(probe) > 5
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return fmt.Sprintf("%.1f PB", v/unit)
}

// ---------- write ----------

type writeTool struct{ d *Deps }

type writeArgs struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Append     bool   `json:"append"`
	CreateDirs *bool  `json:"create_dirs"`
	Mode       string `json:"mode"`
}

func (t *writeTool) Definition() mcp.ToolDef {
	return mcp.ToolDef{
		Name:  "write",
		Title: "写入文件",
		Description: `把内容整体写入文件（不存在则创建，存在则覆盖）。

- 改动已有文件请优先用 apply_patch：它按上下文定位、只动该动的地方，不会把没读过的内容覆盖掉。
- write 适合新建文件、或明确要整体替换的场景。
- append=true 追加而不是覆盖。
- 写入采用"临时文件 + 原子替换"，中途失败不会留下半个文件。`,
		InputSchema: schema(map[string]any{
			"path":        strProp("目标文件路径"),
			"content":     strProp("要写入的完整内容"),
			"append":      boolProp("追加到文件末尾而不是覆盖"),
			"create_dirs": boolProp("父目录不存在时自动创建，默认 true"),
			"mode":        strProp("文件权限，八进制字符串如 \"0644\"；仅新建文件时生效"),
		}, "path", "content"),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: mcp.Ptr(true)},
	}
}

func (t *writeTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a writeArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, mcp.Errorf(mcp.CodeInvalidParams, "%s", err.Error())
	}
	abs, err := t.d.Paths.CheckWrite(a.Path)
	if err != nil {
		t.d.Audit.Policy(cc.Principal, cc.SessionID, "write", "denied", "path-deny", a.Path, "", err.Error())
		return mcp.ToolError("%s", err.Error()), nil
	}
	if max := t.d.Cfg.Tools.FS.MaxWriteBytes; max > 0 && len(a.Content) > max {
		return mcp.ToolError("内容有 %s，超过单次写入上限 %s；请分批写或改用 shell",
			humanBytes(int64(len(a.Content))), humanBytes(int64(max))), nil
	}

	mode := os.FileMode(0o644)
	if a.Mode != "" {
		parsed, err := strconv.ParseUint(strings.TrimPrefix(a.Mode, "0o"), 8, 32)
		if err != nil {
			return mcp.ToolError("mode %q 不是合法的八进制权限，例如 \"0644\"", a.Mode), nil
		}
		mode = os.FileMode(parsed)
	}

	createDirs := a.CreateDirs == nil || *a.CreateDirs
	if createDirs {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return mcp.ToolError("创建父目录失败: %v", err), nil
		}
	}

	existed := false
	var oldSize int64
	if st, err := os.Stat(abs); err == nil {
		existed = true
		oldSize = st.Size()
		mode = st.Mode().Perm()
	}

	if a.Append {
		f, err := os.OpenFile(abs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, mode)
		if err != nil {
			return mcp.ToolError("打开 %s 失败: %v", abs, err), nil
		}
		defer f.Close()
		if _, err := f.WriteString(a.Content); err != nil {
			return mcp.ToolError("追加写入失败: %v", err), nil
		}
	} else if err := atomicWrite(abs, []byte(a.Content), mode); err != nil {
		return mcp.ToolError("写入 %s 失败: %v", abs, err), nil
	}

	action := "创建"
	if existed {
		action = "覆盖"
		if a.Append {
			action = "追加"
		}
	}
	lines := strings.Count(a.Content, "\n")
	if len(a.Content) > 0 && !strings.HasSuffix(a.Content, "\n") {
		lines++
	}
	msg := fmt.Sprintf("已%s %s（%s，%d 行）", action, abs, humanBytes(int64(len(a.Content))), lines)
	if existed && !a.Append {
		msg += fmt.Sprintf("；原文件 %s 已被替换", humanBytes(oldSize))
	}
	return mcp.Text(msg).WithStructured(map[string]any{
		"path": abs, "bytes": len(a.Content), "lines": lines,
		"existed": existed, "old_size": oldSize, "append": a.Append,
	}), nil
}

// atomicWrite 先写同目录临时文件再 rename，避免写坏原文件。
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
