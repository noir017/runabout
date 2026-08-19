// Package tools 实现暴露给 agent 的基础工具集：shell、文件读写、
// apply_patch、search、glob、list_dir。
package tools

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/noir017/runabout/internal/audit"
	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/mcp"
	"github.com/noir017/runabout/internal/policy"
)

// Deps 是所有工具共享的依赖。
type Deps struct {
	Cfg     *config.Config
	Paths   *policy.PathGuard
	Shell   *policy.ShellGuard
	Confirm *policy.ConfirmStore
	Audit   *audit.Logger
	Procs   *ProcManager
	Log     *slog.Logger
}

// All 按配置构造全部启用的工具。
func All(d *Deps) []mcp.Handler {
	candidates := []mcp.Handler{
		&shellTool{d: d},
		&shellListTool{d: d},
		&shellOutputTool{d: d},
		&shellKillTool{d: d},
		&readTool{d: d},
		&writeTool{d: d},
		&applyPatchTool{d: d},
		&searchTool{d: d},
		&globTool{d: d},
		&listDirTool{d: d},
	}
	out := make([]mcp.Handler, 0, len(candidates))
	for _, h := range candidates {
		if d.Cfg.IsToolDisabled(h.Definition().Name) {
			d.Log.Info("工具已按配置关闭", "tool", h.Definition().Name)
			continue
		}
		out = append(out, h)
	}
	return out
}

// decodeArgs 解析工具参数。为了让模型能自我纠错，错误信息里带上原始参数片段。
func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("参数格式不正确: %w（收到 %s）", err, truncate(string(raw), 300))
	}
	return nil
}

// ---------- JSON Schema 小工具 ----------

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	} else {
		s["required"] = []string{}
	}
	s["additionalProperties"] = false
	return s
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string, min, max int) map[string]any {
	p := map[string]any{"type": "integer", "description": desc}
	if min != 0 || max != 0 {
		p["minimum"] = min
		if max != 0 {
			p["maximum"] = max
		}
	}
	return p
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func enumProp(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}

func arrProp(desc string, item map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": item}
}

// ---------- 文本处理 ----------

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 按 rune 边界截断，避免把多字节字符切一半。
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// truncateMiddle 保留头尾、省略中间，适合日志这类"开头和结尾都重要"的输出。
func truncateMiddle(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	head := limit / 2
	tail := limit - head
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	omitted := len(s) - head - (len(s) - start)
	return fmt.Sprintf("%s\n\n…（中间省略 %d 字节）…\n\n%s", s[:head], omitted, s[start:]), true
}

// section 生成 "--- 标题 ---" 分段，内容为空时给出明确提示。
func section(title, body string) string {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return fmt.Sprintf("--- %s ---\n(空)", title)
	}
	return fmt.Sprintf("--- %s ---\n%s", title, body)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
