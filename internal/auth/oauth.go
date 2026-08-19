package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/noir017/runabout/internal/audit"
	"github.com/noir017/runabout/internal/config"
	"github.com/noir017/runabout/internal/idgen"
)

// 端点路径。改动这些会让已接入的客户端需要重新走一次发现流程。
const (
	PathProtectedResource = "/.well-known/oauth-protected-resource"
	PathAuthServerMeta    = "/.well-known/oauth-authorization-server"
	PathOpenIDConfig      = "/.well-known/openid-configuration"
	PathRegister          = "/oauth/register"
	PathAuthorize         = "/oauth/authorize"
	PathToken             = "/oauth/token"
	PathRevoke            = "/oauth/revoke"
	PathLogin             = "/oauth/login"

	sessionCookie = "runabout_session"
	// ScopeDefault 是本服务唯一的作用域：拿到即拥有全部工具。
	// 细粒度 scope 留给以后真有需要时再加，现在多一层只会让接入更容易出错。
	ScopeDefault = "mcp"
)

// pendingAuth 是一次尚未完成登录的授权请求。
type pendingAuth struct {
	clientID      string
	redirectURI   string
	state         string
	codeChallenge string
	scope         string
	resource      string
	createdAt     time.Time
}

// authCode 是已签发、待兑换的授权码。
type authCode struct {
	clientID      string
	redirectURI   string
	codeChallenge string
	subject       string
	scope         string
	resource      string
	expiresAt     time.Time
}

// Server 是内置的 OAuth 授权服务器与 Bearer 校验器。
type Server struct {
	cfg   *config.Config
	store *Store
	audit *audit.Logger
	log   *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingAuth
	codes   map[string]*authCode
}

func New(cfg *config.Config, store *Store, aud *audit.Logger, log *slog.Logger) (*Server, error) {
	for i, u := range cfg.Auth.Users {
		if u.Username == "" || u.PasswordHash == "" {
			return nil, fmt.Errorf("auth.users[%d] 缺少 username 或 password_hash", i)
		}
		if !strings.HasPrefix(u.PasswordHash, "$2") {
			return nil, fmt.Errorf("auth.users[%d] 的 password_hash 不是 bcrypt 格式；"+
				"用 `runabout hash-password` 生成", i)
		}
	}
	s := &Server{
		cfg: cfg, store: store, audit: aud, log: log,
		pending: map[string]*pendingAuth{}, codes: map[string]*authCode{},
	}
	go s.reap()
	return s, nil
}

func (s *Server) reap() {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for range tick.C {
		now := time.Now()
		s.mu.Lock()
		for k, v := range s.pending {
			if now.Sub(v.createdAt) > 15*time.Minute {
				delete(s.pending, k)
			}
		}
		for k, v := range s.codes {
			if now.After(v.expiresAt) {
				delete(s.codes, k)
			}
		}
		s.mu.Unlock()
	}
}

// Routes 把 OAuth 相关端点注册到 mux 上。
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc(PathProtectedResource, s.handleProtectedResource)
	// RFC 9728 规定资源标识带路径时，元数据挂在 /.well-known/... 之后拼上该路径。
	mux.HandleFunc(PathProtectedResource+"/mcp", s.handleProtectedResource)
	mux.HandleFunc(PathAuthServerMeta, s.handleAuthServerMeta)
	mux.HandleFunc(PathAuthServerMeta+"/mcp", s.handleAuthServerMeta)
	mux.HandleFunc(PathOpenIDConfig, s.handleAuthServerMeta)
	mux.HandleFunc(PathRegister, s.handleRegister)
	mux.HandleFunc(PathAuthorize, s.handleAuthorize)
	mux.HandleFunc(PathLogin, s.handleLogin)
	mux.HandleFunc(PathToken, s.handleToken)
	mux.HandleFunc(PathRevoke, s.handleRevoke)
}

func (s *Server) base() string { return strings.TrimRight(s.cfg.Server.BaseURL, "/") }

// ---------- 元数据 ----------

func (s *Server) handleProtectedResource(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                              s.base() + "/mcp",
		"authorization_servers":                 []string{s.base()},
		"scopes_supported":                      []string{ScopeDefault},
		"bearer_methods_supported":              []string{"header"},
		"resource_name":                         "runabout",
		"resource_documentation":                s.base() + "/",
		"authorization_details_types_supported": []string{},
	})
}

