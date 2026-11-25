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
	tr := true
	fa := false
	cfg.Upstreams = []Upstream{{
		Name: "fd", URL: "http://127.0.0.1:9000/mcp", Prefix: "fd",
		TokenEnv: "FD_MCP_TOKEN", Tools: []string{" topic_list ", "topic_read", "topic_list"},
		ToolAnnotations: map[string]UpstreamToolAnnotations{
			" topic_list ": {Title: " 列表 ", ReadOnlyHint: &tr, DestructiveHint: &fa},
		},
	}}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	u := cfg.Upstreams[0]
	if u.Tools[0] != "topic_list" || len(u.Tools) != 2 {
		t.Errorf("工具白名单应 trim 并去重，实际 %v", u.Tools)
	}
	ann, ok := u.ToolAnnotations["topic_list"]
	if !ok || ann.Title != "列表" || ann.ReadOnlyHint == nil || !*ann.ReadOnlyHint || ann.DestructiveHint == nil || *ann.DestructiveHint {
		t.Errorf("tool_annotations 应按 trim 后的工具名保留，实际 %+v", u.ToolAnnotations)
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
		"annotations 工具不在白名单": func(c *Config) {
			tr := true
			c.Upstreams[0].ToolAnnotations = map[string]UpstreamToolAnnotations{
				"not_in_whitelist": {ReadOnlyHint: &tr},
			}
		},
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

func TestFleetExposeLocalAndDefaultTarget(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }

	t.Run("expose_local=false 且无 hosts 报错", func(t *testing.T) {
		cfg := Default()
		cfg.Auth.Enabled = false
		cfg.Fleet.ExposeLocal = boolPtr(false)
		if err := cfg.Normalize(); err == nil {
			t.Fatal("expose_local=false 且无 hosts 应报错")
		}
	})

	t.Run("expose_local=false 且 default_target=local 报错", func(t *testing.T) {
		cfg := Default()
		cfg.Auth.Enabled = false
		cfg.Fleet.ExposeLocal = boolPtr(false)
		cfg.Fleet.DefaultTarget = "local"
		cfg.Fleet.Hosts = []FleetHost{{Name: "web1"}}
		if err := cfg.Normalize(); err == nil {
			t.Fatal("expose_local=false 时 default_target 不能为 local")
		}
	})

	t.Run("default_target 不在 hosts 中报错", func(t *testing.T) {
		cfg := Default()
		cfg.Auth.Enabled = false
		cfg.Fleet.ExposeLocal = boolPtr(false)
		cfg.Fleet.DefaultTarget = "ghost"
		cfg.Fleet.Hosts = []FleetHost{{Name: "web1"}}
		if err := cfg.Normalize(); err == nil {
			t.Fatal("未知 default_target 应报错")
		}
	})

	t.Run("expose_local=false 时 allowed_targets 含 local 报错", func(t *testing.T) {
		cfg := Default()
		cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x", AllowedTargets: []string{"local"}}}
		cfg.Fleet.ExposeLocal = boolPtr(false)
		cfg.Fleet.Hosts = []FleetHost{{Name: "web1"}}
		if err := cfg.Normalize(); err == nil {
			t.Fatal("expose_local=false 时 allowed_targets 不应接受 local")
		}
	})

	t.Run("expose_local=false + 合法 default_target 通过", func(t *testing.T) {
		cfg := Default()
		cfg.Auth.Enabled = false
		cfg.Fleet.ExposeLocal = boolPtr(false)
		cfg.Fleet.DefaultTarget = "web1"
		cfg.Fleet.Hosts = []FleetHost{{Name: "web1"}}
		if err := cfg.Normalize(); err != nil {
			t.Fatalf("合法配置不应报错: %v", err)
		}
		if cfg.Fleet.LocalExposed() {
			t.Error("LocalExposed 应为 false")
		}
	})
}

