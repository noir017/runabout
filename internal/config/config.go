// Package config 定义 runabout 的配置结构、默认值与加载逻辑。
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultSourceURL 是上游仓库地址，用于满足 AGPL 第 13 条的源码提供义务。
const DefaultSourceURL = "https://github.com/noir017/runabout"

type Config struct {
	// normalized 记录是否已跑过 Normalize，避免派生字段被重复追加。
	normalized bool

	Server Server `yaml:"server"`
	Auth   Auth   `yaml:"auth"`
	Tools  Tools  `yaml:"tools"`
	Policy Policy `yaml:"policy"`
	Audit  Audit  `yaml:"audit"`
}

type Server struct {
	// Listen 监听地址。默认只听回环，由外部反代提供 HTTPS。
	Listen string `yaml:"listen"`
	// BaseURL 对外可访问的根地址（含 scheme，不带尾斜杠）。
	// OAuth 元数据、redirect_uri 校验、资源标识都以它为准，必须与 ChatGPT 里填的地址一致。
	BaseURL string `yaml:"base_url"`
	// TrustProxy 为 true 时信任 X-Forwarded-Proto/Host 来推导请求 URL。
	TrustProxy bool `yaml:"trust_proxy"`
	// AllowedOrigins 是允许的浏览器 Origin 白名单，用于防 DNS rebinding。
	// 空列表表示只拒绝明确不匹配 BaseURL 的 Origin；"*" 表示不校验。
	AllowedOrigins []string      `yaml:"allowed_origins"`
	ReadTimeout    time.Duration `yaml:"read_timeout"`
	WriteTimeout   time.Duration `yaml:"write_timeout"`
	// SessionTTL 是 MCP 会话空闲过期时间。
	SessionTTL time.Duration `yaml:"session_ttl"`
	// StrictSessions 为 true 时，缺少 Mcp-Session-Id 的非初始化请求直接 404。
	// 部分客户端（含 ChatGPT 的某些版本）不回传该头，默认宽松处理。
	StrictSessions bool `yaml:"strict_sessions"`
	// DataDir 存放 OAuth 客户端注册信息与令牌。
	DataDir string `yaml:"data_dir"`
	// SourceURL 是对应源码的获取地址，展示在服务根页面上。
	// 本项目按 AGPL-3.0 授权：如果你改了代码并对外提供服务，
	// 第 13 条要求让使用者能拿到你修改后的源码，请把这里改成你自己的仓库。
	SourceURL string `yaml:"source_url"`
}

type User struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}

type Auth struct {
	// Enabled 为 false 时完全关闭鉴权，仅适合本机调试。
	Enabled bool   `yaml:"enabled"`
	Issuer  string `yaml:"issuer"`
	Users   []User `yaml:"users"`
	// AllowDynamicRegistration 打开 RFC 7591 动态客户端注册，ChatGPT 依赖它自助接入。
	AllowDynamicRegistration bool `yaml:"allow_dynamic_registration"`
	// RegistrationToken 非空时，/register 需要携带该 Bearer 令牌。
	RegistrationToken string        `yaml:"registration_token"`
	AccessTokenTTL    time.Duration `yaml:"access_token_ttl"`
	RefreshTokenTTL   time.Duration `yaml:"refresh_token_ttl"`
	AuthCodeTTL       time.Duration `yaml:"auth_code_ttl"`
	SessionCookieTTL  time.Duration `yaml:"session_cookie_ttl"`
	// StaticTokens 是绕过 OAuth 的长期令牌，方便 curl 调试或别的 agent 直连。
	StaticTokens []StaticToken `yaml:"static_tokens"`
}

type StaticToken struct {
	Name  string `yaml:"name"`
	Token string `yaml:"token"`
}

type Tools struct {
	// Disabled 列出要下线的工具名。
	Disabled []string  `yaml:"disabled"`
	Shell    ShellCfg  `yaml:"shell"`
	FS       FSCfg     `yaml:"fs"`
	Search   SearchCfg `yaml:"search"`
}

type ShellCfg struct {
	Path           string        `yaml:"path"`
	DefaultTimeout time.Duration `yaml:"default_timeout"`
	MaxTimeout     time.Duration `yaml:"max_timeout"`
	DefaultWorkdir string        `yaml:"default_workdir"`
	MaxOutputBytes int           `yaml:"max_output_bytes"`
	// MaxBackground 限制同时存活的后台进程数。
	MaxBackground int `yaml:"max_background"`
	// Env 是注入到每条命令的额外环境变量。
	Env map[string]string `yaml:"env"`
}