func (s *Server) handleAuthServerMeta(w http.ResponseWriter, r *http.Request) {
	meta := map[string]any{
		"issuer":                                s.cfg.Auth.Issuer,
		"authorization_endpoint":                s.base() + PathAuthorize,
		"token_endpoint":                        s.base() + PathToken,
		"revocation_endpoint":                   s.base() + PathRevoke,
		"response_types_supported":              []string{"code"},
		"response_modes_supported":              []string{"query"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{ScopeDefault},
		"service_documentation":                 s.base() + "/",
	}
	if s.cfg.Auth.AllowDynamicRegistration {
		meta["registration_endpoint"] = s.base() + PathRegister
	}
	writeJSON(w, http.StatusOK, meta)
}

// ---------- 动态客户端注册 (RFC 7591) ----------

type registerRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !s.cfg.Auth.AllowDynamicRegistration {
		oauthError(w, http.StatusForbidden, "access_denied", "本服务已关闭动态客户端注册；请在配置里预置客户端或打开 auth.allow_dynamic_registration")
		return
	}
	if tok := s.cfg.Auth.RegistrationToken; tok != "" {
		if bearer(r) != tok {
			oauthError(w, http.StatusUnauthorized, "invalid_token", "注册需要携带 auth.registration_token")
			return
		}
	}
	var req registerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris 不能为空")
		return
	}
	for _, u := range req.RedirectURIs {
		parsed, err := url.Parse(u)
		if err != nil || !parsed.IsAbs() {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri 必须是绝对地址: "+u)
			return
		}
	}

	method := req.TokenEndpointAuthMethod
	if method == "" {
		method = "client_secret_post"
	}
	c := &Client{
		ID:              idgen.New("client"),
		Name:            orDefault(req.ClientName, "未命名客户端"),
		RedirectURIs:    req.RedirectURIs,
		GrantTypes:      orSlice(req.GrantTypes, []string{"authorization_code", "refresh_token"}),
		TokenAuthMethod: method,
		Scope:           orDefault(req.Scope, ScopeDefault),
		CreatedAt:       time.Now(),
		Dynamic:         true,
	}
	resp := map[string]any{
		"client_id":                  c.ID,
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                c.GrantTypes,
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": c.TokenAuthMethod,
		"scope":                      c.Scope,
		"client_id_issued_at":        c.CreatedAt.Unix(),
	}
	if method != "none" {
		secret := idgen.Secret(32)
		hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", "生成客户端密钥失败")
			return
		}
		c.SecretHash = string(hash)
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	s.store.SaveClient(c)
	s.audit.Auth("oauth_register", "", c.ID, "动态注册: "+c.Name+" → "+strings.Join(c.RedirectURIs, ", "))
	s.log.Info("动态客户端注册", "client_id", c.ID, "name", c.Name, "redirect", c.RedirectURIs)
	writeJSON(w, http.StatusCreated, resp)
}

// ---------- 授权端点 ----------

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")

	client, ok := s.store.Client(clientID)
	if !ok {
		// 这两类错误不能重定向回去（无法确认 redirect_uri 可信），只能直接展示。
		s.renderError(w, http.StatusBadRequest, "未知的 client_id",
			"客户端 "+clientID+" 没有注册过。如果是 ChatGPT 端的连接器，请删掉重新添加一次。")
		return
	}
	if redirectURI == "" && len(client.RedirectURIs) == 1 {
		redirectURI = client.RedirectURIs[0]
	}
	if !client.AllowsRedirect(redirectURI) {
		s.renderError(w, http.StatusBadRequest, "redirect_uri 不匹配",
			"客户端注册的回调地址是："+strings.Join(client.RedirectURIs, "、")+"，本次请求给的是 "+redirectURI)
		return
	}

	state := q.Get("state")
	if rt := q.Get("response_type"); rt != "code" {
		redirectError(w, r, redirectURI, state, "unsupported_response_type", "只支持 response_type=code")
		return
	}
	challenge := q.Get("code_challenge")
	method := q.Get("code_challenge_method")
	if challenge == "" {
		redirectError(w, r, redirectURI, state, "invalid_request",
			"缺少 code_challenge：本服务按 OAuth 2.1 要求强制 PKCE")
		return
	}
	if method != "S256" {
		redirectError(w, r, redirectURI, state, "invalid_request",
			"code_challenge_method 必须是 S256（收到 "+orDefault(method, "空")+"）")
		return
	}

	scope := orDefault(q.Get("scope"), ScopeDefault)
	resource := q.Get("resource")

	// 已有浏览器登录态就直接签码，省一次输密码。
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if sess, ok := s.store.LookupSession(cookie.Value); ok {
			s.completeAuthorize(w, r, client, redirectURI, state, challenge, scope, resource, sess.Subject)
			return
		}
	}

	reqID := idgen.Secret(18)
	s.mu.Lock()
	s.pending[reqID] = &pendingAuth{
		clientID: clientID, redirectURI: redirectURI, state: state,
		codeChallenge: challenge, scope: scope, resource: resource, createdAt: time.Now(),
	}
	s.mu.Unlock()

	s.renderLogin(w, loginView{ReqID: reqID, ClientName: client.Name, Scope: scope})
}

