package tools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/fleet"
	"github.com/noir017/runabout/internal/mcp"
)

type fleetherdTaskTool struct{ d *Deps }

type fleetherdTaskArgs struct {
	Action      string `json:"action"`
	SessionRef  string `json:"session_ref"`
	ParentTopic string `json:"parent_topic"`
	Agent       string `json:"agent"`
	Cwd         string `json:"cwd"`
	Title       string `json:"title"`
	Model       string `json:"model"`
	Prompt      string `json:"prompt"`
	Topic       string `json:"topic"`
	Interrupt   bool   `json:"interrupt"`
	Forward     bool   `json:"forward"`
	Turn        string `json:"turn"`
	Full        bool   `json:"full"`
	Tail        int    `json:"tail"`
}

// webBridgeMapping 与 web-agent-driver / fleetherd 约定的短命映射文件结构。
type webBridgeMapping struct {
	SessionID string `json:"session_id"`
	Socket    string `json:"socket"`
	Token     string `json:"token"`
	AgentID   string `json:"agent_id"`
	CreatedAt string `json:"created_at"`
}

var (
	validSessionRefRE = regexp.MustCompile(`^[A-Za-z0-9_-]{8,32}$`)
	validTopicIDRE    = regexp.MustCompile(`^[A-Za-z0-9._:@/-]{1,128}$`)
	topicIDExtractRE  = regexp.MustCompile(`(?i)(?:topic(?:_id)?|id)\s*[:=]\s*([A-Za-z0-9._:@/-]+)`)
)

func (t *fleetherdTaskTool) Definition() mcp.ToolDef {
	return mcp.ToolDef{
		Name:  "fleetherd_task",
		Title: "调度 fleetherd 子任务",
		Description: `受控转发 fleetherd 的 task-* 子代理调度命令（严格白名单，argv 安全传参，避免 shell 转义问题）。

- action 白名单：agents | new | send | read | list | status | stop。
- 带 session_ref（fleetherd 托管会话）：自动从受保护的 web-bridge 映射中加载上下文并挂载到当前父话题。
- 无 session_ref（手聊模式）：action=new 未传 parent_topic 时会尝试创建空白顶层协调话题；同批子任务请复用返回的 parent_topic。`,
		InputSchema: schema(map[string]any{
			"action":       enumProp("子任务操作：agents | new | send | read | list | status | stop", "agents", "new", "send", "read", "list", "status", "stop"),
			"session_ref":  strProp("可选；fleetherd 托管会话的 8-32 位短 ID"),
			"parent_topic": strProp("可选；无 session_ref 手聊模式下复用的空白顶层协调话题 ID"),
			"agent":        strProp("可选；action=new 指定执行子任务的 agent ID"),
			"cwd":          strProp("可选；action=new 指定子任务工作目录"),
			"title":        strProp("可选；action=new 指定子任务标题"),
			"model":        strProp("可选；action=new 指定子任务模型"),
			"prompt":       strProp("任务提示词（action=new / send 时必填）"),
			"topic":        strProp("子任务话题 ID（action=send / read / status / stop 时必填）"),
			"interrupt":    boolProp("可选；action=send 时先中断当前 turn 再发送"),
			"forward":      boolProp("可选；action=send 时启用 forward 模式"),
			"turn":         strProp("可选；action=read 指定轮次（如 last、done 或轮次号）"),
			"full":         boolProp("可选；action=read 返回完整消息内容"),
			"tail":         intProp("可选；action=read 仅显示每条消息最后 N 行", 1, 10000),
		}, "action"),
		Annotations: &mcp.ToolAnnotations{
			Title:           "调度 fleetherd 子任务",
			DestructiveHint: mcp.Ptr(false),
			OpenWorldHint:   mcp.Ptr(true),
		},
	}
}

