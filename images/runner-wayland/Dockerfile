# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
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
ARG MISE_VERSION=2026.8.11
ARG MISE_SHA256_AMD64=8e0c3d18e71956a0fb15f55069966bbf1d6139aa8f809f1b88897ea7ef6cf196
ARG PROTO_VERSION=0.61.0
ARG PROTO_SHA256_AMD64=25ed099eeac477427800c12af45c546fc983aa2e090ff10b8fd8f69471022da1

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
    && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    test "${TARGETARCH}" = amd64; \
    curl -fsSLo /tmp/mise.tar.xz \
        "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/mise-v${MISE_VERSION}-linux-x64.tar.xz"; \
    echo "${MISE_SHA256_AMD64}  /tmp/mise.tar.xz" | sha256sum --check --strict; \
    mkdir -p /tmp/mise; \
    tar -xJf /tmp/mise.tar.xz -C /tmp/mise --strip-components=2 mise/bin/mise; \
    install -Dm755 /tmp/mise/mise /out/mise; \
    curl -fsSLo /tmp/proto.tar.xz \
        "https://github.com/moonrepo/proto/releases/download/v${PROTO_VERSION}/proto_cli-x86_64-unknown-linux-gnu.tar.xz"; \
    echo "${PROTO_SHA256_AMD64}  /tmp/proto.tar.xz" | sha256sum --check --strict; \
    mkdir -p /tmp/proto; \
    tar -xJf /tmp/proto.tar.xz -C /tmp/proto --strip-components=1; \
    install -Dm755 /tmp/proto/proto /out/proto; \
    install -Dm755 /tmp/proto/proto-shim /out/proto-shim

FROM node:26-bookworm-slim AS node-runtime

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
ARG COREPACK_VERSION=0.35.0
ARG LOBEHUB_CLI_VERSION=0.0.48
ARG AUV_VERSION=0.0.13
ARG AUV_SHA256_AMD64=bbece403ad0d505611fca629144382e4aca354f7addb3a53a69649b8cf81e68a
ARG S6_OVERLAY_VERSION=3.2.3.2
ARG S6_OVERLAY_NOARCH_SHA256=5379750ed30a84bbd2e2dd74847ba6b5bd29cd0b2e3ea2ec58049b57eb2eda12
ARG S6_OVERLAY_AMD64_SHA256=e6befcc96a437a3831386ecfc51808c5d3e939dc5fe3c02ae9284599e8aa2408

LABEL org.opencontainers.image.source="https://github.com/nekomeowww/rc" \
      org.opencontainers.image.title="rc Wayland runner"

COPY --from=node-runtime /usr/local/ /usr/local/

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        btop \
        ca-certificates \
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
        gcc \
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
        make \
        mesa-utils \
        openssh-client \
        pipewire \
        procps \
        ripgrep \
        sudo \
        sway \
        tar \
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
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/* \
    && usermod --login agent --home /home/agent --move-home --shell /bin/bash ubuntu \
    && groupmod --new-name agent ubuntu \
    && groupadd --gid 65532 rc \
    && useradd --uid 65532 --gid 65532 --create-home --shell /bin/bash rc \
    && git config --system --add safe.directory '*'

RUN set -eux; \
    test "${TARGETARCH}" = amd64; \
    curl -fsSLo /tmp/auv.tar.gz \
        "https://github.com/moeru-ai/auv/releases/download/v${AUV_VERSION}/auv-x86_64-unknown-linux-gnu.tar.gz"; \
    echo "${AUV_SHA256_AMD64}  /tmp/auv.tar.gz" | sha256sum --check --strict; \
    mkdir -p /opt/auv; \
    tar -xzf /tmp/auv.tar.gz -C /opt/auv; \
    test "$(/opt/auv/auv --version)" = "auv ${AUV_VERSION}"; \
    ln -s /opt/auv/auv /usr/local/bin/auv; \
    curl -fsSLo /tmp/s6-overlay-noarch.tar.xz \
        "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz"; \
    echo "${S6_OVERLAY_NOARCH_SHA256}  /tmp/s6-overlay-noarch.tar.xz" | sha256sum --check --strict; \
    curl -fsSLo /tmp/s6-overlay-amd64.tar.xz \
        "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-x86_64.tar.xz"; \
    echo "${S6_OVERLAY_AMD64_SHA256}  /tmp/s6-overlay-amd64.tar.xz" | sha256sum --check --strict; \
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
