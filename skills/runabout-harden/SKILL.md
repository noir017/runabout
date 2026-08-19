---
name: runabout-harden
description: 把 runabout 从"能跑"收紧到"敢放公网"——关闭开放注册、限制暴露面、调策略规则、审计与备份。当用户要上生产、做安全评审、问"这样暴露安全吗"、或要收紧权限时使用。
---

# 收紧 runabout

先认清威胁模型：**runabout 的登录口令等价于这台机器的 shell 权限**。
下面的措施是在这个前提下减少暴露面，而不是把服务变成沙箱 —— 它本来就不是沙箱。

## 必做项

### 1. 关掉开放注册

部署后默认允许任何人调 `/oauth/register`。注册本身不给任何权限（还要过登录+授权），
但没有理由留着：

```yaml
auth:
  allow_dynamic_registration: false    # 客户端都接完之后
```

需要保持开放又想挡住陌生人，用注册令牌代替：

```yaml
auth:
  registration_token: "<runabout gen-token 的输出>"
```

之后 `/oauth/register` 需要带 `Authorization: Bearer <该令牌>`。
注意有些客户端的 DCR 实现不支持带注册令牌，那就临时打开、接完关掉。

### 2. 不要裸暴露明文 HTTP

```ini
BIND_ADDR=127.0.0.1     # 只绑回环，HTTPS 交给隧道/反代
```

`0.0.0.0` + 明文 HTTP 意味着登录口令、access token、静态令牌全部明文过网。
诊断时临时开可以，**用完立刻改回来**。

### 3. 收紧配置文件权限

```bash
chmod 600 deploy/.env deploy/config.yaml         # 含密码哈希与静态令牌
```

裸机部署：`chown root:agent /etc/runabout/config.yaml && chmod 640`
—— 服务要读，但服务身份不该能改自己的配置。

### 4. 令牌库别放在 agent 能写的地方

OAuth 客户端表和令牌存在 `data_dir`（Docker 下是命名卷 `runabout_mcp-data`）。
**不要**把它 bind mount 到 agent 的工作区里面 —— 那等于把令牌库交给 agent 自己改。
`data_dir` 会自动加进文件工具黑名单，但 shell 工具按设计不受路径黑名单约束，
所以物理隔离才是可靠的。

### 5. 用强口令

```bash
runabout hash-password    # 至少 8 位，程序会拒绝更短的
```

这个口令换来的是 shell 权限。用密码管理器生成，别复用。

## 身份与提权

服务用什么身份跑，agent 就是什么身份。**不要 root。**

`no-new-privileges` 与免密 sudo **互斥**，必须二选一：

| 选择 | 代价 | 适用 |
|---|---|---|
| 免密 sudo（`SUDO_NOPASSWD=true`，不开 `no-new-privileges`） | 容器内可提权到 root | 需要装包、改系统配置、管服务 |
| `no-new-privileges`（`SUDO_NOPASSWD=false`） | 装不了包、动不了系统 | agent 只需要读写自己那摊文件 |

第二种更安全，但要提前想清楚 agent 要干的活到底需不需要 root —— 事后发现不够，
得重新构建镜像。裸机部署的等价做法：用 sudoers 精确授予**具体命令**，而不是 `ALL`。

```
# /etc/sudoers.d/agent —— 只给需要的那几条
agent ALL=(root) NOPASSWD: /usr/bin/systemctl restart myapp, /usr/bin/journalctl
```

## 策略规则

先看内置规则怎么判，再决定要不要改：

```bash
runabout check-policy 'rm -rf /var/lib/mysql'    # 0=放行 2=拒绝 3=需确认
```

三种调整方式：

```yaml
policy:
  # 把 deny 降级成"可二次确认"——你确实偶尔需要跑这类命令
  downgrade_to_confirm: ["rm-recursive"]
  # 按 id 直接关掉某条内置规则（check-policy 能看到 id）
  disabled_shell_rules: []
  # 追加自己的规则，action 只能是 deny 或 confirm
  extra_shell_rules:
    - id: "no-terraform-destroy"
      action: "confirm"
      reason: "会销毁云上资源"
      hint: "先 terraform plan -destroy 看清楚要删什么"
      command: "terraform"
      arg_regex: '\bdestroy\b'
```

要点：

- **shell 工具不受 `write_deny_paths` / `read_deny_paths` 约束**，它只走 shell 规则。
  想挡住某类文件操作，文件工具和 shell 规则两边都要写。
- 加自己的机密路径到 `read_deny_paths`（例如 `~/.config/rclone/rclone.conf`、
  应用的 `.env`），它同时会让 shell 里的 `cat`/`base64` 之类需要二次确认。
- 想缩小工具面直接下线工具：`tools.disabled: ["write"]`（只留 `apply_patch` 改文件）。

## 审计

默认开着，落在 `data_dir/audit.jsonl`：

```yaml
audit:
  enabled: true
  log_args: true      # 记录工具参数原文
  max_args: 4000
```

`log_args: false` 只记长度，隐私更好但排查会难很多。审计日志会记下每次工具调用、
每次策略拦截、每次二次确认 —— 它是事后复盘唯一的依据，别关。
定期把它转走（logrotate 或同步到别处），本地留着的那份 agent 有 shell 就能改。

## 资源与可用性

```ini
MEM_LIMIT=2g          # 没 swap 的机器别设满，超限是 OOM kill 不是变慢
```

compose 里刻意用 `cpu_shares` 而不是硬性 cpu 上限：硬限会拖慢正常的编译/测试，
相对权重只在争抢时让它退让。另外：

- 日志已限 `20m × 5`，agent 的命令输出可能很大，别去掉这个限制
- `pids_limit: 4096` 挡 fork bomb 的兜底（策略层也拦，但双保险）
- Docker build cache 会涨到几个 G，磁盘紧张时 `docker builder prune`

## 备份

要带走的只有三样：`deploy/.env`、`deploy/config.yaml`、命名卷 `runabout_mcp-data`。

```bash
docker run --rm -v runabout_mcp-data:/d -v "$PWD":/out alpine \
  tar czf /out/mcp-data.tgz -C /d .
```

**备份别留在部署目录里**：`Dockerfile` 的 `COPY . .` 会把 `config.yaml.bak` 这类文件
打进 build stage 的层，等于把密码哈希写进镜像。`.dockerignore` 用
`config.yaml*` / `.env.?*` / `*.bak` 挡了一层，但正确做法是放到构建上下文之外
（`~/backup/` 之类）并 `chmod 600`。

## 上线前对一遍

- [ ] `base_url` 是 https，与客户端里填的逐字一致
- [ ] `auth.enabled: true`（`/healthz` 里 `"auth":true`）
- [ ] 登录口令是强口令，哈希由 `hash-password` 生成
- [ ] `allow_dynamic_registration` 已关，或配了 `registration_token`
- [ ] `BIND_ADDR=127.0.0.1`，HTTPS 由外层提供
- [ ] `.env` / `config.yaml` 权限 600，且不在 git 里
- [ ] 静态令牌只发给自动化，没进任何会同步到云端的配置
- [ ] 服务不是 root 跑的
- [ ] `no-new-privileges` 与 sudo 的取舍是想清楚的，不是默认来的
- [ ] 审计开着，且日志有转走的去处
- [ ] `smoke.sh` 全绿
- [ ] `RB_LOG_LEVEL` 调回 `info`（排障时开的 debug 记得关）
