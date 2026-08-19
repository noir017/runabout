# 设计说明

这份文档记录 agent-tools-mcp 的结构、每处取舍的理由，以及威胁模型。
代码里的注释解释"这段在做什么"，这里解释"为什么这么做"。

## 1. 目标与约束

**目标**：让 ChatGPT 网页版能像个人助理一样操作一台 Linux 服务器——查状态、改配置、跑部署、看日志。

由此推出几条硬约束：

| 约束 | 来源 | 影响 |
|---|---|---|
| 必须是 remote MCP + Streamable HTTP | ChatGPT 不能连 stdio | 传输层只做 HTTP，不做 stdio |
| 必须支持 OAuth 2.1 + 动态客户端注册 | ChatGPT 自定义连接器的接入方式 | 自带一个完整授权服务器 |
| 不能依赖交互式确认 | ChatGPT 不支持 MCP elicitation | 危险操作用"令牌二次确认"代替弹窗 |
| 单次调用有超时 | HTTP 请求的现实 | shell 必须支持后台执行 + 轮询取输出 |
| 工具描述就是给模型的说明书 | 模型只看得到 description 和 schema | 描述里写清用法、边界和拦截规则 |

## 2. 分层

```
cmd/agent-tools-mcp        CLI：serve / hash-password / gen-token / check-policy
  └── internal/app         装配：把各层拼成一个 http.Handler（Build 可被测试直接调用）
        ├── internal/auth      OAuth 2.1 授权服务器 + Bearer 校验中间件
        ├── internal/mcp       JSON-RPC 消息层、工具注册表、Streamable HTTP 传输
        ├── internal/tools     10 个工具的实现
        ├── internal/policy    shell AST 解析、风险规则、路径黑名单、确认令牌
        ├── internal/audit     JSONL 审计日志
        ├── internal/config    配置结构、默认值、校验
        ├── internal/globmatch 支持 ** 的通配匹配
        └── internal/idgen     随机 id / 密钥 / 哈希
```

`app.Build(cfg, log) (*Built, error)` 与 `app.Run(path)` 分开，是为了让集成测试能用 `httptest`
跑完整的 OAuth + MCP 链路，而不必猜端口、也不必把装配逻辑在测试里重写一遍。
`internal/app/e2e_test.go` 就是这么覆盖"动态注册 → 授权 → 登录 → 换令牌 → initialize → 调工具"的。

## 3. 为什么手写协议层和 OAuth

社区有 MCP 的 Go SDK，也有现成 OAuth 库。这里都没用，理由：

- **协议层**：MCP 的服务端职责本身很薄——JSON-RPC 分发 + 会话管理 + 几个方法，加起来不到 500 行。
  而 OAuth 挑战头、`resource_metadata` 发现、Origin 校验这些要跟传输层紧密配合，套在 SDK 外面反而绕。
- **OAuth**：需要的是一个"极小但完整"的授权服务器——单用户、无 scope 细分、令牌存文件。
  通用库带来的模型（client 存储抽象、多 grant 插件、JWT 签名密钥轮转）在这里全是负担。

代价是要自己盯规范。已实现的点：授权码一次性、PKCE 强制 S256、refresh 轮换（旧的连带失效）、
令牌只存 SHA-256 摘要、redirect_uri 精确匹配、`WWW-Authenticate` 带 `resource_metadata`。

## 4. 危险命令拦截：为什么用 AST 而不是正则

正则黑名单挡不住变形。`rm\s+-rf\s+/` 这条规则可以被绕过的方式包括：

```bash
rm --recursive --force /      # 长选项
rm -fr /                      # 换顺序
rm  -rf   /                   # 多空格
sudo rm -rf /                 # 前缀包装
rm -rf "$HOME"                # 变量
bash -c 'rm -rf /'            # 引号嵌套
RM=rm; $RM -rf /              # 动态命令名
```

改成解析 shell 语法树后，判定基于"这条命令实际的 argv 是什么"：

