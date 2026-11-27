# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.27.4 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o /out/rcctl cmd/rcctl/main.go
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o /out/rc-kube cmd/rc-kube/main.go

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS tool-downloader
ARG TARGETARCH
# renovate: datasource=github-releases depName=jdx/mise extractVersion=^v(?<version>.*)$
ARG MISE_VERSION=2026.11.14
# renovate: datasource=github-releases depName=moonrepo/proto extractVersion=^v(?<version>.*)$
ARG PROTO_VERSION=0.64.1

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
    && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    test "${TARGETARCH}" = amd64; \
    mise_asset="mise-v${MISE_VERSION}-linux-x64.tar.xz"; \
    curl -fsSLo /tmp/mise.tar.xz \
        "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/${mise_asset}"; \
    mise_sha256="$(curl -fsSL "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/SHASUMS256.txt" \
        | awk -v asset="${mise_asset}" '$2 == asset || $2 == ("./" asset) { print $1; exit }')"; \
    test -n "${mise_sha256}"; \
    echo "${mise_sha256}  /tmp/mise.tar.xz" | sha256sum --check --strict; \
    mkdir -p /tmp/mise; \
    tar -xJf /tmp/mise.tar.xz -C /tmp/mise --strip-components=2 mise/bin/mise; \
    install -Dm755 /tmp/mise/mise /out/mise; \
    proto_asset="proto_cli-x86_64-unknown-linux-gnu.tar.xz"; \
    curl -fsSLo /tmp/proto.tar.xz \
        "https://github.com/moonrepo/proto/releases/download/v${PROTO_VERSION}/${proto_asset}"; \
    proto_sha256="$(curl -fsSL "https://github.com/moonrepo/proto/releases/download/v${PROTO_VERSION}/${proto_asset}.sha256" \
        | awk '{ print $1; exit }')"; \
    test -n "${proto_sha256}"; \
    echo "${proto_sha256}  /tmp/proto.tar.xz" | sha256sum --check --strict; \
    mkdir -p /tmp/proto; \
    tar -xJf /tmp/proto.tar.xz -C /tmp/proto --strip-components=1; \
    install -Dm755 /tmp/proto/proto /out/proto; \
    install -Dm755 /tmp/proto/proto-shim /out/proto-shim

FROM node:26.13.1-bookworm-slim AS node-runtime

