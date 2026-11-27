# syntax=docker/dockerfile:1.29

# ------------------------------------------------------------------------------
# Stage 1: Build rcctl and rc-kube binaries
# ------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27.4 AS rc-binaries

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN <<'EOF'
set -eu
CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH}" \
  go build -trimpath -o /out/rcctl ./cmd/rcctl
CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH}" \
  go build -trimpath -o /out/rc-kube ./cmd/rc-kube
EOF

# ------------------------------------------------------------------------------
# Stage 2: Download and verify version managers (mise, proto)
# ------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS tools

ARG TARGETARCH
# renovate: datasource=github-releases depName=jdx/mise extractVersion=^v(?<version>.*)$
ARG MISE_VERSION=2026.11.14
# renovate: datasource=github-releases depName=moonrepo/proto extractVersion=^v(?<version>.*)$
ARG PROTO_VERSION=0.64.1

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
    && rm -rf /var/lib/apt/lists/*

RUN --mount=type=bind,source=images/scripts/install-mise-proto.sh,target=/tmp/install-mise-proto.sh \
    bash /tmp/install-mise-proto.sh

# ------------------------------------------------------------------------------
# Stage 3: Final Linux Workspace runner image
# ------------------------------------------------------------------------------
FROM node:26.13.1-bookworm-slim AS runner

# renovate: datasource=npm depName=corepack
ARG COREPACK_VERSION=0.36.0
# renovate: datasource=npm depName=@lobehub/cli
ARG LOBEHUB_CLI_VERSION=0.0.271

LABEL org.opencontainers.image.source="https://github.com/nekomeowww/rc" \
      org.opencontainers.image.title="rc runner"

# Install system packages. Playwright Chromium runtime dependencies are baked in
# because unprivileged Workspace processes cannot elevate privileges at runtime.
RUN bash -eu <<'EOF'
packages=(
  # Core CLI, shell, archive, and process utilities
  bash btop ca-certificates curl gh git git-lfs gzip htop
  openssh-client openssh-sftp-server procps ripgrep sudo tar tini tmux
  unzip wget xz-utils zip

  # Native build toolchain
  build-essential cmake pkg-config python3

  # Fonts (Latin, CJK, Emoji)
  fonts-freefont-ttf fonts-ipafont-gothic fonts-liberation
  fonts-noto-color-emoji fonts-tlwg-loma-otf fonts-unifont fonts-wqy-zenhei
  xfonts-scalable

  # Playwright / Chromium headless runtime libraries
  libasound2 libatk-bridge2.0-0 libatk1.0-0 libatomic1 libatspi2.0-0
  libcairo2 libcups2 libdbus-1-3 libdrm2 libfontconfig1 libfreetype6
  libgbm1 libglib2.0-0 libnspr4 libnss3 libpango-1.0-0
  libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3
  libxkbcommon0 libxrandr2 xvfb
)
apt-get update
apt-get install -y --no-install-recommends "${packages[@]}"
gh --version
rm -rf /var/lib/apt/lists/*
EOF

# Install pinned global npm CLIs and verify their entrypoints.
RUN <<'EOF'
set -eu
npm install --global --ignore-scripts \
  "corepack@${COREPACK_VERSION}" \
  "@lobehub/cli@${LOBEHUB_CLI_VERSION}"
test "$(lh --version)" = "${LOBEHUB_CLI_VERSION}"
test "$(lobe --version)" = "${LOBEHUB_CLI_VERSION}"
test "$(lobehub --version)" = "${LOBEHUB_CLI_VERSION}"
rm -rf /tmp/lobehub
npm cache clean --force
EOF

# Configure unprivileged runtime users and container-local cache directory.
# Keep rebuildable tool caches on container-local storage, not the home PVC.
RUN <<'EOF'
set -eu
usermod --login agent --home /home/agent --move-home --shell /bin/bash node
groupmod --new-name agent node
groupadd --gid 65532 rc
useradd --uid 65532 --gid 65532 --create-home --shell /bin/bash rc
git config --system --add safe.directory '*'
install -d -m 0700 -o 1000 -g 1000 /tmp/cache
EOF

# Linux runtime Pods run the rc-kube and tini injected by the controller from
# the rc-kube image. The copies below serve tools that invoke the runner image
# directly (for example, rcctl reading logs of a suspended Workspace).
COPY --from=rc-binaries /out/rcctl /usr/local/bin/rcctl
COPY --from=rc-binaries /out/rc-kube /usr/local/bin/rc-kube
COPY --from=tools /out/mise /usr/local/bin/mise
COPY --from=tools /out/proto /usr/local/bin/proto
COPY --from=tools /out/proto-shim /usr/local/bin/proto-shim

ENV SHELL="/bin/bash" \
    XDG_CACHE_HOME="/tmp/cache"

USER 1000:1000
RUN test -w /tmp/cache
WORKDIR /workspace

ENTRYPOINT ["rc-kube"]
