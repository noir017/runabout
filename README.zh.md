# runabout

> 一个小巧的、自托管的 MCP 网关，让 Agent 能够受控地访问 Linux 服务器。

[English](README.md) · [中文](README.zh.md)

runabout 将 Linux 服务器变成一个 **远程 MCP 端点**，让 ChatGPT、Claude Code 等 Agent 可以通过 HTTPS 使用它。它将 Streamable HTTP、OAuth 2.1、精简的工具集合、命令策略、路径保护、确认令牌和审计日志整合在一个自托管二进制程序中。

它的目标非常简单：**让 Agent 能够操作服务器，同时又不把整个服务器变成一个不受控制的 Shell。**

```text
ChatGPT / Claude Code / 其他 MCP 客户端
                    │
                    │ HTTPS + OAuth 2.1
                    ▼
              反向代理 / Tunnel
                    │
                    ▼
                 runabout
          ┌─────────┼─────────┐
          │  策略   │  审计   │
          └─────────┼─────────┘
                    │
                    ▼
                 Linux 主机
```

## 为什么选择 runabout？

大多数远程 MCP Server 暴露的是某个应用的 API，而 runabout 暴露的是**机器本身**，但接口被刻意限制在一个很小的范围内。

典型用途：

- **服务器运维** — 查看服务、日志、进程、软件包、网络状态和系统配置。
- **开发** — 查看代码仓库、搜索代码、编辑文件、运行测试和构建项目。
- **部署** — 拉取代码、构建镜像、重启服务以及排查部署失败。
- **家庭实验室 / 自托管** — 通过 Agent 操作 Docker 主机、NAS、VPS 或其他 Linux 服务器。
- **聊天式服务器管理** — 将 ChatGPT 连接到私有服务器，而不需要直接把 SSH 暴露到公网。

最重要的安全边界是进程身份。如果 runabout 以 `root` 运行，Agent 实际上就拥有 root 权限。建议使用专用用户、容器、明确的文件系统挂载以及范围尽可能小的 sudo 规则。

## 7 个核心工具

默认 MCP 工具面板刻意限制为 **7 个工具**。源码中仍可能保留一些旧实现，但它们默认不会暴露给 MCP 客户端。

| 工具 | 用途 |
|---|---|
| `shell` | 在配置的环境中执行 Bash 命令，支持前台和后台任务。 |
| `shell_output` | 读取后台任务捕获的 stdout/stderr。 |
| `shell_kill` | 向后台任务发送信号并终止任务。 |
| `read` | 读取文件，提供行号并限制输出大小。 |
| `apply_patch` | 使用精确的上下文补丁修改文件，而不是整体覆盖文件。 |
| `search` | 使用正则表达式搜索文件内容。 |
| `glob` | 根据 glob 模式查找文件，并限制结果数量。 |

这 7 个工具有意覆盖 Agent 最核心的工作流：

```text
发现 → 检查 → 编辑 → 执行 → 观察
 │      │      │      │      │
 glob   read   patch  shell  output
 search
```

`write`、`list_dir` 和 `shell_list` 在源码中仍可能保留，用于实现内部功能或其他场景，但默认不会出现在 MCP 工具列表中。这样可以减少模型的选择空间，并避免多个工具提供重复能力。

## 安全模型

runabout 的设计目标是运行在反向代理或 Tunnel 后面，而不是直接把自己的 HTTP 服务暴露到公网。

安全措施分为多个层次：

1. **边缘 HTTPS** — TLS 由 Cloudflare Tunnel、Nginx、Caddy 或其他入口层负责终止。
2. **OAuth 2.1 + PKCE** — 远程 MCP 客户端通过内置授权服务器进行认证。
3. **进程身份** — 命令使用运行 runabout 的 Unix 用户身份执行。
4. **Shell 策略** — 可以拒绝危险命令模式，或要求确认后执行。
5. **路径保护** — 文件操作可以限制敏感路径的读取和写入。
6. **确认令牌** — 需要审批的操作可以通过明确的确认令牌执行。
7. **审计日志** — 可以将工具调用记录为 JSONL 日志。

runabout 不依赖外部身份提供商。客户端注册（RFC 7591）、授权码 + PKCE、Token 签发和 Token 轮换均由本地服务处理。

**除非你明确希望赋予 Agent root 权限，否则不要以 root 身份运行 runabout。**

## 环境要求

- Go **1.25+**，如果需要从源码构建
- **Linux**
- 一个供 ChatGPT 等远程 MCP 客户端访问的公网 **HTTPS** URL

runabout 二进制程序默认监听普通 HTTP。TLS 和公网域名应由前置的反向代理或 Tunnel 负责。

## 构建

```bash
make build     # 构建 bin/runabout
make test      # 单元测试 + E2E 测试
make docker    # 构建 Docker 镜像
```