// handleLogin 处理登录表单提交。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderError(w, http.StatusBadRequest, "表单解析失败", err.Error())
		return
	}
	reqID := r.PostFormValue("req_id")
	s.mu.Lock()
	pend, ok := s.pending[reqID]
	if ok {
		delete(s.pending, reqID)
	}
	s.mu.Unlock()
	if !ok {
		s.renderError(w, http.StatusBadRequest, "授权请求已过期",
			"请回到客户端重新发起一次连接（授权请求 15 分钟内有效）。")
		return
	}
	client, ok := s.store.Client(pend.clientID)
	if !ok {
		s.renderError(w, http.StatusBadRequest, "客户端已失效", "请重新添加连接器。")
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	if !s.verifyUser(username, password) {
		s.audit.Auth("login_failed", username, pend.clientID, "用户名或密码错误")
		s.log.Warn("登录失败", "user", username, "client", pend.clientID, "ip", clientIP(r))
		// 重新放回 pending，让用户可以再试一次。
		newID := idgen.Secret(18)
		pend.createdAt = time.Now()
		s.mu.Lock()
		s.pending[newID] = pend
		s.mu.Unlock()
		s.renderLoginWithStatus(w, loginView{
			ReqID: newID, ClientName: client.Name, Scope: pend.scope,
			Error: "用户名或密码不正确", Username: username,
		}, http.StatusUnauthorized)
		return
	}

	// 设浏览器登录态，后续再授权免密。
	if ttl := s.cfg.Auth.SessionCookieTTL; ttl > 0 {
		raw := s.store.IssueSession(username, ttl)
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: raw, Path: "/", HttpOnly: true,
			Secure: strings.HasPrefix(s.base(), "https://"), SameSite: http.SameSiteLaxMode,
			Expires: time.Now().Add(ttl),
		})
	}
	s.audit.Auth("login_ok", username, pend.clientID, "")
	s.completeAuthorize(w, r, client, pend.redirectURI, pend.state, pend.codeChallenge,
		pend.scope, pend.resource, username)
}

func (s *Server) completeAuthorize(w http.ResponseWriter, r *http.Request, client *Client,
	redirectURI, state, challenge, scope, resource, subject string) {
	code := idgen.Secret(24)
	s.mu.Lock()
	s.codes[code] = &authCode{
		clientID: client.ID, redirectURI: redirectURI, codeChallenge: challenge,
		subject: subject, scope: scope, resource: resource,
		expiresAt: time.Now().Add(s.cfg.Auth.AuthCodeTTL),
	}
	s.mu.Unlock()

	u, err := url.Parse(redirectURI)
	if err != nil {
		s.renderError(w, http.StatusBadRequest, "回调地址非法", err.Error())
		return
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	s.log.Info("签发授权码", "client", client.ID, "user", subject, "redirect", redirectURI)
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *Server) verifyUser(username, password string) bool {
	if username == "" || password == "" {
		return false
	}
	for _, u := range s.cfg.Auth.Users {
		if u.Username != username {
			continue
		}
		return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil
	}
	// 用户名不存在时也走一次 bcrypt，避免通过响应时间探测用户名是否存在。
	_ = bcrypt.CompareHashAndPassword(
		[]byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"), []byte(password))
	return false
}

// ---------- 令牌端点 ----------

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "表单解析失败: "+err.Error())
		return
	}
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		s.grantAuthorizationCode(w, r)
	case "refresh_token":
		s.grantRefreshToken(w, r)
	case "":
		oauthError(w, http.StatusBadRequest, "invalid_request", "缺少 grant_type")
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"只支持 authorization_code 与 refresh_token")
	}
}

