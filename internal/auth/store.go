// Package auth 实现一个自包含的 OAuth 2.1 授权服务器 + 资源服务器保护，
// 覆盖 MCP 规范要求的那套发现流程：
//
//	RFC 9728 受保护资源元数据 → RFC 8414 授权服务器元数据
//	→ RFC 7591 动态客户端注册 → 授权码 + PKCE(S256) → Bearer 访问令牌
//
// ChatGPT 添加自定义连接器时正是按这条链路自助完成注册与授权的。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/noir017/agent-tools-mcp/internal/idgen"
)

// Client 是一个已注册的 OAuth 客户端。
type Client struct {
	ID              string    `json:"client_id"`
	SecretHash      string    `json:"secret_hash,omitempty"`
	Name            string    `json:"client_name,omitempty"`
	RedirectURIs    []string  `json:"redirect_uris"`
	GrantTypes      []string  `json:"grant_types"`
	TokenAuthMethod string    `json:"token_endpoint_auth_method"`
	Scope           string    `json:"scope,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	// Dynamic 标记该客户端来自动态注册。
	Dynamic bool `json:"dynamic"`
}

// AllowsRedirect 判断回调地址是否已登记。
func (c *Client) AllowsRedirect(uri string) bool {
	for _, r := range c.RedirectURIs {
		if r == uri {
			return true
		}
	}
	return false
}

// Token 是一个访问令牌记录。落盘只存哈希，原文只在签发那一刻出现过。
type Token struct {
	Hash      string    `json:"hash"`
	ClientID  string    `json:"client_id"`
	Subject   string    `json:"subject"`
	Scope     string    `json:"scope,omitempty"`
	Resource  string    `json:"resource,omitempty"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// RefreshHash 关联的刷新令牌，便于整串吊销。
	RefreshHash string `json:"refresh_hash,omitempty"`
}

func (t *Token) Expired() bool { return time.Now().After(t.ExpiresAt) }