其他常用目标：

```bash
make fmt
make vet
make check
make test-policy
make clean
```

`make docker` 会构建基于 Debian-slim 的镜像，并包含常用的服务器管理和开发工具。支持 amd64 和 arm64。

## 部署

runabout 默认监听：

```text
127.0.0.1:8484
```

`server.base_url` 必须填写**公网 Origin**，并且必须与 MCP 客户端实际使用的 URL 完全一致。OAuth 元数据和重定向验证都依赖这个值。

### 1. 配置

```bash
cp configs/config.example.yaml configs/config.yaml
```

生成密码哈希：

```bash
./bin/runabout hash-password
```

将生成的 bcrypt 哈希写入：

```yaml
auth:
  users:
    - username: admin
      password_hash: "$2a$..."
```

设置公网 URL：

```yaml
server:
  base_url: "https://your.domain"
```

不要添加结尾的 `/`。

环境变量会覆盖 YAML 配置：

```text
RB_LISTEN
RB_BASE_URL
RB_DATA_DIR
RB_USER
RB_PASSWORD_HASH
RB_STATIC_TOKEN
RB_AUTH_DISABLED
RB_LOG_LEVEL
RB_LOG_FORMAT
```

如果没有通过 `-c` 指定配置文件，可以使用 `RB_CONFIG` 选择配置文件。

### 2. Docker

```bash
cd deploy

cp .env.example .env
cp ../configs/config.example.yaml config.yaml

docker compose up -d --build
```

至少需要配置 `RB_BASE_URL`。

容器默认使用非 root 用户 `agent` 运行。`AGENT_UID` 和 `AGENT_GID` 通常应该与宿主机绑定挂载文件的所有者一致。

默认容器配置提供容器内部的免密码 sudo，方便 Agent 安装缺失的软件包。但这也意味着 Agent 可以在**容器内部**获得 root 权限。

如果希望使用更严格的配置：

```env
SUDO_NOPASSWD=false
```

Ingress 网络应该与 Tunnel 或反向代理共享。

对于 Cloudflare Tunnel：

```bash
docker network create cloudflared
docker network connect cloudflared <cloudflared-container>
```

将公网域名指向：

```text
http://runabout:8484
```

不要从其他容器中使用 `127.0.0.1:8484`，因为这里的 `127.0.0.1` 指向的是 Tunnel 容器自身。

OAuth 状态和审计数据存储在 `mcp-data` Volume 中：

```text
/var/lib/runabout
```

不要将该目录绑定挂载到 Agent 的工作区中。

### 3. 裸机 / systemd

仓库提供了 systemd Unit：

```text
deploy/runabout.service
```

安装二进制到：

```text
/usr/local/bin/runabout
```

配置文件到：

```text
/etc/runabout/config.yaml
```

创建专用的 `agent` 用户，然后启动服务：

```bash
systemctl daemon-reload
systemctl enable --now runabout
```

需要额外权限时，应通过 sudoers 授权，而不是让服务本身以 root 身份运行。

### 4. 公网 HTTPS

ChatGPT 等远程 MCP 客户端要求使用 HTTPS。

#### Cloudflare Tunnel

可以通过 Tunnel 暴露 runabout，而无需开放服务器入站端口：

```bash
cloudflared tunnel --url http://127.0.0.1:8484
```

#### Nginx

示例配置：

```nginx
location / {
    proxy_pass http://127.0.0.1:8484;
    proxy_http_version 1.1;

    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;

    proxy_buffering off;
    proxy_read_timeout 3600s;
}
```

关闭 buffering 对 Streamable HTTP / SSE 流量很重要，同时较长的 read timeout 可以让长时间运行的 Agent 任务正常完成。

## 使用

启动 runabout：

```bash
./bin/runabout serve -c configs/config.yaml
```

根路径：

```text
https://your.domain/
```

会提供一个简短的 Landing Page，其中包含 MCP Endpoint 和基本连接信息。

MCP Endpoint：

```text
https://your.domain/mcp
```

## 客户端

runabout 支持理解远程 MCP 和 OAuth 的客户端。

已验证的客户端包括：

- **ChatGPT Custom Connectors**
- **Claude Code**
- 其他支持 OAuth 的远程 MCP 客户端

Claude Code 示例：

```bash
claude mcp add --transport http
```

然后配置远程 MCP Endpoint：

```text
https://your.domain/mcp
```

默认启用动态客户端注册：

```yaml
auth:
  allow_dynamic_registration: true
```

ChatGPT 依赖动态注册。

如果希望保护注册 Endpoint，可以配置：

```yaml
auth:
  registration_token: "..."
```

对于不使用 OAuth 的客户端，runabout 也支持静态 Bearer Token：

