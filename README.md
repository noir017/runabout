# runabout

> The one who runs up and down.

[English](README.md) · [中文](README.zh.md)

runabout is an MCP server that exposes a Linux machine to agents that speak remote MCP: shell, files, search, and directory listing, behind an OAuth 2.1 authorization server over Streamable HTTP.

It was built so **ChatGPT on the web** can reach a personal server through a custom connector. Once connected, the chat is no longer limited to whatever you paste in — it can inspect the box, edit config, run deploys, and read logs, with policy intercepts and an audit trail on every call.

```
ChatGPT  ──HTTPS──>  reverse proxy / tunnel  ──>  runabout  ──>  this machine
                (OAuth 2.1 + PKCE)                    (policy + audit)
```

The same endpoint works for Claude Code (`claude mcp add --transport http`) and any other client that supports remote MCP plus OAuth. Client registration (RFC 7591), authorization-code + PKCE, token issue and rotation are all handled locally. There is no external IdP.

## What it is for

ChatGPT's custom connectors can only talk to a **remote HTTPS MCP** server. A homelab, a VPS, or the box under your desk is none of those things until something in front of it speaks that protocol. runabout is that something: listen on loopback, terminate TLS at a tunnel or reverse proxy, and let the model use the process identity you chose.

Typical setups:

- **Personal ops assistant.** Ask ChatGPT to check `systemctl`, tail a log, restart a unit, or apply a config change on the machine you already trust.
- **Homelab / self-hosted stack.** Point it at the host (or a container with the right mounts) that runs your compose projects, notes, or git checkouts.
- **Deploy from chat.** Have the model pull, build, and restart services you would otherwise SSH in for.
- **Other agents on the same pipe.** Use a static bearer token for curl, CI, or a local coding agent that already knows MCP, without going through the ChatGPT UI.

The process identity *is* the agent's identity. Running as root hands over the machine. Prefer a dedicated user, a container, and mounts that only cover what the assistant should touch.

## Requirements

- Go **1.25+** to build from source
- **Linux** only (process-group signals)
- A public **HTTPS** URL for ChatGPT (the binary itself serves plain HTTP on loopback)

## Build

```bash
make build     # writes bin/runabout
make test      # unit + e2e
make docker    # image with git, ripgrep, jq, python, compilers, …
```

`make docker` tags `runabout:$(git describe …)`. amd64 and arm64 both build; the image is debian-slim on purpose so the agent has a usable userspace.

Other targets: `make fmt`, `make vet`, `make check` (fmt + vet + test), `make test-policy`, `make clean`.

## Deploy

The service listens on `127.0.0.1:8484` by default. HTTPS, certificates, and the public hostname belong to the layer in front of it. `server.base_url` must be that public origin, **byte-for-byte** the same as the URL you give ChatGPT — OAuth metadata and redirect checks all key off it.

### 1. Config and password

```bash
cp configs/config.example.yaml configs/config.yaml
./bin/runabout hash-password          # interactive; prints a bcrypt hash
```

Put the hash in `auth.users[0].password_hash` and set `server.base_url` to `https://your.domain` (no trailing slash). That password is equivalent to shell on this host; do not use a weak one.

Every field has a default. Only write what you change. Environment variables override the file: `RB_LISTEN`, `RB_BASE_URL`, `RB_DATA_DIR`, `RB_USER`, `RB_PASSWORD_HASH`, `RB_STATIC_TOKEN`, `RB_AUTH_DISABLED`, `RB_LOG_LEVEL`, `RB_LOG_FORMAT`. `RB_CONFIG` selects the YAML path if `-c` is omitted (`/etc/runabout/config.yaml` is also tried).

### 2. Docker

```bash
cd deploy
cp .env.example .env                  # at least RB_BASE_URL; AGENT_UID = workspace owner on the host
cp ../configs/config.example.yaml config.yaml
docker compose up -d --build
```

`.env` and `config.yaml` are gitignored (domain, password hash, tokens).

The container runs as non-root `agent`. `AGENT_UID` / `AGENT_GID` should match the owner of bind-mounted workspace files. Passwordless sudo is on by default so the assistant can `apt install` missing tools — inside the container that is root; the boundary is still the container. Set `SUDO_NOPASSWD=false` and rebuild to drop it, then you can enable the commented `no-new-privileges` in compose (that option and passwordless sudo cannot be used together).

Compose expects an external ingress network (default name `cloudflared`) so a tunnel or reverse proxy can reach the container by name. Create it once:

```bash
docker network create cloudflared
docker network connect cloudflared <cloudflared-container>
```

Point the tunnel's public hostname at `http://runabout:8484`, not `127.0.0.1:8484` (that loopback is the *tunnel* container). If you are not using a shared ingress network, remove `ingress` from both `services.mcp.networks` and the `networks:` block.

OAuth state and audit logs live in the `mcp-data` volume (`/var/lib/runabout`). Do not bind-mount that directory into the workspace — the agent must not be able to rewrite the token store.

### 3. Bare metal

`deploy/runabout.service` is a systemd unit. Install the binary to `/usr/local/bin/runabout`, the config to `/etc/runabout/config.yaml`, create a dedicated `agent` user, then:

```bash
systemctl daemon-reload
systemctl enable --now runabout
```

