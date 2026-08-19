package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/globmatch"
)

// Finding 是一条命中的风险判定。
type Finding struct {
	RuleID  string `json:"rule_id"`
	Action  Action `json:"action"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
	Hint    string `json:"hint,omitempty"`
	Command string `json:"command,omitempty"`
}

// Verdict 是一次命令审查的总结论。
type Verdict struct {
	Action   Action    `json:"action"`
	Findings []Finding `json:"findings,omitempty"`
	// ParseError 非空表示 shell 语法没解析成功，只做了文本层检查。
	ParseError string `json:"parse_error,omitempty"`
}

// Blocked 报告该命令是否需要拦下（拒绝或要求确认）。
func (v Verdict) Blocked() bool { return v.Action != ActionAllow }

// Explain 生成给模型看的说明文本。
func (v Verdict) Explain() string {
	var sb strings.Builder
	switch v.Action {
	case ActionDeny:
		sb.WriteString("命令被安全策略拒绝执行。\n")
	case ActionConfirm:
		sb.WriteString("命令被判定为高风险，需要二次确认。\n")
	}
	for _, f := range v.Findings {
		fmt.Fprintf(&sb, "\n· [%s] %s（%s）", f.RuleID, f.Reason, f.Action)
		if f.Detail != "" {
			fmt.Fprintf(&sb, "\n  细节：%s", f.Detail)
		}
		if f.Command != "" {
			fmt.Fprintf(&sb, "\n  命令：%s", f.Command)
		}
		if f.Hint != "" {
			fmt.Fprintf(&sb, "\n  建议：%s", f.Hint)
		}
	}
	return sb.String()
}

// ShellGuard 审查 shell 命令的风险。
//
// 设计取向：shell 默认拥有完整权限（普通 rm、git、包管理、systemctl 状态查询
// 等一律直接放行），只在命中明确的破坏性模式时拦一下。判定基于 shell AST 而
// 非裸正则，所以 `bash -c "rm  -rf   /"`、`sudo rm -rf $HOME`、
// `rm --recursive --force /` 这些变形都躲不掉。
type ShellGuard struct {
	home        string
	cmdRules    []cmdRule
	pipeRules   []pipeRule
	textRules   []textRule
	extraRules  []compiledExtra
	secretPaths []string
	selfNames   []string
	// downgrade 把内置 deny 规则降级为 confirm（按规则 id）。
	downgrade map[string]bool
}

type compiledExtra struct {
	rule config.ShellRule
	re   *regexp.Regexp
}

// NewShellGuard 按配置组装规则集。selfNames 是本服务自身的进程/单元名，
// 用于识别"把自己干掉"的命令。
func NewShellGuard(p config.Policy, selfNames []string) (*ShellGuard, error) {
	home, _ := os.UserHomeDir()
	g := &ShellGuard{home: home, selfNames: selfNames}

	disabled := map[string]bool{}
	for _, id := range p.DisabledShellRules {
		disabled[strings.TrimSpace(id)] = true
	}
	downgrade := map[string]bool{}
	for _, id := range p.DowngradeToConfirm {
		downgrade[strings.TrimSpace(id)] = true
	}
	g.downgrade = downgrade

	for _, r := range builtinCmdRules() {
		if !disabled[r.id] {
			g.cmdRules = append(g.cmdRules, r)
		}
	}
	for _, r := range builtinPipeRules() {
		if !disabled[r.id] {
			g.pipeRules = append(g.pipeRules, r)
		}
	}
	for _, r := range builtinTextRules() {
		if !disabled[r.id] {
			g.textRules = append(g.textRules, r)
		}
	}
	for _, r := range p.ExtraShellRules {
		if r.ID == "" {
			return nil, fmt.Errorf("policy.extra_shell_rules 里有规则缺少 id")
		}
		if r.Action != string(ActionDeny) && r.Action != string(ActionConfirm) {
			return nil, fmt.Errorf("规则 %s 的 action 必须是 deny 或 confirm，当前为 %q", r.ID, r.Action)
		}
		ce := compiledExtra{rule: r}
		if r.ArgRegex != "" {
			re, err := regexp.Compile(r.ArgRegex)
			if err != nil {
				return nil, fmt.Errorf("规则 %s 的 arg_regex 无法编译: %w", r.ID, err)
			}
			ce.re = re
		}
		if r.Command == "" && ce.re == nil {
			return nil, fmt.Errorf("规则 %s 至少要指定 command 或 arg_regex", r.ID)
		}
		g.extraRules = append(g.extraRules, ce)
	}

	// 凭据路径直接复用 policy.read_deny_paths，两处不必各配一遍。
	g.secretPaths = normalizePatterns(p.ReadDenyPaths, home)
	return g, nil
}

func (g *ShellGuard) matchSecret(target string) (string, bool) {
	t := cleanTarget(target, g.home)
	if t == "" {
		return "", false
	}
	return globmatch.MatchAny(g.secretPaths, t)
}

// Inspect 审查一段 shell 命令。
func (g *ShellGuard) Inspect(command string) Verdict {
	v := Verdict{Action: ActionAllow}
	script := ParseShell(command, g.home)
	if script.ParseErr != nil {
		v.ParseError = script.ParseErr.Error()
	}

	add := func(id string, act Action, reason, detail, hint, cmdText string) {
		if act == ActionAllow {
			return
		}
		if act == ActionDeny && g.downgrade[id] {
			act = ActionConfirm
		}
		v.Findings = append(v.Findings, Finding{
			RuleID: id, Action: act, Reason: reason, Detail: detail, Hint: hint, Command: cmdText,
		})
		v.Action = worse(v.Action, act)
	}

	for _, c := range script.Cmds {
		for _, r := range g.cmdRules {
			if act, detail := r.match(c, g); act != ActionAllow {
				add(r.id, act, r.reason, detail, r.hint, c.Text)
			}
		}
		for _, e := range g.extraRules {
			if act, detail := g.matchExtra(e, c); act != ActionAllow {
				add(e.rule.ID, act, orText(e.rule.Reason, "命中自定义规则"), detail, e.rule.Hint, c.Text)
			}
		}
	}
	for _, p := range script.Pipelines {
		for _, r := range g.pipeRules {
			if act, detail := r.match(p, g); act != ActionAllow {
				add(r.id, act, r.reason, detail, r.hint, "")
			}
		}
	}
	for _, r := range g.textRules {
		if r.match(command) {
			add(r.id, r.action, r.reason, "", r.hint, "")
		}
	}

	// 语法没解析成功时，AST 规则形同虚设；命令里又出现破坏性关键词的话，
	// 宁可要求人确认，也不要闭眼执行。
	if script.ParseErr != nil && v.Action == ActionAllow && looksDestructive(command) {
		add("unparseable-destructive", ActionConfirm, "命令语法无法解析，且包含破坏性关键词",
			script.ParseErr.Error(),
			"把命令拆成几条能正常解析的语句，拦截规则才能逐条检查", command)
	}
	return v
}

func (g *ShellGuard) matchExtra(e compiledExtra, c *Cmd) (Action, string) {
	if e.rule.Command != "" && c.Name() != e.rule.Command {
		return ActionAllow, ""
	}
	if e.re != nil && !e.re.MatchString(c.Text) {
		return ActionAllow, ""
	}
	return Action(e.rule.Action), "自定义规则 " + e.rule.ID
}

var destructiveWords = regexp.MustCompile(`(?i)\brm\s+-[a-z]*[rR]|\bmkfs|\bdd\s+.*of=|\bshred\b|\bwipefs\b|>\s*/dev/(sd|nvme|vd|hd)`)

func looksDestructive(raw string) bool { return destructiveWords.MatchString(raw) }

func orText(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// Fingerprint 计算一条待确认操作的指纹。确认令牌绑定到它，改一个字符就失效。
func Fingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