type FSCfg struct {
	MaxReadBytes  int `yaml:"max_read_bytes"`
	MaxLineChars  int `yaml:"max_line_chars"`
	DefaultLines  int `yaml:"default_lines"`
	MaxWriteBytes int `yaml:"max_write_bytes"`
	MaxImageBytes int `yaml:"max_image_bytes"`
}

type SearchCfg struct {
	// UseRipgrep 优先调用 rg（尊重 .gitignore、速度快），找不到时回退内置实现。
	UseRipgrep    bool          `yaml:"use_ripgrep"`
	RipgrepPath   string        `yaml:"ripgrep_path"`
	MaxResults    int           `yaml:"max_results"`
	Timeout       time.Duration `yaml:"timeout"`
	MaxFileBytes  int64         `yaml:"max_file_bytes"`
	IgnoreDirs    []string      `yaml:"ignore_dirs"`
	MaxGlobResult int           `yaml:"max_glob_results"`
}

type Policy struct {
	// ConfirmTTL 是二次确认令牌的有效期。
	ConfirmTTL time.Duration `yaml:"confirm_ttl"`
	// WriteDenyPaths 命中即拒绝写入/删除（glob，支持 ** 与 ~）。
	WriteDenyPaths []string `yaml:"write_deny_paths"`
	// ReadDenyPaths 命中即拒绝通过文件工具读取（凭据类文件）。
	ReadDenyPaths []string `yaml:"read_deny_paths"`
	// ExtraShellRules 追加到内置 shell 规则之后。
	ExtraShellRules []ShellRule `yaml:"extra_shell_rules"`
	// DisabledShellRules 按 id 关闭内置规则。
	DisabledShellRules []string `yaml:"disabled_shell_rules"`
	// DowngradeToConfirm 把内置的 deny 规则降级为需要二次确认。
	DowngradeToConfirm []string `yaml:"downgrade_to_confirm"`
}

// ShellRule 是一条自定义 shell 拦截规则。
type ShellRule struct {
	ID       string `yaml:"id"`
	Action   string `yaml:"action"` // deny | confirm
	Reason   string `yaml:"reason"`
	Hint     string `yaml:"hint"`
	Command  string `yaml:"command"`   // 匹配的命令名，空表示任意
	ArgRegex string `yaml:"arg_regex"` // 匹配整条命令文本的正则
}

type Audit struct {
	Enabled bool   `yaml:"enabled"`
	File    string `yaml:"file"`
	// LogArgs 为 false 时只记录参数摘要，不落原文。
	LogArgs bool `yaml:"log_args"`
	MaxArgs int  `yaml:"max_args"`
}

func Default() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		Server: Server{
			Listen:         "127.0.0.1:8484",
			BaseURL:        "http://127.0.0.1:8484",
			TrustProxy:     true,
			AllowedOrigins: []string{"https://chatgpt.com", "https://chat.openai.com", "https://claude.ai"},
			ReadTimeout:    0,
			WriteTimeout:   0,
			SessionTTL:     2 * time.Hour,
			StrictSessions: false,
			DataDir:        filepath.Join(home, ".local", "share", "runabout"),
			SourceURL:      DefaultSourceURL,
		},
		Auth: Auth{
			Enabled:                  true,
			AllowDynamicRegistration: true,
			AccessTokenTTL:           time.Hour,
			RefreshTokenTTL:          30 * 24 * time.Hour,
			AuthCodeTTL:              5 * time.Minute,
			SessionCookieTTL:         12 * time.Hour,
		},
		Tools: Tools{
			Shell: ShellCfg{
				Path:           "/bin/bash",
				DefaultTimeout: 120 * time.Second,
				MaxTimeout:     time.Hour,
				MaxOutputBytes: 200_000,
				MaxBackground:  16,
			},
			FS: FSCfg{
				MaxReadBytes:  4 << 20,
				MaxLineChars:  2000,
				DefaultLines:  2000,
				MaxWriteBytes: 8 << 20,
				MaxImageBytes: 4 << 20,
			},
			Search: SearchCfg{
				UseRipgrep:    true,
				MaxResults:    200,
				Timeout:       60 * time.Second,
				MaxFileBytes:  4 << 20,
				MaxGlobResult: 500,
				IgnoreDirs: []string{".git", "node_modules", "vendor", ".venv", "venv",
					"__pycache__", "dist", "build", "target", ".next", ".cache", ".mypy_cache"},
			},
		},
		Policy: Policy{
			ConfirmTTL: 5 * time.Minute,
			WriteDenyPaths: []string{
				"/proc/**", "/sys/**", "/dev/**", "/boot/**", "/run/**",
				"/etc/shadow", "/etc/gshadow", "/etc/sudoers", "/etc/sudoers.d/**",
				"~/.ssh/authorized_keys",
			},
			ReadDenyPaths: []string{
				"/etc/shadow", "/etc/gshadow",
				"~/.ssh/id_*", "~/.ssh/*_rsa", "~/.ssh/*_ed25519", "~/.ssh/*_ecdsa",
				"~/.ssh/igithub", "~/.ssh/gcp_key",
				"~/.aws/credentials", "~/.config/gh/hosts.yml", "~/.docker/config.json",
				"~/.kube/config", "~/.netrc",
			},
		},
		Audit: Audit{Enabled: true, LogArgs: true, MaxArgs: 4000},
	}
}