func TestFleetLocalName(t *testing.T) {
	t.Run("默认本机名是 local", func(t *testing.T) {
		cfg := Default()
		cfg.Auth.Enabled = false
		cfg.Fleet.Hosts = []FleetHost{{Name: "web1"}}
		if err := cfg.Normalize(); err != nil {
			t.Fatal(err)
		}
		if got := cfg.Fleet.LocalTargetName(); got != "local" {
			t.Errorf("默认本机名应为 local，得到 %q", got)
		}
	})

	t.Run("local 作为别名换成本机名", func(t *testing.T) {
		cfg := Default()
		cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x", AllowedTargets: []string{"local", "web1"}}}
		cfg.Fleet.LocalName = " box "
		cfg.Fleet.DefaultTarget = "local"
		cfg.Fleet.Hosts = []FleetHost{{Name: "web1"}}
		cfg.Policy.TargetDenyPaths = map[string][]string{"local": {"/secret/**"}}
		if err := cfg.Normalize(); err != nil {
			t.Fatal(err)
		}
		if cfg.Fleet.LocalName != "box" || cfg.Fleet.DefaultTarget != "box" {
			t.Errorf("本机名与默认目标应规范为 box: %q %q", cfg.Fleet.LocalName, cfg.Fleet.DefaultTarget)
		}
		if got := cfg.Auth.StaticTokens[0].AllowedTargets; len(got) != 2 || got[0] != "box" || got[1] != "web1" {
			t.Errorf("allowed_targets 里的 local 应换成 box: %v", got)
		}
		if _, ok := cfg.Policy.TargetDenyPaths["local"]; ok || len(cfg.Policy.TargetDenyPaths["box"]) != 1 {
			t.Errorf("target_deny_paths 的 local 应并到 box 下: %v", cfg.Policy.TargetDenyPaths)
		}
		if cfg.Extensions.Skill.Target != "box" || cfg.Extensions.FleetherdTask.Target != "box" {
			t.Errorf("扩展的默认目标应为本机名: %q %q", cfg.Extensions.Skill.Target, cfg.Extensions.FleetherdTask.Target)
		}
	})

	t.Run("远端机器不能占用本机名或 local", func(t *testing.T) {
		for _, name := range []string{"box", "local"} {
			cfg := Default()
			cfg.Auth.Enabled = false
			cfg.Fleet.LocalName = "box"
			cfg.Fleet.Hosts = []FleetHost{{Name: name}}
			if err := cfg.Normalize(); err == nil {
				t.Errorf("hosts 里用 %q 应报错", name)
			}
		}
	})

	t.Run("不暴露本机时 default_target 不能指向本机名", func(t *testing.T) {
		noLocal := false
		cfg := Default()
		cfg.Auth.Enabled = false
		cfg.Fleet.ExposeLocal = &noLocal
		cfg.Fleet.LocalName = "box"
		cfg.Fleet.DefaultTarget = "box"
		cfg.Fleet.Hosts = []FleetHost{{Name: "web1"}}
		if err := cfg.Normalize(); err == nil {
			t.Fatal("expose_local=false 时 default_target 不能是本机")
		}
	})
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

func TestImageURLDefaults(t *testing.T) {
	cfg := Default()
	cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x"}}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	iu := cfg.Tools.FS.ImageURL
	if iu.Enabled || iu.OneTime || iu.TTL != 5*time.Minute || iu.MaxItems != 32 || iu.MaxTotalBytes != 64<<20 {
		t.Errorf("image_url 默认值不对: %+v", iu)
	}

	// 只写 enabled 时，其余字段应补默认值。
	cfg = Default()
	cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x"}}
	cfg.Tools.FS.ImageURL = ImageURLCfg{Enabled: true}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	iu = cfg.Tools.FS.ImageURL
	if !iu.Enabled || iu.TTL != 5*time.Minute || iu.MaxItems != 32 || iu.MaxTotalBytes != 64<<20 {
		t.Errorf("零值字段应补默认值: %+v", iu)
	}
}

func TestImageURLYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	raw := `auth:
  static_tokens: [{name: t, token: x}]
server:
  base_url: https://mcp.example.com
tools:
  fs:
    image_url:
      enabled: true
      ttl: 2m
      one_time: true
      max_items: 4
      max_total_bytes: 1048576
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := ImageURLCfg{Enabled: true, TTL: 2 * time.Minute, OneTime: true, MaxItems: 4, MaxTotalBytes: 1 << 20}
	if cfg.Tools.FS.ImageURL != want {
		t.Errorf("image_url 解析结果不对: %+v", cfg.Tools.FS.ImageURL)
	}
}

func TestImageURLRequiresBaseURL(t *testing.T) {
	cfg := Default()
	cfg.Auth.StaticTokens = []StaticToken{{Name: "t", Token: "x"}}
	cfg.Server.BaseURL = ""
	cfg.Tools.FS.ImageURL.Enabled = true
	err := cfg.Normalize()
	if err == nil {
		t.Fatal("启用 image_url 但 base_url 为空时应报错")
	}
	if !strings.Contains(err.Error(), "tools.fs.image_url") || !strings.Contains(err.Error(), "server.base_url") {
		t.Errorf("错误信息应同时点出 image_url 与 base_url，实际: %v", err)
	}
}
