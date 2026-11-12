package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/noir017/runabout/internal/fleet"
	"github.com/noir017/runabout/internal/mcp"
)

// probeScript 只依赖 POSIX sh 与 busybox 常见的 uname/cut/awk/df。
const probeScript = `printf 'host=%s\n' "$(uname -n 2>/dev/null)"
printf 'load=%s\n' "$(cut -d' ' -f1-3 /proc/loadavg 2>/dev/null)"
printf 'mem=%s\n' "$(awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} END{if (t>0) printf "%.0f", (t-a)*100/t}' /proc/meminfo 2>/dev/null)"
printf 'disk=%s\n' "$(df -P / 2>/dev/null | awk 'NR==2{print $3"/"$2" "$5}')"
`

// detailScript 同样只依赖 POSIX sh；没有 docker/systemctl 时给出明确说明。
const detailScript = `printf '## 系统\n'
uname -a 2>/dev/null || true
printf '\n## 运行时间\n'
uptime 2>/dev/null || true
printf '\n## 内存\n'
if command -v free >/dev/null 2>&1; then free -m 2>/dev/null; else grep -E '^(MemTotal|MemFree|MemAvailable|SwapTotal|SwapFree):' /proc/meminfo 2>/dev/null; fi
printf '\n## 磁盘\n'
df -hP 2>/dev/null || true
printf '\n## 容器\n'
if command -v docker >/dev/null 2>&1; then docker ps --format '{{.Names}}	{{.Status}}	{{.Image}}' 2>&1 | head -n 50; else printf '未安装 docker\n'; fi
printf '\n## 失败的 systemd 单元\n'
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then systemctl --failed --no-legend --no-pager 2>&1 | head -n 50; else printf '无 systemd\n'; fi
`

type serversTool struct {
	d *Deps

	mu       sync.Mutex
	cachedAt time.Time
	cached   []hostStatus

	now func() time.Time
}

type serversArgs struct {
	Target string `json:"target"`
}

type hostStatus struct {
	Target   string
	State    string // online | offline | approval
	Hostname string
	Load     string
	Mem      string
	Disk     string
	Err      string
	Dur      time.Duration
}

func (t *serversTool) Definition() mcp.ToolDef {
	desc := `查看服务器状态。只读，可以随便调。

不带 target：并行探测全部机器，给在线状态、负载、内存和根分区占用，结果缓存 30 秒。
带 target：返回这一台的详细快照（内存、磁盘、容器、失败的 systemd 单元）。

用户问服务器怎么样、哪台有问题，或者你不确定该去哪台时，先调它。`
	if !t.d.fleetEnabled() {
		desc = `查看这台机器的状态。只读，可以随便调。
返回负载、内存和根分区占用。`
	}
	return mcp.ToolDef{
		Name:        "servers",
		Title:       "查看服务器状态",
		Description: desc,
		InputSchema: schema(t.d.withTarget(map[string]any{})),
		Annotations: &mcp.ToolAnnotations{
			Title:           "查看服务器状态",
			ReadOnlyHint:    mcp.Ptr(true),
			IdempotentHint:  mcp.Ptr(true),
			DestructiveHint: mcp.Ptr(false),
		},
	}
}

func (t *serversTool) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t *serversTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a serversArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, mcp.Errorf(mcp.CodeInvalidParams, "%s", err.Error())
	}
	if strings.TrimSpace(a.Target) != "" {
		target, err := t.d.resolveTarget(a.Target)
		if err != nil {
			return mcp.ToolError("%s", err.Error()), nil
		}
		return t.detail(cc, target)
	}
	return t.overview(cc)
}

func (t *serversTool) overview(cc *mcp.CallContext) (*mcp.CallToolResult, error) {
	now := t.clock()
	t.mu.Lock()
	if len(t.cached) > 0 && now.Sub(t.cachedAt) < 30*time.Second {
		statuses := append([]hostStatus(nil), t.cached...)
		t.mu.Unlock()
		return t.renderOverview(statuses, true), nil
	}
	t.mu.Unlock()

	hosts := t.d.fleetOrLocal().Hosts()
	statuses := make([]hostStatus, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h fleet.Host) {
			defer wg.Done()
			statuses[i] = t.probeHost(cc.Ctx, h)
		}(i, h)
	}
	wg.Wait()

	t.mu.Lock()
	t.cached, t.cachedAt = append([]hostStatus(nil), statuses...), now
	t.mu.Unlock()
	return t.renderOverview(statuses, false), nil
}

