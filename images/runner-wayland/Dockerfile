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
# Stage 3: Node.js runtime layer
# ------------------------------------------------------------------------------
FROM node:26.13.1-bookworm-slim AS node-runtime

# ------------------------------------------------------------------------------
# Stage 4: Build patched xdg-desktop-portal-wlr (0.8.4 + shm/build patches)
# ------------------------------------------------------------------------------
FROM ubuntu:24.04 AS xdpw-builder

ARG XDPW_VERSION=0.8.4
ARG XDPW_SHA256=3122966d46ab108f505525bcb2498f9121b446ee8438fbfceb73a7a1fa1ad400

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential ca-certificates curl libdrm-dev libgbm-dev libinih-dev \
        libpipewire-0.3-dev libsystemd-dev libwayland-dev meson ninja-build \
        pkg-config wayland-protocols \
    && rm -rf /var/lib/apt/lists/*

COPY images/runner-wayland/patches/xdg-desktop-portal-wlr-0.8.4-*.patch /tmp/

RUN <<'EOF'
set -eux
curl -fsSLo /tmp/xdg-desktop-portal-wlr.tar.gz \
  "https://github.com/emersion/xdg-desktop-portal-wlr/archive/refs/tags/v${XDPW_VERSION}.tar.gz"
echo "${XDPW_SHA256}  /tmp/xdg-desktop-portal-wlr.tar.gz" | sha256sum --check --strict

mkdir -p /tmp/xdg-desktop-portal-wlr
tar -xzf /tmp/xdg-desktop-portal-wlr.tar.gz -C /tmp/xdg-desktop-portal-wlr --strip-components=1
patch --directory=/tmp/xdg-desktop-portal-wlr --fuzz=0 --strip=1 < /tmp/xdg-desktop-portal-wlr-0.8.4-build.patch
patch --directory=/tmp/xdg-desktop-portal-wlr --fuzz=0 --strip=1 < /tmp/xdg-desktop-portal-wlr-0.8.4-shm.patch

meson setup /tmp/xdg-desktop-portal-wlr/build /tmp/xdg-desktop-portal-wlr \
  --buildtype=release \
  --prefix=/usr \
  --libexecdir=libexec \
  -Dman-pages=disabled \
  -Dsystemd=disabled
meson compile -C /tmp/xdg-desktop-portal-wlr/build
install -Dm755 /tmp/xdg-desktop-portal-wlr/build/xdg-desktop-portal-wlr /out/xdg-desktop-portal-wlr
EOF

# ------------------------------------------------------------------------------
# Stage 5: Final Wayland Workspace runner image
# ------------------------------------------------------------------------------
FROM ubuntu:24.04 AS runner-wayland

ARG TARGETARCH
# renovate: datasource=npm depName=corepack
ARG COREPACK_VERSION=0.36.0
# renovate: datasource=npm depName=@lobehub/cli
ARG LOBEHUB_CLI_VERSION=0.0.271
# renovate: datasource=github-releases depName=moeru-ai/auv extractVersion=^v(?<version>.*)$
ARG AUV_VERSION=0.0.43
# renovate: datasource=github-releases depName=just-containers/s6-overlay versioning=loose extractVersion=^v(?<version>.*)$
ARG S6_OVERLAY_VERSION=3.2.3.2

LABEL org.opencontainers.image.source="https://github.com/nekomeowww/rc" \
      org.opencontainers.image.title="rc Wayland runner"

COPY --from=node-runtime /usr/local/ /usr/local/

RUN bash -eu <<'EOF'
packages=(
  # Core CLI, shell, archive, and process utilities
  bash btop ca-certificates curl git git-lfs gzip htop
  openssh-client openssh-sftp-server procps ripgrep sudo tar tmux
  unzip wget xz-utils zip

  # Native build toolchain
  build-essential cmake pkg-config python3

  # Fonts (Latin, CJK, Emoji)
  fonts-dejavu-core fonts-freefont-ttf fonts-ipafont-gothic fonts-liberation
  fonts-noto-color-emoji fonts-unifont fonts-wqy-zenhei

  # Wayland / Sway / PipeWire / Desktop Portal stack
  dbus-bin dbus-daemon grim mesa-utils pipewire sway wireplumber
  xdg-desktop-portal xdg-desktop-portal-wlr

  # Chromium / GTK / OCR runtime libraries
  libasound2t64 libatk-bridge2.0-0t64 libatk1.0-0t64 libatomic1
  libatspi2.0-0t64 libcairo2 libcups2t64 libdbus-1-3 libdrm2 libfontconfig1
  libfreetype6 libgbm1 libglib2.0-0t64 libglib2.0-bin libgtk-3-0t64
  liblept5 libnspr4 libnss3 libpango-1.0-0 libtesseract5
  libx11-6 libxcb1 libxcomposite1 libxdamage1 libxext6 libxfixes3
  libxkbcommon0 libxrandr2
)
apt-get update
apt-get install -y --no-install-recommends "${packages[@]}"
rm -rf /var/lib/apt/lists/*
EOF

RUN <<'EOF'
set -eu
npm install --global --ignore-scripts \
  "corepack@${COREPACK_VERSION}" \
  "@lobehub/cli@${LOBEHUB_CLI_VERSION}"
test "$(lh --version)" = "${LOBEHUB_CLI_VERSION}"
rm -rf /tmp/lobehub
npm cache clean --force
EOF

RUN <<'EOF'
set -eu
usermod --login agent --home /home/agent --move-home --shell /bin/bash ubuntu
groupmod --new-name agent ubuntu
groupadd --gid 65532 rc
useradd --uid 65532 --gid 65532 --create-home --shell /bin/bash rc
git config --system --add safe.directory '*'
EOF

RUN --mount=type=bind,source=images/runner-wayland/scripts/install-wayland-tools.sh,target=/tmp/install-wayland-tools.sh \
    bash /tmp/install-wayland-tools.sh

COPY --from=rc-binaries /out/rcctl /usr/local/bin/rcctl
COPY --from=rc-binaries /out/rc-kube /usr/local/libexec/rc-kube
COPY --from=tools /out/mise /usr/local/bin/mise
COPY --from=tools /out/proto /usr/local/bin/proto
COPY --from=tools /out/proto-shim /usr/local/bin/proto-shim
COPY --from=xdpw-builder /out/xdg-desktop-portal-wlr /usr/libexec/xdg-desktop-portal-wlr
COPY images/runner-wayland/rootfs/ /

RUN --mount=type=bind,source=images/runner-wayland/scripts/setup-s6.sh,target=/tmp/setup-s6.sh \
    bash /tmp/setup-s6.sh

ENV DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/1000/bus" \
    HOME="/home/agent" \
    PATH="/opt/auv:/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin" \
    RC_KUBE_INIT="/usr/local/libexec/rc-wayland/init" \
    RC_WAYLAND_DISPLAY="rc-wayland" \
    S6_CMD_WAIT_FOR_SERVICES="1" \
    S6_KEEP_ENV="1" \
    SHELL="/bin/bash" \
    WAYLAND_DISPLAY="rc-wayland" \
    XDG_CONFIG_HOME="/run/rc-wayland/config" \
    XDG_CURRENT_DESKTOP="sway" \
    XDG_RUNTIME_DIR="/run/user/1000" \
    XDG_SESSION_DESKTOP="sway" \
    XDG_SESSION_TYPE="wayland"

USER 1000:1000
WORKDIR /workspace
ENTRYPOINT ["rc-kube"]
