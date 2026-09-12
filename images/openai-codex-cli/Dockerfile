# syntax=docker/dockerfile:1.7

ARG RUNNER_IMAGE=ghcr.io/nekomeowww/rc/runner:latest

FROM node:26-bookworm-slim AS codex

ARG BUN_VERSION=1.4.0
ARG CODEX_VERSION=0.147.0
ARG PNPM_VERSION=10.26.2

RUN npm install --global --prefix /opt/openai \
        "@openai/codex@${CODEX_VERSION}" \
        "bun@${BUN_VERSION}" \
        "pnpm@${PNPM_VERSION}" \
    && npm cache clean --force \
    && test "$(/opt/openai/bin/bun --version)" = "${BUN_VERSION}" \
    && test "$(/opt/openai/bin/codex --version)" = "codex-cli ${CODEX_VERSION}" \
    && test "$(/opt/openai/bin/pnpm --version)" = "${PNPM_VERSION}"

FROM ${RUNNER_IMAGE}

ARG CODEX_VERSION=0.147.0

LABEL org.opencontainers.image.source="https://github.com/nekomeowww/rc" \
      org.opencontainers.image.title="OpenAI Codex CLI environment" \
      org.opencontainers.image.version="${CODEX_VERSION}"

COPY --from=codex /usr/local/bin/node /usr/local/bin/node
COPY --from=codex /opt/openai /opt/openai

# Debian login shells reset PATH, so expose the pinned tools from a standard
# system path used by both interactive shells and Codex child processes. Keep
# pnpm's content-addressable store outside the repository so development file
# watchers do not traverse it. The previous persistent-home store is replaced
# by rebuildable local storage; pair it with a per-Worktree local virtual store.
USER root
# RUNNER_IMAGE is overridable, so keep the native build toolchain explicit for
# older or custom Debian-compatible runner images.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        cmake \
        pkg-config \
        python3 \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sfn /opt/openai/bin/bun /usr/local/bin/bun \
    && ln -sfn /opt/openai/bin/bunx /usr/local/bin/bunx \
    && ln -sfn /opt/openai/bin/codex /usr/local/bin/codex \
    && ln -sfn /opt/openai/bin/pnpm /usr/local/bin/pnpm \
    && ln -sfn /opt/openai/bin/pnpx /usr/local/bin/pnpx \
    && install -d -m 0700 -o agent -g agent /tmp/cache /tmp/cache/pnpm /tmp/cache/pnpm/store /tmp/cache/pnpm/virtual \
    && install -d /usr/local/etc \
    && printf '%s\n' 'store-dir=/tmp/cache/pnpm/store' >> /usr/local/etc/npmrc \
    && test "$(/opt/openai/bin/pnpm config get store-dir)" = "/tmp/cache/pnpm/store"
USER 1000:1000

# Repeat the cache default for older/custom RUNNER_IMAGE bases. pnpm 11+ no
# longer reads store settings from npmrc; use explicit install flags as documented
# in skills/use-rc/SKILL.md (https://pnpm.io/settings).
ENV PATH="/opt/openai/bin:${PATH}" \
    XDG_CACHE_HOME="/tmp/cache"

RUN test -w /tmp/cache/pnpm/store && test -w /tmp/cache/pnpm/virtual

ENTRYPOINT ["codex"]