Grant extra privileges through sudoers, not by running the unit as root. The unit sets `NoNewPrivileges=true`; do not turn on `ProtectHome` / `ProtectSystem=strict` or the assistant cannot do its job.

### 4. Public HTTPS

ChatGPT will only connect to HTTPS.

**Cloudflare Tunnel** (no open port, no local cert):

```bash
cloudflared tunnel --url http://127.0.0.1:8484
```

**Nginx** (disable buffering for SSE; raise the read timeout for long jobs):

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

## Use

```bash
./bin/runabout serve -c configs/config.yaml
```

Open `https://your.domain/` for a short landing page (MCP URL, source link, a curl snippet).

### Clients

Give the client `https://your.domain/mcp` and select OAuth. Dynamic registration is on by default (`auth.allow_dynamic_registration`); ChatGPT depends on it. Set `auth.registration_token` if you want `/oauth/register` to require a bearer token.

Verified shapes:

- ChatGPT custom connector (developer mode)
- Claude Code: `claude mcp add --transport http`
- Anything else that speaks remote MCP + OAuth

For curl or a non-OAuth agent, add an entry under `auth.static_tokens` (`runabout gen-token` generates one) and send `Authorization: Bearer …`.

Smoke:

```bash
./skills/runabout-deploy/smoke.sh https://your.domain [static-token]
```

Walks every step a client takes on connect — liveness, OAuth metadata, the 401 challenge,
the CORS preflight, the MCP handshake, `tools/list` — and exits non-zero on any failure.
Needs only curl, grep and sed. The two quick checks by hand:

```bash
curl -s https://your.domain/healthz | jq
curl -s https://your.domain/.well-known/oauth-protected-resource | jq
```

### Skills

`skills/` ships agent skills for deploying and operating this thing: `runabout-deploy`,
`runabout-connect`, `runabout-troubleshoot`, `runabout-harden`. Symlink them into
`.claude/skills/` and an agent can do the deploy, the connector wiring, and the 502
post-mortem with the gotchas already loaded. See [skills/README.md](skills/README.md).

### CLI

```
runabout serve [-c config]        start the server
runabout hash-password [password] bcrypt hash for auth.users
runabout gen-token                random static token
runabout check-policy 'command'   how the shell policy would treat a command
runabout version
```

`check-policy` loads extra rules from `-c` if you pass it. Exit status 2 is deny, 3 is confirm.

## Modify

Most changes are configuration. Architecture, threat model, and the rationale for each trade-off live in [DESIGN.md](DESIGN.md).

### Config you will actually touch

| Area | What to change |
|---|---|
| Listen / public URL | `server.listen`, `server.base_url`, `RB_*` |
| Who can log in | `auth.users`, `auth.session_cookie_ttl` |
| Who can register clients | `auth.allow_dynamic_registration`, `auth.registration_token` |
| Disable a tool | `tools.disabled` can further disable any of the 7 core tools (for example `["glob"]`) |
| Shell sandbox-ish knobs | `tools.shell.default_workdir`, timeouts, `max_background`, extra `env` |
| Path denylist | `policy.write_deny_paths`, `policy.read_deny_paths` (file tools only; shell is not bound by these) |
| Shell rules | `policy.disabled_shell_rules`, `policy.downgrade_to_confirm`, `policy.extra_shell_rules` |
| Audit | `audit.enabled`, `audit.log_args`, `audit.file` |
| AGPL source link | `server.source_url` — point this at *your* repo if you ship a modified build |

Example extra rule:

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

`action` is `deny` or `confirm`. Rule ids for builtins show up in `runabout check-policy`. After changing policy tests, run `make test-policy`.

### Code

```
cmd/runabout          CLI: serve / hash-password / gen-token / check-policy
internal/app          HTTP assembly (Build is used by e2e tests)
internal/auth         OAuth 2.1 + Bearer middleware
internal/mcp          JSON-RPC, tool registry, Streamable HTTP
internal/tools        shell / files / search implementations
internal/policy       shell AST, risk rules, path denylist, confirm tokens
internal/audit        JSONL audit log
internal/config       YAML, defaults, validation
```

Extension points:

- **New tool.** Implement `mcp.Handler` (`Definition()` + `Call()`) and register it in `tools.All()`.
- **New builtin shell rule.** Add it in `builtinCmdRules()` with a clear `reason` and a `hint` the model can act on. Prefer `extra_shell_rules` when `command` + `arg_regex` is enough.
- **Different IdP.** Replace `auth.Server.Protect`; the rest of the stack stays.

`configs/config.example.yaml` is parsed in strict mode in tests — keep it in sync with the struct.

If you modify the code and let other people use the service over the network, AGPL §13 requires that those users can obtain your modified source. The landing page and `/healthz` show `server.source_url`; set it to your repository. Self-hosting for yourself does not trigger that clause.

## Compatibility

- MCP protocol `2025-06-18` (also `2025-03-26`, `2024-11-05`)
- Transport: Streamable HTTP — `POST /mcp` submit, `GET /mcp` SSE, `DELETE /mcp` end session
- Dependencies (all AGPL-compatible): `mvdan.cc/sh`, `golang.org/x/crypto`, `golang.org/x/term`, `gopkg.in/yaml.v3`. Protocol and OAuth are implemented in-tree.

## License

[GNU AGPL v3 or later](LICENSE). Copyright (C) 2026 noir017.
