package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/noir017/runabout/internal/fleet"
	"github.com/noir017/runabout/internal/mcp"
	"github.com/noir017/runabout/internal/policy"
)

type shellTool struct{ d *Deps }

type shellArgs struct {
	Target          string            `json:"target"`
	Command         string            `json:"command"`
	Workdir         string            `json:"workdir"`
	TimeoutMS       int               `json:"timeout_ms"`
	Stdin           string            `json:"stdin"`
	Env             map[string]string `json:"env"`
	RunInBackground bool              `json:"run_in_background"`
	Label           string            `json:"label"`
	ConfirmToken    string            `json:"confirm_token"`
}

func (t *shellTool) Definition() mcp.ToolDef {
	cfg := t.d.Cfg.Tools.Shell
	targetDoc := ""
	if t.d.fleetEnabled() {
		targetDoc = "\ntarget 决定在哪台机器上执行，默认 local（runabout 所在容器）。" +
			"\n本机用 bash；远端由目标机器的登录 shell 执行，命令要保持 POSIX sh 兼容。" +
			"\n远端默认工作目录是机器配置的 workdir，没配就是登录用户的家目录。"
	}
	return mcp.ToolDef{
		Name:  "shell",
		Title: "执行 shell 命令",
		Description: fmt.Sprintf(`问到机器上的任何实际情况（进程、容器、服务、日志、磁盘、网络、配置内容），
都用它或 servers 去查，不要凭印象回答。

在机器上执行一条 bash 命令，返回退出码、stdout 与 stderr。%s

用法要点：
- 支持管道、重定向、&&、for 循环等完整 bash 语法，命令在 %s -c 里执行。
- 默认工作目录是 %s，可用 workdir 指定；相对路径都相对于它。
- 默认超时 %s，最长 %s；超时会连同子进程一起杀掉。
- 长任务（编译、备份、压测）请设 run_in_background=true，然后用 shell_output 拉输出、shell_kill 结束。
- stdin 为空时命令的标准输入是 /dev/null，交互式命令会直接失败而不是卡住；需要喂输入就填 stdin。
- 输出超过 %d 字节会保留头尾、省略中间。

安全策略：普通操作（rm 单个文件、git、包管理、systemctl 状态查询等）直接执行。
少数破坏性操作会被拦下：
- 直接拒绝：rm -rf /、格式化磁盘、写块设备、fork bomb 之类没有正当用途的命令。
- 需要确认：rm -rf *、清空防火墙、重启机器、curl | bash、读取私钥等。这时会返回一个
  confirm_token，确认这确实是你要做的事之后，带着同一条命令和该 token 再调用一次即可执行。
  token 一次性、限时，且只对签发它的那条命令和目标机器有效。`,
			targetDoc, cfg.Path, cfg.DefaultWorkdir, cfg.DefaultTimeout, cfg.MaxTimeout, cfg.MaxOutputBytes),
		InputSchema: schema(t.d.withTarget(map[string]any{
			"command":           strProp("要执行的 bash 命令"),
			"workdir":           strProp("工作目录，默认 " + cfg.DefaultWorkdir),
			"timeout_ms":        intProp("超时毫秒数，默认 "+cfg.DefaultTimeout.String(), 100, int(cfg.MaxTimeout.Milliseconds())),
			"stdin":             strProp("写入命令标准输入的内容"),
			"env":               map[string]any{"type": "object", "description": "追加的环境变量；远端机器上会 export 后执行", "additionalProperties": map[string]any{"type": "string"}},
			"run_in_background": boolProp("丢到后台运行，立即返回进程 id"),
			"label":             strProp("后台任务的备注名，便于在 shell_list 里辨认"),
			"confirm_token":     strProp("二次确认令牌：仅当上一次调用因高风险被拦下时填入"),
		}), "command"),
		Annotations: &mcp.ToolAnnotations{
			Title:           "执行 shell 命令",
			DestructiveHint: mcp.Ptr(true),
			OpenWorldHint:   mcp.Ptr(true),
		},
	}
}

