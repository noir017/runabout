package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/noir017/agent-tools-mcp/internal/mcp"
)

// ---------- shell_list ----------

type shellListTool struct{ d *Deps }

func (t *shellListTool) Definition() mcp.ToolDef {
	return mcp.ToolDef{
		Name:        "shell_list",
		Title:       "列出后台任务",
		Description: "列出由 shell(run_in_background=true) 启动的后台进程及其状态。",
		InputSchema: schema(map[string]any{}),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: mcp.Ptr(true)},
	}
}

func (t *shellListTool) Call(cc *mcp.CallContext, _ json.RawMessage) (*mcp.CallToolResult, error) {
	procs := t.d.Procs.List()
	if len(procs) == 0 {
		return mcp.Text("当前没有后台任务。"), nil
	}
	var sb strings.Builder
	items := make([]map[string]any, 0, len(procs))
	fmt.Fprintf(&sb, "共 %d 个后台任务：\n", len(procs))
	for _, p := range procs {
		running, code, runErr, ended := p.Status()
		state := "running"
		if !running {
			state = fmt.Sprintf("exited(%d)", code)
		}
		elapsed := time.Since(p.Started)
		if !running && !ended.IsZero() {
			elapsed = ended.Sub(p.Started)
		}
		label := p.Label
		if label != "" {
			label = " [" + label + "]"
		}
		fmt.Fprintf(&sb, "\n· %s%s  %s  %s  已捕获 %d 字节\n  cwd=%s\n  cmd=%s",
			p.ID, label, state, elapsed.Round(time.Second), p.out.Total(), p.Workdir, truncate(p.Command, 200))
		item := map[string]any{
			"process_id": p.ID, "label": p.Label, "state": state, "running": running,
			"command": p.Command, "workdir": p.Workdir,
			"started_at": p.Started.Format(time.RFC3339), "output_bytes": p.out.Total(),
		}
		if !running {
			item["exit_code"] = code
			if runErr != nil {
				item["error"] = runErr.Error()
			}
		}
		items = append(items, item)
	}
	return mcp.Text(sb.String()).WithStructured(map[string]any{"processes": items}), nil
}

// ---------- shell_output ----------

type shellOutputTool struct{ d *Deps }

type shellOutputArgs struct {
	ProcessID string `json:"process_id"`
	OnlyNew   bool   `json:"only_new"`
	TailLines int    `json:"tail_lines"`
	WaitMS    int    `json:"wait_ms"`
}

func (t *shellOutputTool) Definition() mcp.ToolDef {
	return mcp.ToolDef{
		Name:  "shell_output",
		Title: "读取后台任务输出",
		Description: `读取后台进程（shell 的 run_in_background）已捕获的 stdout+stderr。

- only_new=true 只返回上一次带 only_new 的读取之后新增的部分，适合轮询长任务。
- tail_lines 只看最后 N 行。
- wait_ms 可以先等一会儿再读（最长 60000 毫秒），省得空轮询。`,
		InputSchema: schema(map[string]any{
			"process_id": strProp("shell 返回的后台进程 id"),
			"only_new":   boolProp("只返回自上一次带 only_new 的读取以来的新增输出"),
			"tail_lines": intProp("只返回最后 N 行", 1, 10000),
			"wait_ms":    intProp("读取前先等待的毫秒数（进程结束会提前返回）", 0, 60000),
		}, "process_id"),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: mcp.Ptr(true)},
	}
}

func (t *shellOutputTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a shellOutputArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, mcp.Errorf(mcp.CodeInvalidParams, "%s", err.Error())
	}
	p, ok := t.d.Procs.Get(a.ProcessID)
	if !ok {
		return mcp.ToolError("找不到后台进程 %s（可能已被清理）；用 shell_list 看看还有哪些", a.ProcessID), nil
	}
	if a.WaitMS > 0 {
		p.Wait(time.Duration(a.WaitMS) * time.Millisecond)
	}

	out := p.Output()
	if a.OnlyNew {
		out = p.TakeNew()
	}
	if a.TailLines > 0 {
		out = tailLines(out, a.TailLines)
	}

	running, code, runErr, _ := p.Status()
	var sb strings.Builder
	if running {
		fmt.Fprintf(&sb, "进程 %s 仍在运行（已 %s）\n", p.ID, time.Since(p.Started).Round(time.Second))
	} else {
		fmt.Fprintf(&sb, "进程 %s 已结束，exit_code=%d\n", p.ID, code)
		if runErr != nil {
			fmt.Fprintf(&sb, "错误: %v\n", runErr)
		}
	}
	sb.WriteString("\n")
	sb.WriteString(section("output", out))

	return mcp.Text(sb.String()).WithStructured(map[string]any{
		"process_id": p.ID, "running": running, "exit_code": code,
		"output": out, "total_bytes": p.out.Total(),
	}), nil
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	return fmt.Sprintf("…（省略前 %d 行）…\n%s", len(lines)-n, strings.Join(lines[len(lines)-n:], "\n"))
}

// ---------- shell_kill ----------

type shellKillTool struct{ d *Deps }

type shellKillArgs struct {
	ProcessID string `json:"process_id"`
	Signal    string `json:"signal"`
}

func (t *shellKillTool) Definition() mcp.ToolDef {
	return mcp.ToolDef{
		Name:  "shell_kill",
		Title: "结束后台任务",
		Description: "给后台进程发信号并结束它。默认 TERM（留机会清理），必要时用 KILL 强杀。" +
			"信号发给整个进程组，子进程一起结束。",
		InputSchema: schema(map[string]any{
			"process_id": strProp("shell 返回的后台进程 id"),
			"signal":     enumProp("信号，默认 TERM", "TERM", "KILL", "INT", "HUP", "QUIT", "USR1", "USR2"),
		}, "process_id"),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: mcp.Ptr(true)},
	}
}

var signalNames = map[string]syscall.Signal{
	"TERM": syscall.SIGTERM, "KILL": syscall.SIGKILL, "INT": syscall.SIGINT,
	"HUP": syscall.SIGHUP, "QUIT": syscall.SIGQUIT,
	"USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2,
}

func (t *shellKillTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a shellKillArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, mcp.Errorf(mcp.CodeInvalidParams, "%s", err.Error())
	}
	p, ok := t.d.Procs.Get(a.ProcessID)
	if !ok {
		return mcp.ToolError("找不到后台进程 %s；用 shell_list 看看还有哪些", a.ProcessID), nil
	}
	name := strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(a.Signal), "SIG"))
	if name == "" {
		name = "TERM"
	}
	sig, ok := signalNames[name]
	if !ok {
		return mcp.ToolError("不支持的信号 %q", a.Signal), nil
	}
	if !p.Running() {
		_, code, _, _ := p.Status()
		return mcp.Textf("进程 %s 已经结束了（exit_code=%d），无需再发信号。", p.ID, code), nil
	}
	if err := p.Signal(sig); err != nil {
		return mcp.ToolError("发送信号失败: %v", err), nil
	}
	stopped := p.Wait(3 * time.Second)
	t.d.Audit.Policy(cc.Principal, cc.SessionID, "shell_kill", "signal", "",
		p.Command, p.Workdir, "SIG"+name)

	msg := fmt.Sprintf("已向进程 %s 发送 SIG%s。", p.ID, name)
	if stopped {
		_, code, _, _ := p.Status()
		msg += fmt.Sprintf("进程已退出，exit_code=%d。", code)
	} else {
		msg += "进程 3 秒内未退出；如需强制结束可再用 signal=KILL。"
	}
	return mcp.Textf("%s", msg), nil
}
