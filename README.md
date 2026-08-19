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

## Seven core tools

The default MCP surface is intentionally limited to **7 tools**. Some older implementations still exist in the source tree, but they are not exposed by default.

| Tool | Purpose |
|---|---|
| `shell` | Run a Bash command in the configured environment; supports foreground and background jobs. |
| `shell_output` | Read captured stdout/stderr from a background job. |
| `shell_kill` | Send a signal to a background job and terminate it. |
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

## Security model

runabout is not a sandbox. It is a **policy and access-control layer around a real Linux process**.

Security comes from several layers:

1. **OAuth 2.1 + PKCE** protects the remote MCP endpoint.
2. **Bearer authentication** can be used for static-token clients and automation.
3. **Shell policy** can deny or require confirmation for risky commands.
4. **Path guards** can deny reads/writes to selected paths for file tools.
5. **Confirmation tokens** allow an agent to stop and request approval before a risky action.
6. **Audit logging** records tool calls for later inspection.
7. **OS/container permissions** remain the final boundary.

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
RB_LOG_LEVEL
RB_LOG_FORMAT
```

`RB_CONFIG` can select the configuration file when `-c` is not supplied.

The most commonly changed settings are:

| Area | Configuration |
|---|---|
| Listener / public URL | `server.listen`, `server.base_url` |
| Login accounts | `auth.users`, `auth.session_cookie_ttl` |
| Client registration | `auth.allow_dynamic_registration`, `auth.registration_token` |
| MCP tools | `tools.disabled` |
| Shell execution | `tools.shell.default_workdir`, timeouts, `max_background`, `env` |
| File access | `policy.write_deny_paths`, `policy.read_deny_paths` |
| Shell policy | `policy.disabled_shell_rules`, `policy.downgrade_to_confirm`, `policy.extra_shell_rules` |
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
internal/policy       shell rules, path guards, confirmations
internal/audit        JSONL audit logging
internal/config       YAML configuration and validation
```

To add a tool, implement `mcp.Handler` (`Definition()` + `Call()`) and register it in `tools.All()`. Keep the default tool surface small; a new tool should provide a capability that cannot be expressed cleanly by the existing seven.

`configs/config.example.yaml` is parsed in strict mode by tests, so keep it synchronized with the configuration structs.

## Compatibility

- MCP protocol: `2025-06-18`, `2025-03-26`, `2024-11-05`
- Transport: Streamable HTTP
- MCP endpoint: `POST /mcp`, `GET /mcp`, `DELETE /mcp`
- OAuth: OAuth 2.1-style authorization code + PKCE, local token issuance, and RFC 7591 dynamic registration
- Architectures: amd64 and arm64

## License

[GNU AGPL v3 or later](LICENSE). Copyright (C) 2026 noir017.
