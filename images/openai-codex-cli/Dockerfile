# syntax=docker/dockerfile:1.7

FROM node:26-trixie-slim

ARG CODEX_VERSION=0.147.0

LABEL org.opencontainers.image.source="https://github.com/nekomeowww/rc" \
      org.opencontainers.image.title="OpenAI Codex CLI" \
      org.opencontainers.image.version="${CODEX_VERSION}"

# Keep the runtime focused on the CLI while retaining the tools Codex needs to
# inspect a mounted Git worktree and establish TLS connections.
RUN apt-get update \
    && apt-get install --yes --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && npm install --global --prefix /opt/openai "@openai/codex@${CODEX_VERSION}" \
    && npm cache clean --force \
    && /opt/openai/bin/codex --version

ENV PATH="/opt/openai/bin:${PATH}"

RUN mkdir /workspace && chown node:node /workspace

USER node
WORKDIR /workspace

ENTRYPOINT ["codex"]
