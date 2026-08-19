package tools

import (
	"fmt"
	"strings"
)

// 本文件实现 apply_patch 的补丁格式（业界称 V4A，codex / ChatGPT 最熟悉的那种）：
//
//	*** Begin Patch
//	*** Update File: path/to/file.go
//	@@ func Foo(
//	 保持不变的上下文行
//	-要删掉的行
//	+要加上的行
//	*** Add File: path/to/new.txt
//	+第一行
//	*** Delete File: path/to/old.txt
//	*** End Patch
//
// 与 unified diff 的关键区别是不带行号：定位完全靠上下文，所以模型不需要
// 先数行号，改动也不会因为行号偏移而失败。

const (
	markBeginPatch = "*** Begin Patch"
	markEndPatch   = "*** End Patch"
	markAddFile    = "*** Add File:"
	markUpdateFile = "*** Update File:"
	markDeleteFile = "*** Delete File:"
	markMoveTo     = "*** Move to:"
	markEndOfFile  = "*** End of File"
)

type patchKind string

const (
	kindAdd    patchKind = "add"
	kindUpdate patchKind = "update"
	kindDelete patchKind = "delete"
)

type hunkLine struct {
	op   byte // ' ' 上下文, '-' 删除, '+' 新增
	text string
}

type hunk struct {
	header string // @@ 后面的内容，用作定位提示，可为空
	lines  []hunkLine
	atEOF  bool // 带 *** End of File 标记，锚定在文件末尾
}

type patchOp struct {
	kind    patchKind
	path    string
	moveTo  string
	content string // kind==add 时的完整文件内容
	hunks   []hunk
}

// parsePatch 解析补丁信封。为了少给模型添麻烦，做了几处宽容处理：
// 缺少 Begin/End Patch 包裹也能解析，Add File 段落里没写 + 前缀也接受。
func parsePatch(src string) ([]patchOp, error) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	lines := strings.Split(src, "\n")

	// 去掉可能存在的 ``` 代码围栏。
	lines = stripFences(lines)

	i := 0
	// 跳过 Begin Patch 之前的闲话。
	for i < len(lines) && strings.TrimSpace(lines[i]) != markBeginPatch {
		if isFileMarker(lines[i]) {
			break // 没写 Begin Patch，直接从第一个文件段开始
		}
		i++
	}
	if i < len(lines) && strings.TrimSpace(lines[i]) == markBeginPatch {
		i++
	}
	if i >= len(lines) {
		return nil, fmt.Errorf("补丁内容里找不到任何文件段落；格式应为 %q 后接 %q / %q / %q",
			markBeginPatch, markUpdateFile, markAddFile, markDeleteFile)
	}

	var ops []patchOp
	var cur *patchOp
	var curHunk *hunk

	flushHunk := func() {
		if cur != nil && curHunk != nil {
			cur.hunks = append(cur.hunks, *curHunk)
			curHunk = nil
		}
	}
	flushOp := func() {
		flushHunk()
		if cur != nil {
			ops = append(ops, *cur)
			cur = nil
		}
	}

	var addLines []string
	for ; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == markEndPatch:
			if cur != nil && cur.kind == kindAdd {
				cur.content = joinAdd(addLines)
				addLines = nil
			}
			flushOp()
			return validateOps(ops)

		case strings.HasPrefix(trimmed, markUpdateFile):
			if cur != nil && cur.kind == kindAdd {
				cur.content = joinAdd(addLines)
				addLines = nil
			}
			flushOp()
			cur = &patchOp{kind: kindUpdate, path: markerValue(trimmed, markUpdateFile)}

		case strings.HasPrefix(trimmed, markAddFile):
			if cur != nil && cur.kind == kindAdd {
				cur.content = joinAdd(addLines)
			}
			flushOp()
			addLines = nil
			cur = &patchOp{kind: kindAdd, path: markerValue(trimmed, markAddFile)}

		case strings.HasPrefix(trimmed, markDeleteFile):
			if cur != nil && cur.kind == kindAdd {
				cur.content = joinAdd(addLines)
				addLines = nil
			}
			flushOp()
			cur = &patchOp{kind: kindDelete, path: markerValue(trimmed, markDeleteFile)}
			flushOp()

		case strings.HasPrefix(trimmed, markMoveTo):
			if cur == nil || cur.kind != kindUpdate {
				return nil, fmt.Errorf("%q 只能紧跟在 %q 之后", markMoveTo, markUpdateFile)
			}
			cur.moveTo = markerValue(trimmed, markMoveTo)

		case trimmed == markEndOfFile:
			if curHunk != nil {
				curHunk.atEOF = true
			}

		case strings.HasPrefix(line, "@@"):
			if cur == nil || cur.kind != kindUpdate {
				return nil, fmt.Errorf("@@ 段落必须出现在 %q 之后（第 %d 行）", markUpdateFile, i+1)
			}
			flushHunk()
			curHunk = &hunk{header: strings.TrimSpace(strings.TrimPrefix(line, "@@"))}

		default:
			if cur == nil {
				continue // Begin Patch 之前/之后的杂项文本，忽略
			}
			switch cur.kind {
			case kindAdd:
				addLines = append(addLines, strings.TrimPrefix(line, "+"))
			case kindUpdate:
				if curHunk == nil {
					// 没写 @@ 就直接给差异行：补一个匿名 hunk。
					if strings.TrimSpace(line) == "" {
						continue
					}
					curHunk = &hunk{}
				}
				if strings.HasPrefix(line, `\ No newline`) {
					continue
				}
				curHunk.lines = append(curHunk.lines, parseHunkLine(line))
			}
		}
	}

	// 没有 End Patch 也收尾，不因为缺一行标记就整体失败。
	if cur != nil && cur.kind == kindAdd {
		cur.content = joinAdd(addLines)
	}
	flushOp()
	return validateOps(ops)
}

