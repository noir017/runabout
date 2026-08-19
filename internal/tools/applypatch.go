package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/noir017/agent-tools-mcp/internal/mcp"
)

type applyPatchTool struct{ d *Deps }

type applyPatchArgs struct {
	Patch  string `json:"patch"`
	DryRun bool   `json:"dry_run"`
}

func (t *applyPatchTool) Definition() mcp.ToolDef {
	return mcp.ToolDef{
		Name:  "apply_patch",
		Title: "按补丁修改文件",
		Description: `用补丁信封精确修改文件，是改动已有文件的首选方式（比 write 安全：只动指定位置，不会覆盖没读过的内容）。

补丁格式（不带行号，靠上下文定位）：

*** Begin Patch
*** Update File: internal/app/server.go
@@ func (s *Server) Start(
 	s.log.Info("starting")
-	return http.ListenAndServe(addr, s.mux)
+	return s.srv.ListenAndServe()
*** Add File: docs/notes.md
+# 备注
+第二行
*** Delete File: legacy/old.go
*** End Patch

规则：
- 每行前缀：空格=上下文（保持不变）、"-"=删除、"+"=新增。上下文行必须与文件里现有内容一字不差（含缩进）。
- @@ 后面可以跟一段就近的代码（如函数签名）帮助定位，也可以留空。
- 一个 Update File 段落里可以有多个 @@ 块；一个补丁里可以同时改多个文件。
- 需要重命名：在 Update File 下一行写 *** Move to: 新路径。
- 改动前建议先 read 目标区域，确保上下文抄得准确。
- dry_run=true 只校验补丁能不能套上，不实际写盘。
- 全部文件要么一起成功、要么一起不动：任何一个文件套不上，整个补丁都不会落盘。`,
		InputSchema: schema(map[string]any{
			"patch":   strProp("完整的补丁文本，包含 *** Begin Patch / *** End Patch"),
			"dry_run": boolProp("只校验不写入"),
		}, "patch"),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: mcp.Ptr(true)},
	}
}

// plannedWrite 是一次待落盘的改动。
type plannedWrite struct {
	op       patchOp
	absPath  string
	absMove  string
	content  string
	mode     os.FileMode
	added    int
	removed  int
	notes    []string
	existing bool
}

func (t *applyPatchTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a applyPatchArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, mcp.Errorf(mcp.CodeInvalidParams, "%s", err.Error())
	}
	if strings.TrimSpace(a.Patch) == "" {
		return mcp.ToolError("patch 不能为空"), nil
	}
	ops, err := parsePatch(a.Patch)
	if err != nil {
		return mcp.ToolError("补丁格式有问题: %s", err.Error()), nil
	}

	// 第一阶段：全部算清楚，一个都不写。
	plans := make([]plannedWrite, 0, len(ops))
	for _, op := range ops {
		plan, err := t.plan(cc, op)
		if err != nil {
			return mcp.ToolError("处理 %s 时失败：%s\n\n（本次没有任何文件被改动）", op.path, err.Error()), nil
		}
		plans = append(plans, *plan)
	}

	if a.DryRun {
		return mcp.Text("dry_run：补丁可以正常套用，未写入任何文件。\n\n" + summarize(plans)).
			WithStructured(structuredPlans(plans, true)), nil
	}

	// 第二阶段：落盘。
	for _, p := range plans {
		if err := t.commit(p); err != nil {
			return mcp.ToolError("写入 %s 失败：%v\n\n注意：本次补丁涉及多个文件，前面的文件可能已经写入，"+
				"请用 read 或 git diff 确认当前状态", p.absPath, err), nil
		}
	}
	t.d.Log.Info("apply_patch 完成", "files", len(plans), "user", cc.Principal.Subject)
	return mcp.Text(summarize(plans)).WithStructured(structuredPlans(plans, false)), nil
}

