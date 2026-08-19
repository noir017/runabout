# runabout 运行镜像。
#
# 交叉说明：镜像本身与架构无关（apt 包 + 纯 Go 编译），amd64 / arm64 都能直接构建。
# 构建：docker compose -f deploy/docker-compose.yml build
#
# ---------- 构建 ----------
FROM golang:1.25-bookworm AS build
WORKDIR /src

# 先只拷依赖清单，改代码不必重下模块。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
ARG VERSION=dev
ARG TARGETARCH
# 缓存挂载让重复构建只编译改动部分；单核机器上这一条省下的时间最明显。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/noir017/runabout/internal/app.Version=${VERSION}" \
      -o /out/runabout ./cmd/runabout \
    && /out/runabout version

# ---------- 运行 ----------
# 这个服务的价值就在于容器里工具齐全，所以刻意不用 scratch/alpine：
# agent 需要 git、rg、jq、python、编译器这些才干得了活。
FROM debian:bookworm-slim

ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git openssh-client \
      ripgrep fd-find jq less tree file procps psmisc lsof \
      bash coreutils findutils diffutils patch tar gzip zip unzip xz-utils \
      python3 python3-pip python3-venv \
      build-essential pkg-config \
      sqlite3 rsync netcat-openbsd dnsutils iputils-ping vim-tiny \
      sudo tini \
    && ln -sf /usr/bin/fdfind /usr/local/bin/fd \
    && rm -rf /var/lib/apt/lists/*

# 工作区大概率是宿主机 bind mount，属主 uid 和容器里未必一致；
# 不加这条，agent 在里面跑任何 git 命令都会撞上 "dubious ownership"。
RUN git config --system --add safe.directory '*'

COPY --from=build /out/runabout /usr/local/bin/runabout
COPY LICENSE NOTICE /usr/share/doc/runabout/

# 非 root 运行：agent 拿到的权限就是这个用户的权限。
# uid/gid 做成参数，好和宿主机上工作区目录的属主对齐，省去一堆 chown。
ARG AGENT_UID=10001
ARG AGENT_GID=10001
# SUDO_NOPASSWD=true 时给 agent 免密 sudo：容器内提权 == 容器内 root，
# 边界仍是容器本身，换来的是助理能自己 apt install 缺的工具。
# 注意：开了这个就不能再在 compose 里加 no-new-privileges，否则 sudo 直接失效。
ARG SUDO_NOPASSWD=true
RUN groupadd -g "${AGENT_GID}" agent \
    && useradd -m -s /bin/bash -u "${AGENT_UID}" -g "${AGENT_GID}" agent \
    && mkdir -p /workspace /var/lib/runabout \
    && chown -R agent:agent /workspace /var/lib/runabout \
    && if [ "${SUDO_NOPASSWD}" = "true" ]; then \
         echo 'agent ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/agent \
         && chmod 0440 /etc/sudoers.d/agent \
         && visudo -cf /etc/sudoers.d/agent; \
       fi

USER agent
WORKDIR /workspace

ENV RB_LISTEN=0.0.0.0:8484 \
    RB_DATA_DIR=/var/lib/runabout \
    PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/home/agent/.local/bin \
    LANG=C.UTF-8 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

EXPOSE 8484
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8484/healthz >/dev/null || exit 1

LABEL org.opencontainers.image.title="runabout" \
      org.opencontainers.image.description="MCP server exposing shell/file tools to agents" \
      org.opencontainers.image.source="https://github.com/noir017/runabout" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/runabout"]
CMD ["serve", "-c", "/etc/runabout/config.yaml"]
