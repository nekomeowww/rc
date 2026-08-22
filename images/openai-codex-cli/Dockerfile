# syntax=docker/dockerfile:1.7

ARG RUNNER_IMAGE=ghcr.io/nekomeowww/rc/runner:latest

FROM node:26-bookworm-slim AS codex

ARG CODEX_VERSION=0.147.0

RUN npm install --global --prefix /opt/openai "@openai/codex@${CODEX_VERSION}" \
    && npm cache clean --force \
    && /opt/openai/bin/codex --version

FROM ${RUNNER_IMAGE}

ARG CODEX_VERSION=0.147.0

LABEL org.opencontainers.image.source="https://github.com/nekomeowww/rc" \
      org.opencontainers.image.title="OpenAI Codex CLI environment" \
      org.opencontainers.image.version="${CODEX_VERSION}"

COPY --from=codex /usr/local/bin/node /usr/local/bin/node
COPY --from=codex /opt/openai /opt/openai

ENV PATH="/opt/openai/bin:${PATH}"

ENTRYPOINT ["codex"]
