package auth

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"

	"github.com/noir017/runabout/internal/mcp"
)

// Protect 是资源服务器侧的 Bearer 校验中间件。
//
// 关键细节：401 必须带上 WWW-Authenticate: Bearer resource_metadata="…"。
// MCP 客户端（含 ChatGPT）正是靠这个头发现"该找哪个授权服务器",
// 少了它就只会显示一个没有下文的连接失败。
func (s *Server) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Auth.Enabled {
			next.ServeHTTP(w, mcp.RequestWithPrincipal(r, mcp.Principal{
				Subject: "anonymous", Method: "disabled",
			}))
			return
		}
		raw := bearer(r)
		if raw == "" {
			s.challenge(w, "invalid_request", "缺少 Authorization: Bearer <token> 请求头")
			return
		}
		// 静态令牌：给 curl 调试和不方便走 OAuth 的客户端留的后门（需显式配置）。
		for _, st := range s.cfg.Auth.StaticTokens {
			if st.Token != "" && subtle.ConstantTimeCompare([]byte(st.Token), []byte(raw)) == 1 {
				next.ServeHTTP(w, mcp.RequestWithPrincipal(r, mcp.Principal{
					Subject: orDefault(st.Name, "static"), Method: "static",
				}))
				return
			}
		}
		tok, ok := s.store.LookupAccessToken(raw)
		if !ok {
			s.challenge(w, "invalid_token", "令牌无效")
			return
		}
		if tok.Expired() {
			s.challenge(w, "invalid_token", "令牌已过期，请用 refresh_token 换新的")
			return
		}
		// 受众核对：只记录不拒绝。自签自用场景下签发方就是本服务，
		// 严格拒绝反而容易因为客户端把 resource 写成 base_url 少个 /mcp 而连不上。
		if tok.Resource != "" && !s.resourceMatches(tok.Resource) {
			s.log.Warn("令牌的 resource 与本服务不一致", "token_resource", tok.Resource, "expected", s.base()+"/mcp")
		}
		next.ServeHTTP(w, mcp.RequestWithPrincipal(r, mcp.Principal{
			Subject: tok.Subject, ClientID: tok.ClientID, TokenID: tok.Hash[:12], Method: "oauth",
		}))
	})
}

func (s *Server) resourceMatches(res string) bool {
	res = strings.TrimRight(res, "/")
	base := s.base()
	return res == base || res == base+"/mcp"
}

func (s *Server) challenge(w http.ResponseWriter, errCode, desc string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer realm="runabout", error=%q, error_description=%q, resource_metadata=%q`,
		errCode, desc, s.base()+PathProtectedResource))
	oauthError(w, http.StatusUnauthorized, errCode, desc)
}
