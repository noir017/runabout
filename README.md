# agent-tools-mcp

把一台 Linux 机器的基础操作能力，通过 MCP 暴露给能连 MCP 的 agent。

自带 OAuth 2.1 授权服务器，走 Streamable HTTP 传输，可以直接作为 **ChatGPT 网页版的自定义连接器**接入——
接上之后 ChatGPT 就成了一个能真正动手的个人助理：查服务状态、改配置、跑部署、看日志。

```
ChatGPT ──HTTPS──> 反代/隧道 ──> agent-tools-mcp ──> 这台机器
              (OAuth 2.1 + PKCE)      (策略拦截 + 审计)
```

## 工具

| 工具 | 用途 |
|---|---|
| `shell` | 执行 bash 命令；支持超时、stdin、环境变量、丢后台跑 |
| `shell_list` / `shell_output` / `shell_kill` | 管后台任务：列出、拉输出（支持只看新增）、发信号结束 |
| `read` | 读文件，默认带行号；图片按图像返回，二进制拒绝并给替代方案 |
| `write` | 整体写入/追加，临时文件 + 原子替换 |
| `apply_patch` | 按上下文定位改文件（codex 的 V4A 信封格式），多文件全成功或全不动 |
| `search` | 按内容搜正则，有 `rg` 就用 `rg`，否则回退内置实现 |
| `glob` | 按文件名模式找文件，默认按修改时间倒序 |
| `list_dir` | 列目录，可多层展开成树 |

设计上参考了 Claude Code / opencode / codex 的工具划分：**改文件优先 `apply_patch` 而不是整体覆盖**，
搜索分成"搜内容"和"找文件名"两个工具，长任务走后台而不是把一次调用拖到超时。

## 安全模型

shell 默认拥有完整权限——`rm 单个文件`、`git` 全套、包管理、`systemctl` 操作都直接执行，不打扰你。
只有明确的破坏性模式会被拦下，分两档：

**直接拒绝**（没有正当用途，二次确认也不放行）
`rm -rf /`、`rm -rf /etc`、`mkfs.*  /dev/sda`、`dd of=/dev/sda`、`> /dev/nvme0n1`、fork bomb、`kill -9 -1`

**需要二次确认**（返回一次性 `confirm_token`，带着同一条命令重发才执行）
`rm -rf *`、`rm -rf /var/log`、`chmod -R 777 /var/www`、`reboot`、`iptables -F`、`nft flush ruleset`、
`curl … | bash`、`rmmod`、`crontab -r`、`docker system prune --volumes`、`cat ~/.ssh/id_rsa`、
`umount -a`、`userdel`、`find / -delete` 等等

判定基于 **shell AST 解析**（mvdan.cc/sh）而不是正则匹配，所以下面这些变形一个都躲不过：

```bash
sudo rm -rf /                 # 剥掉 sudo/nohup/env/timeout 等包装
rm --recursive --force /      # 长选项等价于 -rf
rm -rf "$HOME"                # 展开 $HOME 与 ~
bash -lc 'rm  -rf   /'        # 递归解析 -c 里的内层脚本
ssh server 'rm -rf /'         # 连远端命令也过一遍规则
if true; then rm -rf /; fi    # 复合语句里的命令同样被检查
```

同时：

- **文件工具有路径黑名单**：`~/.ssh/id_*`、`~/.aws/credentials`、`~/.config/gh/hosts.yml`、`/etc/shadow` 等
  凭据文件禁止读取，`/proc`、`/sys`、`/dev`、`/boot` 禁止写入。服务自己的令牌库自动进黑名单。
- **全量审计**：每次工具调用、每次策略判定、每次登录与发令牌都落 JSONL，能回答"谁在什么时候干了什么"。
- 规则可配置：按 id 关掉、把 deny 降级为 confirm、用 `command` + `arg_regex` 加自己的规则。

用 `check-policy` 可以随时验证某条命令会被怎么处理：

```console
$ agent-tools-mcp check-policy 'rm -rf ./node_modules'
判定: allow
没有命中任何规则，会直接执行。

$ agent-tools-mcp check-policy 'sudo rm -rf $HOME'
判定: deny
· [rm-recursive] 递归删除高风险路径（deny）
  细节：删除目标: /home/agent
  建议：删除请指向具体子目录，例如 rm -rf ./build 而不是 rm -rf * 或 rm -rf /path
```

## 快速开始

### 1. 构建

```bash
make build                    # 产出 bin/agent-tools-mcp
# 或者
make docker                   # 镜像里带 git/rg/jq/python/编译器等常用工具
```

