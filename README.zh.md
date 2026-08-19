# runabout

> 跑上跑下的那位。

[English](README.md) · [中文](README.zh.md)

runabout 是一个 MCP 服务：把一台 Linux 机器的 shell、文件、检索和列目录能力，通过 Streamable HTTP 暴露给能连远程 MCP 的 agent，并自带 OAuth 2.1 授权服务器。

它一开始就是为 **ChatGPT 网页版**准备的——用自定义连接器接到你自己的服务器。接上之后，对话不再只靠你粘贴内容：它可以查这台机器、改配置、跑部署、看日志；每次调用都过策略拦截，并留下审计。

```
ChatGPT  ──HTTPS──>  反代 / 隧道  ──>  runabout  ──>  这台机器
              (OAuth 2.1 + PKCE)            (策略 + 审计)
```

同一端点也适用于 Claude Code（`claude mcp add --transport http`）以及任何支持 remote MCP + OAuth 的客户端。客户端注册（RFC 7591）、授权码 + PKCE、令牌颁发与轮换都由本服务完成，不需要外部 IdP。

## 做什么用

ChatGPT 的自定义连接器只能连 **远程 HTTPS MCP**。家里的机器、VPS、桌下那台盒子本身都不是这种东西，除非前面有一层能说这套协议。runabout 就是这一层：默认只听回环，TLS 交给隧道或反代，模型拿到的身份就是你选定的那个进程身份。

常见场景：

- **个人运维助理。** 让 ChatGPT 查 `systemctl`、跟日志、重启单元、改你本来就信任的那台机器上的配置。
- **Homelab / 自建服务。** 指向跑 compose、笔记或 git 仓库的主机（或挂好卷的容器）。
- **在对话里部署。** 让模型 pull、构建、重启服务，省掉再开一次 SSH。
- **同一条管道上的其他 agent。** 给 curl、CI 或已经会 MCP 的本地编程助手配静态 Bearer 令牌，不必走 ChatGPT 界面。

跑这个服务的身份，就是 agent 拿到的身份。默认给 root 等于把整台机器交出去。建议用专用用户或容器，并且只把助理该碰的目录挂进去。

## 环境要求

- 从源码构建需要 Go **1.25+**
- 仅 **Linux**（用到进程组信号）
- 给 ChatGPT 用时需要公网 **HTTPS** 地址（二进制本身只在回环上提供明文 HTTP）

## 构建

```bash
make build     # 产出 bin/runabout
make test      # 单元测试 + e2e
make docker    # 镜像里带 git、ripgrep、jq、python、编译器等
```

`make docker` 会打上 `runabout:$(git describe …)`。amd64 / arm64 都能直接构建。镜像基于 debian-slim，刻意不用 scratch，好让 agent 有一套能干活的用户空间。

其它目标：`make fmt`、`make vet`、`make check`（fmt + vet + test）、`make test-policy`、`make clean`。

## 部署

服务默认监听 `127.0.0.1:8484`。HTTPS、证书和公网主机名交给外层。`server.base_url` 必须是那个公网 origin，且与你在 ChatGPT 里填的地址 **逐字一致**——OAuth 元数据和回调校验都以它为准。

### 1. 配置和密码

```bash
cp configs/config.example.yaml configs/config.yaml
./bin/runabout hash-password          # 交互输入，输出 bcrypt 哈希
```

把哈希填进 `auth.users[0].password_hash`，并把 `server.base_url` 设成 `https://你的域名`（不要尾斜杠）。这个口令等价于这台机器上的 shell 权限，不要用弱密码。

所有字段都有默认值，只写要改的即可。环境变量优先于配置文件：`RB_LISTEN`、`RB_BASE_URL`、`RB_DATA_DIR`、`RB_USER`、`RB_PASSWORD_HASH`、`RB_STATIC_TOKEN`、`RB_AUTH_DISABLED`、`RB_LOG_LEVEL`、`RB_LOG_FORMAT`。未传 `-c` 时会看 `RB_CONFIG`，再尝试 `/etc/runabout/config.yaml`。

### 2. Docker

```bash
cd deploy
cp .env.example .env                  # 至少填 RB_BASE_URL；AGENT_UID 设成宿主机工作区属主
cp ../configs/config.example.yaml config.yaml
docker compose up -d --build
```

`.env` 和 `config.yaml` 不入库（含域名、密码哈希、令牌）。

容器以非 root 的 `agent` 运行。`AGENT_UID` / `AGENT_GID` 应对齐 bind mount 工作区文件的属主。默认给免密 sudo，方便助理自己 `apt install`——容器内提权等于容器内 root，边界仍是容器。不想要就把 `SUDO_NOPASSWD=false` 后重建，此时可以打开 compose 里注释着的 `no-new-privileges`（它和免密 sudo 不能同时开）。

compose 默认接入一张 external 入站网络（名字默认 `cloudflared`），好让隧道或反代按容器名访问。先建好：

```bash
docker network create cloudflared
docker network connect cloudflared <cloudflared 容器名>
```

隧道的公网主机名应指向 `http://runabout:8484`，不要填 `127.0.0.1:8484`（那是隧道容器自己的回环）。不用共享入站网络的话，把 `ingress` 从 `services.mcp.networks` 和 `networks:` 里一起删掉。

OAuth 状态和审计日志在 `mcp-data` 卷（`/var/lib/runabout`）。不要把这个目录 bind mount 进工作区，否则 agent 能改令牌库。

### 3. 裸机

`deploy/runabout.service` 是 systemd 单元。把二进制放到 `/usr/local/bin/runabout`，配置放到 `/etc/runabout/config.yaml`，建一个专用 `agent` 用户，然后：