// authenticateClient 校验客户端身份，支持 none / client_secret_post / basic。
func (s *Server) authenticateClient(r *http.Request) (*Client, error) {
	id, secret := r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	if bid, bsecret, ok := r.BasicAuth(); ok {
		id, secret = bid, bsecret
	}
	if id == "" {
		return nil, fmt.Errorf("缺少 client_id")
	}
	c, ok := s.store.Client(id)
	if !ok {
		return nil, fmt.Errorf("未知的 client_id")
	}
	if c.SecretHash == "" {
		return c, nil // 公开客户端，靠 PKCE 保证安全
	}
	if secret == "" {
		return nil, fmt.Errorf("该客户端需要 client_secret")
	}
	if bcrypt.CompareHashAndPassword([]byte(c.SecretHash), []byte(secret)) != nil {
		return nil, fmt.Errorf("client_secret 不正确")
	}
	return c, nil
}

func (s *Server) grantAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	client, err := s.authenticateClient(r)
	if err != nil {
		oauthError(w, http.StatusUnauthorized, "invalid_client", err.Error())
		return
	}
	code := r.PostFormValue("code")
	s.mu.Lock()
	ac, ok := s.codes[code]
	if ok {
		delete(s.codes, code) // 授权码一次性
	}
	s.mu.Unlock()
	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "授权码无效或已被使用")
		return
	}
	if time.Now().After(ac.expiresAt) {
		oauthError(w, http.StatusBadRequest, "invalid_grant",
			fmt.Sprintf("授权码已过期（有效期 %s）", s.cfg.Auth.AuthCodeTTL))
		return
	}
	if ac.clientID != client.ID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "授权码不属于该客户端")
		return
	}
	if ru := r.PostFormValue("redirect_uri"); ru != "" && ru != ac.redirectURI {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri 与授权时不一致")
		return
	}
	verifier := r.PostFormValue("code_verifier")
	if verifier == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "缺少 code_verifier")
		return
	}
	if !verifyPKCE(verifier, ac.codeChallenge) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier 与 code_challenge 不匹配")
		return
	}

	s.issueTokenPair(w, client.ID, ac.subject, ac.scope, ac.resource)
	s.audit.Auth("token_issued", ac.subject, client.ID, "授权码兑换")
}

func (s *Server) grantRefreshToken(w http.ResponseWriter, r *http.Request) {
	client, err := s.authenticateClient(r)
	if err != nil {
		oauthError(w, http.StatusUnauthorized, "invalid_client", err.Error())
		return
	}
	raw := r.PostFormValue("refresh_token")
	if raw == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "缺少 refresh_token")
		return
	}
	ref, ok := s.store.ConsumeRefreshToken(raw)
	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token 无效或已被使用（本服务每次刷新都会轮换）")
		return
	}
	if ref.Expired() {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token 已过期，请重新授权")
		return
	}
	if ref.ClientID != client.ID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token 不属于该客户端")
		return
	}
	s.issueTokenPair(w, client.ID, ref.Subject, ref.Scope, ref.Resource)
	s.audit.Auth("token_refreshed", ref.Subject, client.ID, "")
}

func (s *Server) issueTokenPair(w http.ResponseWriter, clientID, subject, scope, resource string) {
	refreshRaw, ref := s.store.IssueRefreshToken(clientID, subject, scope, resource, s.cfg.Auth.RefreshTokenTTL)
	accessRaw, tok := s.store.IssueAccessToken(clientID, subject, scope, resource, s.cfg.Auth.AccessTokenTTL, ref.Hash)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  accessRaw,
		"token_type":    "Bearer",
		"expires_in":    int(time.Until(tok.ExpiresAt).Seconds()),
		"refresh_token": refreshRaw,
		"scope":         orDefault(scope, ScopeDefault),
	})
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	token := r.PostFormValue("token")
	if token == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "缺少 token")
		return
	}
	// RFC 7009：无论令牌是否存在都返回 200，避免成为探测接口。
	if s.store.RevokeByRaw(token) {
		s.audit.Auth("token_revoked", "", r.PostFormValue("client_id"), "")
	}
	w.WriteHeader(http.StatusOK)
}

// verifyPKCE 校验 S256 挑战。
func verifyPKCE(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}

// ---------- 工具函数 ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]any{"error": code, "error_description": desc})
}

func redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		oauthError(w, http.StatusBadRequest, code, desc)
		return
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", desc)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "只接受 "+allow)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.TrimSpace(strings.Split(ip, ",")[0])
	}
	return r.RemoteAddr
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func orSlice(v, def []string) []string {
	if len(v) == 0 {
		return def
	}
	return v
}