func (t *serversTool) probeHost(ctx context.Context, h fleet.Host) hostStatus {
	st := hostStatus{Target: h.Name, State: "offline"}
	if h.Approval && !t.d.fleetOrLocal().ApprovalAllowed(ctx, h.Name) {
		st.State = "approval"
		return st
	}
	ctx, cancel := context.WithTimeout(ctx, t.d.fleetOrLocal().ProbeTimeout())
	defer cancel()

	workdir := ""
	if !h.Local {
		workdir = "~"
	}
	start := time.Now()
	res, err := t.d.fleetOrLocal().Run(ctx, fleet.Spec{
		Target: h.Name, Command: probeScript, Workdir: workdir, MaxOutputBytes: 16 << 10,
	})
	st.Dur = time.Since(start)
	if err != nil {
		var ce *fleet.ConnectError
		if errors.As(err, &ce) {
			st.Err = ce.Detail
		} else {
			st.Err = err.Error()
		}
		return st
	}
	if res.ExitCode != 0 {
		if res.TimedOut {
			st.Err = fmt.Sprintf("探测超时（%s）", t.d.fleetOrLocal().ProbeTimeout())
			return st
		}
		st.Err = firstLine(res.Stderr)
		if st.Err == "" {
			st.Err = fmt.Sprintf("exit=%d", res.ExitCode)
		}
		return st
	}
	st.State = "online"
	for _, line := range strings.Split(res.Stdout, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "host":
			st.Hostname = v
		case "load":
			st.Load = v
		case "mem":
			st.Mem = v
		case "disk":
			st.Disk = v
		}
	}
	return st
}

func (t *serversTool) renderOverview(statuses []hostStatus, cached bool) *mcp.CallToolResult {
	var sb strings.Builder
	if len(statuses) == 1 && statuses[0].Target == fleet.LocalTarget {
		sb.WriteString("本机")
	} else {
		fmt.Fprintf(&sb, "机群 %d 台", len(statuses))
	}
	if cached {
		sb.WriteString("（30 秒内复用上次结果）")
	}
	items := make([]map[string]any, 0, len(statuses))
	needHint := false
	for _, st := range statuses {
		line := "[" + st.Target + "] "
		switch st.State {
		case "online":
			line += "在线"
			if st.Load != "" {
				line += " · 负载 " + st.Load
			}
			if st.Mem != "" {
				line += " · 内存 " + st.Mem + "%"
			}
			if st.Disk != "" {
				line += " · 根分区 " + st.Disk
			}
		case "approval":
			line += "需审批 · 连接会先给用户手机发授权申请"
			needHint = true
		default:
			line += "连不上：" + truncate(st.Err, 300)
			needHint = true
		}
		sb.WriteString("\n")
		sb.WriteString(line)

		item := map[string]any{"target": st.Target, "state": st.State}
		if st.Hostname != "" {
			item["hostname"] = st.Hostname
		}
		if st.Load != "" {
			item["load"] = st.Load
		}
		if st.Mem != "" {
			item["mem_percent"] = st.Mem
		}
		if st.Disk != "" {
			item["root_disk"] = st.Disk
		}
		if st.Err != "" {
			item["error"] = st.Err
		}
		items = append(items, item)
	}
	if needHint {
		sb.WriteString("\n\n连不上的机器可以用 shell target=<机器> 看具体错误；需审批的机器先请用户批准，再原样重试。")
	}
	return mcp.Text(sb.String()).WithStructured(map[string]any{
		"cached": cached, "hosts": items,
	})
}

func (t *serversTool) detail(cc *mcp.CallContext, target targetInfo) (*mcp.CallToolResult, error) {
	if target.host.Approval && !t.d.fleetOrLocal().ApprovalAllowed(cc.Ctx, target.name) {
		return mcp.ToolError("[%s] 需审批：这台机器需要用户批准后才能连接。\n\n"+
			"请先向用户说明要做什么，让用户批准；批准后原样重试。", target.name), nil
	}
	ctx, cancel := context.WithTimeout(cc.Ctx, t.d.fleetOrLocal().ProbeTimeout())
	defer cancel()
	workdir := ""
	if target.remote {
		workdir = "~"
	}
	start := time.Now()
	res, err := t.d.fleetOrLocal().Run(ctx, fleet.Spec{
		Target: target.name, Command: detailScript, Workdir: workdir, MaxOutputBytes: 128 << 10,
	})
	if err != nil {
		return mcp.ToolError("%s", targetError(target.name, err)), nil
	}
	var sb strings.Builder
	sb.WriteString(resultHeader(target.name, res.ExitCode, time.Since(start).Round(time.Millisecond).String(), workdir))
	if res.Truncated {
		sb.WriteString("\n⚠️ 输出过长，已省略中间部分")
	}
	sb.WriteString("\n\n")
	sb.WriteString(strings.TrimRight(res.Stdout, "\n"))
	if strings.TrimSpace(res.Stderr) != "" {
		sb.WriteString("\n")
		sb.WriteString(section("stderr", res.Stderr))
	}
	return (&mcp.CallToolResult{
		Content: []mcp.Content{mcp.TextContent(sb.String())},
		IsError: res.ExitCode != 0,
	}).WithStructured(map[string]any{
		"target": target.name, "exit_code": res.ExitCode,
		"duration_ms": time.Since(start).Milliseconds(), "truncated": res.Truncated,
	}), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
