package app_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/noir017/agent-tools-mcp/internal/app"
	"github.com/noir017/agent-tools-mcp/internal/config"
)

const testPassword = "correct-horse-battery"

// harness 起一个完整的服务实例：真实的 OAuth 授权服务器 + MCP 端点。
type harness struct {
	t         *testing.T
	srv       *httptest.Server
	base      string
	workdir   string
	token     string
	sessionID string
	reqID     int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()

	// 先拿到监听地址，再据此设置 base_url —— OAuth 元数据里的地址必须和实际访问地址一致。
	srv := httptest.NewUnstartedServer(nil)
	base := "http://" + srv.Listener.Addr().String()

	cfg := config.Default()
	cfg.Server.BaseURL = base
	cfg.Server.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.Server.AllowedOrigins = []string{"*"}
	cfg.Auth.Users = []config.User{{Username: "tester", PasswordHash: string(hash)}}
	cfg.Tools.Shell.DefaultWorkdir = workdir
	cfg.Audit.File = filepath.Join(cfg.Server.DataDir, "audit.jsonl")

	built, err := app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("装配服务失败: %v", err)
	}
	srv.Config.Handler = built.Handler
	srv.Start()
	t.Cleanup(func() {
		srv.Close()
		built.Close()
	})
	return &harness{t: t, srv: srv, base: base, workdir: workdir}
}