func (t *shellTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a shellArgs
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
	cfg := t.d.Cfg.Tools.Shell

	workdir, err := t.resolveWorkdir(target, a.Workdir)
	if err != nil {
		return mcp.ToolError("%s", err.Error()), nil
	}

	// ---- 安全策略 ----
	if res := t.gate(cc, target, a.Command, workdir, a.ConfirmToken); res != nil {
		return res, nil
	}

	timeout := cfg.DefaultTimeout
	if a.TimeoutMS > 0 {
		timeout = time.Duration(a.TimeoutMS) * time.Millisecond
	}
	if timeout > cfg.MaxTimeout {
		timeout = cfg.MaxTimeout
	}

	if a.RunInBackground {
		return t.runBackground(cc, a, target, workdir)
	}
	return t.runForeground(cc, a, target, workdir, timeout)
}

// gate 执行策略判定与二次确认。返回非 nil 表示这次调用应当被拦下。
func (t *shellTool) gate(cc *mcp.CallContext, target targetInfo, command, workdir, token string) *mcp.CallToolResult {
	verdict := t.d.Shell.Inspect(command)
	fp := policy.Fingerprint("shell", target.name, command, workdir)

	if token != "" {
		// 带了令牌：无论本次判定如何，都先校验令牌与命令、目标机器是否对得上。
		if err := t.d.Confirm.Redeem(token, fp, cc.Principal.Subject); err != nil {
			t.d.Audit.PolicyTarget(cc.Principal, cc.SessionID, "shell", auditTarget(target), "confirm_rejected",
				firstRuleID(verdict), command, workdir, err.Error())
			return mcp.ToolError("二次确认失败：%s", err.Error())
		}
		if verdict.Action == policy.ActionDeny {
			t.d.Audit.PolicyTarget(cc.Principal, cc.SessionID, "shell", auditTarget(target), "denied",
				firstRuleID(verdict), command, workdir, "deny 级规则不接受确认")
			return mcp.ToolError("该命令属于禁止执行的类别，确认令牌也无法放行。%s", verdict.Explain())
		}
		t.d.Audit.PolicyTarget(cc.Principal, cc.SessionID, "shell", auditTarget(target), "confirmed",
			firstRuleID(verdict), command, workdir, "已通过二次确认")
		return nil
	}

	switch verdict.Action {
	case policy.ActionDeny:
		t.d.Audit.PolicyTarget(cc.Principal, cc.SessionID, "shell", auditTarget(target), "denied",
			firstRuleID(verdict), command, workdir, "")
		return mcp.ToolError("%s\n\n这类命令不接受二次确认。如果确实需要，请人工登录服务器执行，"+
			"或在服务端配置 policy.downgrade_to_confirm 后重启服务。", verdict.Explain())
	case policy.ActionConfirm:
		tok, ttl := t.d.Confirm.Issue(fp, cc.Principal.Subject, firstRuleID(verdict))
		t.d.Audit.PolicyTarget(cc.Principal, cc.SessionID, "shell", auditTarget(target), "confirm_required",
			firstRuleID(verdict), command, workdir, "")
		return mcp.ToolError("%s\n\n命令尚未执行。如果确认要继续，请用【完全相同的 command、workdir 和 target】"+
			"再调用一次 shell，并带上：\n  confirm_token = %s\n（有效期 %s，仅可使用一次）\n\n"+
			"如果不确定，请先向用户说明这条命令会做什么、影响哪些数据，得到同意后再执行。",
			verdict.Explain(), tok, ttl)
	}
	return nil
}

func firstRuleID(v policy.Verdict) string {
	if len(v.Findings) == 0 {
		return ""
	}
	return v.Findings[0].RuleID
}

func (t *shellTool) resolveWorkdir(target targetInfo, dir string) (string, error) {
	if target.remote {
		base := t.d.remoteWorkdir(target)
		if strings.TrimSpace(dir) == "" {
			if base == "~" {
				return "", nil // 不 cd，交给远端登录 shell 的家目录
			}
			return base, nil
		}
		return remoteJoin(base, dir), nil
	}
	if strings.TrimSpace(dir) == "" {
		dir = t.d.Cfg.Tools.Shell.DefaultWorkdir
	}
	abs, err := t.d.Paths.Resolve(dir)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("工作目录 %s 不可用: %v", abs, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("workdir %s 不是目录", abs)
	}
	return abs, nil
}

