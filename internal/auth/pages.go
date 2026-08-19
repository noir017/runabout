package auth

import (
	"html/template"
	"net/http"
)

// 登录页与错误页。刻意做成自包含的单个模板：不引外部字体/CSS，
// 反代后面、内网、离线环境都能正常渲染。
type loginView struct {
	ReqID      string
	ClientName string
	Scope      string
	Username   string
	Error      string
}

type errorView struct {
	Title  string
	Detail string
}

const pageStyle = `
:root { color-scheme: light dark; --bg:#f6f7f9; --card:#fff; --fg:#1a1c1f; --muted:#6b7280;
        --border:#e4e6eb; --accent:#2f6feb; --danger:#c0362c; }
@media (prefers-color-scheme: dark) {
  :root { --bg:#16181d; --card:#1f2228; --fg:#e6e8eb; --muted:#9aa2ad;
          --border:#31353d; --accent:#4c8dff; --danger:#ff6b5e; }
}
* { box-sizing: border-box; }
body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
       background:var(--bg); color:var(--fg); padding:24px;
       font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans CJK SC","PingFang SC",sans-serif; }
.card { background:var(--card); border:1px solid var(--border); border-radius:12px;
        padding:32px; width:100%; max-width:400px; }
h1 { font-size:19px; margin:0 0 6px; }
p.sub { color:var(--muted); font-size:13px; margin:0 0 24px; }
label { display:block; font-size:13px; color:var(--muted); margin:16px 0 6px; }
input { width:100%; padding:10px 12px; border:1px solid var(--border); border-radius:8px;
        background:var(--bg); color:var(--fg); font-size:15px; }
input:focus { outline:2px solid var(--accent); outline-offset:1px; border-color:var(--accent); }
button { width:100%; margin-top:24px; padding:11px; border:0; border-radius:8px;
         background:var(--accent); color:#fff; font-size:15px; font-weight:500; cursor:pointer; }
button:hover { filter:brightness(1.08); }
.err { margin:16px 0 0; padding:10px 12px; border-radius:8px; font-size:13px;
       background:color-mix(in srgb, var(--danger) 14%, transparent); color:var(--danger); }
.meta { margin-top:24px; padding-top:16px; border-top:1px solid var(--border);
        font-size:12px; color:var(--muted); }
code { background:var(--bg); padding:1px 5px; border-radius:4px; font-size:12px; }
`

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>授权 · runabout</title><style>` + pageStyle + `</style></head>
<body><form class="card" method="post" action="` + PathLogin + `">
  <h1>授权访问服务器工具</h1>
  <p class="sub"><strong>{{.ClientName}}</strong> 请求连接到这台机器的 runabout。
     授权后它将能执行 shell 命令、读写文件。</p>
  <input type="hidden" name="req_id" value="{{.ReqID}}">
  <label for="u">用户名</label>
  <input id="u" name="username" autocomplete="username" autofocus required value="{{.Username}}">
  <label for="p">密码</label>
  <input id="p" name="password" type="password" autocomplete="current-password" required>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  <button type="submit">授权</button>
  <p class="meta">权限范围：<code>{{.Scope}}</code>（全部工具）。
     所有调用都会写入服务端审计日志。不是你本人操作的话，直接关掉此页面。</p>
</form></body></html>`))

var errorTmpl = template.Must(template.New("err").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · runabout</title><style>` + pageStyle + `</style></head>
<body><div class="card">
  <h1>{{.Title}}</h1>
  <p class="sub">{{.Detail}}</p>
</div></body></html>`))

func (s *Server) renderLogin(w http.ResponseWriter, v loginView) {
	s.renderLoginWithStatus(w, v, http.StatusOK)
}

func (s *Server) renderLoginWithStatus(w http.ResponseWriter, v loginView, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'")
	w.WriteHeader(status)
	if err := loginTmpl.Execute(w, v); err != nil {
		s.log.Error("渲染登录页失败", "err", err)
	}
}

func (s *Server) renderError(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := errorTmpl.Execute(w, errorView{Title: title, Detail: detail}); err != nil {
		s.log.Error("渲染错误页失败", "err", err)
	}
}
