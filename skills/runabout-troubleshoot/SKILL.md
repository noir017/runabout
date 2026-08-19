---
name: runabout-troubleshoot
description: 排查 runabout 的故障——502、客户端"建立连接时发生意外错误"、401 死循环、容器起不来、工具被策略拦下。当用户说 runabout 连不上、报错、或"容器里没有报错"时使用。
---

# 排查 runabout

## 第 0 步：把日志打开（几乎每次都要先做这个）

**请求日志写在 Debug 级**，默认 `RB_LOG_LEVEL=info` 下所有 4xx 都看不见。
"容器里没报错"在这个前提下不是有效信息 —— 它可能正在每秒回 401。

```bash
# Docker
sed -i 's/^RB_LOG_LEVEL=.*/RB_LOG_LEVEL=debug/' deploy/.env
docker compose up -d && docker compose logs -f

# 裸机
sudo systemctl edit runabout   # 加 Environment=RB_LOG_LEVEL=debug
sudo systemctl restart runabout && journalctl -u runabout -f
```

然后**复现一次**，看每条请求的 `method / path / status`。定位到具体是哪个请求、
哪个状态码之后再往下查，不要凭现象猜。

排完记得调回 `info`：debug 会把每个请求都记下来，日志涨得快。

## 定位顺序：从里往外

一层层往外排，能立刻把范围砍掉一半：

```bash
# 1. 进程本身
docker exec runabout curl -s localhost:8484/healthz

# 2. 宿主机（端口映射对不对）
curl -s 127.0.0.1:8484/healthz

# 3. 入口内部（隧道/反代能不能按名字找到服务）
docker run --rm --network <入口网络> alpine sh -c \
  'apk add -q curl; curl -s -o /dev/null -w "%{http_code}\n" http://runabout:8484/healthz'

# 4. 公网
curl -s https://你的域名/healthz
```

哪一层开始挂，问题就在那一层。

## 症状对照

### 公网 502 / 503，本机正常

看入口日志，它会直接说原因：

```bash
docker logs <隧道容器> 2>&1 | grep -i 'error\|originService' | tail
```

| 日志里的话 | 根因 |
|---|---|
| `dial tcp 127.0.0.1:8484: connection refused` | 隧道 Service 填了 `127.0.0.1` —— 那是隧道容器**自己的**回环。改成 `http://<容器名>:8484` |
| `lookup <某个名字>: no such host` | 容器名对不上（改过名？），或隧道容器不在同一个 docker 网络里 |
| 完全没有请求记录 | 请求没到入口：DNS、隧道没跑、或云厂商安全组 |

远程托管的隧道（Cloudflare 面板配的）**改不了本地文件**，Service 只能在面板里改。

### 客户端提示「建立连接时发生意外错误」，服务端看不到错误

先确认 OAuth 到底走到哪一步了：

```bash
curl -s https://你的域名/healthz    # 看 oauth 里的计数
```

- `clients: 0` → 连注册都没成功，查发现链路（下一条）
- `clients: 1` 但 `access_tokens: 0` → 卡在授权码换令牌
- `access_tokens > 0` → **OAuth 已经通了**，问题在拿到令牌之后的 `/mcp` 调用

最后那种情况，重点查 CORS 预检 —— 浏览器规范不允许预检携带 `Authorization`，
如果它被鉴权拦下，客户端只会显示一句没有下文的"意外错误"：

```bash
curl -si -X OPTIONS https://你的域名/mcp \
  -H 'Origin: https://chatgpt.com' \
  -H 'Access-Control-Request-Method: POST' \
  -H 'Access-Control-Request-Headers: content-type,authorization' | head -8
```

- `204` + `access-control-allow-*` 头 → 正常
- `401` → 预检被鉴权拦了（这是 bug，升级到含该修复的版本）
- `403` → `Origin` 不在 `server.allowed_origins` 白名单，把客户端的域名加进去

### 401 死循环 / 客户端反复要求授权

```bash
docker compose logs | grep -i 'resource 与本服务不一致'
```

有这条 WARN 说明令牌里的 `resource` 和 `base_url + /mcp` 对不上 —— 通常是客户端里填的
地址与 `base_url` 有细微差异（尾斜杠、http/https、域名不同）。runabout 只记录不拒绝，
所以它不会直接导致 401，但它是"地址配错了"的确凿信号，顺着改。