```bash
runabout gen-token
```

然后将生成的 Token 配置到：

```yaml
auth:
  static_tokens:
    - "..."
```

客户端发送：

```http
Authorization: Bearer <token>
```

## 验证部署

runabout 提供 Smoke Test：

```bash
./skills/runabout-deploy/smoke.sh https://your.domain [static-token]
```

该脚本会检查：

- 服务存活状态
- OAuth 元数据
- `401` 身份验证挑战
- CORS Preflight
- MCP Handshake
- `tools/list`

任一步骤失败都会返回非零退出码。

手动检查：

```bash
curl -s https://your.domain/healthz | jq
curl -s https://your.domain/.well-known/oauth-protected-resource | jq
```

## Skills

仓库包含用于部署和故障排查的 Agent Skills：

```text
skills/
├── runabout-deploy
├── runabout-connect
├── runabout-troubleshoot
└── runabout-harden
```

可以将它们链接到：

```text
.claude/skills/
```

详见 [skills/README.md](skills/README.md)。

## CLI

```text
runabout serve [-c config]        启动服务器
runabout hash-password [password] 生成 bcrypt 密码哈希
runabout gen-token                生成随机静态 Token
runabout check-policy 'command'   检查 Shell 策略如何处理命令
runabout version                  显示版本信息
```

`check-policy` 可以从配置文件中加载额外规则。

退出状态：

```text
0  允许
2  拒绝
3  需要确认
```

## 配置

大多数部署实际上只需要修改少量配置。

| 区域 | 配置 |
|---|---|
| 监听 / 公网 URL | `server.listen`、`server.base_url`、`RB_*` |
| 身份认证 | `auth.users`、`auth.session_cookie_ttl` |
| 客户端注册 | `auth.allow_dynamic_registration`、`auth.registration_token` |
| 禁用工具 | `tools.disabled` |
| Shell | `tools.shell.default_workdir`、timeout、`max_background`、`env` |
| 文件访问 | `policy.write_deny_paths`、`policy.read_deny_paths` |
| Shell 策略 | `policy.disabled_shell_rules`、`policy.downgrade_to_confirm`、`policy.extra_shell_rules` |
| 审计 | `audit.enabled`、`audit.log_args`、`audit.file` |
| Source URL | `server.source_url` |

### Shell 策略示例

```yaml
policy:
  extra_shell_rules:
    - id: "no-terraform-destroy"
      action: "confirm"
      reason: "destroys cloud resources"
      hint: "run terraform plan -destroy first"
      command: "terraform"
      arg_regex: '\\bdestroy\\b'
```

规则可以设置为：

```text
deny
confirm
```

修改策略规则后运行：

```bash
make test-policy
```

## 架构

仓库由几个职责明确的组件组成：

```text
cmd/runabout
    CLI

internal/app
    HTTP Server 组装

internal/auth
    OAuth 2.1 和 Bearer 身份认证

internal/mcp
    JSON-RPC 和 Streamable HTTP

internal/tools
    MCP Tool 实现

internal/policy
    Shell 规则、路径保护和确认令牌

internal/audit
    JSONL 审计日志

internal/config
    YAML 配置和校验
```

### 添加工具

实现：

```text
mcp.Handler
```

其中包含：

```text
Definition()
Call()
```

然后通过：

```text
tools.All()
```

进行注册。

默认工具集合应该保持精简。新增工具应该提供现有 7 个工具无法清晰表达的新能力，而不是简单地为已有操作提供另一种实现方式。

### Shell 规则

内置 Shell 规则位于：

```text
builtinCmdRules()
```

每条规则都应该提供明确的原因，以及 Agent 可以据此采取行动的提示。

如果一个规则可以通过：

```text
command + arg_regex
```

表达，优先使用配置中的 `extra_shell_rules`，而不是直接修改源码。

### 配置兼容性

`configs/config.example.yaml` 会在测试中以严格模式解析。修改配置结构时，请确保示例配置与配置 Struct 保持同步。

## 源码提供

runabout 使用 AGPLv3 许可证。

如果你修改 runabout，并通过网络向用户提供修改后的服务，AGPL §13 可能要求这些用户能够获得对应的修改后源码。

设置：

```yaml
server:
  source_url: "https://github.com/yourname/your-runabout"
```

让用户能够访问与当前运行版本对应的源码仓库。

## 兼容性

- MCP 协议：`2025-06-18`
- 同时支持：`2025-03-26`、`2024-11-05`
- Transport：Streamable HTTP
- MCP Endpoint：`POST /mcp`
- SSE：`GET /mcp`
- Session 终止：`DELETE /mcp`

协议和 OAuth 实现均维护在项目源码中。

## 许可证

[GNU AGPL v3 or later](LICENSE)

Copyright (C) 2026 noir017.