func (t *fleetherdTaskTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var a fleetherdTaskArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, mcp.Errorf(mcp.CodeInvalidParams, "%s", err.Error())
	}
	cfg := t.d.Cfg.Extensions.FleetherdTask
	action := strings.TrimSpace(a.Action)
	switch action {
	case "agents", "new", "send", "read", "list", "status", "stop":
	default:
		return mcp.ToolError("不支持的 action %q；仅允许 agents、new、send、read、list、status、stop", a.Action), nil
	}

	// 先做目标机器白名单校验。
	target, err := t.d.resolveTargetFor(cc.Principal, cfg.Target)
	if err != nil {
		return mcp.ToolError("%s", err.Error()), nil
	}

	var env map[string]string
	var secretToken string
	sessionRef := strings.TrimSpace(a.SessionRef)
	parentTopic := strings.TrimSpace(a.ParentTopic)

	if sessionRef != "" {
		if !validSessionRefRE.MatchString(sessionRef) {
			return mcp.ToolError("非法的 session_ref %q（须匹配 ^[A-Za-z0-9_-]{8,32}$）", a.SessionRef), nil
		}
		mapping, err := t.loadBridgeMapping(cc, target, cfg.WebBridgeDir, sessionRef)
		if err != nil {
			return mcp.ToolError("读取 web-bridge 映射失败：%s", err.Error()), nil
		}
		secretToken = mapping.Token
		env = map[string]string{
			"FLEETHERD_SOCKET":     mapping.Socket,
			"FLEETHERD_TURN_TOKEN": mapping.Token,
		}
	} else if parentTopic != "" {
		if !validTopicIDRE.MatchString(parentTopic) || strings.Contains(parentTopic, "..") {
			return mcp.ToolError("非法的 parent_topic %q", a.ParentTopic), nil
		}
	}

	// 无 session_ref 且 action=new 时，若未提供 parent_topic，则通过可配置模板创建空白顶层协调话题。
	if sessionRef == "" && action == "new" && parentTopic == "" {
		if len(cfg.EmptyTopicCommand) == 0 {
			return mcp.ToolError("未传 session_ref 与 parent_topic，且服务端未配置 extensions.fleetherd_task.empty_topic_command；请传入已有 parent_topic 或在配置中启用空白顶层话题创建命令"), nil
		}
		createdTopic, err := t.createEmptyParentTopic(cc, cfg, a.Title)
		if err != nil {
			return mcp.ToolError("创建空白顶层协调话题失败：%s", err.Error()), nil
		}
		parentTopic = createdTopic
	}

	argv, err := buildFleetherdTaskArgv(cfg.Binary, action, a, sessionRef, parentTopic)
	if err != nil {
		return mcp.ToolError("%s", err.Error()), nil
	}

	_, res, err := t.d.runExtensionArgv(cc, cfg.Target, argv, env, cfg.Timeout, cfg.MaxOutputBytes)
	if err != nil {
		if isConnectError(err) {
			return mcp.ToolError("%s", scrubSecret(targetError(cfg.Target, err), secretToken)), nil
		}
		return mcp.ToolError("%s", scrubSecret(err.Error(), secretToken)), nil
	}
	if res.TimedOut {
		return mcp.ToolError("%sfleetherd_task %s 执行超时（%s）", label(target), action, cfg.Timeout), nil
	}
	stdout := scrubSecret(strings.TrimRight(res.Stdout, "\n"), secretToken)
	stderr := scrubSecret(strings.TrimRight(res.Stderr, "\n"), secretToken)
	if res.ExitCode != 0 {
		msg := stderr
		if msg == "" {
			msg = stdout
		}
		if msg == "" {
			msg = fmt.Sprintf("exit=%d", res.ExitCode)
		}
		return mcp.ToolError("%sfleetherd_task %s 失败：%s", label(target), action, msg), nil
	}
	if res.Truncated {
		stdout += "\n\n⚠️ 输出过长，已省略中间部分"
	}

	meta := map[string]any{
		"action":    action,
		"target":    target.name,
		"exit_code": res.ExitCode,
		"truncated": res.Truncated,
	}
	if parentTopic != "" {
		meta["parent_topic"] = parentTopic
	}
	childTopic := extractTopicID(stdout)
	if childTopic != "" && action == "new" {
		meta["topic_id"] = childTopic
	}
	if cfg.WebUIBaseURL != "" {
		linkID := childTopic
		if linkID == "" {
			linkID = parentTopic
		}
		if linkID != "" {
			webURL := cfg.WebUIBaseURL + "/topics/" + linkID
			meta["web_ui_url"] = webURL
			if stdout != "" {
				stdout += "\n"
			}
			stdout += "Web UI: " + webURL
		}
	}
	if parentTopic != "" && sessionRef == "" && action == "new" {
		stdout = fmt.Sprintf("parent_topic: %s\n%s", parentTopic, stdout)
	}
	if strings.TrimSpace(stdout) == "" {
		stdout = "(空)"
	}
	return mcp.Text(stdout).WithStructured(meta), nil
}

