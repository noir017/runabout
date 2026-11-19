package tools

import (
	"encoding/json"

	"github.com/noir017/runabout/internal/mcp"
)

// upstreamTool 是一个透传给上游 MCP 服务的工具。
// 工具名与参数定义来自配置白名单，定义内容随上游刷新而变化。
type upstreamTool struct {
	d        *Deps
	upstream string
	tool     string
}

func (t *upstreamTool) Definition() mcp.ToolDef {
	return t.d.Upstreams.Definition(t.upstream, t.tool)
}

func (t *upstreamTool) Call(cc *mcp.CallContext, raw json.RawMessage) (*mcp.CallToolResult, error) {
	res, err := t.d.Upstreams.Call(cc.Ctx, t.upstream, t.tool, raw)
	if err != nil {
		// 上游故障只影响这一个工具，按工具错误返回，让模型看到原因。
		return mcp.ToolError("上游 %s 的工具 %s 调用失败: %v", t.upstream, t.tool, err), nil
	}
	return res, nil
}

// upstreamTools 按配置为白名单里的每个上游工具注册一个 handler。
// 工具集合在启动时固定，上游不可达不会改变 tools/list 的稳定性。
func upstreamTools(d *Deps) []mcp.Handler {
	if d.Upstreams == nil {
		return nil
	}
	var out []mcp.Handler
	for _, u := range d.Cfg.Upstreams {
		for _, name := range u.Tools {
			out = append(out, &upstreamTool{d: d, upstream: u.Name, tool: name})
		}
	}
	return out
}