func parseHunkLine(line string) hunkLine {
	if line == "" {
		return hunkLine{op: ' ', text: ""}
	}
	switch line[0] {
	case '+', '-', ' ':
		return hunkLine{op: line[0], text: line[1:]}
	default:
		// 少了前导空格的上下文行，按上下文处理。
		return hunkLine{op: ' ', text: line}
	}
}

func joinAdd(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func markerValue(line, marker string) string {
	return strings.TrimSpace(strings.TrimPrefix(line, marker))
}

func isFileMarker(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, markAddFile) || strings.HasPrefix(t, markUpdateFile) ||
		strings.HasPrefix(t, markDeleteFile)
}

func stripFences(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			continue
		}
		out = append(out, l)
	}
	return out
}

func validateOps(ops []patchOp) ([]patchOp, error) {
	if len(ops) == 0 {
		return nil, fmt.Errorf("补丁里没有任何文件改动")
	}
	for i, op := range ops {
		if op.path == "" {
			return nil, fmt.Errorf("第 %d 个文件段落缺少路径", i+1)
		}
		if op.kind == kindUpdate && len(op.hunks) == 0 {
			return nil, fmt.Errorf("%s 的 Update File 段落里没有 @@ 差异块；"+
				"如果是要整体替换文件，请用 write 工具", op.path)
		}
		if op.kind == kindUpdate {
			for j, h := range op.hunks {
				if len(h.lines) == 0 {
					return nil, fmt.Errorf("%s 的第 %d 个 @@ 块是空的", op.path, j+1)
				}
			}
		}
	}
	return ops, nil
}

