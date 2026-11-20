# runabout

> A small, self-hosted MCP gateway for giving agents controlled access to a Linux machine.

[English](README.md) · [中文](README.zh.md)

runabout turns a Linux server into a **remote MCP endpoint** that agents such as ChatGPT and Claude Code can use over HTTPS. It combines Streamable HTTP, OAuth 2.1, a small tool surface, command policy, path guards, confirmation tokens, and an audit log in one self-hosted binary.

The goal is deliberately simple: **let an agent operate a server without turning the entire server into an uncontrolled shell.**

```text
ChatGPT / Claude Code / other MCP client
                    │
                    │ HTTPS + OAuth 2.1
                    ▼
          reverse proxy / tunnel
                    │
                    ▼
                 runabout
          ┌─────────┼─────────┐
          │ policy  │  audit  │
          └─────────┼─────────┘
                    │
                    ▼
                Linux host
```

## Why runabout?

Most remote MCP servers expose an application API. runabout exposes the **machine itself**, but keeps the interface intentionally narrow.

Typical uses:

- **Server operations** — inspect services, logs, processes, packages, network state, and system configuration.
- **Development** — inspect a repository, search code, edit files, run tests, and build projects.
- **Deployment** — pull code, build images, restart services, and diagnose failed deployments.
- **Homelab / self-hosting** — operate a Docker host, NAS, VPS, or other Linux machine from an agent.
- **Chat-based administration** — connect ChatGPT to a private server without exposing SSH directly to the Internet.

The important security boundary is the process identity. If runabout runs as `root`, the agent effectively has root access. Prefer a dedicated user, container, filesystem mounts, and narrowly scoped sudo rules.

## Core tools

The default MCP surface is limited to **7 tools**. `servers` is added only when `fleet.hosts` is configured. Some older implementations still exist in the source tree, but they are not exposed by default.

| Tool | Purpose |
|---|---|
| `shell` | Run a Bash command in the configured environment; supports foreground and background jobs. |
| `shell_output` | Read captured stdout/stderr from a background job. |
| `shell_kill` | Send a signal to a background job and terminate it. |
| `servers` | With fleet configured: probe one machine or all machines (load, memory, root disk, containers, failed units). |
| `read` | Read files, with line numbers and bounded output. |
| `apply_patch` | Make precise, context-aware file changes without replacing an entire file. |
| `search` | Search file contents using regular expressions. |
| `glob` | Find files by glob pattern, with bounded results. |

This split is intentional:

- `shell` handles actions that are inherently command-oriented.
- `read` + `apply_patch` provide a safer file-editing workflow than an unrestricted file writer.
- `search` + `glob` give the agent codebase navigation without adding separate directory/listing tools.
- `shell_output` + `shell_kill` complete the lifecycle for long-running commands.

The result is a small tool vocabulary that is easier for models to choose from and easier for operators to audit.

## Fleet mode

By default runabout operates the machine it runs on. A `fleet` section lets one endpoint reach several machines over ssh:

```yaml
fleet:
  ssh_command: ["ssh", "-o", "BatchMode=yes"]
  probe_timeout: 10s
  approval_check: ""        # optional: exit 0 means this host is already approved
  hosts:
    - name: web1
      desc: public web host
      workdir: /srv/app        # optional; defaults to the remote user's home
    - name: nas
      desc: home NAS
      approval: true
```

Each host can set `workdir`; relative paths on that host resolve there, and without it they resolve to the remote user's home. With `hosts` non-empty, `servers` is added and `shell`, `read`, `search`, `glob`, `apply_patch` gain a `target` parameter whose enum lists the machines plus `local`. Without it, no `target` appears and behavior matches the single-machine version, with two intentional differences: `shell` no longer duplicates stdout/stderr in `structuredContent`, and `read`, `search`, `glob` now carry complete read-only annotations. `servers` probes all hosts in parallel (30-second cache) or returns one host's snapshot.

Hosts marked `approval: true` are probed only when `approval_check` exits 0; otherwise they show as needing approval. runabout does not implement approval — the ssh layer blocks until the user approves.

Remote file writes overwrite in place (`cat > file`) so the inode does not change. Multi-file patches cannot be rolled back if the connection drops; the result lists what was written and what was not. Remote path deny rules match literally and do not resolve symlinks.

Images returned by `read` are downscaled and re-encoded as JPEG when their long edge exceeds `image_max_edge` (1568px by default) or their size exceeds `max_image_bytes`; the original and processed dimensions are both reported. Image detection uses file magic, not the extension. The original file limit for images is `max_image_source_bytes` (32MB by default), separate from `max_read_bytes`; `max_image_pixels` and `max_image_concurrency` bound decoding memory and parallelism.