func (t *applyPatchTool) plan(cc *mcp.CallContext, op patchOp) (*plannedWrite, error) {
	abs, err := t.d.Paths.CheckWrite(op.path)
	if err != nil {
		t.d.Audit.Policy(cc.Principal, cc.SessionID, "apply_patch", "denied", "path-deny", op.path, "", err.Error())
		return nil, err
	}
	p := &plannedWrite{op: op, absPath: abs, mode: 0o644}
	if op.moveTo != "" {
		moveAbs, err := t.d.Paths.CheckWrite(op.moveTo)
		if err != nil {
			return nil, err
		}
		p.absMove = moveAbs
	}

	st, statErr := os.Stat(abs)
	p.existing = statErr == nil
	if p.existing {
		if st.IsDir() {
			return nil, fmt.Errorf("%s 是目录", abs)
		}
		p.mode = st.Mode().Perm()
	}

	switch op.kind {
	case kindAdd:
		if p.existing {
			return nil, fmt.Errorf("%s 已存在。要改动现有文件请用 Update File 段落；"+
				"确实要整体覆盖就用 write 工具", abs)
		}
		p.content = op.content
		p.added = strings.Count(op.content, "\n")

	case kindDelete:
		if !p.existing {
			return nil, fmt.Errorf("%s 不存在，无法删除", abs)
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, err
		}
		p.removed = strings.Count(string(data), "\n")

	case kindUpdate:
		if !p.existing {
			return nil, fmt.Errorf("%s 不存在。新建文件请用 Add File 段落", abs)
		}
		if int64(t.d.Cfg.Tools.FS.MaxReadBytes) < st.Size() {
			return nil, fmt.Errorf("%s 有 %s，超过 apply_patch 能处理的上限 %s",
				abs, humanBytes(st.Size()), humanBytes(int64(t.d.Cfg.Tools.FS.MaxReadBytes)))
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, err
		}
		original := string(data)
		if isBinary(data) {
			return nil, fmt.Errorf("%s 是二进制文件，无法用文本补丁修改", abs)
		}
		updated, notes, err := applyHunks(original, op.hunks)
		if err != nil {
			return nil, err
		}
		// 保持原文件"末尾有没有换行"这一特征。
		if strings.HasSuffix(original, "\n") && updated != "" && !strings.HasSuffix(updated, "\n") {
			updated += "\n"
		}
		if updated == original && p.absMove == "" {
			notes = append(notes, "套用后内容与原文件完全相同（补丁可能已经生效过一次）")
		}
		p.content = updated
		p.notes = notes
		p.added, p.removed = countChanges(op.hunks)
	}
	return p, nil
}

func (t *applyPatchTool) commit(p plannedWrite) error {
	switch p.op.kind {
	case kindDelete:
		return os.Remove(p.absPath)
	case kindAdd:
		if err := os.MkdirAll(filepath.Dir(p.absPath), 0o755); err != nil {
			return err
		}
		return atomicWrite(p.absPath, []byte(p.content), p.mode)
	case kindUpdate:
		target := p.absPath
		if p.absMove != "" {
			target = p.absMove
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
		}
		if err := atomicWrite(target, []byte(p.content), p.mode); err != nil {
			return err
		}
		if p.absMove != "" && p.absMove != p.absPath {
			return os.Remove(p.absPath)
		}
	}
	return nil
}

func summarize(plans []plannedWrite) string {
	var sb strings.Builder
	totalAdd, totalDel := 0, 0
	fmt.Fprintf(&sb, "补丁涉及 %d 个文件：\n", len(plans))
	for _, p := range plans {
		totalAdd += p.added
		totalDel += p.removed
		switch p.op.kind {
		case kindAdd:
			fmt.Fprintf(&sb, "\n· 新建 %s（%d 行）", p.absPath, p.added)
		case kindDelete:
			fmt.Fprintf(&sb, "\n· 删除 %s（原 %d 行）", p.absPath, p.removed)
		case kindUpdate:
			fmt.Fprintf(&sb, "\n· 修改 %s（+%d/-%d，%d 个差异块）",
				p.absPath, p.added, p.removed, len(p.op.hunks))
			if p.absMove != "" {
				fmt.Fprintf(&sb, "\n  重命名为 %s", p.absMove)
			}
		}
		for _, n := range p.notes {
			fmt.Fprintf(&sb, "\n  提示：%s", n)
		}
	}
	fmt.Fprintf(&sb, "\n\n合计 +%d/-%d 行。", totalAdd, totalDel)
	return sb.String()
}

func structuredPlans(plans []plannedWrite, dryRun bool) map[string]any {
	items := make([]map[string]any, 0, len(plans))
	for _, p := range plans {
		item := map[string]any{
			"path": p.absPath, "action": string(p.op.kind),
			"lines_added": p.added, "lines_removed": p.removed,
		}
		if p.absMove != "" {
			item["moved_to"] = p.absMove
		}
		if len(p.notes) > 0 {
			item["notes"] = p.notes
		}
		items = append(items, item)
	}
	return map[string]any{"dry_run": dryRun, "files": items}
}