1. **解析**（`shellparse.go`）：用 `mvdan.cc/sh/v3/syntax` 解析成 AST，遍历所有 `Stmt`，
   把每条简单命令还原成 `Argv []string` + `Unresolved []bool`。
   - 词还原时展开单双引号，`$HOME` 和 `~` 会被真实展开（直接影响"是不是在删家目录"的判定）；
     其他变量与命令替换标记为 unresolved。
   - 剥掉 `sudo`/`doas`/`nohup`/`env`/`timeout`/`nice`/`xargs` 这类不改变语义的包装。
   - 递归解析 `bash -c "…"` 和 `ssh host "…"` 里的内层脚本（深度上限 3），一层引号不能当护身符。
   - 管道单独建视图，供"下载后直接喂给解释器"这类跨命令规则使用。
2. **风险分级**（`risk.go`）：`classifyPath` 把路径映射到 allow/confirm/deny。
   阶梯是：根目录、家目录、`/etc` `/usr` `/boot` 等关键目录 → deny；关键目录的子目录、
   任意一级绝对路径、`/home/别人`、裸 `*`、`.` → confirm；项目内的具体路径 → allow。
   路径先 `filepath.Clean`，所以 `$HOME/..` 会被识别成 `/home`。
3. **规则**（`shellrules.go`）：19 条内置规则，分单命令 / 管道 / 原文三类。原文规则用来兜住
   AST 不好表达的模式（fork bomb），以及**语法解析失败时的降级**——解析不了又含破坏性关键词，
   宁可要人确认也不闭眼执行。
4. **裁决**（`guard.go`）：取所有 finding 里最严格的动作，配置可按 id 关闭规则或把 deny 降级为 confirm。

### 为什么大部分是 confirm 而不是 deny

拦得太狠，助理就没用了：运维本来就要重启服务、清日志、改防火墙。所以只有"没有任何正当用途"的
才 deny（`rm -rf /`、抹盘、fork bomb），其余给一次确认机会。

确认令牌（`confirm.go`）绑定 `sha256(工具名 + 命令原文 + 工作目录)`：
一次性、限时 5 分钟、命令改一个字符就失效。这样模型没法"拿着上次的令牌去执行另一条命令"——
e2e 测试里专门验了这一条。

工具描述里还写明了一句给模型的行为要求：**收到 confirm_token 时应先向用户说明后果、得到同意再重发，
不要自己替用户确认**。这是提示层面的约束，不是强制的，但配合审计日志够用。

### 已知边界

- `eval "$CMD"`、`$(curl …)` 这类内容运行时才确定的命令，静态分析看不到。做法是把这类
  "证明不了安全"的情况归到 confirm（`eval-dynamic` 规则），而不是假装安全。
- 拿到 shell 的人可以写个脚本文件再执行，脚本内容不过规则。这不是漏洞而是设计：
  shell 按需求就是完整权限，拦截层的目标是**挡住误操作和顺手的灾难**，
  不是防御一个存心搞破坏的对手。真要防后者，需要的是容器隔离和只读挂载，不是命令过滤。

## 5. 文件工具

- **路径黑名单只作用于文件工具**，不作用于 shell。原因：shell 明确要完整权限，
  而文件工具是模型最常"顺手"调用的东西，凭据文件（私钥、云 token）一旦被 `read` 出来就进了对话历史，
  等于泄露给模型服务方。所以 `~/.ssh/id_*`、`~/.aws/credentials`、`~/.config/gh/hosts.yml`
  这些默认禁读；shell 里 `cat` 它们则是 confirm 级——因为 `git push`（用密钥但不打印）必须放行。
- **软链接逃逸**：`Resolve` 会对路径中已存在的部分做 `EvalSymlinks`，保留不存在的尾部，
  所以 `/tmp/x -> /etc/shadow` 挡得住。匹配时同时用解析后路径和原始输入比一遍。
- **服务自己的 data_dir 自动进黑名单**，否则 agent 可以直接改 `oauth.json` 给自己签令牌。
- **写入一律"临时文件 + rename"**，中途失败不会留下半个文件。