An optional vision model (`tools.fs.vision`, off by default) can append a text description of each image, for clients that do not pass MCP image blocks to the model. It calls an OpenAI-compatible `/chat/completions` endpoint; the API key is read from the environment variable named by `api_key_env`.

An optional `query` tool (`tools.query.enabled`, off by default) runs a whitelist of read-only commands (ls, cat, grep, journalctl, systemctl status, docker ps, …) without triggering confirmations. Options, subcommands and find predicates are checked against explicit allowlists; anything that cannot be proven read-only is rejected with a hint to use `shell`.

## Upstream MCP passthrough

An optional `upstreams` list lets one runabout endpoint front other MCP servers. It is **off by default** (empty list). Each entry exposes a whitelist of the upstream's tools as `<prefix>_<tool>`; runabout does not interpret the upstream's business logic.

```yaml
upstreams:
  - name: example                      # unique; used in logs and error messages
    url: "http://127.0.0.1:8790/mcp"   # MCP Streamable HTTP endpoint
    token_env: "EXAMPLE_MCP_TOKEN"    # Bearer token env var; omit for no auth
    prefix: "ex"                       # exposed as ex_<tool>
    tools: ["list_items", "read_item"]  # required whitelist
    refresh_interval: 10m              # default 10m
    timeout: 60s                       # default 60s
    instructions: false                # append the upstream's initialize instructions

  - name: local                        # stdio upstream
    command: ["node", "/opt/example/server.mjs"]
    env: {EXAMPLE_MODE: "readonly"}    # literal values; see below for inherited vars
    workdir: "/opt/example"
    prefix: "lo"
    tools: ["search"]
```

- Each entry sets exactly one of `url` and `command`. Setting both, or neither, is rejected at startup.
- HTTP upstreams use Streamable HTTP; `token_env` names the environment variable holding the Bearer token. Tokens are read at request time; do not put credentials in the config file.
- stdio upstreams are spawned at startup and speak newline-delimited JSON-RPC on one stdin/stdout. Requests are multiplexed concurrently and matched by id. The child environment contains only `PATH`, `HOME`, `LANG` plus the literal values in `env`; nothing else is inherited. `workdir` must be an absolute path. stderr is logged line by line, with long lines truncated.
- If a stdio child exits, in-flight calls fail immediately. The child is restarted with the usual retry backoff and re-initialized before serving calls again. On shutdown runabout closes stdin, waits a few seconds for in-flight calls, then kills the process group.
- `instructions: true` stores the `instructions` from the upstream's `initialize` result and appends it to runabout's own instructions, under a header naming the upstream and its tool prefix (`<prefix>_<tool>`). Upstreams that have not connected yet are skipped. Each section is capped at 8 KiB; longer text is truncated and a warning is logged.
- The tool list is fetched in the background at startup and refreshed every `refresh_interval`. Descriptions, titles and `inputSchema` come from the upstream.
- Only whitelisted tools are exposed. The exposed name set is fixed by config, so `tools/list` stays stable even while the upstream is down.
- Tool definitions are size-bounded. A single `tools/list` page is capped at 4 MiB and all pages together at 32 MiB; each tool's name, title, description and `inputSchema` are checked as soon as the page is decoded (128 B / 1 KiB / 64 KiB / 256 KiB). A whitelisted tool that exceeds a limit is skipped for that refresh only: it keeps its previous definition (or a placeholder if there is none), other tools still update, and runabout logs one warning per tool instead of failing the whole refresh.
- `tools/call` is forwarded verbatim, and the upstream result is returned unchanged, including content blocks and fields runabout does not model.
- An unreachable upstream only makes its own tools return an error; runabout's built-in tools are unaffected.
- Passthrough calls are audited like any other tool call, under the prefixed tool name.

## Security model

runabout is not a sandbox. It is a **policy and access-control layer around a real Linux process**.

Security comes from several layers:

1. **OAuth 2.1 + PKCE** protects the remote MCP endpoint.
2. **Bearer authentication** can be used for static-token clients and automation.
3. **Shell policy** can deny or require confirmation for risky commands.
4. **Path guards** can deny reads/writes to selected paths for file tools (remote paths are matched literally).
5. **Confirmation tokens** allow an agent to stop and request approval before a risky action.
6. **Audit logging** records tool calls for later inspection.
7. **OS/container permissions** remain the final boundary.

Upstream passthrough adds two boundaries to be aware of:

- All passthrough tools share the single upstream token named by `token_env`. runabout does not scope that token per caller or per tool, and upstream tools are **not** constrained by `auth.static_tokens[].allowed_targets` or `auth.client_policies`.
- Upstream `annotations` are forwarded as-is. runabout does not verify `readOnlyHint`, and its shell and path policy does not apply to upstream calls; the upstream's own permissions are the boundary.
- stdio upstreams run as child processes with runabout's OS privileges. runabout narrows their environment to `PATH`, `HOME`, `LANG` plus `env`, but it is not a sandbox.

Do not treat runabout's policy as a replacement for Linux permissions. A process that can run unrestricted `sudo` can usually bypass application-level restrictions.

## Requirements

- Linux
- Go **1.25+** when building from source
- A public **HTTPS** origin when connecting from ChatGPT or another remote MCP client
- A reverse proxy or tunnel is recommended; runabout itself normally listens on loopback over plain HTTP

## Quick start

### Build

```bash
make build
make test
```

The binary is written to `bin/runabout`.

### Configure authentication

```bash
cp configs/config.example.yaml configs/config.yaml
./bin/runabout hash-password
```

Put the generated bcrypt hash in `auth.users[0].password_hash` and set:

```yaml
server:
  base_url: "https://runabout.example.com"
```

`server.base_url` must exactly match the public origin used by the MCP client. OAuth metadata and redirect validation depend on it.

### Start

```bash
./bin/runabout serve -c configs/config.yaml
```

The default listener is `127.0.0.1:8484`.

## Expose it over HTTPS

Keep runabout private and put TLS at a tunnel or reverse proxy.

### Cloudflare Tunnel

```bash
cloudflared tunnel --url http://127.0.0.1:8484
```

Configure the public hostname to forward to `http://127.0.0.1:8484` and use that HTTPS origin as `server.base_url`.

### Nginx

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

`proxy_buffering off` and a long read timeout are important for Streamable HTTP and long-running commands.

## Docker

The repository includes a Docker Compose deployment.

```bash
cd deploy
cp .env.example .env
cp ../configs/config.example.yaml config.yaml
docker compose up -d --build
```

Set at least `RB_BASE_URL` in `.env`. The container runs as the non-root `agent` user by default.

When bind-mounting a workspace, set `AGENT_UID` / `AGENT_GID` to match the owner of the files on the host.

The container intentionally includes a useful Linux userspace for agent operations. If passwordless sudo is enabled, remember that it is effectively root **inside the container**. Keep the container boundary meaningful by limiting mounts and capabilities.

OAuth state and audit data live in the `mcp-data` volume. Do not expose that volume to the agent's workspace.

## Bare-metal systemd

`deploy/runabout.service` provides a systemd unit. A typical installation is:

```text
/usr/local/bin/runabout
/etc/runabout/config.yaml
```

Then:

```bash
systemctl daemon-reload
systemctl enable --now runabout
```

Run the service as a dedicated user. Grant additional privileges through sudoers rather than running runabout itself as root.

## Connect an MCP client

The MCP endpoint is:

```text
https://your.domain/mcp
```

For an OAuth-capable client, use the endpoint and select OAuth. Dynamic client registration is enabled by default because clients such as ChatGPT can use it.

Supported/verified client shapes include:

- ChatGPT custom connectors / developer mode
- Claude Code with remote HTTP MCP
- Other clients implementing remote MCP + OAuth

Some clients send a bare `initialize` while validating that the URL is an MCP
server, and treat the resulting 401 as "not a valid MCP server" without ever
following the OAuth entry point advertised in `WWW-Authenticate`. Gemini custom
connectors behave this way (`User-Agent: Google`, server-side, no `Origin` and no
`Authorization`). Such clients need the handshake itself to be let through:

```yaml
auth:
  allow_unauthenticated_initialize: true
```

Only `initialize` and `notifications/initialized` pass anonymously; `tools/list`
and `tools/call` still require a Bearer token, because the auth middleware checks
every request and the anonymous session is rejected on the next hop. The cost is
that serverInfo (name, version) and the instructions text become readable without
authentication. The tool list is unaffected. Disabled by default.

For automation or a client that does not use OAuth, generate a static token:

```bash
./bin/runabout gen-token
```

Add it under `auth.static_tokens` and send:

```http
Authorization: Bearer <token>
```

## Health check and smoke test

Basic checks:

```bash
curl -s https://your.domain/healthz | jq
curl -s https://your.domain/.well-known/oauth-protected-resource | jq
```

The repository also ships an end-to-end smoke test:

```bash
./skills/runabout-deploy/smoke.sh https://your.domain [static-token]
```

It checks liveness, OAuth metadata, authentication challenges, CORS, the MCP handshake, and `tools/list`.

## Configuration

Environment variables override the YAML configuration:

