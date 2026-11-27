# syntax=docker/dockerfile:1.29

# ------------------------------------------------------------------------------
# Stage 1: Build controller manager binary
# ------------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27.4 AS builder

ARG TARGETOS
ARG TARGETARCH
# The release version. The manager uses it to select the rc-kube image of the
# same version.
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
    -o /out/manager ./cmd/main.go
EOF

# ------------------------------------------------------------------------------
# Stage 2: Final distroless controller image
# ------------------------------------------------------------------------------
FROM gcr.io/distroless/static:nonroot AS controller

LABEL org.opencontainers.image.source="https://github.com/nekomeowww/rc" \
      org.opencontainers.image.title="rc controller"

WORKDIR /
COPY --from=builder /out/manager /manager

USER 65532:65532
ENTRYPOINT ["/manager"]
