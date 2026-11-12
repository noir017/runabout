package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/noir017/runabout/internal/mcp"
)

type queryTool struct{ d *Deps }

type queryArgs struct {
	Target    string `json:"target"`
	Command   string `json:"command"`
	Workdir   string `json:"workdir"`
	TimeoutMS int    `json:"timeout_ms"`
}

func (t *queryTool) Definition() mcp.ToolDef {
	cfg := t.d.Cfg.Tools.Shell
	targetDoc := ""
	if t.d.fleetEnabled() {
		targetDoc = "\ntarget 决定在哪台机器上查询，默认 local（runabout 所在容器）。" +
			"\n远端由目标机器的登录 shell 执行，命令要保持 POSIX sh 兼容。"
	}
	return mcp.ToolDef{
		Name:  "query",
		Title: "只读查询",
		Description: fmt.Sprintf(`执行只读命令查看机器状态，不会触发确认。

- 排查问题、查看服务、日志、磁盘、网络、容器状态时优先用它。%s
- 只接受明确只读的命令：ls、cat、head、tail、grep、rg、find、df、du、free、ps、uptime、ss、ip、journalctl、systemctl status、docker ps 等；支持管道。
- 写重定向、命令替换、修改状态的命令一律拒绝；需要时改用 shell。
- 默认工作目录 %s；默认超时 %s，输出超限会截断。`, targetDoc, cfg.DefaultWorkdir, cfg.DefaultTimeout),
		InputSchema: schema(t.d.withTarget(map[string]any{
			"command":    strProp("要执行的只读命令，支持管道"),
			"workdir":    strProp("工作目录，默认 " + cfg.DefaultWorkdir),
			"timeout_ms": intProp("超时毫秒数，默认 "+cfg.DefaultTimeout.String(), 100, int(cfg.MaxTimeout.Milliseconds())),
		}), "command"),
		Annotations: &mcp.ToolAnnotations{
			Title:           "只读查询",
			ReadOnlyHint:    mcp.Ptr(true),
			IdempotentHint:  mcp.Ptr(true),
			DestructiveHint: mcp.Ptr(false),
		},
	}
}

func (t *queryTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a queryArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, mcp.Errorf(mcp.CodeInvalidParams, "%s", err.Error())
	}
	if strings.TrimSpace(a.Command) == "" {
		return mcp.ToolError("command 不能为空"), nil
	}
	target, err := t.d.resolveTarget(a.Target)
	if err != nil {
		return mcp.ToolError("%s", err.Error()), nil
	}
	sh := &shellTool{d: t.d}
	workdir, err := sh.resolveWorkdir(target, a.Workdir)
	if err != nil {
		return mcp.ToolError("%s", err.Error()), nil
	}
	// 只读判定只看命令文本，与 target 无关。
	if err := t.d.Shell.CheckReadOnly(a.Command); err != nil {
		t.d.Audit.PolicyTarget(cc.Principal, cc.SessionID, "query", auditTarget(target),
			"denied", "readonly", a.Command, workdir, err.Error())
		return mcp.ToolError("query 拒绝执行：%s\n\n这条命令无法证明是只读的。需要执行请改用 shell。", err.Error()), nil
	}

	cfg := t.d.Cfg.Tools.Shell
	timeout := cfg.DefaultTimeout
	if a.TimeoutMS > 0 {
		timeout = time.Duration(a.TimeoutMS) * time.Millisecond
	}
	if timeout > cfg.MaxTimeout {
		timeout = cfg.MaxTimeout
	}
	return sh.runForeground(cc, shellArgs{Command: a.Command}, target, workdir, timeout)
}
