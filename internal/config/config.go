// Package config 定义 runabout 的配置结构、默认值与加载逻辑。
package config

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
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
	Fleet  Fleet  `yaml:"fleet"`
	Policy Policy `yaml:"policy"`
	Audit  Audit  `yaml:"audit"`
	// Extensions 是默认关闭、按配置启用的可选集成扩展工具（如 skill、fleetherd_task）。
	Extensions Extensions `yaml:"extensions,omitempty"`
	// Upstreams 是可选的上游 MCP 透传配置。空列表表示关闭（默认）。
	Upstreams []Upstream `yaml:"upstreams,omitempty"`
}

// Upstream 是一个上游 MCP 服务（Streamable HTTP 或 stdio 子进程）。启用后，
// 白名单里的工具以 <prefix>_<工具名> 出现在 tools/list，调用原样转发给上游。
// runabout 不关心上游是什么，也不解析它的业务语义。
type Upstream struct {
	// Name 用于日志与错误信息，必须在 upstreams 里唯一。
	Name string `yaml:"name"`
	// URL 是上游的 MCP Streamable HTTP 端点（http 或 https）。
	// 与 Command 二选一。
	URL string `yaml:"url,omitempty"`
	// Command 是 stdio 上游的启动 argv，如 ["node", "/opt/example/server.mjs"]。
	// 与 URL 二选一。
	Command []string `yaml:"command,omitempty"`
	// Env 是额外注入给 stdio 子进程的环境变量（字面值）。
	// 子进程只继承 PATH、HOME、LANG，不继承 runabout 的其他环境变量。
	Env map[string]string `yaml:"env,omitempty"`
	// Workdir 是 stdio 子进程的工作目录，必须是绝对路径。
	Workdir string `yaml:"workdir,omitempty"`
	// TokenEnv 是存放 Bearer 令牌的环境变量名；留空表示不带鉴权。
	// 只用于 HTTP 上游；配置文件里永远不写明文令牌。
	TokenEnv string `yaml:"token_env,omitempty"`
	// Prefix 是暴露给客户端的工具名前缀，最终名字为 <prefix>_<工具名>。
	Prefix string `yaml:"prefix"`
	// Tools 是允许透传的上游工具白名单，必填。白名单之外的工具不会被暴露。
	Tools []string `yaml:"tools"`
	// ToolAnnotations 按上游工具名覆写 MCP ToolAnnotations 提示（只覆盖显式填写的字段）。
	// 用于给不返回 annotations 的上游补齐 readOnlyHint 等提示，避免客户端把只读工具当写操作弹确认。
	ToolAnnotations map[string]UpstreamToolAnnotations `yaml:"tool_annotations,omitempty"`
	// RefreshInterval 是重新拉取工具列表的间隔，默认 10m。
	RefreshInterval time.Duration `yaml:"refresh_interval,omitempty"`
	// Timeout 是单次上游请求（含 tools/call）的超时，默认 60s。
	Timeout time.Duration `yaml:"timeout,omitempty"`
	// Instructions 为 true 时，把上游 initialize 返回的 instructions
	// 追加到 runabout 回复客户端的说明里。默认 false。
	Instructions bool `yaml:"instructions,omitempty"`
}

// UpstreamToolAnnotations 用于在配置里按工具覆写 MCP ToolAnnotations 提示。
// 未填写的字段保持 nil/空字符串，合并时保留上游原值。
type UpstreamToolAnnotations struct {
	Title           string `yaml:"title,omitempty"`
	ReadOnlyHint    *bool  `yaml:"read_only,omitempty"`
	DestructiveHint *bool  `yaml:"destructive,omitempty"`
	IdempotentHint  *bool  `yaml:"idempotent,omitempty"`
	OpenWorldHint   *bool  `yaml:"open_world,omitempty"`
}

// Extensions 控制可选扩展工具。所有扩展默认关闭，不改变默认工具列表。
type Extensions struct {
	Skill         SkillExtCfg         `yaml:"skill,omitempty"`
	FleetherdTask FleetherdTaskExtCfg `yaml:"fleetherd_task,omitempty"`
}

