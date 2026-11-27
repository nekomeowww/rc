#!/usr/bin/env bash
set -euo pipefail

# Download, verify against upstream release SHA-256 files, and install
# auv and s6-overlay for the amd64 Wayland runner image.

: "${TARGETARCH:?TARGETARCH must be set}"
: "${AUV_VERSION:?AUV_VERSION must be set}"
: "${S6_OVERLAY_VERSION:?S6_OVERLAY_VERSION must be set}"

if [[ "${TARGETARCH}" != "amd64" ]]; then
  echo "runner-wayland only supports amd64 (got ${TARGETARCH})" >&2
  exit 1
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

download_and_verify() {
  local url="$1"
  local sha_url="$2"
  local dest="$3"

  curl -fsSLo "${dest}" "${url}"
  local expected
  expected="$(curl -fsSL "${sha_url}" | awk '{ print $1; exit }')"
  test -n "${expected}"
  echo "${expected}  ${dest}" | sha256sum --check --strict
}

# --- auv ---
auv_base="https://github.com/moeru-ai/auv/releases/download/v${AUV_VERSION}"
auv_asset="auv-x86_64-unknown-linux-gnu.tar.gz"
download_and_verify "${auv_base}/${auv_asset}" "${auv_base}/${auv_asset}.sha256" "${work_dir}/auv.tar.gz"

mkdir -p /opt/auv
tar -xzf "${work_dir}/auv.tar.gz" -C /opt/auv
test "$(/opt/auv/auv --version)" = "auv ${AUV_VERSION}"
ln -sfn /opt/auv/auv /usr/local/bin/auv

# --- s6-overlay ---
s6_base="https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}"
for archive in s6-overlay-noarch.tar.xz s6-overlay-x86_64.tar.xz; do
  download_and_verify "${s6_base}/${archive}" "${s6_base}/${archive}.sha256" "${work_dir}/${archive}"
  tar -C / -Jxpf "${work_dir}/${archive}"
done
