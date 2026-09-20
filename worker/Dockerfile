# Worker image: the pinned Claude Code CLI plus the toolchains the task kinds
# need, running as a non-root user (design §4.1, plan Step 14).
#
#   docker build -t harness/worker:cli-2.1.270 --build-arg CLI_VERSION=2.1.270 worker/
#
# The harness bind-mounts the workspace at /workspace and an empty per-run
# CLAUDE_CONFIG_DIR at /run/claude-config; credentials arrive through the
# environment only. Egress goes through HTTPS_PROXY (deploy/docker-compose.yaml).
ARG NODE_IMAGE=node:22-bookworm-slim
ARG GO_IMAGE=golang:1.27-bookworm

FROM ${GO_IMAGE} AS go

FROM ${NODE_IMAGE}
ARG CLI_VERSION=2.1.270
ARG WORKER_UID=1000

# Debian ships uid 1000 as "node" in the node image; reuse it as the worker.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates curl openssh-client \
 && rm -rf /var/lib/apt/lists/* \
 && npm install -g "@anthropic-ai/claude-code@${CLI_VERSION}" \
 && npm cache clean --force \
 && usermod -l worker -d /home/worker -m node \
 && groupmod -n worker node \
 && mkdir -p /workspace /run/claude-config /home/worker \
 && chown -R ${WORKER_UID}:${WORKER_UID} /workspace /run/claude-config /home/worker \
 # Any uid may run the container (--user on Linux hosts): HOME must stay writable.
 && chmod 0777 /home/worker /run/claude-config /workspace \
 # Bind-mounted checkouts are owned by the host user; git must not refuse them.
 && git config --system safe.directory '*' \
 && git config --system user.name harness-worker \
 && git config --system user.email harness-worker@users.noreply.github.com

# Go toolchain for code_fix tasks on Go repositories.
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/home/worker/go/bin:$PATH \
    GOPATH=/home/worker/go \
    GOFLAGS=-mod=mod \
    HOME=/home/worker \
    CLAUDE_CONFIG_DIR=/run/claude-config \
    CLAUDE_CODE_DISABLE_AUTOUPDATER=1 \
    DISABLE_AUTOUPDATER=1 \
    DISABLE_TELEMETRY=1 \
    CI=1

USER ${WORKER_UID}
WORKDIR /workspace
# No ENTRYPOINT: the harness passes the full argv (`claude -p …` or an acceptance command).
CMD ["claude", "--version"]
