package app

import (
	"encoding/json"
	"html/template"
	"net/http"

	"github.com/noir017/agent-tools-mcp/internal/config"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type landingData struct {
	BaseURL   string
	MCPURL    string
	SourceURL string
	Tools     []string
	AuthOn    bool
	Version   string
	ToolCount int
}

// 打开根路径时给一页"怎么接"的说明，省得每次去翻文档。
var landingTmpl = template.Must(template.New("landing").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>agent-tools-mcp</title>
<style>
:root { color-scheme: light dark; --bg:#f6f7f9; --card:#fff; --fg:#1a1c1f; --muted:#6b7280;
        --border:#e4e6eb; --accent:#2f6feb; }
@media (prefers-color-scheme: dark) {
  :root { --bg:#16181d; --card:#1f2228; --fg:#e6e8eb; --muted:#9aa2ad; --border:#31353d; --accent:#4c8dff; }
}
body { margin:0; background:var(--bg); color:var(--fg); padding:40px 20px;
       font:15px/1.7 -apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans CJK SC","PingFang SC",sans-serif; }
main { max-width:760px; margin:0 auto; }
.card { background:var(--card); border:1px solid var(--border); border-radius:12px; padding:28px; margin-bottom:20px; }
h1 { font-size:22px; margin:0 0 4px; }
h2 { font-size:15px; margin:0 0 12px; }
p.sub { color:var(--muted); margin:0 0 24px; font-size:13px; }
code { background:var(--bg); border:1px solid var(--border); padding:2px 6px; border-radius:5px; font-size:13px; }
pre { background:var(--bg); border:1px solid var(--border); padding:14px; border-radius:8px;
      overflow-x:auto; font-size:13px; margin:12px 0 0; }
ol { padding-left:22px; margin:0; } li { margin-bottom:8px; }
a { color:var(--accent); }
.tools { display:flex; flex-wrap:wrap; gap:8px; margin-top:4px; }
.tag { background:var(--bg); border:1px solid var(--border); border-radius:999px; padding:3px 11px; font-size:13px; }
.warn { color:#b8541b; }
</style></head>
<body><main>
  <div class="card">
    <h1>agent-tools-mcp</h1>
    <p class="sub">版本 {{.Version}}｜MCP 端点 <code>{{.MCPURL}}</code>｜
      鉴权 {{if .AuthOn}}OAuth 2.1 已启用{{else}}<span class="warn">已关闭（仅限本机调试）</span>{{end}}</p>
    <h2>已注册的 {{.ToolCount}} 个工具</h2>
    <div class="tools">{{range .Tools}}<span class="tag">{{.}}</span>{{end}}</div>
  </div>
  <div class="card">
    <h2>在 ChatGPT 里接入</h2>
    <ol>
      <li>设置 → 连接器 → 创建自定义连接器（需要打开开发者模式）。</li>
      <li>MCP 服务器 URL 填 <code>{{.MCPURL}}</code>，鉴权方式选 OAuth。</li>
      <li>保存后点击连接，会跳到本服务的登录页；用配置里的账号密码登录并授权。</li>
      <li>回到对话即可调用工具。客户端注册、令牌颁发全部由本服务自己完成，不依赖外部 IdP。</li>
    </ol>
  </div>
  <div class="card">
    <h2>用 curl 自检</h2>
    <pre>curl -s {{.BaseURL}}/.well-known/oauth-protected-resource | jq
curl -s {{.BaseURL}}/healthz | jq

# 配了 static_tokens 时可以直接列工具
curl -s {{.MCPURL}} -H 'Authorization: Bearer &lt;token&gt;' \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | jq</pre>
  </div>
  <div class="card">
    <h2>许可</h2>
    <p class="sub">agent-tools-mcp，Copyright (C) 2026 noir017，按
      <a href="https://www.gnu.org/licenses/agpl-3.0.html">GNU AGPL v3 或更新版本</a>授权。<br>
      本服务的对应源码：<a href="{{.SourceURL}}">{{.SourceURL}}</a>
      （AGPL 第 13 条要求网络服务向使用者提供源码；如果你部署的是改过的版本，
      请把 <code>server.source_url</code> 指向你自己的仓库）。</p>
  </div>
</main></body></html>`))

func renderLanding(w http.ResponseWriter, cfg *config.Config, tools []string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := landingData{
		BaseURL: cfg.Server.BaseURL, MCPURL: cfg.Server.BaseURL + "/mcp",
		SourceURL: cfg.Server.SourceURL,
		Tools:     tools, AuthOn: cfg.Auth.Enabled, Version: Version, ToolCount: len(tools),
	}
	_ = landingTmpl.Execute(w, data)
}