// SkillExtCfg 配置 skill 扩展工具（桥接 fleetherd 的 BM25 技能搜索与读取）。
type SkillExtCfg struct {
	Enabled        bool          `yaml:"enabled"`
	Target         string        `yaml:"target,omitempty"`
	Binary         string        `yaml:"binary,omitempty"`
	Timeout        time.Duration `yaml:"timeout,omitempty"`
	MaxOutputBytes int           `yaml:"max_output_bytes,omitempty"`
}

// FleetherdTaskExtCfg 配置 fleetherd_task 扩展工具（受控转发 task-* 子命令）。
type FleetherdTaskExtCfg struct {
	Enabled           bool          `yaml:"enabled"`
	Target            string        `yaml:"target,omitempty"`
	Binary            string        `yaml:"binary,omitempty"`
	WebBridgeDir      string        `yaml:"web_bridge_dir,omitempty"`
	WebUIBaseURL      string        `yaml:"web_ui_base_url,omitempty"`
	EmptyTopicCommand []string      `yaml:"empty_topic_command,omitempty"`
	Timeout           time.Duration `yaml:"timeout,omitempty"`
	MaxOutputBytes    int           `yaml:"max_output_bytes,omitempty"`
}

// Fleet 是可选的机群配置。不写 hosts 时保持单机模式：工具里不出现 target 参数。
type Fleet struct {
	// ExposeLocal 控制是否把 runabout 所在的本机（local）作为可操作目标暴露给工具。
	// 留空默认 true（兼容单机模式与旧配置）；设为 false 时要求至少配置一台远端主机，
	// local 不会出现在 target 枚举与 servers 列表中，纯远端网关模式也不依赖本机 bash。
	ExposeLocal *bool `yaml:"expose_local,omitempty"`
	// DefaultTarget 指定省略 target 参数时的默认目标机器。
	// 留空时：ExposeLocal 为 true 则默认 local；ExposeLocal 为 false 则 target 为必填参数。
	DefaultTarget string `yaml:"default_target,omitempty"`
	// SSHCommand 是远端调用的前缀，后面依次追加 <主机> <命令>。
	SSHCommand []string `yaml:"ssh_command"`
	// ProbeTimeout 是 servers 总览里单台机器的探测超时。
	ProbeTimeout time.Duration `yaml:"probe_timeout"`
	// ApprovalCheck 用来判断需审批的机器当前是否已授权：{host} 替换成机器名，
	// 退出码 0 表示已授权。它只查询本地状态，不发起申请。
	ApprovalCheck string      `yaml:"approval_check"`
	Hosts         []FleetHost `yaml:"hosts"`
}

// LocalExposed 报告是否暴露本机（local）作为可操作目标。未配置时默认 true。
func (f Fleet) LocalExposed() bool {
	if f.ExposeLocal == nil {
		return true
	}
	return *f.ExposeLocal
}

// FleetHost 是一台远端机器。
type FleetHost struct {
	// Name 是机器名，同时也是 ssh 别名；别名不同时用 SSH 指定。
	Name string `yaml:"name"`
	Desc string `yaml:"desc"`
	SSH  string `yaml:"ssh"`
	// Workdir 是这台机器的默认工作目录。留空表示登录用户的家目录（~）。
	Workdir string `yaml:"workdir"`
	// Approval 表示连接这台机器需要人工审批。runabout 不实现审批，
	// 它只影响工具描述里的提示和 servers 总览是否主动去连。
	Approval bool `yaml:"approval"`
	// DenyPaths 追加仅针对这台机器生效的敏感路径黑名单（glob，支持 ** 与 ~）。
	DenyPaths []string `yaml:"deny_paths,omitempty"`
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
	// ClientPolicies 按 OAuth client_id 或 client_name 限制可访问的 target 白名单。
	ClientPolicies []ClientPolicy `yaml:"client_policies,omitempty"`
	// AllowUnauthenticatedInitialize 放行不带令牌的 initialize 握手（仅它一个）。
	//
	// 有些客户端在"验证这个 URL 是不是 MCP 服务器"的阶段会先裸发一次 initialize，
	// 拿到 401 就直接判定无效，根本不会去读 WWW-Authenticate 里的 OAuth 入口
	// —— Gemini 的自定义连接器实测就是这样（User-Agent: Google，服务端直连，
	// 不带 Origin 也不带 Authorization）。打开后它才走得到"下一步"那个授权页。
	//
	// 代价：serverInfo（名字、版本）与 instructions 文本对匿名访问可见。
	// 工具清单不受影响 —— tools/list 与 tools/call 仍然要求 Bearer。
	AllowUnauthenticatedInitialize bool `yaml:"allow_unauthenticated_initialize"`
}