### apply_patch 为什么用 V4A 格式

选了 codex 那套 `*** Begin Patch` / `@@` 信封，而不是标准 unified diff，理由：

1. **不带行号**。模型不需要先数行，改动也不会因为行号偏移而失败。
2. **ChatGPT 对这个格式最熟**——它本来就是给这类模型设计的。

定位策略是三级放宽：精确匹配 → 忽略行尾空白 → 忽略首尾空白，用了后两级会在结果里提示。
模型经常吃掉行尾空白，为这点让整个补丁失败不值得，但也要让人知道发生了什么。

**多文件要么全成功要么全不动**：先把所有文件都算完（`plan`），一个都不写；全部通过后才逐个落盘
（`commit`）。补丁改到一半失败是最难收拾的状态。

失败信息刻意写得可操作：上下文找不到时打印期望匹配的前 5 行，并明确让模型"先 read 确认再重试"。
对已存在的文件用 `Add File` 会提示改用 `Update File` 或 `write`。这些都有测试守着。

## 6. shell 的后台执行

HTTP 调用有超时，但编译、备份、压测动辄几分钟。所以 `shell` 支持 `run_in_background=true`，
返回进程 id，再用 `shell_output`（支持 `only_new` 增量读、`wait_ms` 先等再读）和 `shell_kill` 收尾。

两个细节：

- **后台进程用独立的 `context.Background()`**，不绑在发起它的那次 HTTP 请求上，
  否则请求一结束进程就被杀。
- **所有子进程放进独立进程组**（`Setpgid`），超时和 `shell_kill` 都对**整个进程组**发信号。
  只杀父进程会留下一堆孤儿——`npm`、`make`、`docker compose` 全都会 fork。
  超时的处理是先 `SIGTERM`、留 3 秒清理时间、再 `SIGKILL`。

输出用 `capWriter` 截断：**保留开头一半和结尾一半，省略中间**。命令输出通常"开头有报错、
结尾有结论"，只留 head 或只留 tail 都会丢关键信息。
（这里踩过一个坑：`Write` 必须返回输入的完整长度，否则 `io.Writer` 约定下会被判为 short write，
`exec` 会直接报"启动命令失败"。e2e 测试抓到了这个 bug。）

## 7. 传输层细节

- `POST /mcp` 提交消息、`GET /mcp` 开 SSE 下行流、`DELETE /mcp` 结束会话，符合 Streamable HTTP。
- **会话默认宽松**：规范要求客户端回传 `Mcp-Session-Id`，但实际客户端不一定照做。
  找不到会话时默认隐式补一个（`strict_sessions: false`），确定客户端规范的话可以打开严格模式。
- **Origin 校验**只在请求带 `Origin` 时生效（防 DNS rebinding）；非浏览器客户端不带该头，不受影响。
- 全是通知的请求返回 `202` 且无响应体；协议版本按客户端请求协商，不认识的版本回落到最新支持版。
- 工具内的 panic 被 `safeCall` 兜住并转成 isError，单个坏工具不会带崩服务。

**错误分层**：参数结构不对（模型不该犯的错）→ JSON-RPC error；工具执行失败、策略拦截 →
`isError: true` 的正常结果。后者模型能看到内容并自我修正，前者是协议级问题。

## 8. 威胁模型

| 威胁 | 应对 |
|---|---|
| 陌生人访问端点 | OAuth 2.1，401 带 `resource_metadata` 引导授权；默认只监听回环 |
| 授权码被截获 | 强制 PKCE S256，授权码一次性且 5 分钟过期 |
| 令牌泄露 | 落盘只存 SHA-256 摘要；refresh 轮换，旧的连带失效；可 `/oauth/revoke` 吊销 |
| 模型误删数据 | shell AST 规则 + 确认令牌 + 文件路径黑名单 |
| 模型泄露凭据 | 凭据文件禁读（文件工具）/ 需确认（shell） |
| agent 自己改策略/令牌 | data_dir 与配置目录进写黑名单 |
| agent 把服务自己弄死 | 识别针对自身进程/单元的 kill、systemctl stop 并要求确认 |
| 事后无法追责 | 每次调用、判定、登录都落 JSONL 审计 |
| DNS rebinding | Origin 白名单 |
| 密码爆破 | bcrypt cost 12；用户名不存在时也走一次比对，避免时序探测 |

