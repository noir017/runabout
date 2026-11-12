package tools

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/fleet"
	"github.com/noir017/runabout/internal/policy"
)

// targetInfo 是一次调用解析后的机器。
type targetInfo struct {
	name    string
	host    fleet.Host
	remote  bool
	labeled bool // 单机模式不加 [local] 前缀，保持原有输出
}

// fleetOrLocal 在测试或单机装配下给一个只有 local 的机群。
func (d *Deps) fleetOrLocal() *fleet.Fleet {
	if d.Fleet != nil {
		return d.Fleet
	}
	d.localOnce.Do(func() {
		d.localFleet = fleet.New(config.Fleet{}, d.Cfg.Tools.Shell.Path, d.Cfg.Tools.Shell.Env)
	})
	return d.localFleet
}

func (d *Deps) fleetEnabled() bool { return d.fleetOrLocal().Enabled() }

// targetNames 是 target 的 enum，本机永远在第一位。
func (d *Deps) targetNames() []string {
	hosts := d.fleetOrLocal().Hosts()
	names := make([]string, 0, len(hosts))
	for _, h := range hosts {
		names = append(names, h.Name)
	}
	return names
}

// targetProp 生成 target 参数。单机模式返回 nil，schema 里就不会出现它。
func (d *Deps) targetProp() map[string]any {
	if !d.fleetEnabled() {
		return nil
	}
	var lines []string
	for _, h := range d.fleetOrLocal().Hosts() {
		line := h.Name + "：" + h.Desc
		if h.Approval {
			line += "【需审批】"
		}
		lines = append(lines, line)
	}
	return map[string]any{
		"type":        "string",
		"enum":        d.targetNames(),
		"description": strings.Join(lines, "\n"),
	}
}

// withTarget 在配置了机群时把 target 加进参数表。
func (d *Deps) withTarget(props map[string]any) map[string]any {
	if p := d.targetProp(); p != nil {
		props["target"] = p
	}
	return props
}

func (d *Deps) resolveTarget(name string) (targetInfo, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = fleet.LocalTarget
	}
	h, ok := d.fleetOrLocal().Lookup(name)
	if !ok {
		return targetInfo{}, fmt.Errorf("未知的 target %q；可选：%s", name, strings.Join(d.targetNames(), "、"))
	}
	return targetInfo{name: h.Name, host: h, remote: !h.Local, labeled: d.fleetEnabled()}, nil
}

// fsFor 返回目标机器上的文件操作层与路径守卫。
func (d *Deps) fsFor(t targetInfo) (fleet.FS, *policy.PathGuard) {
	if !t.remote {
		return fleet.LocalFS{}, d.Paths
	}
	limit := d.Cfg.Tools.FS.MaxReadBytes
	if d.Cfg.Tools.FS.MaxImageBytes > limit {
		limit = d.Cfg.Tools.FS.MaxImageBytes
	}
	if d.Cfg.Tools.FS.MaxImageSourceBytes > limit {
		limit = d.Cfg.Tools.FS.MaxImageSourceBytes
	}
	return &fleet.RemoteFS{Runner: d.fleetOrLocal(), Target: t.name, MaxBytes: limit}, d.Paths
}

// remoteJoin 把相对路径接到 base 下。~ 开头的路径把 ~ 后面的部分锚定到根，
// 避免 ~/.. 一路穿越到黑名单之外；~ 本身保留给远端 shell 展开。
func remoteJoin(base, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	switch {
	case p == "~":
		return "~"
	case strings.HasPrefix(p, "~/"):
		return anchorHome(p[1:])
	case path.IsAbs(p):
		return path.Clean(p)
	}
	base = strings.TrimSpace(base)
	if base == "" {
		base = "~"
	}
	if base == "~" || strings.HasPrefix(base, "~/") {
		return anchorHome(strings.TrimPrefix(base, "~") + "/" + p)
	}
	return path.Join(base, p)
}

// anchorHome 把 rest 锚定在 ~ 下再 Clean：~/../../etc/shadow 变成 ~/etc/shadow。
func anchorHome(rest string) string {
	cleaned := path.Clean("/" + rest)
	if cleaned == "/" {
		return "~"
	}
	return "~" + cleaned
}

func hasDotDot(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// targetError 给连接失败补上下一步该做什么；其余错误原样返回。
func targetError(target string, err error) string {
	var ce *fleet.ConnectError
	if errors.As(err, &ce) {
		return fmt.Sprintf("[%s] 连不上：%s\n\n%s", target, ce.Detail, connectHint())
	}
	return fmt.Sprintf("[%s] %v", target, err)
}

func connectHint() string {
	return "先用 servers 看它在不在线；如果是需审批的机器，可能是申请超时，请用户批准后重试。"
}

func isConnectError(err error) bool {
	var ce *fleet.ConnectError
	return errors.As(err, &ce)
}

func resultHeader(target string, exit int, dur string, workdir string) string {
	if workdir == "" {
		workdir = "-"
	}
	return fmt.Sprintf("[%s] exit=%d · %s · %s", target, exit, dur, workdir)
}

func label(t targetInfo) string {
	if !t.labeled {
		return ""
	}
	return "[" + t.name + "] "
}

// auditTarget 在单机模式下不写 target，保持与 main 的审计格式一致。
func auditTarget(t targetInfo) string {
	if !t.labeled {
		return ""
	}
	return t.name
}

// remoteWorkdir 是远端 shell 和文件工具共用的默认工作目录。
// ~/... 形式也走 anchorHome，保证 cd 和文件工具的基准一致。
func (d *Deps) remoteWorkdir(t targetInfo) string {
	wd := t.host.Workdir
	if wd == "" || wd == "~" {
		return "~"
	}
	if strings.HasPrefix(wd, "~/") {
		return anchorHome(strings.TrimPrefix(wd, "~"))
	}
	return wd
}

// resolveReadPath / resolveWritePath 统一本机与远端的路径解析：
// 本机照旧解析软链接，远端只做字面清理与黑名单匹配。
func resolveReadPath(d *Deps, t targetInfo, guard *policy.PathGuard, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if t.remote {
		return resolveRemotePath(d, t, guard, p, false)
	}
	return guard.CheckRead(p)
}

func resolveWritePath(d *Deps, t targetInfo, guard *policy.PathGuard, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if t.remote {
		return resolveRemotePath(d, t, guard, p, true)
	}
	return guard.CheckWrite(p)
}

func resolveRemotePath(d *Deps, t targetInfo, guard *policy.PathGuard, p string, write bool) (string, error) {
	joined := remoteJoin(d.remoteWorkdir(t), p)
	var resolved string
	var err error
	if write {
		resolved, err = guard.CheckWriteLiteral(joined)
	} else {
		resolved, err = guard.CheckReadLiteral(joined)
	}
	if err != nil {
		return "", err
	}
	// 原始路径带 .. 时，Clean 之后再匹配一次，防止折叠后绕过黑名单。
	if hasDotDot(p) {
		if write {
			_, err = guard.CheckWriteLiteral(path.Clean(resolved))
		} else {
			_, err = guard.CheckReadLiteral(path.Clean(resolved))
		}
		if err != nil {
			return "", err
		}
	}
	return resolved, nil
}