type StaticToken struct {
	Name           string   `yaml:"name"`
	Token          string   `yaml:"token"`
	AllowedTargets []string `yaml:"allowed_targets,omitempty"`
}

// ClientPolicy 为特定 OAuth 客户端配置 target 访问白名单。
type ClientPolicy struct {
	ClientID       string   `yaml:"client_id,omitempty"`
	ClientName     string   `yaml:"client_name,omitempty"`
	AllowedTargets []string `yaml:"allowed_targets,omitempty"`
}

type Tools struct {
	// Disabled 列出要下线的工具名。
	Disabled []string  `yaml:"disabled"`
	Shell    ShellCfg  `yaml:"shell"`
	FS       FSCfg     `yaml:"fs"`
	Search   SearchCfg `yaml:"search"`
	Query    QueryCfg  `yaml:"query"`
}

// QueryCfg 控制只读查询工具。默认关闭，打开后只接受 AST 判定为只读的命令。
type QueryCfg struct {
	Enabled bool `yaml:"enabled"`
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
	// MaxImageBytes 是处理后图片的体积上限，超过会降质量或继续缩小。
	MaxImageBytes int `yaml:"max_image_bytes"`
	// MaxImageSourceBytes 是图片原始文件的大小上限。图片不套用 max_read_bytes，
	// 否则一张几 MB 的截图会在压缩前就被拒绝。
	MaxImageSourceBytes int `yaml:"max_image_source_bytes"`
	// ImageMaxEdge 是读图时自动缩放的长边上限（像素）。
	ImageMaxEdge int `yaml:"image_max_edge"`
	// MaxImagePixels 是解码前的像素数上限，用来挡住解压炸弹。
	MaxImagePixels int `yaml:"max_image_pixels"`
	// MaxImageConcurrency 限制同时处理的图片张数。
	MaxImageConcurrency int `yaml:"max_image_concurrency"`
	// Vision 是读图后的可选视觉描述。BaseURL 与 Model 都非空才启用。
	Vision VisionCfg `yaml:"vision"`
}