**不在防御范围内**：拿到有效令牌的人等于拿到了这台机器上该运行身份的全部权限。
这是功能定义，不是缺陷。想缩小爆炸半径应该在部署层做——用低权限用户跑、容器化、
敏感目录不挂进去、需要提权的操作用 sudoers 精确授予。

## 9. 测试策略

- `internal/policy/guard_test.go`：**40+ 条"必须放行"的日常命令**（`rm -rf ./node_modules`、
  `git reset --hard`、`docker compose up`、`chmod -R 755 ./public`、`dd of=/tmp/swapfile`……）
  和成组的"必须拦下"用例，外加专门的绕过尝试（`nohup sudo`、`sh -c` 嵌套、复合语句、`$HOME` 展开）。
  这两类用例同等重要：**误拦一条常用命令，助理就变得难用**。
- `internal/tools/tools_test.go`：apply_patch 的更新/新增/删除/重命名/dry-run/空白容错/
  **失败时不落盘**、read 的行号与二进制拒绝、write 的原子与追加、search/glob 的忽略规则、
  shell 的超时/stdin/后台任务全链路。
- `internal/app/e2e_test.go`：起真实 HTTP 服务，跑完整的动态注册 → 授权 → 登录 → 换令牌 →
  initialize → tools/list → 调工具；并验证 401 挑战头、授权码不可重放、
  确认令牌不可挪用/不可复用、被拦命令确实没执行。
- `internal/config/config_test.go`：**校验 `configs/config.example.yaml` 能被严格模式解析**，
  防止示例配置和结构体脱节——这种问题通常要等部署时才暴露。

## 10. 可扩展点

- **加工具**：实现 `mcp.Handler`（`Definition()` + `Call()`），在 `tools.All()` 里挂上。
  配置 `tools.disabled` 可以按名字下线。
- **加规则**：配置里 `extra_shell_rules` 支持 `command` + `arg_regex`；
  更复杂的判定在 `builtinCmdRules()` 里加一条，写清 `reason` 和 `hint`
  （hint 是给模型看的整改建议，别省）。
- **换认证**：`auth.Server.Protect` 是标准中间件，要接外部 OIDC 就替换它，其余不动。
- **多用户**：`auth.users` 已经是列表，令牌里带 subject，审计也记了。
  真要做多用户还需要按 subject 分权，目前所有人权限相同。

## 11. 许可与 AGPL 第 13 条

项目按 AGPL-3.0-or-later 授权。选它而不是 MIT，是因为这类"代人操作服务器"的工具很容易被
包装成闭源 SaaS 卖出去；AGPL 的网络条款要求改动后对外提供服务时必须开放对应源码。

代码里为此留了一处配套设计：`server.source_url`（默认指向上游仓库）会渲染在服务根页面上，
同时出现在 `/healthz` 里。第 13 条要求"向通过网络使用该程序的用户提供获取源码的机会"，
一个网络服务最自然的落点就是它自己的首页。**部署改过的版本时请把这个配置指向你自己的仓库**，
否则源码链接指向的是上游代码，而不是使用者实际在跑的那份。

自己部署给自己用不触发这条义务——第 13 条约束的是"让其他人通过网络使用"。

## 12. 有意没做的事

- **细粒度 scope**：单人使用场景下，多一层 scope 只会让接入更容易出错。拿到令牌即拥有全部工具。
- **内置 TLS / Let's Encrypt**：这台机器上已经有反代或隧道，再塞一套证书管理是重复劳动。
- **数据库**：单实例、令牌数量以十计，JSON 文件 + 原子替换足够，省一个依赖就省一处运维。
- **工具级 sandbox**：真正的隔离应该由容器提供，在应用层做半套沙箱只会给人虚假的安全感。