func (t *shellTool) runForeground(cc *mcp.CallContext, a shellArgs, target targetInfo, workdir string, timeout time.Duration) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(cc.Ctx, timeout)
	defer cancel()

	res, err := t.d.fleetOrLocal().Run(ctx, fleet.Spec{
		Target:         target.name,
		Command:        a.Command,
		Workdir:        workdir,
		Env:            a.Env,
		Stdin:          a.Stdin,
		MaxOutputBytes: t.d.Cfg.Tools.Shell.MaxOutputBytes,
	})
	if err != nil {
		if !target.labeled {
			return mcp.ToolError("启动命令失败: %v", err), nil
		}
		return mcp.ToolError("%s", targetError(target.name, err)), nil
	}

	var sb strings.Builder
	if target.labeled {
		sb.WriteString(resultHeader(target.name, res.ExitCode, res.Duration.Round(time.Millisecond).String(), workdir))
		if res.ExitCode != 0 {
			fmt.Fprintf(&sb, "\n⚠️ 命令以非零状态结束（exit_code: %d）", res.ExitCode)
		}
	} else {
		fmt.Fprintf(&sb, "exit_code: %d\n工作目录: %s\n耗时: %s", res.ExitCode, workdir, res.Duration.Round(time.Millisecond))
	}
	if res.TimedOut {
		fmt.Fprintf(&sb, "\n⚠️ 命令超时（%s）已被终止；下次可调大 timeout_ms 或改用 run_in_background=true", timeout)
	}
	if res.Truncated {
		sb.WriteString("\n⚠️ 输出过长，已省略中间部分")
	}
	sb.WriteString("\n\n")
	sb.WriteString(section("stdout", res.Stdout))
	if strings.TrimSpace(res.Stderr) != "" {
		sb.WriteString("\n")
		sb.WriteString(section("stderr", res.Stderr))
	}

	meta := map[string]any{
		"exit_code":   res.ExitCode,
		"duration_ms": res.Duration.Milliseconds(),
		"timed_out":   res.TimedOut,
		"workdir":     workdir,
		"truncated":   res.Truncated,
	}
	if target.labeled {
		meta["target"] = target.name
	}
	return (&mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent(sb.String())},
		IsError: res.ExitCode != 0 || res.TimedOut,
	}).WithStructured(meta), nil
}

func (t *shellTool) runBackground(cc *mcp.CallContext, a shellArgs, target targetInfo, workdir string) (*mcp.CallToolResult, error) {
	// 后台任务的生命周期不能绑在单次 HTTP 请求上，所以用独立的 context。
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := t.d.fleetOrLocal().Start(ctx, fleet.Spec{
		Target:         target.name,
		Command:        a.Command,
		Workdir:        workdir,
		Env:            a.Env,
		Stdin:          a.Stdin,
		MaxOutputBytes: t.d.Cfg.Tools.Shell.MaxOutputBytes,
		MergeStderr:    true,
	})
	if err != nil {
		cancel()
		if !target.labeled {
			return mcp.ToolError("启动后台命令失败: %v", err), nil
		}
		return mcp.ToolError("%s", targetError(target.name, err)), nil
	}

	p := &Proc{
		ID: newProcID(), Target: target.name, Label: a.Label, Command: a.Command, Workdir: workdir,
		Started: time.Now(), proc: proc, out: proc.Out(), done: make(chan struct{}),
	}
	if err := t.d.Procs.Add(p); err != nil {
		cancel()
		_ = proc.Signal(syscall.SIGTERM)
		return mcp.ToolError("%s", err.Error()), nil
	}
	go func() {
		defer cancel()
		code, waitErr := proc.Wait()
		p.markDone(code, waitErr)
		t.d.Log.Info("后台任务结束", "id", p.ID, "target", p.Target, "exit", code, "dur", time.Since(p.Started))
	}()

	var text string
	if target.labeled {
		text = fmt.Sprintf("已在 [%s] 后台启动，进程 id: %s\nPID: %d\n工作目录: %s\n命令: %s\n\n"+
			"用 shell_output 查看输出（支持只看新增部分），用 shell_kill 结束它。",
			target.name, p.ID, proc.Pid(), workdir, truncate(a.Command, 400))
	} else {
		text = fmt.Sprintf("已在后台启动，进程 id: %s\nPID: %d\n工作目录: %s\n命令: %s\n\n"+
			"用 shell_output 查看输出（支持只看新增部分），用 shell_kill 结束它。",
			p.ID, proc.Pid(), workdir, truncate(a.Command, 400))
	}
	meta := map[string]any{"process_id": p.ID, "pid": proc.Pid(), "workdir": workdir}
	if target.labeled {
		meta["target"] = target.name
	}
	return mcp.Text(text).WithStructured(meta), nil
}
