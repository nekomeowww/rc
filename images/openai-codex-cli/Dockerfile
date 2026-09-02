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
# pnpm's content-addressable store in the persistent home volume: when the home
# and Worktree use different filesystems, pnpm otherwise places the store inside
# the repository and development file watchers traverse it.
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends g++ \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sfn /opt/openai/bin/bun /usr/local/bin/bun \
    && ln -sfn /opt/openai/bin/bunx /usr/local/bin/bunx \
    && ln -sfn /opt/openai/bin/codex /usr/local/bin/codex \
    && ln -sfn /opt/openai/bin/pnpm /usr/local/bin/pnpm \
    && ln -sfn /opt/openai/bin/pnpx /usr/local/bin/pnpx \
    && install -d -o agent -g agent /home/agent/.local/share/pnpm/store \
    && install -d /usr/local/etc \
    && printf '%s\n' 'store-dir=/home/agent/.local/share/pnpm/store' >> /usr/local/etc/npmrc \
    && test "$(/opt/openai/bin/pnpm config get store-dir)" = "/home/agent/.local/share/pnpm/store"
USER 1000:1000

ENV PATH="/opt/openai/bin:${PATH}"

ENTRYPOINT ["codex"]