```text
RB_LISTEN
RB_BASE_URL
RB_DATA_DIR
RB_USER
RB_PASSWORD_HASH
RB_STATIC_TOKEN
RB_AUTH_DISABLED
RB_ALLOW_ANON_INITIALIZE
RB_LOG_LEVEL
RB_LOG_FORMAT
```

`RB_CONFIG` can select the configuration file when `-c` is not supplied.

The most commonly changed settings are:

| Area | Configuration |
|---|---|
| Listener / public URL | `server.listen`, `server.base_url` |
| Login accounts | `auth.users`, `auth.session_cookie_ttl` |
| Client registration | `auth.allow_dynamic_registration`, `auth.registration_token`, `auth.allow_unauthenticated_initialize` |
| MCP tools | `tools.disabled` |
| Shell execution | `tools.shell.default_workdir`, timeouts, `max_background`, `env` |
| Fleet | `fleet.hosts`, `fleet.ssh_command`, `fleet.probe_timeout`, `fleet.approval_check` |
| Target scoping | `auth.static_tokens[].allowed_targets`, `auth.client_policies` |
| File access | `policy.write_deny_paths`, `policy.read_deny_paths`, `policy.deny_paths`, `policy.target_deny_paths` |
| Shell policy | `policy.disabled_shell_rules`, `policy.downgrade_to_confirm`, `policy.extra_shell_rules` |
| Extensions | `extensions.skill`, `extensions.fleetherd_task` (disabled by default) |
| Upstream passthrough | `upstreams` (disabled by default) |
| Audit | `audit.enabled`, `audit.log_args`, `audit.file` |
| Source disclosure | `server.source_url` |

### Example shell rule

```yaml
policy:
  extra_shell_rules:
    - id: "no-terraform-destroy"
      action: "confirm"
      reason: "destroys cloud resources"
      hint: "run terraform plan -destroy first"
      command: "terraform"
      arg_regex: '\bdestroy\b'
```

Use `deny` for commands that should never be allowed and `confirm` for commands that require an explicit approval step.

After changing policy rules:

```bash
make test-policy
```

### Optional extensions (`skill` & `fleetherd_task`)

Optional integration tools live under `extensions` and are **disabled by default** so the core tool surface stays minimal. When enabled, they reuse the fleet execution channel, output truncation, timeouts, `allowed_targets` checks, and audit logging:

- `extensions.skill`: exposes `skill` (`list | search | read`), bridging `fleetherd skills` (BM25 search and read) on the configured `target`.
- `extensions.fleetherd_task`: exposes `fleetherd_task` (`agents | new | send | read | list | status`) with strict action whitelisting, argv-safe execution, internal `web-bridge` mapping resolution for `session_ref` (never logging or returning `FLEETHERD_TURN_TOKEN`), and optional `empty_topic_command` for blank coordination topics when `session_ref` is absent.

## CLI

```text
runabout serve [-c config]        start the server
runabout hash-password [password] bcrypt hash for auth.users
runabout gen-token                generate a random static token
runabout check-policy 'command'   inspect shell policy decisions
runabout version                  print the version
```

`check-policy` returns exit status `2` for deny and `3` for confirm.

## Skills

The `skills/` directory contains reusable agent instructions for operating runabout:

- `runabout-deploy` — deployment and smoke testing
- `runabout-connect` — MCP connector setup
- `runabout-troubleshoot` — diagnosing connectivity and 502 failures
- `runabout-harden` — security hardening

See [skills/README.md](skills/README.md).

## Development

```bash
make fmt
make vet
make test
make check
make test-policy
make docker
```

The main packages are:

```text
cmd/runabout          CLI
internal/app          HTTP application assembly
internal/auth         OAuth 2.1 and bearer authentication
internal/mcp          JSON-RPC, MCP registry, Streamable HTTP
internal/tools        shell and agent tools
internal/fleet        local/ssh runners and remote file access
internal/policy       shell rules, path guards, confirmations
internal/audit        JSONL audit logging
internal/config       YAML configuration and validation
```

To add a tool, implement `mcp.Handler` (`Definition()` + `Call()`) and register it in `tools.All()`. Keep the default tool surface small; a new tool should provide a capability that cannot be expressed cleanly by the existing tools.

`configs/config.example.yaml` is parsed in strict mode by tests, so keep it synchronized with the configuration structs.

## Compatibility

- MCP protocol: `2025-06-18`, `2025-03-26`, `2024-11-05`
- Transport: Streamable HTTP
- MCP endpoint: `POST /mcp`, `GET /mcp`, `DELETE /mcp`
- OAuth: OAuth 2.1-style authorization code + PKCE, local token issuance, and RFC 7591 dynamic registration
- Architectures: amd64 and arm64

## License

[GNU AGPL v3 or later](LICENSE). Copyright (C) 2026 noir017.
