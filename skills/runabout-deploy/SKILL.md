---
name: runabout-deploy
description: 把 runabout 部署到一台 Linux 服务器——Docker Compose 或裸机 systemd 两条路线，含部署后自检脚本 smoke.sh。当用户要安装或上线 runabout、换对外域名、换监听端口、迁移到新机器时使用。
---

# 部署 runabout

runabout 是把一台机器的 shell/文件/检索能力通过 MCP 暴露出去的服务，自带 OAuth 2.1
授权服务器。**部署它等于把这台机器的操作权限接到网上**，所以下面每一处"为什么"都值得读，
不要跳着抄命令。

## 开工前必须先定的三件事

缺任何一件都会返工，先问清楚再动手。

### 1. 对外地址（base_url）

这是最容易出错、且错了会让人查半天的一项。`base_url` 是 OAuth 元数据、回调校验、
资源标识（RFC 8707 的 `resource`）的唯一来源，客户端里填的地址必须与它**逐字一致**：

- 必须是 `https`（唯一例外是本机 `127.0.0.1` 调试）
- **不带尾斜杠**
- 只到主机名，**不含 `/mcp`**（`/mcp` 是客户端填的端点路径，不是 base_url 的一部分）

```
对: https://mcp.example.com
错: https://mcp.example.com/      ← 尾斜杠
错: https://mcp.example.com/mcp   ← 把端点路径写进来了
错: http://mcp.example.com        ← 明文，口令和令牌裸奔
```

### 2. 用哪个身份跑

服务用什么身份跑，agent 就拿到什么身份。**不要用 root。**
建一个专用用户，需要的额外权限用 sudoers 精确授予。

Docker 路线要额外注意 uid 对齐：容器里的 `agent` 用户默认 uid 10001，
而配置文件是从宿主机 bind mount 进去的。uid 不一致 → 容器起来就
`读取配置 ...: permission denied` 然后 crash loop。

```bash
id -u   # 在宿主机上跑，把结果填进 .env 的 AGENT_UID / AGENT_GID
```

别假定是 1000：Oracle Cloud、部分云厂商的 Ubuntu 镜像里默认用户是 **1001**。

### 3. 公网入口

服务自己只说 HTTP，HTTPS 一律交给外层。三选一：

| 入口 | 适用 |
|---|---|
| Cloudflare Tunnel | 机器没有公网 IP / 不想开端口（最常见） |
| Nginx / Caddy 反代 | 已经有反代在跑 |
| 只绑 127.0.0.1 | 纯本机调试 |

**绝不要**把 `BIND_ADDR` 设成 `0.0.0.0` 直接对公网提供明文 HTTP —— 那样 OAuth
登录口令、access token、静态令牌全部明文过网。临时诊断用完就改回去。

## 路线 A：Docker Compose（推荐）

```bash
git clone https://github.com/noir017/runabout && cd runabout/deploy

cp .env.example .env
cp ../configs/config.example.yaml config.yaml
```

**编辑 `.env`**（只有 `RB_BASE_URL` 是必填，其余有默认值）：

```ini
RB_BASE_URL=https://mcp.example.com   # 见上面第 1 条
AGENT_UID=1000                        # 宿主机 id -u 的结果
AGENT_GID=1000
BIND_ADDR=127.0.0.1                   # 入口交给隧道/反代
HOST_PORT=8484
MEM_LIMIT=2g                          # 机器没 swap 时别设满，超限直接 OOM kill
INGRESS_NETWORK=cloudflared           # 用隧道时填隧道所在的 docker 网络
```

**编辑 `config.yaml`**：

```bash
# 先构建镜像（下面两条要用它）
docker compose build

# 生成登录口令的哈希。这个口令等价于服务器 shell 权限，别用弱密码。
docker run --rm -it runabout:0.1.0 hash-password
# 生成一个静态令牌，给 curl 自检用（可选但强烈建议，排障时省一整套 OAuth）
docker run --rm runabout:0.1.0 gen-token
```

注意子命令直接跟在镜像名后面：镜像的 ENTRYPOINT 已经是那个二进制，
再写一遍 `runabout` 会被当成子命令名，只会打出用法。
（`docker exec` 不走 ENTRYPOINT，所以那边要写全：`docker exec runabout runabout gen-token`。）

镜像还没构建时用本机 Go 也行：`go run ./cmd/runabout hash-password`。
把哈希填到 `auth.users[0].password_hash`，令牌填到 `auth.static_tokens`。
容器里必须 `listen: "0.0.0.0:8484"`，写 `127.0.0.1` 宿主机映射不进来。

`.env` 与 `config.yaml` 都含密码哈希/令牌，**不入库**，权限收紧：