真正的 401 原因按概率排：令牌过期（默认 1h，客户端该用 refresh_token）、
静态令牌抄错、`Authorization` 头没带上。

### 容器起不来 / crash loop

```bash
docker compose logs --tail=30
```

| 日志 | 修法 |
|---|---|
| `读取配置 ...: permission denied` | `AGENT_UID`/`AGENT_GID` 与宿主机文件属主不一致。`stat -c '%u:%g' deploy/config.yaml` 对照 `.env`，改完要 `--build` 重建（uid 是构建参数） |
| `server.base_url 必须是形如 https://host[:port] 的绝对地址` | `RB_BASE_URL` 填了 http 或写错；没填的话 compose 的必填守卫会先拦下 |
| `bind: address already in use` | `HOST_PORT` 被占了，`ss -ltnp \| grep <端口>` |

容器 **healthy 但宿主机 `curl 127.0.0.1:<HOST_PORT>` 不通**是另一类：容器内的
`listen` 写成了 `127.0.0.1:8484`。healthcheck 是在容器**内部**跑的，
所以它照样通过 —— 这个组合最容易看错。容器里必须是 `0.0.0.0:8484`。

### 免密 sudo 在容器里不管用

`security_opt: no-new-privileges` 与 NOPASSWD sudo **互斥**，开了前者 sudo 必然失效。
二者只能选一个：要 sudo 就注释掉 `no-new-privileges`；要那层加固就
`SUDO_NOPASSWD=false` 重新构建，然后靠挂载和 uid 授权而不是 sudo。

### 工具调用被拦

```bash
# 直接问策略引擎会怎么判这条命令，比试错快
docker exec runabout runabout check-policy 'rm -rf /var/log'
```

退出码 0=放行、2=拒绝、3=需二次确认。要放宽：
`policy.downgrade_to_confirm` 把 deny 降级成可确认，
`policy.disabled_shell_rules` 按 id 直接关掉某条规则。
文件工具（read/write/apply_patch/search/glob/list_dir）受 `write_deny_paths` /
`read_deny_paths` 约束，**shell 工具按设计不受这两项约束**，它只走 shell 规则。

### 后台任务的输出读不全

`shell_output` 的 `only_new` 语义是"自上一次带 `only_new` 的读取以来的新增"，
不是"自上次任何读取"。轮询长任务要每次都带 `only_new=true`，
中间夹一次不带的整体读取不会推进游标。

## 判断"是不是被 CDN / WAF 挡了"

别凭猜。三个证据一起看：

```bash
# 1. 响应头里有没有拦截痕迹
curl -sD - -o /dev/null https://你的域名/healthz | grep -iE 'cf-mitigated|^location|^server'
# 2. 绕过 CDN 直连对比（临时把 BIND_ADDR 改 0.0.0.0 + 开一个高位端口）
curl -s http://<公网IP>:<端口>/healthz
# 3. 入口容器日志里有没有这次请求
docker logs --since 5m <隧道容器> | tail
```

被 WAF 挡的特征是：**入口日志里根本没有这次请求**，且响应头带 `cf-mitigated`
或跳到一个挑战页。如果 OAuth 已经完成过（`access_tokens > 0`）、SSE 下行也能正常
吐出 `: stream open`，那就**不是**风控 —— 风控会在第一步就把人挡在登录页外面。

临时开的直连端口是明文 HTTP，诊断完立刻把 `BIND_ADDR` 改回 `127.0.0.1`。

> 注意：Docker 发布的端口走 PREROUTING DNAT + FORWARD/DOCKER 链，**不经过 INPUT**，
> 所以 `iptables -L INPUT` 里那条 REJECT 拦不住它。看着防火墙关着其实端口是通的，
> 别被误导（反过来也一样：以为开了 INPUT 就能通，云厂商安全组才是另一道）。

## 兜底：留一条不依赖 OAuth 的通路

配一个静态令牌（`auth.static_tokens`），排障时它能把整个 OAuth 链路排除在外：

```bash
./skills/runabout-deploy/smoke.sh https://你的域名 <静态令牌>
```

一次跑完存活、元数据、401 挑战、CORS 预检、MCP 握手、工具列表六组检查，
比手工敲 curl 快得多，也不会漏项。