### 2. 生成配置

```bash
cp configs/config.example.yaml configs/config.yaml
./bin/agent-tools-mcp hash-password        # 交互输入，输出 bcrypt 哈希
```

把哈希填进 `auth.users[0].password_hash`，并把 `server.base_url` 改成你的公网地址。
**`base_url` 必须与 ChatGPT 里填的地址完全一致**，OAuth 元数据和回调校验都以它为准。

### 3. 出公网

服务默认只监听 `127.0.0.1:8484` 纯 HTTP，HTTPS 交给外层。ChatGPT 只能连 HTTPS 地址。

```bash
# Cloudflare Tunnel（不用开端口、不用管证书）
cloudflared tunnel --url http://127.0.0.1:8484

# 或者 Nginx
location / {
    proxy_pass http://127.0.0.1:8484;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;          # SSE 需要
    proxy_read_timeout 3600s;     # 长任务需要
}
```

### 4. 启动并接入

```bash
./bin/agent-tools-mcp serve -c configs/config.yaml
```

浏览器打开 `https://你的域名/` 会有一页接入说明。然后在 ChatGPT 里：

1. 设置 → 连接器 → 创建自定义连接器（需要开发者模式）
2. URL 填 `https://你的域名/mcp`，鉴权选 OAuth
3. 保存并连接 → 跳到本服务登录页 → 输入配置里的账号密码 → 授权

客户端注册（RFC 7591 动态注册）、授权码 + PKCE、令牌颁发与轮换全部由本服务自己完成，不需要外部 IdP。

### 自检

```bash
curl -s https://你的域名/healthz | jq
curl -s https://你的域名/.well-known/oauth-protected-resource | jq

# 配了 static_tokens 的话可以直接调
curl -s https://你的域名/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | jq '.result.tools[].name'
```

## 部署形态

- **Docker**：`deploy/docker-compose.yml`。镜像基于 debian-slim，预装 git、ripgrep、fd、jq、python3、
  build-essential 等——这个服务的价值就在于工具齐全，所以刻意不用 scratch。默认以 uid 10001 的
  `agent` 用户运行，不是 root。
- **裸机**：`deploy/agent-tools-mcp.service`。建议专门建一个用户跑，需要提权的操作通过 sudoers 精确授予。

> 跑这个服务的身份，就是 agent 拿到的身份。默认给 root 等于把整台机器交出去。

## 命令行

```
agent-tools-mcp serve [-c 配置]        启动服务
agent-tools-mcp hash-password [密码]   生成 bcrypt 密码哈希
agent-tools-mcp gen-token              生成随机静态令牌
agent-tools-mcp check-policy '命令'    看某条命令会被策略怎么处理
agent-tools-mcp version
```

环境变量可覆盖常改项：`ATM_LISTEN`、`ATM_BASE_URL`、`ATM_DATA_DIR`、`ATM_USER`、`ATM_PASSWORD_HASH`、
`ATM_STATIC_TOKEN`、`ATM_AUTH_DISABLED`、`ATM_LOG_LEVEL`、`ATM_LOG_FORMAT`。

## 兼容性

- MCP 协议版本：`2025-06-18`（同时兼容 `2025-03-26`、`2024-11-05`）
- 传输：Streamable HTTP（`POST /mcp` 提交、`GET /mcp` 开 SSE 下行、`DELETE /mcp` 结束会话）
- 已验证的客户端形态：ChatGPT 自定义连接器、Claude Code（`claude mcp add --transport http`）、
  任何支持 remote MCP + OAuth 的客户端
- Go 1.25+，仅 Linux（用到进程组信号）

## 更多

- [DESIGN.md](DESIGN.md)：架构、每个设计取舍的理由、威胁模型、可扩展点
- 依赖只有四个（均为宽松许可，与 AGPL 兼容）：`mvdan.cc/sh`（shell 解析）、`golang.org/x/crypto`（bcrypt）、
  `golang.org/x/term`（密码不回显）、`gopkg.in/yaml.v3`。协议层与 OAuth 都是手写的。

## 许可

[GNU AGPL v3 或更新版本](LICENSE)。Copyright (C) 2026 noir017。

AGPL 第 13 条对网络服务有额外要求：**如果你改了代码并让别人通过网络用它，
就得让这些使用者能拿到你修改后的源码。** 本服务的根页面会展示源码链接，
自建时请把配置里的 `server.source_url` 指向你自己的仓库。
自己部署给自己用不触发这条义务。