// Refresh 是一个刷新令牌记录。
type Refresh struct {
	Hash      string    `json:"hash"`
	ClientID  string    `json:"client_id"`
	Subject   string    `json:"subject"`
	Scope     string    `json:"scope,omitempty"`
	Resource  string    `json:"resource,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (r *Refresh) Expired() bool { return time.Now().After(r.ExpiresAt) }

// Session 是浏览器登录态，让同一个人再次授权时不必重复输密码。
type Session struct {
	Hash      string    `json:"hash"`
	Subject   string    `json:"subject"`
	ExpiresAt time.Time `json:"expires_at"`
}

type storeFile struct {
	Clients  map[string]*Client  `json:"clients"`
	Tokens   map[string]*Token   `json:"tokens"`
	Refresh  map[string]*Refresh `json:"refresh_tokens"`
	Sessions map[string]*Session `json:"sessions"`
}

// Store 是 OAuth 状态的持久化存储：一个 JSON 文件 + 进程内缓存。
//
// 用文件而不是数据库是刻意的——这个服务是单实例个人用途，令牌量极小，
// 少一个依赖就少一处运维负担；写入走"临时文件 + rename"保证不会写坏。
type Store struct {
	path string

	mu   sync.RWMutex
	data storeFile
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s 失败: %w", dir, err)
	}
	s := &Store{
		path: filepath.Join(dir, "oauth.json"),
		data: storeFile{
			Clients: map[string]*Client{}, Tokens: map[string]*Token{},
			Refresh: map[string]*Refresh{}, Sessions: map[string]*Session{},
		},
	}
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, s.flush()
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", s.path, err)
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w（如需重置，删掉该文件并重新授权）", s.path, err)
	}
	s.ensureMaps()
	s.gc()
	return s, nil
}

func (s *Store) ensureMaps() {
	if s.data.Clients == nil {
		s.data.Clients = map[string]*Client{}
	}
	if s.data.Tokens == nil {
		s.data.Tokens = map[string]*Token{}
	}
	if s.data.Refresh == nil {
		s.data.Refresh = map[string]*Refresh{}
	}
	if s.data.Sessions == nil {
		s.data.Sessions = map[string]*Session{}
	}
}

// flush 把当前状态原子写盘。调用方需自行持有锁（或在初始化阶段独占）。
func (s *Store) flush() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) save() {
	if err := s.flush(); err != nil {
		fmt.Fprintf(os.Stderr, "警告：OAuth 状态写盘失败: %v\n", err)
	}
}

// gc 清掉过期条目，顺手控制文件大小。
func (s *Store) gc() {
	now := time.Now()
	for k, v := range s.data.Tokens {
		if now.After(v.ExpiresAt.Add(24 * time.Hour)) {
			delete(s.data.Tokens, k)
		}
	}
	for k, v := range s.data.Refresh {
		if now.After(v.ExpiresAt) {
			delete(s.data.Refresh, k)
		}
	}
	for k, v := range s.data.Sessions {
		if now.After(v.ExpiresAt) {
			delete(s.data.Sessions, k)
		}
	}
}

// ---------- 客户端 ----------

func (s *Store) SaveClient(c *Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Clients[c.ID] = c
	s.save()
}

func (s *Store) Client(id string) (*Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.data.Clients[id]
	return c, ok
}

func (s *Store) ClientCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.Clients)
}

// ---------- 访问令牌 ----------

// IssueAccessToken 生成并保存一个访问令牌，返回原文（此后只存哈希）。
func (s *Store) IssueAccessToken(clientID, subject, scope, resource string, ttl time.Duration, refreshHash string) (string, *Token) {
	raw := idgen.Secret(32)
	tok := &Token{
		Hash: idgen.Hash(raw), ClientID: clientID, Subject: subject, Scope: scope,
		Resource: resource, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(ttl),
		RefreshHash: refreshHash,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Tokens[tok.Hash] = tok
	s.gc()
	s.save()
	return raw, tok
}

// LookupAccessToken 按原文查找令牌。
func (s *Store) LookupAccessToken(raw string) (*Token, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.data.Tokens[idgen.Hash(raw)]
	return t, ok
}

// ---------- 刷新令牌 ----------

func (s *Store) IssueRefreshToken(clientID, subject, scope, resource string, ttl time.Duration) (string, *Refresh) {
	raw := idgen.Secret(32)
	r := &Refresh{
		Hash: idgen.Hash(raw), ClientID: clientID, Subject: subject,
		Scope: scope, Resource: resource, ExpiresAt: time.Now().Add(ttl),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Refresh[r.Hash] = r
	s.save()
	return raw, r
}

// ConsumeRefreshToken 取出并删除刷新令牌（轮换：每次刷新都换新的）。
func (s *Store) ConsumeRefreshToken(raw string) (*Refresh, bool) {
	h := idgen.Hash(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.data.Refresh[h]
	if !ok {
		return nil, false
	}
	delete(s.data.Refresh, h)
	// 同一串里的访问令牌一并失效，避免刷新后旧令牌还能用。
	for k, t := range s.data.Tokens {
		if t.RefreshHash == h {
			delete(s.data.Tokens, k)
		}
	}
	s.save()
	return r, true
}

// RevokeByRaw 吊销一个访问令牌或刷新令牌。
func (s *Store) RevokeByRaw(raw string) bool {
	h := idgen.Hash(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	if t, ok := s.data.Tokens[h]; ok {
		delete(s.data.Tokens, h)
		if t.RefreshHash != "" {
			delete(s.data.Refresh, t.RefreshHash)
		}
		found = true
	}
	if _, ok := s.data.Refresh[h]; ok {
		delete(s.data.Refresh, h)
		for k, t := range s.data.Tokens {
			if t.RefreshHash == h {
				delete(s.data.Tokens, k)
			}
		}
		found = true
	}
	if found {
		s.save()
	}
	return found
}

// ---------- 浏览器会话 ----------

func (s *Store) IssueSession(subject string, ttl time.Duration) string {
	raw := idgen.Secret(24)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Sessions[idgen.Hash(raw)] = &Session{
		Hash: idgen.Hash(raw), Subject: subject, ExpiresAt: time.Now().Add(ttl),
	}
	s.save()
	return raw
}

func (s *Store) LookupSession(raw string) (*Session, bool) {
	if raw == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.data.Sessions[idgen.Hash(raw)]
	if !ok || sess.Expired() {
		return nil, false
	}
	return sess, true
}

func (sess *Session) Expired() bool { return time.Now().After(sess.ExpiresAt) }

// Stats 返回观测用的计数。
func (s *Store) Stats() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]int{
		"clients": len(s.data.Clients), "access_tokens": len(s.data.Tokens),
		"refresh_tokens": len(s.data.Refresh), "browser_sessions": len(s.data.Sessions),
	}
}