// applyHunks 把若干 hunk 依次套到文件内容上，返回新内容。
// 定位策略：先精确匹配，失败后依次放宽到"忽略行尾空白"和"忽略首尾空白"。
func applyHunks(original string, hunks []hunk) (string, []string, error) {
	lines := splitKeepEnding(original)
	var notes []string
	pos := 0

	for hi, h := range hunks {
		expected := make([]string, 0, len(h.lines))
		for _, hl := range h.lines {
			if hl.op == ' ' || hl.op == '-' {
				expected = append(expected, hl.text)
			}
		}
		replacement := make([]string, 0, len(h.lines))
		for _, hl := range h.lines {
			if hl.op == ' ' || hl.op == '+' {
				replacement = append(replacement, hl.text)
			}
		}

		searchFrom := pos
		if h.header != "" {
			if idx := findHeader(lines, h.header, pos); idx >= 0 {
				searchFrom = idx
			} else {
				notes = append(notes, fmt.Sprintf("第 %d 块的 @@ 提示 %q 没在文件里找到，已忽略该提示、仅按上下文定位",
					hi+1, truncate(h.header, 60)))
			}
		}

		if len(expected) == 0 {
			// 纯新增：有 @@ 提示就插在提示行之后，否则追加到文件末尾。
			at := len(lines)
			if h.header != "" && searchFrom < len(lines) {
				at = searchFrom + 1
			}
			lines = insertAt(lines, at, replacement)
			pos = at + len(replacement)
			continue
		}

		start, how := findBlock(lines, expected, searchFrom, h.atEOF)
		if start < 0 {
			return "", notes, fmt.Errorf("第 %d 个 @@ 块的上下文在文件里找不到。\n"+
				"期望匹配的内容（前 5 行）：\n%s\n"+
				"请先用 read 看一眼这段的当前内容，再按实际文本重新生成补丁；"+
				"注意上下文行要一字不差（含缩进）",
				hi+1, indentLines(firstN(expected, 5), "  | "))
		}
		if how != "exact" {
			notes = append(notes, fmt.Sprintf("第 %d 块以%s方式匹配（文件里的空白与补丁不完全一致）", hi+1, how))
		}
		lines = append(lines[:start], append(replacement, lines[start+len(expected):]...)...)
		pos = start + len(replacement)
	}
	return strings.Join(lines, "\n"), notes, nil
}

// splitKeepEnding 按行切分；末尾换行不产出空行元素，拼回去时再补。
func splitKeepEnding(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		return lines[:n-1]
	}
	return lines
}

func findHeader(lines []string, header string, from int) int {
	for i := from; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == strings.TrimSpace(header) ||
			strings.Contains(lines[i], header) {
			return i
		}
	}
	// 提示行可能在 pos 之前（模型给的顺序不一定单调），再从头找一遍。
	for i := 0; i < from && i < len(lines); i++ {
		if strings.Contains(lines[i], header) {
			return i
		}
	}
	return -1
}

// findBlock 在 lines 里定位 expected，返回起始下标与匹配严格程度。
func findBlock(lines, expected []string, from int, atEOF bool) (int, string) {
	if len(expected) == 0 || len(expected) > len(lines) {
		return -1, ""
	}
	matchers := []struct {
		name string
		eq   func(a, b string) bool
	}{
		{"exact", func(a, b string) bool { return a == b }},
		{"忽略行尾空白", func(a, b string) bool {
			return strings.TrimRight(a, " \t") == strings.TrimRight(b, " \t")
		}},
		{"忽略首尾空白", func(a, b string) bool {
			return strings.TrimSpace(a) == strings.TrimSpace(b)
		}},
	}
	tryAt := func(start int, eq func(a, b string) bool) bool {
		for k, want := range expected {
			if !eq(lines[start+k], want) {
				return false
			}
		}
		return true
	}
	for _, m := range matchers {
		if atEOF {
			if start := len(lines) - len(expected); start >= 0 && tryAt(start, m.eq) {
				return start, m.name
			}
		}
		for start := max(0, from); start+len(expected) <= len(lines); start++ {
			if tryAt(start, m.eq) {
				return start, m.name
			}
		}
		// from 之后找不到时，允许回头再全文找一次。
		for start := 0; start < from && start+len(expected) <= len(lines); start++ {
			if tryAt(start, m.eq) {
				return start, m.name
			}
		}
	}
	return -1, ""
}

func insertAt(lines []string, at int, add []string) []string {
	if at > len(lines) {
		at = len(lines)
	}
	out := make([]string, 0, len(lines)+len(add))
	out = append(out, lines[:at]...)
	out = append(out, add...)
	return append(out, lines[at:]...)
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func indentLines(lines []string, prefix string) string {
	var sb strings.Builder
	for i, l := range lines {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(prefix)
		sb.WriteString(l)
	}
	return sb.String()
}

// countChanges 统计一组 hunk 的增删行数。
func countChanges(hunks []hunk) (added, removed int) {
	for _, h := range hunks {
		for _, l := range h.lines {
			switch l.op {
			case '+':
				added++
			case '-':
				removed++
			}
		}
	}
	return added, removed
}