// VisionCfg 指向一个 OpenAI 兼容的 /chat/completions 接口。
type VisionCfg struct {
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
	// APIKeyEnv 是存放 API key 的环境变量名；留空表示接口不需要鉴权。
	APIKeyEnv string        `yaml:"api_key_env"`
	Timeout   time.Duration `yaml:"timeout"`
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
	// DenyPaths 全局追加的敏感路径黑名单（glob，支持 ** 与 ~），同时作用于读写与 shell 检查，
	// 与内置规则做并集合并，不会覆盖默认黑名单。
	DenyPaths []string `yaml:"deny_paths,omitempty"`
	// TargetDenyPaths 按 target 机器名追加的敏感路径黑名单（键为 target 名，如 uniagent）。
	TargetDenyPaths map[string][]string `yaml:"target_deny_paths,omitempty"`
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
				MaxReadBytes:        4 << 20,
				MaxLineChars:        2000,
				DefaultLines:        2000,
				MaxWriteBytes:       8 << 20,
				MaxImageBytes:       1536 << 10,
				MaxImageSourceBytes: 32 << 20,
				ImageMaxEdge:        1568,
				MaxImagePixels:      30_000_000,
				MaxImageConcurrency: 2,
				Vision:              VisionCfg{Timeout: 30 * time.Second},
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
		Fleet: Fleet{
			SSHCommand:   []string{"ssh", "-o", "BatchMode=yes"},
			ProbeTimeout: 10 * time.Second,
		},
		Policy: Policy{
			ConfirmTTL: 5 * time.Minute,
			WriteDenyPaths: []string{
				"/proc/**", "/sys/**", "/dev/**", "/boot/**", "/run/**",
				"/etc/shadow", "/etc/gshadow", "/etc/sudoers", "/etc/sudoers.d/**",
				"**/.ssh/authorized_keys",
			},
			ReadDenyPaths: []string{
				"/etc/shadow", "/etc/gshadow",
				"**/.ssh/id_*", "**/.ssh/*_rsa", "**/.ssh/*_ed25519", "**/.ssh/*_ecdsa",
				"**/.ssh/igithub", "**/.ssh/gcp_key",
				"**/.aws/credentials", "**/.config/gh/hosts.yml", "**/.docker/config.json",
				"**/.kube/config", "**/.netrc",
				"**/.fleetherd/web-bridge/**",
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
	if v := os.Getenv("RB_ALLOW_ANON_INITIALIZE"); v == "1" || v == "true" {
		cfg.Auth.AllowUnauthenticatedInitialize = true
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
	if c.Tools.FS.MaxImageSourceBytes <= 0 {
		c.Tools.FS.MaxImageSourceBytes = 32 << 20
	}
	if c.Tools.FS.ImageMaxEdge <= 0 {
		c.Tools.FS.ImageMaxEdge = 1568
	}
	if c.Tools.FS.MaxImagePixels <= 0 {
		c.Tools.FS.MaxImagePixels = 30_000_000
	}
	if c.Tools.FS.MaxImageConcurrency <= 0 {
		c.Tools.FS.MaxImageConcurrency = 2
	}
	if c.Tools.FS.Vision.Timeout <= 0 {
		c.Tools.FS.Vision.Timeout = 30 * time.Second
	}
	if c.Audit.Enabled && c.Audit.File == "" {
		c.Audit.File = filepath.Join(c.Server.DataDir, "audit.jsonl")
	}
	if err := c.normalizeFleet(); err != nil {
		return err
	}
	if err := c.normalizeAuthPolicies(); err != nil {
		return err
	}
	if err := c.normalizeExtensions(); err != nil {
		return err
	}
	if err := c.normalizeUpstreams(); err != nil {
		return err
	}
	// 服务自身的数据目录永远不允许被文件工具改写，否则 agent 可以自签令牌。
	c.Policy.WriteDenyPaths = append(c.Policy.WriteDenyPaths,
		c.Server.DataDir, filepath.Join(c.Server.DataDir, "**"))
	c.Policy.ReadDenyPaths = append(c.Policy.ReadDenyPaths, filepath.Join(c.Server.DataDir, "**"))
	return nil
}

// normalizeFleet 补齐机群默认值并校验机器名。
func (c *Config) normalizeFleet() error {
	f := &c.Fleet
	if len(f.SSHCommand) == 0 {
		f.SSHCommand = []string{"ssh", "-o", "BatchMode=yes"}
	}
	if f.ProbeTimeout <= 0 {
		f.ProbeTimeout = 10 * time.Second
	}
	if !f.LocalExposed() && len(f.Hosts) == 0 {
		return fmt.Errorf("fleet.expose_local 为 false 时必须至少配置一台 fleet.hosts")
	}
	seen := map[string]bool{"local": true}
	validTargets := map[string]bool{}
	if f.LocalExposed() {
		validTargets["local"] = true
	}
	for i := range f.Hosts {
		h := &f.Hosts[i]
		h.Name = strings.TrimSpace(h.Name)
		if h.Name == "" {
			return fmt.Errorf("fleet.hosts[%d].name 不能为空", i)
		}
		if h.Name == "local" {
			return fmt.Errorf("fleet.hosts 里不能用保留名 %q", h.Name)
		}
		if seen[h.Name] {
			return fmt.Errorf("fleet.hosts 里的机器名 %q 重复", h.Name)
		}
		seen[h.Name] = true
		validTargets[h.Name] = true
		h.Workdir = strings.TrimSpace(h.Workdir)
		if h.Workdir != "" && !validRemoteWorkdir(h.Workdir) {
			return fmt.Errorf("fleet.hosts[%d].workdir 必须是绝对路径、~ 或 ~/...，当前为 %q", i, h.Workdir)
		}
		if strings.TrimSpace(h.SSH) == "" {
			h.SSH = h.Name
		}
		if len(h.DenyPaths) > 0 {
			if c.Policy.TargetDenyPaths == nil {
				c.Policy.TargetDenyPaths = map[string][]string{}
			}
			c.Policy.TargetDenyPaths[h.Name] = append(c.Policy.TargetDenyPaths[h.Name], h.DenyPaths...)
		}
	}
	f.DefaultTarget = strings.TrimSpace(f.DefaultTarget)
	if f.DefaultTarget != "" {
		if f.DefaultTarget == "local" && !f.LocalExposed() {
			return fmt.Errorf("fleet.expose_local 为 false 时不能将 fleet.default_target 设为 %q", f.DefaultTarget)
		}
		if !validTargets[f.DefaultTarget] {
			return fmt.Errorf("fleet.default_target %q 不在可用目标列表中", f.DefaultTarget)
		}
	}
	return nil
}

// normalizeAuthPolicies 清理并校验静态令牌与 OAuth 客户端的 allowed_targets 白名单。
func (c *Config) normalizeAuthPolicies() error {
	known := map[string]bool{}
	if c.Fleet.LocalExposed() {
		known["local"] = true
	}
	for _, h := range c.Fleet.Hosts {
		known[h.Name] = true
	}
	cleanTargets := func(label string, list []string) ([]string, error) {
		if len(list) == 0 {
			return nil, nil
		}
		out := make([]string, 0, len(list))
		seen := map[string]bool{}
		for _, raw := range list {
			t := strings.TrimSpace(raw)
			if t == "" {
				continue
			}
			if !known[t] {
				return nil, fmt.Errorf("%s.allowed_targets 包含未知 target %q", label, t)
			}
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
		return out, nil
	}
	for i := range c.Auth.StaticTokens {
		cleaned, err := cleanTargets(fmt.Sprintf("auth.static_tokens[%d]", i), c.Auth.StaticTokens[i].AllowedTargets)
		if err != nil {
			return err
		}
		c.Auth.StaticTokens[i].AllowedTargets = cleaned
	}
	for i := range c.Auth.ClientPolicies {
		cp := &c.Auth.ClientPolicies[i]
		cp.ClientID = strings.TrimSpace(cp.ClientID)
		cp.ClientName = strings.TrimSpace(cp.ClientName)
		if cp.ClientID == "" && cp.ClientName == "" {
			return fmt.Errorf("auth.client_policies[%d] 至少要指定 client_id 或 client_name", i)
		}
		cleaned, err := cleanTargets(fmt.Sprintf("auth.client_policies[%d]", i), cp.AllowedTargets)
		if err != nil {
			return err
		}
		cp.AllowedTargets = cleaned
	}
	return nil
}

// normalizeExtensions 补齐扩展工具默认值，并在启用时校验目标机器与安全黑名单。
func (c *Config) normalizeExtensions() error {
	known := map[string]bool{"local": true}
	for _, h := range c.Fleet.Hosts {
		known[h.Name] = true
	}

	sk := &c.Extensions.Skill
	sk.Target = strings.TrimSpace(sk.Target)
	if sk.Target == "" {
		sk.Target = "local"
	}
	sk.Binary = strings.TrimSpace(sk.Binary)
	if sk.Binary == "" {
		sk.Binary = "fleetherd"
	}
	if sk.Timeout <= 0 {
		sk.Timeout = 30 * time.Second
	}
	if sk.MaxOutputBytes <= 0 {
		sk.MaxOutputBytes = 200_000
	}
	if sk.Enabled && !known[sk.Target] {
		return fmt.Errorf("extensions.skill.target 指向未知机器 %q", sk.Target)
	}

	ft := &c.Extensions.FleetherdTask
	ft.Target = strings.TrimSpace(ft.Target)
	if ft.Target == "" {
		ft.Target = "local"
	}
	ft.Binary = strings.TrimSpace(ft.Binary)
	if ft.Binary == "" {
		ft.Binary = "fleetherd"
	}
	ft.WebBridgeDir = strings.TrimSpace(ft.WebBridgeDir)
	if ft.WebBridgeDir == "" {
		ft.WebBridgeDir = "~/.fleetherd/web-bridge"
	}
	ft.WebUIBaseURL = strings.TrimRight(strings.TrimSpace(ft.WebUIBaseURL), "/")
	if ft.Timeout <= 0 {
		ft.Timeout = 60 * time.Second
	}
	if ft.MaxOutputBytes <= 0 {
		ft.MaxOutputBytes = 200_000
	}
	if ft.Enabled {
		if !known[ft.Target] {
			return fmt.Errorf("extensions.fleetherd_task.target 指向未知机器 %q", ft.Target)
		}
		// 安全不变量：启用 fleetherd_task 时强制将 web_bridge_dir 纳入全局 deny_paths，
		// 防止模型通过 read / search / apply_patch / shell 重定向直读明文 token。
		pat := strings.TrimRight(ft.WebBridgeDir, "/") + "/**"
		c.Policy.DenyPaths = append(c.Policy.DenyPaths, pat)
	}
	return nil
}

// upstreamNameRE 约束上游名与工具前缀：都只允许字母、数字、下划线、连字符。
var (
	upstreamNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	upstreamToolRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	envNameRE      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// reservedToolNames 是 runabout 自己实现过的工具名。上游工具加了前缀后
// 仍可能撞上（例如 prefix=shell、tool=output），启动前直接拒绝。
var reservedToolNames = map[string]bool{
	"shell": true, "shell_output": true, "shell_kill": true, "shell_list": true,
	"read": true, "write": true, "apply_patch": true, "search": true, "glob": true,
	"list_dir": true, "servers": true, "query": true, "skill": true, "fleetherd_task": true,
}

// normalizeUpstreams 校验上游 MCP 透传配置并补齐默认值。
func (c *Config) normalizeUpstreams() error {
	seenName := map[string]bool{}
	// exposed 记录已占用的对外工具名，值为上游名，便于报错时定位。
	exposed := map[string]string{}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		u.Name = strings.TrimSpace(u.Name)
		if u.Name == "" {
			return fmt.Errorf("upstreams[%d].name 不能为空", i)
		}
		if !upstreamNameRE.MatchString(u.Name) {
			return fmt.Errorf("upstreams[%d].name 只能包含字母、数字、下划线与连字符，当前为 %q", i, u.Name)
		}
		if seenName[u.Name] {
			return fmt.Errorf("upstreams 里的名字 %q 重复", u.Name)
		}
		seenName[u.Name] = true

		u.URL = strings.TrimSpace(u.URL)
		u.Workdir = strings.TrimSpace(u.Workdir)
		switch {
		case u.URL != "" && len(u.Command) > 0:
			return fmt.Errorf("upstreams[%d] 的 url 与 command 只能二选一", i)
		case u.URL == "" && len(u.Command) == 0:
			return fmt.Errorf("upstreams[%d] 必须配置 url（HTTP 上游）或 command（stdio 上游）", i)
		}
		if len(u.Command) > 0 {
			u.Command[0] = strings.TrimSpace(u.Command[0])
			if u.Command[0] == "" {
				return fmt.Errorf("upstreams[%d].command 的第一项不能为空", i)
			}
			if strings.TrimSpace(u.TokenEnv) != "" {
				return fmt.Errorf("upstreams[%d].token_env 只用于 HTTP 上游，stdio 上游请用 env 传凭据", i)
			}
			if u.Workdir != "" && !filepath.IsAbs(u.Workdir) {
				return fmt.Errorf("upstreams[%d].workdir 必须是绝对路径，当前为 %q", i, u.Workdir)
			}
			for k := range u.Env {
				if !envNameRE.MatchString(k) {
					return fmt.Errorf("upstreams[%d].env 里的变量名 %q 不合法", i, k)
				}
			}
		} else {
			parsed, err := url.Parse(u.URL)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return fmt.Errorf("upstreams[%d].url 必须是 http(s) 绝对地址，当前为 %q", i, redactURL(u.URL))
			}
			if parsed.User != nil {
				return fmt.Errorf("upstreams[%d].url 里不要写凭据，令牌请用 token_env", i)
			}
			// query 与 fragment 不参与 MCP 端点定位，且容易夹带凭据，直接拒绝。
			if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
				return fmt.Errorf("upstreams[%d].url 不能带 query 或 fragment，当前为 %q", i, redactURL(u.URL))
			}
			if len(u.Env) > 0 {
				return fmt.Errorf("upstreams[%d].env 只用于 stdio 上游（配置 command）", i)
			}
			if u.Workdir != "" {
				return fmt.Errorf("upstreams[%d].workdir 只用于 stdio 上游（配置 command）", i)
			}
		}

		u.Prefix = strings.TrimSpace(u.Prefix)
		if !upstreamNameRE.MatchString(u.Prefix) {
			return fmt.Errorf("upstreams[%d].prefix 只能包含字母、数字、下划线与连字符，当前为 %q", i, u.Prefix)
		}
		u.TokenEnv = strings.TrimSpace(u.TokenEnv)
		if u.TokenEnv != "" && !envNameRE.MatchString(u.TokenEnv) {
			return fmt.Errorf("upstreams[%d].token_env 不是合法的环境变量名: %q", i, u.TokenEnv)
		}

		if len(u.Tools) == 0 {
			return fmt.Errorf("upstreams[%d].tools 白名单不能为空（按需列出要透传的上游工具）", i)
		}
		cleaned := make([]string, 0, len(u.Tools))
		seenTool := map[string]bool{}
		for _, raw := range u.Tools {
			tool := strings.TrimSpace(raw)
			if !upstreamToolRE.MatchString(tool) {
				return fmt.Errorf("upstreams[%d].tools 里的工具名 %q 不合法", i, raw)
			}
			if seenTool[tool] {
				continue
			}
			seenTool[tool] = true
			full := u.Prefix + "_" + tool
			if len(full) > 128 {
				return fmt.Errorf("upstreams[%d] 暴露的工具名 %q 超过 128 个字符", i, full)
			}
			if reservedToolNames[full] {
				return fmt.Errorf("upstreams[%d] 暴露的工具名 %q 与内置工具重名", i, full)
			}
			if owner, ok := exposed[full]; ok {
				return fmt.Errorf("上游工具名 %q 与 %s 重复", full, owner)
			}
			exposed[full] = u.Name
			cleaned = append(cleaned, tool)
		}
		u.Tools = cleaned

		if len(u.ToolAnnotations) > 0 {
			cleanedAnn := make(map[string]UpstreamToolAnnotations, len(u.ToolAnnotations))
			for rawName, ann := range u.ToolAnnotations {
				tool := strings.TrimSpace(rawName)
				if !seenTool[tool] {
					return fmt.Errorf("upstreams[%d].tool_annotations 里的工具名 %q 不在 tools 白名单中", i, rawName)
				}
				if _, dup := cleanedAnn[tool]; dup {
					return fmt.Errorf("upstreams[%d].tool_annotations 里的工具名 %q 重复", i, tool)
				}
				ann.Title = strings.TrimSpace(ann.Title)
				cleanedAnn[tool] = ann
			}
			u.ToolAnnotations = cleanedAnn
		}

		if u.RefreshInterval <= 0 {
			u.RefreshInterval = 10 * time.Minute
		}
		if u.Timeout <= 0 {
			u.Timeout = 60 * time.Second
		}
	}
	return nil
}

// redactURL 只保留 scheme、host 与 path，避免错误信息回显 query 里的凭据。
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid url)"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	u.ForceQuery = false
	return u.String()
}

// validRemoteWorkdir 只接受远端可判定的路径：绝对路径、~ 或 ~/...。
// 相对路径和 ~user 形式会绕过绝对路径黑名单，直接拒绝。
func validRemoteWorkdir(p string) bool {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return true
	}
	if strings.HasPrefix(p, "~") {
		return false
	}
	return path.IsAbs(p)
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