func (t *fleetherdTaskTool) loadBridgeMapping(cc *mcp.CallContext, target targetInfo, bridgeDir, sessionRef string) (*webBridgeMapping, error) {
	var bridgePath string
	if target.remote {
		bridgePath = remoteJoin(bridgeDir, sessionRef+".json")
	} else {
		dir := bridgeDir
		if dir == "~" || strings.HasPrefix(dir, "~/") {
			resolved, err := t.d.Paths.Resolve(dir)
			if err != nil {
				return nil, err
			}
			dir = resolved
		}
		bridgePath = filepath.Join(dir, sessionRef+".json")
	}
	// 仅 fleetherd_task 内部直接经 fleet.FS 读取映射文件（绕过对模型生效的 PathGuard）。
	var fsys fleet.FS
	if target.remote {
		fsys = &fleet.RemoteFS{Runner: t.d.fleetOrLocal(), Target: target.name, MaxBytes: 64 << 10}
	} else {
		fsys = fleet.LocalFS{}
	}
	data, err := fsys.ReadFile(cc.Ctx, bridgePath)
	if err != nil {
		return nil, fmt.Errorf("无法读取映射文件（session_ref=%s）", sessionRef)
	}
	var m webBridgeMapping
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("映射文件 JSON 格式无效（session_ref=%s）", sessionRef)
	}
	m.Socket = strings.TrimSpace(m.Socket)
	m.Token = strings.TrimSpace(m.Token)
	if m.Socket == "" || m.Token == "" {
		return nil, fmt.Errorf("映射文件缺少 socket 或 token（session_ref=%s）", sessionRef)
	}
	return &m, nil
}

func (t *fleetherdTaskTool) createEmptyParentTopic(cc *mcp.CallContext, cfg config.FleetherdTaskExtCfg, title string) (string, error) {
	topicTitle := strings.TrimSpace(title)
	if topicTitle == "" {
		topicTitle = "Web Coordination"
	}
	if !strings.HasPrefix(topicTitle, "[Web]") {
		topicTitle = "[Web] " + topicTitle
	}
	argv := make([]string, 0, len(cfg.EmptyTopicCommand))
	for _, part := range cfg.EmptyTopicCommand {
		argv = append(argv, strings.ReplaceAll(part, "{title}", topicTitle))
	}
	_, res, err := t.d.runExtensionArgv(cc, cfg.Target, argv, nil, cfg.Timeout, cfg.MaxOutputBytes)
	if err != nil {
		return "", err
	}
	if res.TimedOut {
		return "", fmt.Errorf("执行超时（%s）", cfg.Timeout)
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		if msg == "" {
			msg = fmt.Sprintf("exit=%d", res.ExitCode)
		}
		return "", fmt.Errorf("%s", msg)
	}
	out := strings.TrimSpace(res.Stdout)
	if id := extractTopicID(out); id != "" {
		return id, nil
	}
	// 若命令直接输出单行 topic ID，取首行校验。
	first := firstLine(out)
	if validTopicIDRE.MatchString(first) && !strings.Contains(first, "..") {
		return first, nil
	}
	return "", fmt.Errorf("无法从命令输出解析 parent_topic：%s", truncate(out, 200))
}

