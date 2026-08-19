// Package policy 负责所有"能不能做"的判断：敏感路径拦截、shell 命令风险
// 识别，以及危险操作的二次确认令牌。
package policy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/noir017/agent-tools-mcp/internal/config"
	"github.com/noir017/agent-tools-mcp/internal/globmatch"
)

// PathGuard 按黑名单拦截文件工具对敏感路径的读写。
// shell 工具不走这里（按设计它拥有完整权限），只受 shell 规则约束。
type PathGuard struct {
	home string
	// baseDir 是相对路径的解析基准。必须和 shell 工具的默认工作目录一致，
	// 否则同一个 "src/main.go" 在 read 和 shell 里会指向两个不同的文件。
	baseDir   string
	writeDeny []string
	readDeny  []string
}

// NewPathGuard 构造路径守卫。baseDir 为空时回落到进程当前目录。
func NewPathGuard(p config.Policy, baseDir string) *PathGuard {
	home, _ := os.UserHomeDir()
	if baseDir == "" {
		baseDir, _ = os.Getwd()
	}
	g := &PathGuard{home: home, baseDir: baseDir}
	g.writeDeny = normalizePatterns(p.WriteDenyPaths, home)
	g.readDeny = normalizePatterns(p.ReadDenyPaths, home)
	return g
}

func normalizePatterns(pats []string, home string) []string {
	out := make([]string, 0, len(pats))
	for _, p := range pats {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, ExpandHome(p, home))
	}
	return out
}

// ExpandHome 把开头的 ~ 展开为 home。
func ExpandHome(p, home string) string {
	if home == "" {
		return p
	}
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, p[2:])
	}
	return p
}

// DeniedError 表示某个路径被策略拦截。
type DeniedError struct {
	Path    string
	Pattern string
	Mode    string // read | write
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("策略拒绝%s %s：命中敏感路径规则 %q。"+
		"如确需操作，请改配置 policy.%s_deny_paths 或改用 shell 工具（会留审计记录）",
		map[string]string{"read": "读取", "write": "写入"}[e.Mode], e.Path, e.Pattern, e.Mode)
}

// Resolve 把用户给的路径变成绝对路径，并尽力解开软链接，
// 避免用 /tmp/x -> /etc/shadow 这类软链接绕过黑名单。
func (g *PathGuard) Resolve(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	p := ExpandHome(path, g.home)
	if !filepath.IsAbs(p) {
		p = filepath.Join(g.baseDir, p)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("无法解析路径 %s: %w", path, err)
	}
	return resolveSymlinks(abs), nil
}

// resolveSymlinks 解开路径中已存在部分的软链接，保留尚不存在的尾部。
func resolveSymlinks(abs string) string {
	remainder := ""
	cur := abs
	for i := 0; i < 64; i++ {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, remainder)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		remainder = filepath.Join(filepath.Base(cur), remainder)
		cur = parent
	}
	return abs
}

// CheckRead 校验读取权限，返回解析后的绝对路径。
func (g *PathGuard) CheckRead(path string) (string, error) {
	abs, err := g.Resolve(path)
	if err != nil {
		return "", err
	}
	if pat, hit := matchPath(g.readDeny, abs, path); hit {
		return "", &DeniedError{Path: abs, Pattern: pat, Mode: "read"}
	}
	return abs, nil
}

// CheckWrite 校验写入/删除权限，返回解析后的绝对路径。
func (g *PathGuard) CheckWrite(path string) (string, error) {
	abs, err := g.Resolve(path)
	if err != nil {
		return "", err
	}
	if pat, hit := matchPath(g.writeDeny, abs, path); hit {
		return "", &DeniedError{Path: abs, Pattern: pat, Mode: "write"}
	}
	// 写入同样受读黑名单里"凭据文件"的保护：能写就能覆盖成已知内容再读回。
	if pat, hit := matchPath(g.readDeny, abs, path); hit {
		return "", &DeniedError{Path: abs, Pattern: pat, Mode: "write"}
	}
	return abs, nil
}

// matchPath 同时用解析后的绝对路径和原始输入去匹配，避免软链接解析把
// ~/.ssh/id_rsa 变成别的真实路径后漏过规则。
func matchPath(patterns []string, abs, original string) (string, bool) {
	if pat, ok := globmatch.MatchAny(patterns, abs); ok {
		return pat, true
	}
	return globmatch.MatchAny(patterns, filepath.Clean(original))
}
