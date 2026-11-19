package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/noir017/runabout/internal/fleet"
	"github.com/noir017/runabout/internal/mcp"
)

// extensionTools 按配置返回已启用的可选扩展工具；默认全部关闭。
func extensionTools(d *Deps) []mcp.Handler {
	var out []mcp.Handler
	if d.Cfg.Extensions.Skill.Enabled {
		out = append(out, &skillTool{d: d})
	}
	if d.Cfg.Extensions.FleetherdTask.Enabled {
		out = append(out, &fleetherdTaskTool{d: d})
	}
	return out
}

// quoteArgv 把 (binary, args...) 拼成严格单引号转义的 exec 命令，杜绝 shell 注入与长文本转义问题。
func quoteArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = fleet.Quote(a)
	}
	return "exec " + strings.Join(quoted, " ")
}

// runExtensionArgv 复用 fleet 通道、capWriter 输出截断、超时控制与 target 白名单校验执行受控子命令。
func (d *Deps) runExtensionArgv(
	cc *mcp.CallContext,
	targetName string,
	argv []string,
	env map[string]string,
	timeout time.Duration,
	maxOutput int,
) (targetInfo, fleet.Result, error) {
	target, err := d.resolveTargetFor(cc.Principal, targetName)
	if err != nil {
		return targetInfo{}, fleet.Result{}, err
	}
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return target, fleet.Result{}, fmt.Errorf("扩展命令为空")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxOutput <= 0 {
		maxOutput = 200_000
	}
	ctx, cancel := context.WithTimeout(cc.Ctx, timeout)
	defer cancel()

	workdir := ""
	if target.remote {
		workdir = d.remoteWorkdir(target)
	} else {
		workdir = d.Cfg.Tools.Shell.DefaultWorkdir
	}
	res, err := d.fleetOrLocal().Run(ctx, fleet.Spec{
		Target:         target.name,
		Command:        quoteArgv(argv),
		Workdir:        workdir,
		Env:            env,
		MaxOutputBytes: maxOutput,
	})
	return target, res, err
}

// scrubSecret 从输出或错误信息中抹除明文密钥，防止任何情况下泄露给模型或日志。
func scrubSecret(s, secret string) string {
	if secret == "" || s == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[REDACTED]")
}
