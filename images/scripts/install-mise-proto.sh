#!/usr/bin/env bash
set -euo pipefail

# Download, verify against upstream release SHA-256 manifests, and install
# mise and proto into OUT_DIR (default: /out).

: "${TARGETARCH:?TARGETARCH must be set (amd64 or arm64)}"
: "${MISE_VERSION:?MISE_VERSION must be set}"
: "${PROTO_VERSION:?PROTO_VERSION must be set}"
OUT_DIR="${OUT_DIR:-/out}"

case "${TARGETARCH}" in
  amd64)
    mise_arch="x64"
    proto_target="x86_64-unknown-linux-gnu"
    ;;
  arm64)
    mise_arch="arm64"
    proto_target="aarch64-unknown-linux-gnu"
    ;;
  *)
    echo "Unsupported target architecture: ${TARGETARCH}" >&2
    exit 1
    ;;
esac

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

# --- mise ---
mise_asset="mise-v${MISE_VERSION}-linux-${mise_arch}.tar.xz"
mise_base="https://github.com/jdx/mise/releases/download/v${MISE_VERSION}"

curl -fsSLo "${work_dir}/mise.tar.xz" "${mise_base}/${mise_asset}"
mise_sha256="$(
  curl -fsSL "${mise_base}/SHASUMS256.txt" \
    | awk -v asset="${mise_asset}" '$2 == asset || $2 == ("./" asset) { print $1; exit }'
)"
test -n "${mise_sha256}"
echo "${mise_sha256}  ${work_dir}/mise.tar.xz" | sha256sum --check --strict

mkdir -p "${work_dir}/mise"
tar -xJf "${work_dir}/mise.tar.xz" -C "${work_dir}/mise" --strip-components=2 mise/bin/mise
install -Dm755 "${work_dir}/mise/mise" "${OUT_DIR}/mise"

# --- proto ---
proto_asset="proto_cli-${proto_target}.tar.xz"
proto_base="https://github.com/moonrepo/proto/releases/download/v${PROTO_VERSION}"

curl -fsSLo "${work_dir}/proto.tar.xz" "${proto_base}/${proto_asset}"
proto_sha256="$(curl -fsSL "${proto_base}/${proto_asset}.sha256" | awk '{ print $1; exit }')"
test -n "${proto_sha256}"
echo "${proto_sha256}  ${work_dir}/proto.tar.xz" | sha256sum --check --strict

mkdir -p "${work_dir}/proto"
tar -xJf "${work_dir}/proto.tar.xz" -C "${work_dir}/proto" --strip-components=1
install -Dm755 "${work_dir}/proto/proto" "${OUT_DIR}/proto"
install -Dm755 "${work_dir}/proto/proto-shim" "${OUT_DIR}/proto-shim"
