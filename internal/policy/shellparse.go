package policy

import (
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Cmd 是从 shell AST 里还原出来的一条简单命令。
// Argv 只保留能静态确定的字面量；含变量或命令替换的词标记为 unresolved，
// 规则可以据此选择"证明不了安全就要求确认"。
type Cmd struct {
	Argv       []string
	Unresolved []bool
	Redirects  []Redir
	Text       string
	// Depth 表示嵌套层级：0 为顶层，1 表示来自 bash -c "..." 之类的内层脚本。
	Depth int
}

type Redir struct {
	Op         string
	Target     string
	Unresolved bool
}

// Pipeline 是一条管道里按顺序排列的命令。
type Pipeline struct {
	Cmds []*Cmd
}

// Script 是一次 shell 解析的结果。
type Script struct {
	Raw       string
	Cmds      []*Cmd
	Pipelines []*Pipeline
	// ParseErr 非空表示语法没解析成功，此时只能退化到文本规则。
	ParseErr error
}

const maxNestedParse = 3

// ParseShell 解析一段 bash 脚本。解析失败时仍返回 Script（Cmds 为空、
// ParseErr 非空），由调用方决定如何降级处理。home 用于展开 ~ 与 $HOME，
// 这两者直接影响"是不是在删整个家目录"的判定。
func ParseShell(src, home string) *Script {
	sc := &Script{Raw: src}
	parseInto(sc, src, 0, home)
	return sc
}

func parseInto(sc *Script, src string, depth int, home string) {
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	file, err := parser.Parse(strings.NewReader(src), "cmd")
	if err != nil {
		if depth == 0 {
			sc.ParseErr = err
		}
		return
	}

	// 只从最外层的管道节点开始展开，避免嵌套管道被重复登记。
	innerPipes := map[*syntax.BinaryCmd]bool{}
	syntax.Walk(file, func(node syntax.Node) bool {
		if b, ok := node.(*syntax.BinaryCmd); ok && isPipe(b.Op) && !innerPipes[b] {
			stmts := flattenPipe(b, innerPipes)
			if len(stmts) > 1 {
				pl := &Pipeline{}
				for _, st := range stmts {
					pl.Cmds = append(pl.Cmds, buildCmd(st, src, depth, home))
				}
				sc.Pipelines = append(sc.Pipelines, pl)
			}
		}
		return true
	})

	syntax.Walk(file, func(node syntax.Node) bool {
		st, ok := node.(*syntax.Stmt)
		if !ok {
			return true
		}
		if _, ok := st.Cmd.(*syntax.CallExpr); !ok {
			return true
		}
		c := buildCmd(st, src, depth, home)
		if len(c.Argv) == 0 && len(c.Redirects) == 0 {
			return true
		}
		sc.Cmds = append(sc.Cmds, c)
		// bash -c "..." / ssh host "..." 里的命令同样要过一遍规则，
		// 否则一层引号就能绕过所有拦截。
		if depth < maxNestedParse {
			for _, nested := range nestedScripts(c) {
				parseInto(sc, nested, depth+1, home)
			}
		}
		return true
	})
}

func isPipe(op syntax.BinCmdOperator) bool {
	return op == syntax.Pipe || op == syntax.PipeAll
}

func flattenPipe(b *syntax.BinaryCmd, inner map[*syntax.BinaryCmd]bool) []*syntax.Stmt {
	var out []*syntax.Stmt
	var walkSide func(st *syntax.Stmt)
	walkSide = func(st *syntax.Stmt) {
		if st == nil || st.Cmd == nil {
			return
		}
		if cmd, ok := st.Cmd.(*syntax.BinaryCmd); ok && isPipe(cmd.Op) {
			inner[cmd] = true
			walkSide(cmd.X)
			walkSide(cmd.Y)
			return
		}
		// 非管道节点（含子 shell、复合命令）作为管道的一段整体登记；
		// 其内部命令另由 Stmt 遍历单独覆盖。
		out = append(out, st)
	}
	inner[b] = true
	walkSide(b.X)
	walkSide(b.Y)
	return out
}

func buildCmd(st *syntax.Stmt, src string, depth int, home string) *Cmd {
	c := &Cmd{Depth: depth, Text: nodeText(st, src)}
	if ce, ok := st.Cmd.(*syntax.CallExpr); ok {
		for _, w := range ce.Args {
			lit, ok := wordLiteral(w, home)
			c.Argv = append(c.Argv, lit)
			c.Unresolved = append(c.Unresolved, !ok)
		}
	}
	for _, r := range st.Redirs {
		if r.Word == nil {
			continue
		}
		lit, ok := wordLiteral(r.Word, home)
		c.Redirects = append(c.Redirects, Redir{Op: r.Op.String(), Target: lit, Unresolved: !ok})
	}
	return c
}

func nodeText(n syntax.Node, src string) string {
	start, end := int(n.Pos().Offset()), int(n.End().Offset())
	if start < 0 || end > len(src) || start > end {
		return ""
	}
	return strings.TrimSpace(src[start:end])
}

// wordLiteral 尽力把一个词还原成字面字符串。返回的第二个值为 false 表示
// 词里含变量、命令替换或算术展开，静态无法确定。
func wordLiteral(w *syntax.Word, home string) (string, bool) {
	if w == nil {
		return "", false
	}
	var sb strings.Builder
	resolved := true
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			sb.WriteString(p.Value)
		case *syntax.SglQuoted:
			sb.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, ip := range p.Parts {
				switch q := ip.(type) {
				case *syntax.Lit:
					sb.WriteString(q.Value)
				case *syntax.ParamExp:
					if v, ok := knownParam(q, home); ok {
						sb.WriteString(v)
					} else {
						resolved = false
						sb.WriteString("${" + paramName(q) + "}")
					}
				default:
					resolved = false
				}
			}
		case *syntax.ParamExp:
			if v, ok := knownParam(p, home); ok {
				sb.WriteString(v)
			} else {
				resolved = false
				sb.WriteString("${" + paramName(p) + "}")
			}
		default:
			// 命令替换、进程替换、算术展开等一律视作不可确定。
			resolved = false
		}
	}
	s := sb.String()
	if home != "" {
		s = ExpandHome(s, home)
	}
	return s, resolved
}

