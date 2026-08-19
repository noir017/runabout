# ---------- 构建 ----------
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/noir017/agent-tools-mcp/internal/app.Version=${VERSION}" \
      -o /out/agent-tools-mcp ./cmd/agent-tools-mcp

# ---------- 运行 ----------
# 这个服务的价值就在于容器里工具齐全，所以刻意不用 scratch/alpine：
# agent 需要 git、rg、jq、python、编译器这些才干得了活。
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git openssh-client \
      ripgrep fd-find jq less tree file procps psmisc lsof \
      bash coreutils findutils diffutils patch tar gzip zip unzip xz-utils \
      python3 python3-pip python3-venv \
      build-essential pkg-config \
      sqlite3 rsync netcat-openbsd dnsutils iputils-ping vim-tiny tini \
    && ln -sf /usr/bin/fdfind /usr/local/bin/fd \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/agent-tools-mcp /usr/local/bin/agent-tools-mcp

# 非 root 运行：agent 拿到的权限就是这个用户的权限，需要更多权限时
# 自己在 compose 里换 user 或加 sudoers，别默认给 root。
RUN useradd -m -s /bin/bash -u 10001 agent \
    && mkdir -p /workspace /var/lib/agent-tools-mcp \
    && chown -R agent:agent /workspace /var/lib/agent-tools-mcp

USER agent
WORKDIR /workspace

ENV ATM_LISTEN=0.0.0.0:8484 \
    ATM_DATA_DIR=/var/lib/agent-tools-mcp \
    PATH=/usr/local/bin:/usr/bin:/bin:/home/agent/.local/bin

EXPOSE 8484
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD curl -fsS http://127.0.0.1:8484/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/agent-tools-mcp"]
CMD ["serve", "-c", "/etc/agent-tools-mcp/config.yaml"]
