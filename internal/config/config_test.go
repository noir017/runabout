package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestExampleConfigParses 保证仓库里的示例配置与结构体不脱节。
// 配置字段是严格模式解析的，示例里写错一个字段名会让人在部署时才发现。
func TestExampleConfigParses(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "config.example.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("找不到示例配置: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("示例配置解析失败: %v", err)
	}
	if cfg.Server.BaseURL == "" || cfg.Auth.AccessTokenTTL == 0 {
		t.Error("示例配置没有填上关键字段")
	}
	if len(cfg.Policy.ReadDenyPaths) == 0 || len(cfg.Policy.WriteDenyPaths) == 0 {
		t.Error("示例配置应带上路径黑名单")
	}
}

func TestFleetDefaultsAndValidation(t *testing.T) {
	base := func() *Config {
		cfg := Default()
		cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x"}}
		return cfg
	}

	cfg := base()
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Fleet.Hosts) != 0 {
		t.Error("默认不应有机群机器")
	}
	if len(cfg.Fleet.SSHCommand) == 0 || cfg.Fleet.ProbeTimeout <= 0 {
		t.Error("fleet 默认值没填上")
	}

	cfg = base()
	cfg.Fleet.Hosts = []FleetHost{{Name: "web1"}}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if cfg.Fleet.Hosts[0].SSH != "web1" {
		t.Errorf("ssh 别名应默认等于机器名，得到 %q", cfg.Fleet.Hosts[0].SSH)
	}

	for _, bad := range [][]FleetHost{
		{{Name: "local"}},
		{{Name: "web1"}, {Name: "web1"}},
		{{Name: ""}},
	} {
		cfg = base()
		cfg.Fleet.Hosts = bad
		if err := cfg.Normalize(); err == nil {
			t.Errorf("非法机器清单应报错: %+v", bad)
		}
	}
}

func TestFleetRejectsUnsafeWorkdir(t *testing.T) {
	base := func() *Config {
		cfg := Default()
		cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x"}}
		return cfg
	}
	for _, wd := range []string{"sub", "..", "../app", "~user/app"} {
		cfg := base()
		cfg.Fleet.Hosts = []FleetHost{{Name: "web1", Workdir: wd}}
		if err := cfg.Normalize(); err == nil {
			t.Errorf("workdir %q 应被拒绝", wd)
		}
	}
	for _, wd := range []string{"", "~", "~/app", "/srv/app"} {
		cfg := base()
		cfg.Fleet.Hosts = []FleetHost{{Name: "web1", Workdir: wd}}
		if err := cfg.Normalize(); err != nil {
			t.Errorf("workdir %q 应被接受: %v", wd, err)
		}
	}
}

func TestBaseURLRequired(t *testing.T) {
	cfg := Default()
	cfg.Server.BaseURL = ""
	if err := cfg.Normalize(); err == nil {
		t.Error("base_url 为空时应报错")
	}
	cfg = Default()
	cfg.Server.BaseURL = "不是网址"
	if err := cfg.Normalize(); err == nil {
		t.Error("base_url 非绝对地址时应报错")
	}
}

func TestAuthRequiresCredentials(t *testing.T) {
	cfg := Default()
	cfg.Auth.Enabled = true
	cfg.Auth.Users = nil
	cfg.Auth.StaticTokens = nil
	err := cfg.Normalize()
	if err == nil {
		t.Fatal("开了鉴权却没有任何凭据时应报错")
	}
	// 报错信息要能直接告诉人下一步做什么
	if want := "hash-password"; !contains(err.Error(), want) {
		t.Errorf("错误信息应提示怎么生成密码哈希，实际: %v", err)
	}
}

func TestUpstreamConfig(t *testing.T) {
	base := func() *Config {
		cfg := Default()
		cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x"}}
		return cfg
	}

	cfg := base()
	cfg.Upstreams = []Upstream{{
		Name: "fd", URL: "http://127.0.0.1:9000/mcp", Prefix: "fd",
		TokenEnv: "FD_MCP_TOKEN", Tools: []string{" topic_list ", "topic_read", "topic_list"},
	}}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	u := cfg.Upstreams[0]
	if u.Tools[0] != "topic_list" || len(u.Tools) != 2 {
		t.Errorf("工具白名单应 trim 并去重，实际 %v", u.Tools)
	}
	if u.RefreshInterval != 10*time.Minute || u.Timeout != 60*time.Second {
		t.Errorf("默认值不对: refresh=%v timeout=%v", u.RefreshInterval, u.Timeout)
	}

	// 默认配置必须没有上游（默认关闭）。
	if len(Default().Upstreams) != 0 {
		t.Error("默认配置不应启用任何上游")
	}
}