// Load 读取 YAML 配置并与默认值合并；path 为空时只用默认值。
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置 %s: %w", path, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("解析配置 %s: %w", path, err)
		}
	}
	applyEnv(cfg)
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnv 允许用环境变量覆盖最常改的几项，方便容器部署。
func applyEnv(cfg *Config) {
	if v := os.Getenv("RB_LISTEN"); v != "" {
		cfg.Server.Listen = v
	}
	if v := os.Getenv("RB_BASE_URL"); v != "" {
		cfg.Server.BaseURL = v
	}
	if v := os.Getenv("RB_DATA_DIR"); v != "" {
		cfg.Server.DataDir = v
	}
	if v := os.Getenv("RB_AUTH_DISABLED"); v == "1" || v == "true" {
		cfg.Auth.Enabled = false
	}
	if u, p := os.Getenv("RB_USER"), os.Getenv("RB_PASSWORD_HASH"); u != "" && p != "" {
		cfg.Auth.Users = append(cfg.Auth.Users, User{Username: u, PasswordHash: p})
	}
	if v := os.Getenv("RB_STATIC_TOKEN"); v != "" {
		cfg.Auth.StaticTokens = append(cfg.Auth.StaticTokens, StaticToken{Name: "env", Token: v})
	}
}

// Normalize 校验配置并填好派生字段。Load 会自动调用；直接在代码里构造
// Config（测试、嵌入使用）时需要自己调一次。重复调用是安全的。
func (c *Config) Normalize() error {
	if c.normalized {
		return nil
	}
	c.normalized = true
	c.Server.BaseURL = strings.TrimRight(c.Server.BaseURL, "/")
	if c.Server.BaseURL == "" {
		return fmt.Errorf("server.base_url 不能为空")
	}
	u, err := url.Parse(c.Server.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("server.base_url 必须是形如 https://host[:port] 的绝对地址，当前为 %q", c.Server.BaseURL)
	}
	if strings.TrimSpace(c.Server.SourceURL) == "" {
		c.Server.SourceURL = DefaultSourceURL
	}
	if c.Auth.Issuer == "" {
		c.Auth.Issuer = c.Server.BaseURL
	}
	c.Auth.Issuer = strings.TrimRight(c.Auth.Issuer, "/")
	if c.Auth.Enabled && len(c.Auth.Users) == 0 && len(c.Auth.StaticTokens) == 0 {
		return fmt.Errorf("auth.enabled 为 true 时必须配置至少一个 auth.users 或 auth.static_tokens；" +
			"用 `runabout hash-password` 生成 password_hash")
	}
	if c.Tools.Shell.Path == "" {
		c.Tools.Shell.Path = "/bin/bash"
	}
	if c.Tools.Shell.DefaultWorkdir == "" {
		if wd, err := os.Getwd(); err == nil {
			c.Tools.Shell.DefaultWorkdir = wd
		} else {
			c.Tools.Shell.DefaultWorkdir = "/"
		}
	}
	if c.Tools.Shell.MaxTimeout < c.Tools.Shell.DefaultTimeout {
		c.Tools.Shell.MaxTimeout = c.Tools.Shell.DefaultTimeout
	}
	if c.Audit.Enabled && c.Audit.File == "" {
		c.Audit.File = filepath.Join(c.Server.DataDir, "audit.jsonl")
	}
	// 服务自身的数据目录永远不允许被文件工具改写，否则 agent 可以自签令牌。
	c.Policy.WriteDenyPaths = append(c.Policy.WriteDenyPaths,
		c.Server.DataDir, filepath.Join(c.Server.DataDir, "**"))
	c.Policy.ReadDenyPaths = append(c.Policy.ReadDenyPaths, filepath.Join(c.Server.DataDir, "**"))
	return nil
}

// IsToolDisabled 报告某工具是否被配置关闭。
func (c *Config) IsToolDisabled(name string) bool {
	for _, d := range c.Tools.Disabled {
		if strings.EqualFold(strings.TrimSpace(d), name) {
			return true
		}
	}
	return false
}