func buildFleetherdTaskArgv(binary, action string, a fleetherdTaskArgs, sessionRef, parentTopic string) ([]string, error) {
	addParentChannel := func(argv []string) []string {
		if sessionRef == "" && parentTopic != "" {
			return append(argv, "-c", parentTopic)
		}
		return argv
	}

	switch action {
	case "agents":
		return addParentChannel([]string{binary, "task-agents"}), nil
	case "list":
		return addParentChannel([]string{binary, "tasks"}), nil
	case "new":
		prompt := strings.TrimSpace(a.Prompt)
		if prompt == "" {
			return nil, fmt.Errorf("action=new 时 prompt 不能为空")
		}
		argv := []string{binary, "task-new"}
		if v := strings.TrimSpace(a.Agent); v != "" {
			argv = append(argv, "--agent", v)
		}
		if v := strings.TrimSpace(a.Cwd); v != "" {
			argv = append(argv, "--cwd", v)
		}
		if v := strings.TrimSpace(a.Title); v != "" {
			argv = append(argv, "--title", v)
		}
		if v := strings.TrimSpace(a.Model); v != "" {
			argv = append(argv, "--model", v)
		}
		argv = addParentChannel(argv)
		return append(argv, "--", prompt), nil
	case "send":
		topic := strings.TrimSpace(a.Topic)
		prompt := strings.TrimSpace(a.Prompt)
		if topic == "" || prompt == "" {
			return nil, fmt.Errorf("action=send 时 topic 与 prompt 均不能为空")
		}
		if !validTopicIDRE.MatchString(topic) || strings.Contains(topic, "..") {
			return nil, fmt.Errorf("非法的 topic %q", a.Topic)
		}
		argv := []string{binary, "task-send"}
		if a.Interrupt {
			argv = append(argv, "--interrupt")
		}
		if a.Forward {
			argv = append(argv, "--forward")
		}
		argv = addParentChannel(argv)
		return append(argv, "--", topic, prompt), nil
	case "read":
		topic := strings.TrimSpace(a.Topic)
		if topic == "" {
			return nil, fmt.Errorf("action=read 时 topic 不能为空")
		}
		if !validTopicIDRE.MatchString(topic) || strings.Contains(topic, "..") {
			return nil, fmt.Errorf("非法的 topic %q", a.Topic)
		}
		argv := []string{binary, "task-read"}
		if v := strings.TrimSpace(a.Turn); v != "" {
			argv = append(argv, "--turn", v)
		}
		if a.Full {
			argv = append(argv, "--full")
		}
		if a.Tail > 0 {
			argv = append(argv, "--tail", strconv.Itoa(a.Tail))
		}
		argv = addParentChannel(argv)
		return append(argv, "--", topic), nil
	case "status":
		topic := strings.TrimSpace(a.Topic)
		if topic == "" {
			return nil, fmt.Errorf("action=status 时 topic 不能为空")
		}
		if !validTopicIDRE.MatchString(topic) || strings.Contains(topic, "..") {
			return nil, fmt.Errorf("非法的 topic %q", a.Topic)
		}
		argv := addParentChannel([]string{binary, "task-status"})
		return append(argv, "--", topic), nil
	case "stop":
		topic := strings.TrimSpace(a.Topic)
		if topic == "" {
			return nil, fmt.Errorf("action=stop 时 topic 不能为空")
		}
		if !validTopicIDRE.MatchString(topic) || strings.Contains(topic, "..") {
			return nil, fmt.Errorf("非法的 topic %q", a.Topic)
		}
		argv := addParentChannel([]string{binary, "task-stop"})
		return append(argv, "--", topic), nil
	default:
		return nil, fmt.Errorf("不支持的 action %q", action)
	}
}

func extractTopicID(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	// 优先尝试 JSON 输出
	var obj map[string]any
	if json.Unmarshal([]byte(out), &obj) == nil {
		for _, k := range []string{"topic_id", "parent_topic", "topic", "id"} {
			if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	if m := topicIDExtractRE.FindStringSubmatch(out); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}
