package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/mcp"
)

// Manager 持有所有上游连接与最近一次拉取到的工具定义。
//
// 对外暴露的工具集合由配置里的白名单决定，因此 tools/list 在启动瞬间
// 就能给出稳定结果；上游不可达时只是定义退化为占位符、调用返回错误，
// 不影响 runabout 自身的工具。
type Manager struct {
	log *slog.Logger
	eps map[string]*endpoint
	// order 保持配置顺序，便于日志与测试。
	order []*endpoint

	startOnce sync.Once
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

type endpoint struct {
	cfg    config.Upstream
	client *Client

	mu      sync.RWMutex
	defs    map[string]mcp.ToolDef // 上游工具名 -> 对外定义
	fetched bool
}

// New 构造 Manager。cfgs 为空时所有方法都是安全的空操作。
func New(cfgs []config.Upstream, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{log: log, eps: map[string]*endpoint{}}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	for _, cfg := range cfgs {
		// 允许直接构造未经过 Normalize 的配置（测试、嵌入使用）。
		if cfg.Timeout <= 0 {
			cfg.Timeout = 60 * time.Second
		}
		if cfg.RefreshInterval <= 0 {
			cfg.RefreshInterval = 10 * time.Minute
		}
		ep := &endpoint{cfg: cfg, client: NewClient(cfg), defs: map[string]mcp.ToolDef{}}
		m.eps[cfg.Name] = ep
		m.order = append(m.order, ep)
	}
	return m
}

// Start 启动后台刷新：每个上游立即拉一次，之后按 refresh_interval 周期刷新。
func (m *Manager) Start() {
	if len(m.order) == 0 {
		return
	}
	m.startOnce.Do(func() {
		for _, ep := range m.order {
			m.wg.Add(1)
			go func(ep *endpoint) {
				defer m.wg.Done()
				m.loop(m.ctx, ep)
			}(ep)
		}
	})
}

// Close 停止后台刷新并等待在途请求退出。
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) loop(ctx context.Context, ep *endpoint) {
	for {
		cctx, cancel := context.WithTimeout(ctx, ep.cfg.Timeout)
		err := m.refresh(cctx, ep)
		cancel()
		if err != nil && ctx.Err() == nil {
			m.log.Warn("拉取上游工具列表失败", "upstream", ep.cfg.Name, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(ep.cfg.RefreshInterval):
		}
	}
}

// RefreshAll 同步刷新所有上游，主要供测试与自检使用。
func (m *Manager) RefreshAll(ctx context.Context) error {
	var errs []error
	for _, ep := range m.order {
		cctx, cancel := context.WithTimeout(ctx, ep.cfg.Timeout)
		if err := m.refresh(cctx, ep); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ep.cfg.Name, err))
		}
		cancel()
	}
	return errors.Join(errs...)
}

func (m *Manager) refresh(ctx context.Context, ep *endpoint) error {
	tools, err := ep.client.ListTools(ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	defs := make(map[string]mcp.ToolDef, len(ep.cfg.Tools))
	for _, name := range ep.cfg.Tools {
		if t, ok := byName[name]; ok {
			defs[name] = ep.buildDef(t)
		}
	}
	ep.mu.Lock()
	ep.defs = defs
	ep.fetched = true
	ep.mu.Unlock()
	m.log.Info("上游工具列表已刷新", "upstream", ep.cfg.Name, "exposed", len(defs))
	return nil
}

// Definition 返回某个白名单工具的对外定义；上游定义还没拉到时返回占位定义。
func (m *Manager) Definition(upstreamName, tool string) mcp.ToolDef {
	ep := m.eps[upstreamName]
	if ep == nil {
		return mcp.ToolDef{
			Name:        tool,
			Description: "上游工具（配置里找不到对应上游）",
			InputSchema: fallbackSchema(),
		}
	}
	ep.mu.RLock()
	def, ok := ep.defs[tool]
	fetched := ep.fetched
	ep.mu.RUnlock()
	if ok {
		return def
	}
	return ep.fallbackDef(tool, fetched)
}

// Call 把调用原样转发给上游，返回上游结果。
func (m *Manager) Call(ctx context.Context, upstreamName, tool string, args json.RawMessage) (*mcp.CallToolResult, error) {
	ep := m.eps[upstreamName]
	if ep == nil {
		return nil, fmt.Errorf("未知上游 %q", upstreamName)
	}
	cctx, cancel := context.WithTimeout(ctx, ep.cfg.Timeout)
	defer cancel()

	raw, err := ep.client.CallTool(cctx, tool, args)
	if err != nil {
		return nil, err
	}
	// 解析一份用于审计与协议层判定，序列化时仍输出上游原始 JSON。
	var parsed struct {
		Content []mcp.Content `json:"content"`
		IsError bool          `json:"isError"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("上游结果不是合法的 CallToolResult: %w", err)
	}
	res := mcp.RawResult(raw)
	res.Content = parsed.Content
	res.IsError = parsed.IsError
	return res, nil
}

func (ep *endpoint) buildDef(t Tool) mcp.ToolDef {
	def := mcp.ToolDef{
		Name:        ep.cfg.Prefix + "_" + t.Name,
		Title:       t.Title,
		Description: t.Description,
		InputSchema: schemaOrFallback(t.InputSchema),
	}
	if def.Description == "" {
		def.Description = fmt.Sprintf("上游 %s 的工具 %s", ep.cfg.Name, t.Name)
	}
	if len(t.Annotations) > 0 {
		var ann mcp.ToolAnnotations
		if err := json.Unmarshal(t.Annotations, &ann); err == nil {
			def.Annotations = &ann
		}
	}
	return def
}

func (ep *endpoint) fallbackDef(tool string, fetched bool) mcp.ToolDef {
	desc := fmt.Sprintf("上游 %s 的工具 %s。尚未取到上游工具定义，调用时参数会原样转发。", ep.cfg.Name, tool)
	if fetched {
		desc = fmt.Sprintf("上游 %s 当前的工具列表里没有 %s，调用会返回上游错误。", ep.cfg.Name, tool)
	}
	return mcp.ToolDef{
		Name:        ep.cfg.Prefix + "_" + tool,
		Description: desc,
		InputSchema: fallbackSchema(),
	}
}

func fallbackSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": true,
	}
}

// schemaOrFallback 只接受 JSON 对象形式的 inputSchema，其余情况给一个宽松 schema，
// 避免把上游的脏数据原样发给客户端导致 tools/list 失效。
func schemaOrFallback(raw json.RawMessage) any {
	if len(raw) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err == nil && obj != nil {
			return raw
		}
	}
	return fallbackSchema()
}