```bash
chmod 600 .env config.yaml
```

**起服务**：

```bash
docker compose up -d --build
docker compose logs -f     # 看到 "runabout 启动" 且带正确的 base_url 就对了
```

### 入站网络的方向

如果用隧道/反代容器，**网络归入口持有，服务挂进去**，不是反过来：

```bash
docker network create cloudflared                       # 入口自己的共享网络
docker network connect cloudflared <隧道容器名>          # 入口先进来
# compose 里 ingress 声明为 external，服务 up 时自动挂上
```

入口是多个服务共享的前门，让它反过来加入每个业务网络，加到第三个服务就乱了。

然后**隧道的 Service 填容器名**：

```
http://runabout:8484
```

填 `http://127.0.0.1:8484` 是隧道容器**自己的**回环，必然 502 —— 这是最高频的一个坑。

## 路线 B：裸机 systemd

```bash
make build && sudo install -m755 bin/runabout /usr/local/bin/

sudo useradd -r -m -d /srv/workspace -s /bin/bash agent
sudo mkdir -p /etc/runabout
sudo cp configs/config.example.yaml /etc/runabout/config.yaml
sudo chown root:agent /etc/runabout/config.yaml && sudo chmod 640 /etc/runabout/config.yaml
# 按上面同样的方式填 base_url / password_hash / static_tokens

sudo cp deploy/runabout.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now runabout
journalctl -u runabout -f
```

单元文件里刻意**没有** `ProtectHome` / `ProtectSystem=strict`：这个服务本来就是要读写
机器上的文件，锁太死它就没法干活了。`StateDirectory=runabout` 会自动建
`/var/lib/runabout` 存 OAuth 客户端表和令牌。

## 部署后自检

跑同目录下的脚本，它把一次真实客户端接入会走的每一步都验一遍（元数据、401 挑战、
CORS 预检、MCP 握手、工具列表）：

```bash
./smoke.sh https://mcp.example.com                 # 免鉴权部分
./smoke.sh https://mcp.example.com <静态令牌>       # 连握手一起验
```

全通过退出 0，有失败退出 1，可以直接塞进 CI 或部署脚本。
只依赖 curl/grep/sed，部署现场没装 jq 也能跑。

## 换域名 / 换端口 / 迁移

**换域名**：只改 `.env` 的 `RB_BASE_URL`，然后 `docker compose up -d`（要重建容器才
会读新环境变量，`restart` 不够）。改完必须同步：入口那侧的 hostname、客户端里填的
连接器地址。三者对不齐 OAuth 就断。

**换端口**：改 `.env` 的 `HOST_PORT`（宿主侧），`BIND_ADDR` 保持 `127.0.0.1`。
容器内固定 8484，不用动。

**迁移服务器**：源码可以重新拉，但要带走两样东西：

```bash
# 1. 配置（含密码哈希与静态令牌）
scp deploy/.env deploy/config.yaml 新机器:...
# 2. 令牌库与审计日志（不带走的话，所有客户端都得重新授权）
docker run --rm -v runabout_mcp-data:/d -v "$PWD":/out alpine tar czf /out/mcp-data.tgz -C /d .
```

备份配置时**别把副本留在部署目录里**：`Dockerfile` 的 `COPY . .` 会把
`config.yaml.bak` 这类文件打进 build stage 的层，等于把密码哈希写进镜像。
`.dockerignore` 已经用 `config.yaml*` / `*.bak` 挡了一层，但备份放到部署目录之外
（比如 `~/backup/`）才是对的做法。

## 踩坑清单

| 现象 | 原因 |
|---|---|
| 容器 crash loop，日志 `读取配置 ...: permission denied` | `AGENT_UID` 与宿主机文件属主不一致（`id -u` 确认，别假定 1000） |
| 公网 502 | 隧道 Service 填了 `127.0.0.1` 而不是容器名 |
| 502 且日志 `lookup xxx: no such host` | 容器改过名，入口那侧还指着旧名 |
| 客户端连不上但服务本机正常 | `base_url` 与客户端里填的地址不一致（尾斜杠、http/https、多了 `/mcp`） |
| 免密 sudo 在容器里失效 | compose 开了 `security_opt: no-new-privileges`，它与 NOPASSWD sudo 互斥，二者只能选一 |
| 日志里什么都看不到 | 请求日志是 Debug 级，把 `RB_LOG_LEVEL` 调成 `debug` |
| 磁盘被吃满 | Docker build cache 会涨到几个 G，`docker builder prune` |

收紧生产配置见 `runabout-harden`，接入客户端见 `runabout-connect`，
出问题见 `runabout-troubleshoot`。