FROM ubuntu:24.04 AS xdpw-builder
ARG XDPW_VERSION=0.8.4
ARG XDPW_SHA256=3122966d46ab108f505525bcb2498f9121b446ee8438fbfceb73a7a1fa1ad400

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        libdrm-dev \
        libgbm-dev \
        libinih-dev \
        libpipewire-0.3-dev \
        libsystemd-dev \
        libwayland-dev \
        meson \
        ninja-build \
        pkg-config \
        wayland-protocols \
    && rm -rf /var/lib/apt/lists/*

COPY images/runner-wayland/patches/xdg-desktop-portal-wlr-0.8.4-*.patch /tmp/

RUN set -eux; \
    curl -fsSLo /tmp/xdg-desktop-portal-wlr.tar.gz \
        "https://github.com/emersion/xdg-desktop-portal-wlr/archive/refs/tags/v${XDPW_VERSION}.tar.gz"; \
    echo "${XDPW_SHA256}  /tmp/xdg-desktop-portal-wlr.tar.gz" | sha256sum --check --strict; \
    mkdir -p /tmp/xdg-desktop-portal-wlr; \
    tar -xzf /tmp/xdg-desktop-portal-wlr.tar.gz \
        -C /tmp/xdg-desktop-portal-wlr --strip-components=1; \
    patch --directory=/tmp/xdg-desktop-portal-wlr --fuzz=0 --strip=1 \
        < /tmp/xdg-desktop-portal-wlr-0.8.4-build.patch; \
    patch --directory=/tmp/xdg-desktop-portal-wlr --fuzz=0 --strip=1 \
        < /tmp/xdg-desktop-portal-wlr-0.8.4-shm.patch; \
    meson setup /tmp/xdg-desktop-portal-wlr/build /tmp/xdg-desktop-portal-wlr \
        --buildtype=release \
        --prefix=/usr \
        --libexecdir=libexec \
        -Dman-pages=disabled \
        -Dsystemd=disabled; \
    meson compile -C /tmp/xdg-desktop-portal-wlr/build; \
    install -Dm755 \
        /tmp/xdg-desktop-portal-wlr/build/xdg-desktop-portal-wlr \
        /out/xdg-desktop-portal-wlr

FROM ubuntu:24.04

ARG TARGETARCH
# renovate: datasource=npm depName=corepack
ARG COREPACK_VERSION=0.36.0
# renovate: datasource=npm depName=@lobehub/cli
ARG LOBEHUB_CLI_VERSION=0.0.270
# renovate: datasource=github-releases depName=moeru-ai/auv extractVersion=^v(?<version>.*)$
ARG AUV_VERSION=0.0.43
# renovate: datasource=github-releases depName=just-containers/s6-overlay versioning=loose extractVersion=^v(?<version>.*)$
ARG S6_OVERLAY_VERSION=3.2.3.2

LABEL org.opencontainers.image.source="https://github.com/nekomeowww/rc" \
      org.opencontainers.image.title="rc Wayland runner"

COPY --from=node-runtime /usr/local/ /usr/local/

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        btop \
        build-essential \
        ca-certificates \
        cmake \
        curl \
        dbus-bin \
        dbus-daemon \
        fonts-dejavu-core \
        fonts-freefont-ttf \
        fonts-ipafont-gothic \
        fonts-liberation \
        fonts-noto-color-emoji \
        fonts-unifont \
        fonts-wqy-zenhei \
        git \
        git-lfs \
        grim \
        gzip \
        htop \
        libasound2t64 \
        libatk-bridge2.0-0t64 \
        libatk1.0-0t64 \
        libatomic1 \
        libatspi2.0-0t64 \
        libcairo2 \
        libcups2t64 \
        libdbus-1-3 \
        libdrm2 \
        libfontconfig1 \
        libfreetype6 \
        libgbm1 \
        libglib2.0-0t64 \
        libglib2.0-bin \
        libgtk-3-0t64 \
        liblept5 \
        libnspr4 \
        libnss3 \
        libpango-1.0-0 \
        libtesseract5 \
        libx11-6 \
        libxcb1 \
        libxcomposite1 \
        libxdamage1 \
        libxext6 \
        libxfixes3 \
        libxkbcommon0 \
        libxrandr2 \
        mesa-utils \
        openssh-client \
        openssh-sftp-server \
        pipewire \
        pkg-config \
        procps \
        python3 \
        ripgrep \
        sudo \
        sway \
        tar \
        tmux \
        unzip \
        wget \
        wireplumber \
        xdg-desktop-portal \
        xdg-desktop-portal-wlr \
        xz-utils \
        zip \
    && npm install --global --ignore-scripts \
        "corepack@${COREPACK_VERSION}" \
        "@lobehub/cli@${LOBEHUB_CLI_VERSION}" \
    && test "$(lh --version)" = "${LOBEHUB_CLI_VERSION}" \
    && rm -rf /tmp/lobehub \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/* \
    && usermod --login agent --home /home/agent --move-home --shell /bin/bash ubuntu \
    && groupmod --new-name agent ubuntu \
    && groupadd --gid 65532 rc \
    && useradd --uid 65532 --gid 65532 --create-home --shell /bin/bash rc \
    && git config --system --add safe.directory '*'

RUN set -eux; \
    test "${TARGETARCH}" = amd64; \
    auv_asset="auv-x86_64-unknown-linux-gnu.tar.gz"; \
    curl -fsSLo /tmp/auv.tar.gz \
        "https://github.com/moeru-ai/auv/releases/download/v${AUV_VERSION}/${auv_asset}"; \
    auv_sha256="$(curl -fsSL "https://github.com/moeru-ai/auv/releases/download/v${AUV_VERSION}/${auv_asset}.sha256" \
        | awk '{ print $1; exit }')"; \
    test -n "${auv_sha256}"; \
    echo "${auv_sha256}  /tmp/auv.tar.gz" | sha256sum --check --strict; \
    mkdir -p /opt/auv; \
    tar -xzf /tmp/auv.tar.gz -C /opt/auv; \
    test "$(/opt/auv/auv --version)" = "auv ${AUV_VERSION}"; \
    ln -s /opt/auv/auv /usr/local/bin/auv; \
    curl -fsSLo /tmp/s6-overlay-noarch.tar.xz \
        "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz"; \
    s6_noarch_sha256="$(curl -fsSL "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz.sha256" \
        | awk '{ print $1; exit }')"; \
    test -n "${s6_noarch_sha256}"; \
    echo "${s6_noarch_sha256}  /tmp/s6-overlay-noarch.tar.xz" | sha256sum --check --strict; \
    curl -fsSLo /tmp/s6-overlay-amd64.tar.xz \
        "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-x86_64.tar.xz"; \
    s6_amd64_sha256="$(curl -fsSL "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-x86_64.tar.xz.sha256" \
        | awk '{ print $1; exit }')"; \
    test -n "${s6_amd64_sha256}"; \
    echo "${s6_amd64_sha256}  /tmp/s6-overlay-amd64.tar.xz" | sha256sum --check --strict; \
    tar -C / -Jxpf /tmp/s6-overlay-noarch.tar.xz; \
    tar -C / -Jxpf /tmp/s6-overlay-amd64.tar.xz; \
    rm -f /tmp/auv.tar.gz /tmp/s6-overlay-noarch.tar.xz /tmp/s6-overlay-amd64.tar.xz

COPY --from=builder /out/rcctl /usr/local/bin/rcctl
COPY --from=builder /out/rc-kube /usr/local/libexec/rc-kube
COPY --from=tool-downloader /out/mise /usr/local/bin/mise
COPY --from=tool-downloader /out/proto /usr/local/bin/proto
COPY --from=tool-downloader /out/proto-shim /usr/local/bin/proto-shim
COPY --from=xdpw-builder /out/xdg-desktop-portal-wlr /usr/libexec/xdg-desktop-portal-wlr
COPY images/runner-wayland/rootfs/ /

RUN chmod 0755 \
        /usr/local/bin/rc-kube \
        /usr/local/bin/rc-wayland-health \
        /usr/local/libexec/rc-wayland/* \
    && mkdir -p /etc/s6-overlay/user-bundles.d/user/contents.d \
    && for service in dbus pipewire wireplumber sway portal-wlr portal; do \
        mkdir -p "/etc/s6-overlay/s6-rc.d/${service}/dependencies.d"; \
        printf 'longrun\n' > "/etc/s6-overlay/s6-rc.d/${service}/type"; \
        printf '3\n' > "/etc/s6-overlay/s6-rc.d/${service}/notification-fd"; \
        ln -s "/usr/local/libexec/rc-wayland/${service}" "/etc/s6-overlay/s6-rc.d/${service}/run"; \
        ln -s /usr/local/libexec/rc-wayland/finish "/etc/s6-overlay/s6-rc.d/${service}/finish"; \
        touch "/etc/s6-overlay/user-bundles.d/user/contents.d/${service}"; \
      done \
    && touch /etc/s6-overlay/s6-rc.d/dbus/dependencies.d/base \
    && touch /etc/s6-overlay/s6-rc.d/sway/dependencies.d/base \
    && touch /etc/s6-overlay/s6-rc.d/pipewire/dependencies.d/dbus \
    && touch /etc/s6-overlay/s6-rc.d/wireplumber/dependencies.d/dbus \
    && touch /etc/s6-overlay/s6-rc.d/wireplumber/dependencies.d/pipewire \
    && touch /etc/s6-overlay/s6-rc.d/portal-wlr/dependencies.d/dbus \
    && touch /etc/s6-overlay/s6-rc.d/portal-wlr/dependencies.d/pipewire \
    && touch /etc/s6-overlay/s6-rc.d/portal-wlr/dependencies.d/wireplumber \
    && touch /etc/s6-overlay/s6-rc.d/portal-wlr/dependencies.d/sway \
    && touch /etc/s6-overlay/s6-rc.d/portal/dependencies.d/dbus \
    && touch /etc/s6-overlay/s6-rc.d/portal/dependencies.d/portal-wlr \
    && mkdir -p /etc/s6-overlay/s6-rc.d/rc-kube/dependencies.d \
    && printf 'longrun\n' > /etc/s6-overlay/s6-rc.d/rc-kube/type \
    && ln -s /usr/local/libexec/rc-wayland/rc-kube /etc/s6-overlay/s6-rc.d/rc-kube/run \
    && ln -s /usr/local/libexec/rc-wayland/finish /etc/s6-overlay/s6-rc.d/rc-kube/finish \
    && touch /etc/s6-overlay/s6-rc.d/rc-kube/dependencies.d/base \
    && touch /etc/s6-overlay/user-bundles.d/user/contents.d/rc-kube \
    && mkdir -p /run/user/1000 /run/rc-wayland/config /run/rc \
    && cp -a /etc/rc-wayland/config/. /run/rc-wayland/config/ \
    && chown -R 1000:1000 /run /home/agent \
    && chmod 0700 /run/user/1000 \
    && chmod 0755 /run

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