func paramName(p *syntax.ParamExp) string {
	if p.Param != nil {
		return p.Param.Value
	}
	return "?"
}

// knownParam 只解析少数意义明确、且直接关系到危险判定的变量。
func knownParam(p *syntax.ParamExp, home string) (string, bool) {
	if p.Param == nil || p.Excl || p.Length || p.Index != nil || p.Slice != nil || p.Repl != nil || p.Exp != nil {
		return "", false
	}
	switch p.Param.Value {
	case "HOME":
		if home != "" {
			return home, true
		}
	}
	return "", false
}

// wrapperCmds 是不改变"真正要执行什么"的前缀命令，判定风险时应剥掉。
var wrapperCmds = map[string]bool{
	"sudo": true, "doas": true, "nohup": true, "setsid": true, "stdbuf": true,
	"nice": true, "ionice": true, "time": true, "eatmydata": true, "command": true,
	"builtin": true, "exec": true,
}

// Effective 返回剥掉 sudo/nohup/env 之类包装后的实际 argv 与对齐的 unresolved 标记。
func (c *Cmd) Effective() ([]string, []bool) {
	argv, unres := c.Argv, c.Unresolved
	for i := 0; i < 8 && len(argv) > 1; i++ {
		base := baseName(argv[0])
		switch {
		case wrapperCmds[base]:
			// 跳过包装命令自己的选项（-u user、-E 等）。
			j := 1
			for j < len(argv) && strings.HasPrefix(argv[j], "-") {
				j++
			}
			argv, unres = argv[j:], unres[j:]
		case base == "env":
			j := 1
			for j < len(argv) && (strings.HasPrefix(argv[j], "-") || strings.Contains(argv[j], "=")) {
				j++
			}
			argv, unres = argv[j:], unres[j:]
		case base == "timeout":
			j := 1
			for j < len(argv) && strings.HasPrefix(argv[j], "-") {
				j++
			}
			if j < len(argv) {
				j++ // 时长参数
			}
			argv, unres = argv[j:], unres[j:]
		case base == "xargs":
			j := 1
			for j < len(argv) && strings.HasPrefix(argv[j], "-") {
				j++
			}
			argv, unres = argv[j:], unres[j:]
		default:
			return argv, unres
		}
	}
	return argv, unres
}

