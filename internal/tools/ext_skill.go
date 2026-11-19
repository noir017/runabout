package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/noir017/runabout/internal/mcp"
)

type skillTool struct{ d *Deps }

type skillArgs struct {
	Action string `json:"action"`
	Query  string `json:"query"`
	Name   string `json:"name"`
	File   string `json:"file"`
}

var validSkillNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func (t *skillTool) Definition() mcp.ToolDef {
	return mcp.ToolDef{
		Name:  "skill",
		Title: "查询与读取技能",
		Description: `查询或读取 fleetherd 技能库中的技能（复用 BM25 检索与未命中统计闭环）。

- action="list"：列出可用技能。
- action="search"：按关键词检索技能（必填 query）。
- action="read"：读取指定技能的 SKILL.md 或附属参考文件（必填 name，可选 file，默认 SKILL.md）。`,
		InputSchema: schema(map[string]any{
			"action": enumProp("操作类型：list | search | read", "list", "search", "read"),
			"query":  strProp("检索关键词（action=search 时必填）"),
			"name":   strProp("技能名称（action=read 时必填）"),
			"file":   strProp("附属相对文件路径，默认 SKILL.md（如 references/guide.md）"),
		}, "action"),
		Annotations: &mcp.ToolAnnotations{
			Title:           "查询与读取技能",
			ReadOnlyHint:    mcp.Ptr(true),
			IdempotentHint:  mcp.Ptr(true),
			DestructiveHint: mcp.Ptr(false),
		},
	}
}

func (t *skillTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a skillArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, mcp.Errorf(mcp.CodeInvalidParams, "%s", err.Error())
	}
	cfg := t.d.Cfg.Extensions.Skill
	var argv []string

	switch strings.TrimSpace(a.Action) {
	case "list":
		argv = []string{cfg.Binary, "skills", "list"}
	case "search":
		q := strings.TrimSpace(a.Query)
		if q == "" {
			return mcp.ToolError("action=search 时 query 不能为空"), nil
		}
		argv = []string{cfg.Binary, "skills", "search", q}
	case "read":
		name := strings.TrimSpace(a.Name)
		if !validSkillNameRE.MatchString(name) {
			return mcp.ToolError("非法的技能名 %q（仅允许字母、数字、点、下划线与连字符，禁止路径穿越）", a.Name), nil
		}
		file := strings.TrimSpace(a.File)
		if file != "" && file != "SKILL.md" {
			if err := validateSkillRelFile(file); err != nil {
				return mcp.ToolError("%s", err.Error()), nil
			}
			argv = []string{cfg.Binary, "skills", "show", "--again", name, file}
		} else {
			argv = []string{cfg.Binary, "skills", "show", "--again", name}
		}
	default:
		return mcp.ToolError("不支持的 action %q；仅允许 list、search、read", a.Action), nil
	}

	target, res, err := t.d.runExtensionArgv(cc, cfg.Target, argv, nil, cfg.Timeout, cfg.MaxOutputBytes)
	if err != nil {
		if isConnectError(err) {
			return mcp.ToolError("%s", targetError(cfg.Target, err)), nil
		}
		return mcp.ToolError("%s", err.Error()), nil
	}
	if res.TimedOut {
		return mcp.ToolError("%sskill 执行超时（%s）", label(target), cfg.Timeout), nil
	}
	out := strings.TrimRight(res.Stdout, "\n")
	errOut := strings.TrimRight(res.Stderr, "\n")
	if res.ExitCode != 0 {
		msg := errOut
		if msg == "" {
			msg = out
		}
		if msg == "" {
			msg = fmt.Sprintf("exit=%d", res.ExitCode)
		}
		return mcp.ToolError("%sskill %s 失败：%s", label(target), a.Action, msg), nil
	}
	if res.Truncated {
		out += "\n\n⚠️ 输出过长，已省略中间部分"
	}
	if out == "" {
		out = "(空)"
	}
	return mcp.Text(out).WithStructured(map[string]any{
		"action":    a.Action,
		"target":    target.name,
		"exit_code": res.ExitCode,
		"truncated": res.Truncated,
	}), nil
}

// validateSkillRelFile 严格校验技能目录下的附属相对路径，防止路径穿越。
func validateSkillRelFile(p string) error {
	if strings.ContainsAny(p, "\x00\\") {
		return fmt.Errorf("非法的 file 路径 %q：包含禁止字符", p)
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return fmt.Errorf("非法的 file 路径 %q：必须为技能目录内的相对路径", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("非法的 file 路径 %q：不允许包含 ..", p)
		}
	}
	return nil
}
