package auth

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/noir017/runabout/internal/mcp"
)

// initProbeLimit 是为判断"这是不是 initialize"而预读的上限。
// 握手包只有一两百字节，超过这个量的一定不是，不必把整个 body 读进内存。
const initProbeLimit = 64 << 10

// Protect 是资源服务器侧的 Bearer 校验中间件。
//
// 关键细节：401 必须带上 WWW-Authenticate: Bearer resource_metadata="…"。
// MCP 客户端（含 ChatGPT）正是靠这个头发现"该找哪个授权服务器",
// 少了它就只会显示一个没有下文的连接失败。
func (s *Server) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CORS 预检不带 Authorization —— 浏览器规范就不允许它带。
		// 在这里 401 掉，浏览器侧的 MCP 客户端只会看到一个没有下文的
		// "unexpected error"，而服务端日志里只有一条 Debug 级的 401，
		// 极难定位。预检交给下游 transport 处理（它只回 204 + CORS 头，不碰数据）。
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if !s.cfg.Auth.Enabled {
			next.ServeHTTP(w, mcp.RequestWithPrincipal(r, mcp.Principal{
				Subject: "anonymous", Method: "disabled",
			}))
			return
		}
		raw := bearer(r)
		if raw == "" {
			// 见 config.Auth.AllowUnauthenticatedInitialize：只放握手本身，
			// 别的方法一律照旧要令牌，匿名会话在下一跳就会被这里挡住。
			if s.cfg.Auth.AllowUnauthenticatedInitialize && isHandshake(r) {
				next.ServeHTTP(w, mcp.RequestWithPrincipal(r, mcp.Principal{
					Subject: "anonymous", Method: "anon-handshake",
				}))
				return
			}
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

// isHandshake 判断请求体是不是 MCP 握手（initialize 或紧随其后的
// notifications/initialized）。读过的字节会原样接回 r.Body，下游照常能再读一遍。
func isHandshake(r *http.Request) bool {
	if r.Method != http.MethodPost || r.Body == nil {
		return false
	}
	orig := r.Body
	head, err := io.ReadAll(io.LimitReader(orig, initProbeLimit))
	// 无论解析结果如何都要把流接回去：前半截来自已读缓冲，后半截还在原 Body 里。
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), orig), orig}
	if err != nil {
		return false
	}
	var probe struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(head, &probe) != nil {
		return false
	}
	return probe.Method == "initialize" || probe.Method == "notifications/initialized"
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