```bash
systemctl daemon-reload
systemctl enable --now runabout
```

需要的权限用 sudoers 精确授予，不要用 root 跑这个单元。单元开了 `NoNewPrivileges=true`；不要再开 `ProtectHome` / `ProtectSystem=strict`，否则助理没法干活。

### 4. 出公网 HTTPS

ChatGPT 只能连 HTTPS。

**Cloudflare Tunnel**（不用开端口、不用管证书）：

```bash
cloudflared tunnel --url http://127.0.0.1:8484
```

**Nginx**（SSE 需要关掉缓冲；长任务需要加大读超时）：

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

## 使用

```bash
./bin/runabout serve -c configs/config.yaml
```

浏览器打开 `https://你的域名/` 会有一页接入说明（MCP 地址、源码链接、curl 示例）。

### 客户端

把 `https://你的域名/mcp` 交给客户端，鉴权选 OAuth。默认开启动态注册（`auth.allow_dynamic_registration`），ChatGPT 依赖它。若希望 `/oauth/register` 必须带 Bearer 令牌，设置 `auth.registration_token`。

已验证的形态：

- ChatGPT 自定义连接器（开发者模式）
- Claude Code：`claude mcp add --transport http`
- 其它支持 remote MCP + OAuth 的客户端

给 curl 或不走 OAuth 的 agent 用时，在 `auth.static_tokens` 里加一条（`runabout gen-token` 可生成），请求头带 `Authorization: Bearer …`。

自检：

```bash
curl -s https://你的域名/healthz | jq
curl -s https://你的域名/.well-known/oauth-protected-resource | jq
```

### 命令行

```
runabout serve [-c 配置]        启动服务
runabout hash-password [密码]   生成 bcrypt 哈希，填到 auth.users
runabout gen-token              生成随机静态令牌
runabout check-policy '命令'    查看 shell 策略会如何处理这条命令
runabout version
```

`check-policy` 加上 `-c` 会加载配置里的自定义规则。退出码 2 表示 deny，3 表示 confirm。

## 修改

多数改动只动配置。架构、威胁模型和每处取舍的理由见 [DESIGN.md](DESIGN.md)。

### 常用配置

| 方面 | 改哪里 |
|---|---|
| 监听 / 公网地址 | `server.listen`、`server.base_url`、`RB_*` |
| 谁能登录 | `auth.users`、`auth.session_cookie_ttl` |
| 谁能注册客户端 | `auth.allow_dynamic_registration`、`auth.registration_token` |
| 下线某个工具 | `tools.disabled`（例如 `["write"]`，只留 `apply_patch` 改文件） |
| shell 工作目录与限额 | `tools.shell.default_workdir`、超时、`max_background`、额外 `env` |
| 路径黑名单 | `policy.write_deny_paths`、`policy.read_deny_paths`（只约束文件工具，不约束 shell） |
| shell 规则 | `policy.disabled_shell_rules`、`policy.downgrade_to_confirm`、`policy.extra_shell_rules` |
| 审计 | `audit.enabled`、`audit.log_args`、`audit.file` |
| AGPL 源码链接 | `server.source_url` —— 对外提供改过的版本时，指向你自己的仓库 |

追加规则示例：

```yaml
policy:
  extra_shell_rules:
    - id: "no-terraform-destroy"
      action: "confirm"
      reason: "会销毁云上资源"
      hint: "先 terraform plan -destroy 看清楚要删什么"
      command: "terraform"
      arg_regex: '\bdestroy\b'
```

`action` 只能是 `deny` 或 `confirm`。内置规则的 id 可以在 `runabout check-policy` 的输出里看到。改了策略相关测试后跑 `make test-policy`。

### 代码

```
cmd/runabout          CLI：serve / hash-password / gen-token / check-policy
internal/app          HTTP 装配（Build 给 e2e 测试直接调用）
internal/auth         OAuth 2.1 + Bearer 中间件
internal/mcp          JSON-RPC、工具注册表、Streamable HTTP
internal/tools        shell / 文件 / 检索实现
internal/policy       shell AST、风险规则、路径黑名单、确认令牌
internal/audit        JSONL 审计日志
internal/config       YAML、默认值、校验
```

扩展点：

- **加工具。** 实现 `mcp.Handler`（`Definition()` + `Call()`），在 `tools.All()` 里挂上。
- **加内置 shell 规则。** 在 `builtinCmdRules()` 里加一条，写清 `reason` 和给模型看的 `hint`。`command` + `arg_regex` 够用时优先走 `extra_shell_rules`。
- **换认证。** 替换 `auth.Server.Protect`，其余不动。

`configs/config.example.yaml` 会在测试里按严格模式解析，改结构体时记得同步示例。

如果你改了代码并让别人通过网络使用，AGPL 第 13 条要求这些使用者能拿到你修改后的源码。根页面和 `/healthz` 会展示 `server.source_url`，请把它改成你自己的仓库。自己部署给自己用不触发这条义务。

## 兼容性

- MCP 协议 `2025-06-18`（同时兼容 `2025-03-26`、`2024-11-05`）
- 传输：Streamable HTTP——`POST /mcp` 提交、`GET /mcp` 开 SSE、`DELETE /mcp` 结束会话
- 依赖（均为宽松许可，与 AGPL 兼容）：`mvdan.cc/sh`、`golang.org/x/crypto`、`golang.org/x/term`、`gopkg.in/yaml.v3`。协议层与 OAuth 都在本仓库内实现。

## 许可

[GNU AGPL v3 或更新版本](LICENSE)。Copyright (C) 2026 noir017.
