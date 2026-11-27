# syntax=docker/dockerfile:1.29

# rc-kube image. The controller runs it as the first init container of every
# Linux Workspace and WorkspaceEnvironment runtime Pod:
#
#   /usr/local/bin/rc-kube install /opt/rc/bin
#
# The install command copies rc-kube and the tini next to it into an emptyDir.
# The runtime container then runs that rc-kube, so it always has the version of
# the controller, whatever the Workspace image holds. Both binaries are static,
# and the image needs no shell.

# ------------------------------------------------------------------------------
# Stage 1: Build rc-kube binary
# ------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27.4 AS builder

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=""

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN <<'EOF'
set -eu
CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH}" \
  go build -trimpath \
    -ldflags "-X github.com/nekomeowww/rc/internal/version.Version=${VERSION}" \
    -o /out/rc-kube ./cmd/rc-kube
EOF

# ------------------------------------------------------------------------------
# Stage 2: Download and verify tini against upstream SHA-256
# ------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS tini-downloader

ARG TARGETARCH
# renovate: datasource=github-releases depName=krallin/tini extractVersion=^v(?<version>.*)$
ARG TINI_VERSION=0.19.0

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

RUN <<'EOF'
set -eu
case "${TARGETARCH}" in
  amd64|arm64) ;;
  *)
    echo "Unsupported target architecture: ${TARGETARCH}" >&2
    exit 1
    ;;
esac

tini_base="https://github.com/krallin/tini/releases/download/v${TINI_VERSION}"
tini_asset="tini-static-${TARGETARCH}"

curl -fsSLo /tmp/tini "${tini_base}/${tini_asset}"
tini_sha256="$(curl -fsSL "${tini_base}/${tini_asset}.sha256sum" | awk '{ print $1; exit }')"
test -n "${tini_sha256}"
echo "${tini_sha256}  /tmp/tini" | sha256sum --check --strict
install -Dm755 /tmp/tini /out/tini
rm -f /tmp/tini
EOF

# ------------------------------------------------------------------------------
# Stage 3: Final scratch rc-kube image
# ------------------------------------------------------------------------------
FROM scratch AS rc-kube

LABEL org.opencontainers.image.source="https://github.com/nekomeowww/rc" \
      org.opencontainers.image.title="rc-kube"

COPY --from=builder /out/rc-kube /usr/local/bin/rc-kube
COPY --from=tini-downloader /out/tini /usr/local/bin/tini

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/rc-kube"]