func TestUpstreamStdioConfig(t *testing.T) {
	cfg := Default()
	cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x"}}
	cfg.Upstreams = []Upstream{{
		Name:    "node",
		Command: []string{" node ", "/opt/example/server.mjs"},
		Env:     map[string]string{"EXAMPLE_TOKEN": "literal", "LANG": "C"},
		Workdir: "/opt/example",
		Prefix:  "nd",
		Tools:   []string{"echo"},
	}}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	u := cfg.Upstreams[0]
	if u.URL != "" {
		t.Errorf("stdio 上游不应有 url，实际 %q", u.URL)
	}
	if len(u.Command) != 2 || u.Command[0] != "node" || u.Command[1] != "/opt/example/server.mjs" {
		t.Errorf("command 首项应 trim，其余原样保留，实际 %v", u.Command)
	}
	if u.Instructions {
		t.Error("instructions 默认应为 false")
	}
	if u.RefreshInterval != 10*time.Minute || u.Timeout != 60*time.Second {
		t.Errorf("默认值不对: refresh=%v timeout=%v", u.RefreshInterval, u.Timeout)
	}
}

func TestUpstreamConfigRejectsInvalid(t *testing.T) {
	valid := func() Upstream {
		return Upstream{Name: "fd", URL: "http://127.0.0.1:9000/mcp", Prefix: "fd", Tools: []string{"topic_list"}}
	}
	cases := map[string]func(*Config){
		"空名字":                func(c *Config) { c.Upstreams[0].Name = "" },
		"名字含空格":              func(c *Config) { c.Upstreams[0].Name = "bad name" },
		"重复名字":               func(c *Config) { c.Upstreams = append(c.Upstreams, valid()) },
		"空 URL":              func(c *Config) { c.Upstreams[0].URL = "" },
		"非 http":             func(c *Config) { c.Upstreams[0].URL = "ftp://example.com/mcp" },
		"URL 带凭据":            func(c *Config) { c.Upstreams[0].URL = "http://user:pass@example.com/mcp" },
		"URL 带 query":        func(c *Config) { c.Upstreams[0].URL = "http://example.com/mcp?token=x" },
		"URL 带 fragment":     func(c *Config) { c.Upstreams[0].URL = "http://example.com/mcp#frag" },
		"空前缀":                func(c *Config) { c.Upstreams[0].Prefix = "" },
		"非法前缀":               func(c *Config) { c.Upstreams[0].Prefix = "fd x" },
		"空白名单":               func(c *Config) { c.Upstreams[0].Tools = nil },
		"非法工具名":              func(c *Config) { c.Upstreams[0].Tools = []string{"bad tool"} },
		"非法 token_env":       func(c *Config) { c.Upstreams[0].TokenEnv = "1BAD" },
		"url 与 command 同时配置": func(c *Config) { c.Upstreams[0].Command = []string{"node"} },
		"两者都没配置":             func(c *Config) { c.Upstreams[0].URL = "" },
		"command 首项为空": func(c *Config) {
			c.Upstreams[0].URL = ""
			c.Upstreams[0].Command = []string{" "}
		},
		"stdio 带 token_env": func(c *Config) {
			c.Upstreams[0].URL = ""
			c.Upstreams[0].Command = []string{"node"}
			c.Upstreams[0].TokenEnv = "EXAMPLE_TOKEN"
		},
		"stdio 相对 workdir": func(c *Config) {
			c.Upstreams[0].URL = ""
			c.Upstreams[0].Command = []string{"node"}
			c.Upstreams[0].Workdir = "relative/dir"
		},
		"stdio 非法 env 名": func(c *Config) {
			c.Upstreams[0].URL = ""
			c.Upstreams[0].Command = []string{"node"}
			c.Upstreams[0].Env = map[string]string{"1BAD": "x"}
		},
		"HTTP 带 env":     func(c *Config) { c.Upstreams[0].Env = map[string]string{"A": "b"} },
		"HTTP 带 workdir": func(c *Config) { c.Upstreams[0].Workdir = "/opt/example" },
		"与内置工具重名":        func(c *Config) { c.Upstreams[0].Prefix = "shell"; c.Upstreams[0].Tools = []string{"output"} },
		"跨上游重名": func(c *Config) {
			second := valid()
			second.Name = "other"
			second.Prefix = "fd"
			c.Upstreams = append(c.Upstreams, second)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x"}}
			cfg.Upstreams = []Upstream{valid()}
			mutate(cfg)
			if err := cfg.Normalize(); err == nil {
				t.Error("非法配置应报错")
			}
		})
	}
}

func TestUpstreamURLErrorRedactsQuery(t *testing.T) {
	cfg := Default()
	cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x"}}
	cfg.Upstreams = []Upstream{{
		Name: "fd", URL: "http://example.com/mcp?token=secret", Prefix: "fd", Tools: []string{"a"},
	}}
	err := cfg.Normalize()
	if err == nil {
		t.Fatal("带 query 的 URL 应被拒绝")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("错误信息不应回显 query: %v", err)
	}
	if !strings.Contains(err.Error(), "http://example.com/mcp") {
		t.Errorf("错误信息应保留 scheme+host+path: %v", err)
	}
}

func TestDataDirIsProtected(t *testing.T) {
	cfg := Default()
	cfg.Server.DataDir = "/var/lib/runabout"
	cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x"}}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range cfg.Policy.WriteDenyPaths {
		if p == "/var/lib/runabout/**" {
			found = true
		}
	}
	if !found {
		t.Error("data_dir 必须自动进入写黑名单，否则 agent 能改令牌库")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
