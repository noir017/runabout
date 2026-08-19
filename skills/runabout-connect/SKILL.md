---
name: runabout-connect
description: 把已部署的 runabout 接到客户端上——ChatGPT 自定义连接器的逐字段填法、Claude Code / 其它 MCP 客户端、以及 curl 直连。当用户要添加连接器、"这些字段怎么填"、或客户端提示建立连接失败时使用。
---

# 接入客户端

前提：服务已部署且 `skills/runabout-deploy/smoke.sh` 全绿。**没自检过就先去自检**，
绝大多数"客户端连不上"其实是服务端 `base_url` 配错。

## 端点地址

| 用途 | 地址 |
|---|---|
| 客户端填的 MCP 端点 | `https://你的域名/mcp` |
| 服务根页面（含接入说明） | `https://你的域名/` |
| 健康检查 | `https://你的域名/healthz` |

注意区分：配置里的 `server.base_url` 写到主机名为止**不含** `/mcp`；
客户端里填的地址**要带** `/mcp`。

## ChatGPT 自定义连接器

设置 → 连接器 → 创建自定义连接器（需要先打开开发者模式）。

| 字段 | 填什么 |
|---|---|
| 名称 | 随意，例如 `runabout`（只是显示名） |
| 描述 | 可选，例如"操作某台服务器：跑命令、读写文件、看日志" |
| **服务器 URL** | `https://你的域名/mcp` —— https、带 `/mcp`、**无尾斜杠** |
| 身份验证 | OAuth |
| **注册方法** | **DCR（动态客户端注册）** |
| OAuth 客户端 ID | **留空** |
| OAuth 客户端密钥 | **留空** |
| 令牌端点认证方法 | `none` |
| 作用域 | `mcp` |

选 DCR 是因为 runabout 自带 RFC 7591 注册端点，客户端 ID 由服务器现场发放，
不需要你手工预置。`none` 是对的：DCR 注册出来的是**公开客户端**，安全性靠 PKCE S256
而不是客户端密钥。

保存后点连接 → 跳到 runabout 自己的登录页 → 用 `auth.users` 里的账号密码登录并授权 →
回到对话即可调用工具。

### 「DCR 不可用」怎么办

界面提示 *"在下面的 OAuth 端点部分提供注册 URL 之前，DCR 不可用"* ——
意思是它没能从你填的地址发现授权服务器元数据。**九成是 URL 填错**（`http://`、
少了 `/mcp`、多了尾斜杠），或者服务端 `base_url` 与该域名不一致。先验证发现链路：

```bash
curl -s https://你的域名/.well-known/oauth-authorization-server | grep registration_endpoint
```

有输出就说明服务端没问题，回头改 URL 那一栏。确认无误后仍然灰着，展开
「高级 OAuth 设置」手填：

```
注册 URL:  https://你的域名/oauth/register
授权 URL:  https://你的域名/oauth/authorize
令牌 URL:  https://你的域名/oauth/token
作用域:    mcp
认证方法:  none
```

*"由于服务器未公布 CIMD 支持，CIMD 不可用"* 是**正常的**，runabout 不实现 CIMD，
不影响接入。

### 忘了登录密码

服务端只存 bcrypt 哈希，明文找不回来，重设一个：

```bash
docker exec -it runabout runabout hash-password
# 输出的哈希填进 config.yaml 的 auth.users[0].password_hash，然后
docker compose up -d
```

## 其它 MCP 客户端

支持 Streamable HTTP + OAuth 的客户端同理：端点填 `https://你的域名/mcp`，
其余交给自动发现。不支持 OAuth 的客户端用静态令牌：

```json
{
  "mcpServers": {
    "runabout": {
      "type": "http",
      "url": "https://你的域名/mcp",
      "headers": { "Authorization": "Bearer <静态令牌>" }
    }
  }
}
```

静态令牌配在 `auth.static_tokens`，用 `runabout gen-token` 生成。
它**绕过整个 OAuth**，泄露等于服务器权限外流：只给自动化和自检用，别放进任何会同步
到云端的配置文件。

## curl 直连（排障用）

```bash
TOKEN=<静态令牌>
BASE=https://你的域名

# 握手，拿会话 id
curl -si -X POST "$BASE/mcp" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2025-06-18' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}'

# 用上一步响应头里的 Mcp-Session-Id 列工具
curl -s -X POST "$BASE/mcp" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H "Mcp-Session-Id: $SID" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

`Accept` 必须同时给 `application/json` 和 `text/event-stream`（MCP 规范要求）。
用完记得 `curl -X DELETE "$BASE/mcp" -H "Mcp-Session-Id: $SID"` 收掉会话。

## 接入成功后

第一件事是**关掉开放注册**，否则任何人都能往 `/oauth/register` 注册客户端
（注册本身不给权限，还得过登录，但没必要留着）：

```yaml
auth:
  allow_dynamic_registration: false   # 已注册的客户端不受影响
```

改完 `docker compose up -d`。之后要再加客户端时临时打开即可。
更多收紧项见 `runabout-harden`。