// Name 返回剥掉包装后的命令名（不含目录）。
func (c *Cmd) Name() string {
	argv, _ := c.Effective()
	if len(argv) == 0 {
		return ""
	}
	return baseName(argv[0])
}

// Args 返回剥掉包装后的参数（不含命令名）。
func (c *Cmd) Args() []string {
	argv, _ := c.Effective()
	if len(argv) <= 1 {
		return nil
	}
	return argv[1:]
}

// Targets 返回参数里的非选项项（操作对象），并给出各项是否可静态确定。
func (c *Cmd) Targets() (vals []string, unresolved []bool) {
	argv, unres := c.Effective()
	endOfOpts := false
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if !endOfOpts && a == "--" {
			endOfOpts = true
			continue
		}
		if !endOfOpts && strings.HasPrefix(a, "-") && a != "-" {
			continue
		}
		vals = append(vals, a)
		unresolved = append(unresolved, unres[i])
	}
	return vals, unresolved
}

// HasFlag 判断命令是否带某个短选项（支持 -rf 合并写法）或长选项。
func (c *Cmd) HasFlag(short rune, long ...string) bool {
	argv, _ := c.Effective()
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			return false
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue
		}
		if strings.HasPrefix(a, "--") {
			name := strings.TrimPrefix(a, "--")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
			}
			for _, l := range long {
				if name == l {
					return true
				}
			}
			continue
		}
		if short != 0 && strings.ContainsRune(a[1:], short) {
			return true
		}
	}
	return false
}

// HasArg 判断参数里是否出现过某个字面量（用于 -delete、-exec 之类）。
func (c *Cmd) HasArg(want string) bool {
	for _, a := range c.Args() {
		if a == want {
			return true
		}
	}
	return false
}

func baseName(cmd string) string {
	if cmd == "" {
		return ""
	}
	return filepath.Base(cmd)
}

// nestedScripts 提取需要递归解析的内层脚本文本。
func nestedScripts(c *Cmd) []string {
	argv, unres := c.Effective()
	if len(argv) < 2 {
		return nil
	}
	name := baseName(argv[0])
	switch name {
	case "bash", "sh", "zsh", "dash", "ksh", "ash":
		for i := 1; i < len(argv); i++ {
			a := argv[i]
			// -c / -lc / -xc 等组合写法都算。
			if strings.HasPrefix(a, "-") && strings.Contains(a, "c") && !strings.HasPrefix(a, "--") {
				if i+1 < len(argv) && !unres[i+1] {
					return []string{argv[i+1]}
				}
				return nil
			}
		}
	case "ssh":
		// ssh [选项] host 命令...：把尾部命令也过一遍规则，免得一条 ssh 就把
		// 远端机器清空。选项的参数无法精确区分，这里按"跳过 - 开头及其值"处理。
		i := 1
		for i < len(argv) && strings.HasPrefix(argv[i], "-") {
			if len(argv[i]) == 2 && strings.ContainsAny(argv[i][1:], "bcDeFiJLlmOopQRSWw") {
				i++
			}
			i++
		}
		i++ // host
		if i < len(argv) {
			return []string{strings.Join(argv[i:], " ")}
		}
	}
	return nil
}
