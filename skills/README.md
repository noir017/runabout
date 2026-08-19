# skills

Agent skills for deploying and operating runabout — for [Claude Code](https://claude.com/claude-code)
and any other agent that reads the `SKILL.md` convention.

They are not command cheatsheets. Each one carries the judgement calls: which three
decisions to settle before touching the server, why the tunnel owns the network instead of
the other way round, which failure looks like a firewall but isn't.

| Skill | Covers |
|---|---|
| `runabout-deploy` | First deploy (Docker Compose or bare-metal systemd), changing domain or port, migrating hosts. Ships `smoke.sh`. |
| `runabout-connect` | Wiring up a ChatGPT custom connector field by field, other MCP clients, curl |
| `runabout-troubleshoot` | 502s, "unexpected error while connecting", 401 loops, crash loops, policy denials |
| `runabout-harden` | What to tighten before this faces the internet, plus a pre-flight checklist |

**The `SKILL.md` bodies are written in Chinese**, matching `DESIGN.md` and the config
comments. The knowledge is not language-bound — point your agent at them and ask in
whatever language you prefer, or translate them; they are plain Markdown.

## Install

Claude Code discovers skills under `.claude/skills/`, so symlink them in:

```bash
# this repository only
mkdir -p .claude/skills
ln -s ../../skills/runabout-* .claude/skills/

# or globally, available from any directory
mkdir -p ~/.claude/skills
ln -s "$PWD"/skills/runabout-* ~/.claude/skills/
```

Glob `runabout-*` rather than `*` so this README does not get linked in as a skill.
Once installed the agent picks them up on relevant work, or you can invoke one by name:
`/runabout-deploy`, `/runabout-troubleshoot`.

## smoke.sh on its own

`runabout-deploy/smoke.sh` needs no agent — it is a usable deployment check by itself:

```bash
./skills/runabout-deploy/smoke.sh https://mcp.example.com [static-token]
```

It walks every step a real client takes on connect: liveness, RFC 9728 and RFC 8414
metadata (including that `resource` and `issuer` match your `base_url` exactly), the 401
challenge and its `WWW-Authenticate: … resource_metadata=…`, the CORS preflight, then the
MCP handshake and `tools/list`. Exit 0 when everything passes, 1 on any failure, 2 on bad
usage — drop it straight into CI or a deploy script. Only curl, grep and sed, because
deployment hosts rarely have jq.

---

# skills（中文）

给 agent 用的技能包，把部署和运维 runabout 需要的判断都写在里面 —— 不只是命令清单，
还有每处取舍的理由和踩过的坑。

| 技能 | 覆盖 |
|---|---|
| `runabout-deploy` | 从零部署（Docker Compose / 裸机 systemd）、换域名换端口、迁移，附自检脚本 |
| `runabout-connect` | 接入 ChatGPT 自定义连接器（逐字段）、其它 MCP 客户端、curl 直连 |
| `runabout-troubleshoot` | 502、"建立连接时发生意外错误"、401 死循环、容器起不来、策略拦截 |
| `runabout-harden` | 上生产前的收紧项与检查清单 |

安装见上面 Install 一节。装好后 agent 会在相关任务上自动用，
也可以直接点名：`/runabout-deploy`、`/runabout-troubleshoot`。

自检脚本可以脱离 agent 单独用：

```bash
./skills/runabout-deploy/smoke.sh https://mcp.example.com [静态令牌]
```

一次跑完存活、元数据（含 `resource`/`issuer` 与 `base_url` 是否逐字一致）、
401 挑战与 `WWW-Authenticate`、CORS 预检、MCP 握手、`tools/list` 六组检查。
全通过退出 0，有失败退出 1，用法错误退出 2。只依赖 curl/grep/sed。

## 改这些文件时

技能的价值在于**准确**——写错的排障指引比没有更糟。改动时：

- 命令要在真机上跑过，不要凭记忆写。镜像的 ENTRYPOINT 已经是二进制，
  子命令直接跟在镜像名后（`docker run --rm image gen-token`）；`docker exec` 才要写全命令名
- 改了 `smoke.sh` 至少验三种退出码：全绿 0、有失败 1、缺参数 2
- 保持"结论 + 为什么"的写法，只有命令的清单读者照抄一遍就忘了