func (h *harness) get(path string) (*http.Response, []byte) {
	h.t.Helper()
	resp, err := h.srv.Client().Get(h.base + path)
	if err != nil {
		h.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

// authorize 走完整的 注册 → 授权 → 登录 → 换令牌 流程。
func (h *harness) authorize() {
	h.t.Helper()
	client := &http.Client{
		// 授权码回调指向一个不存在的地址，所以要拦住重定向自己解析 code。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// 1. 动态客户端注册（ChatGPT 就是这么自助接入的）
	regBody, _ := json.Marshal(map[string]any{
		"client_name":                "测试客户端",
		"redirect_uris":              []string{"https://example.invalid/callback"},
		"token_endpoint_auth_method": "client_secret_post",
	})
	resp, err := client.Post(h.base+"/oauth/register", "application/json", strings.NewReader(string(regBody)))
	if err != nil {
		h.t.Fatalf("注册失败: %v", err)
	}
	var reg struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	decodeBody(h.t, resp, &reg)
	if reg.ClientID == "" || reg.ClientSecret == "" {
		h.t.Fatal("动态注册没有返回 client_id/client_secret")
	}

	// 2. 发起授权，取登录页里的 req_id
	verifier := "verifier-0123456789-0123456789-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	authURL := fmt.Sprintf("%s/oauth/authorize?response_type=code&client_id=%s&redirect_uri=%s"+
		"&code_challenge=%s&code_challenge_method=S256&state=xyz&resource=%s",
		h.base, reg.ClientID, url.QueryEscape("https://example.invalid/callback"),
		challenge, url.QueryEscape(h.base+"/mcp"))
	resp, err = client.Get(authURL)
	if err != nil {
		h.t.Fatalf("授权请求失败: %v", err)
	}
	page := readBody(h.t, resp)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("授权页状态 %d: %s", resp.StatusCode, page)
	}
	m := regexp.MustCompile(`name="req_id" value="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		h.t.Fatalf("登录页里找不到 req_id:\n%s", page)
	}

	// 3. 提交登录，从 302 的 Location 里取授权码
	form := url.Values{"req_id": {m[1]}, "username": {"tester"}, "password": {testPassword}}
	resp, err = client.PostForm(h.base+"/oauth/login", form)
	if err != nil {
		h.t.Fatalf("登录失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		h.t.Fatalf("登录后应 302，实际 %d: %s", resp.StatusCode, readBody(h.t, resp))
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		h.t.Fatal(err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		h.t.Fatalf("回调地址里没有 code: %s", loc.String())
	}
	if got := loc.Query().Get("state"); got != "xyz" {
		h.t.Errorf("state 应原样回传，得到 %q", got)
	}

	// 4. 用授权码 + code_verifier 换访问令牌
	resp, err = client.PostForm(h.base+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"https://example.invalid/callback"},
		"client_id":     {reg.ClientID},
		"client_secret": {reg.ClientSecret},
		"code_verifier": {verifier},
	})
	if err != nil {
		h.t.Fatalf("换令牌失败: %v", err)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
	}
	decodeBody(h.t, resp, &tok)
	if tok.AccessToken == "" || tok.TokenType != "Bearer" || tok.ExpiresIn <= 0 {
		h.t.Fatalf("令牌响应不完整: %+v", tok)
	}
	if tok.RefreshToken == "" {
		h.t.Error("应当同时下发 refresh_token")
	}
	h.token = tok.AccessToken

	// 授权码不可重放
	resp, _ = client.PostForm(h.base+"/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"client_id": {reg.ClientID}, "client_secret": {reg.ClientSecret},
		"code_verifier": {verifier},
	})
	if resp.StatusCode == http.StatusOK {
		h.t.Error("同一个授权码被兑换了两次")
	}
	resp.Body.Close()
}

// rpc 发一条 JSON-RPC 请求到 /mcp。
func (h *harness) rpc(method string, params any) map[string]any {
	h.t.Helper()
	h.reqID++
	payload := map[string]any{"jsonrpc": "2.0", "id": h.reqID, "method": method}
	if params != nil {
		payload["params"] = params
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, h.base+"/mcp", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	if h.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", h.sessionID)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s 请求失败: %v", method, err)
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.sessionID = sid
	}
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("%s 返回 %d: %s", method, resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatalf("%s 响应不是 JSON: %v (%s)", method, err, raw)
	}
	if e, ok := out["error"]; ok {
		h.t.Fatalf("%s 返回 JSON-RPC 错误: %v", method, e)
	}
	result, _ := out["result"].(map[string]any)
	return result
}

// callTool 调一个工具，返回文本内容与 isError。
func (h *harness) callTool(name string, args map[string]any) (string, bool) {
	h.t.Helper()
	res := h.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	isErr, _ := res["isError"].(bool)
	var sb strings.Builder
	for _, c := range res["content"].([]any) {
		block := c.(map[string]any)
		if txt, ok := block["text"].(string); ok {
			sb.WriteString(txt)
		}
	}
	return sb.String(), isErr
}

func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("响应不是 JSON（状态 %d）: %v\n%s", resp.StatusCode, err, raw)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(raw)
}

// TestDiscoveryMetadata 校验 MCP 客户端依赖的两份发现文档。
func TestDiscoveryMetadata(t *testing.T) {
	h := newHarness(t)

	resp, body := h.get("/.well-known/oauth-protected-resource")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("受保护资源元数据状态 %d", resp.StatusCode)
	}
	var prm struct {
		Resource   string   `json:"resource"`
		AuthServer []string `json:"authorization_servers"`
	}
	if err := json.Unmarshal(body, &prm); err != nil {
		t.Fatal(err)
	}
	if prm.Resource != h.base+"/mcp" {
		t.Errorf("resource 应为 %s，得到 %s", h.base+"/mcp", prm.Resource)
	}
	if len(prm.AuthServer) != 1 || prm.AuthServer[0] != h.base {
		t.Errorf("authorization_servers 不对: %v", prm.AuthServer)
	}

	resp, body = h.get("/.well-known/oauth-authorization-server")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("授权服务器元数据状态 %d", resp.StatusCode)
	}
	var asm struct {
		Issuer     string   `json:"issuer"`
		AuthEP     string   `json:"authorization_endpoint"`
		TokenEP    string   `json:"token_endpoint"`
		RegEP      string   `json:"registration_endpoint"`
		Challenges []string `json:"code_challenge_methods_supported"`
	}
	if err := json.Unmarshal(body, &asm); err != nil {
		t.Fatal(err)
	}
	if asm.Issuer != h.base || asm.AuthEP == "" || asm.TokenEP == "" || asm.RegEP == "" {
		t.Errorf("授权服务器元数据不完整: %+v", asm)
	}
	if len(asm.Challenges) != 1 || asm.Challenges[0] != "S256" {
		t.Errorf("应只支持 S256，得到 %v", asm.Challenges)
	}
}

// TestUnauthorizedChallenge 校验 401 带回可发现授权服务器的头，
// 这是 ChatGPT 能自动弹出授权流程的前提。
func TestUnauthorizedChallenge(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest(http.MethodPost, h.base+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌访问应返回 401，实际 %d", resp.StatusCode)
	}
	wa := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(wa, "resource_metadata=") {
		t.Errorf("WWW-Authenticate 必须包含 resource_metadata，实际: %q", wa)
	}

	// 伪造令牌同样应被拒绝（重新构造请求：上一个 body 已被读空）
	req2, _ := http.NewRequest(http.MethodPost, h.base+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer 假的令牌")
	resp2, err := h.srv.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("伪造令牌应返回 401，实际 %d", resp2.StatusCode)
	}
}

// TestFullFlow 走通"授权 → initialize → 列工具 → 用工具"的完整链路。
func TestFullFlow(t *testing.T) {
	h := newHarness(t)
	h.authorize()

	init := h.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test-client", "version": "1.0"},
	})
	if init["protocolVersion"] != "2025-06-18" {
		t.Errorf("协议版本协商结果不对: %v", init["protocolVersion"])
	}
	if h.sessionID == "" {
		t.Error("initialize 响应应带 Mcp-Session-Id")
	}
	if _, ok := init["instructions"].(string); !ok {
		t.Error("initialize 应返回 instructions 指导模型如何使用工具")
	}

	list := h.rpc("tools/list", nil)
	names := map[string]bool{}
	for _, tl := range list["tools"].([]any) {
		td := tl.(map[string]any)
		names[td["name"].(string)] = true
		if td["description"] == "" || td["inputSchema"] == nil {
			t.Errorf("工具 %v 缺少描述或 schema", td["name"])
		}
	}
	for _, want := range []string{"shell", "read", "write", "apply_patch", "search", "glob", "list_dir"} {
		if !names[want] {
			t.Errorf("工具列表里缺少 %s（实际: %v）", want, names)
		}
	}

	// shell 正常执行
	out, isErr := h.callTool("shell", map[string]any{"command": "echo hello && pwd"})
	if isErr {
		t.Errorf("echo 不该失败: %s", out)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, h.workdir) {
		t.Errorf("输出里应有 hello 和工作目录:\n%s", out)
	}

	// 非零退出码要如实反映
	out, isErr = h.callTool("shell", map[string]any{"command": "exit 3"})
	if !isErr || !strings.Contains(out, "exit_code: 3") {
		t.Errorf("退出码 3 应被标记为错误并如实展示:\n%s", out)
	}
}

// TestShellPolicyOverMCP 验证拦截与二次确认在真实调用链上生效。
func TestShellPolicyOverMCP(t *testing.T) {
	h := newHarness(t)
	h.authorize()
	h.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18",
		"clientInfo": map[string]any{"name": "t", "version": "1"}})

	// deny：不给令牌，直接拒绝
	out, isErr := h.callTool("shell", map[string]any{"command": "rm -rf /"})
	if !isErr {
		t.Fatal("rm -rf / 必须被拒绝")
	}
	if strings.Contains(out, "confirm_token =") {
		t.Error("deny 级命令不应下发确认令牌")
	}

	// confirm：先拿令牌，再带令牌执行
	if err := os.WriteFile(filepath.Join(h.workdir, "victim.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(h.workdir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	out, isErr = h.callTool("shell", map[string]any{"command": "rm -rf *"})
	if !isErr {
		t.Fatal("rm -rf * 应先要求确认")
	}
	m := regexp.MustCompile(`confirm_token = (\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("没有下发 confirm_token:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.workdir, "victim.txt")); err != nil {
		t.Fatal("被拦下的命令绝不能已经执行")
	}

	// 令牌不能挪用给另一条命令
	_, isErr = h.callTool("shell", map[string]any{
		"command": "rm -rf ./sub", "confirm_token": m[1]})
	if !isErr {
		t.Error("令牌被挪用到别的命令上时应当失败")
	}

	// 重新取令牌并对同一条命令使用
	out, _ = h.callTool("shell", map[string]any{"command": "rm -rf *"})
	m = regexp.MustCompile(`confirm_token = (\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("第二次没有下发 confirm_token:\n%s", out)
	}
	out, isErr = h.callTool("shell", map[string]any{"command": "rm -rf *", "confirm_token": m[1]})
	if isErr {
		t.Fatalf("带正确令牌应当执行成功:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.workdir, "victim.txt")); !os.IsNotExist(err) {
		t.Error("确认后命令应真正执行")
	}

	// 令牌一次性
	_, isErr = h.callTool("shell", map[string]any{"command": "rm -rf *", "confirm_token": m[1]})
	if !isErr {
		t.Error("同一个令牌不应能用第二次")
	}
}

// TestSensitivePathBlocked 验证文件工具的凭据路径黑名单。
func TestSensitivePathBlocked(t *testing.T) {
	h := newHarness(t)
	h.authorize()
	h.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18",
		"clientInfo": map[string]any{"name": "t", "version": "1"}})

	out, isErr := h.callTool("read", map[string]any{"path": "/etc/shadow"})
	if !isErr || !strings.Contains(out, "敏感路径") {
		t.Errorf("/etc/shadow 应被策略拒绝:\n%s", out)
	}
	out, isErr = h.callTool("write", map[string]any{"path": "/proc/sysrq-trigger", "content": "c"})
	if !isErr {
		t.Errorf("写 /proc 应被拒绝:\n%s", out)
	}
}

// TestSessionTermination 验证 DELETE 能结束会话。
func TestSessionTermination(t *testing.T) {
	h := newHarness(t)
	h.authorize()
	h.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18",
		"clientInfo": map[string]any{"name": "t", "version": "1"}})

	req, _ := http.NewRequest(http.MethodDelete, h.base+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Mcp-Session-Id", h.sessionID)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE 应返回 204，实际 %d", resp.StatusCode)
	}
}
