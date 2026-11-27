#!/usr/bin/env bash
set -euo pipefail

# Register the Wayland desktop stack and rc-kube as s6-rc longrun services
# and initialize unprivileged runtime directories for UID 1000 (agent).

chmod 0755 \
  /usr/local/bin/rc-kube \
  /usr/local/bin/rc-wayland-health \
  /usr/local/libexec/rc-wayland/*

bundle_dir="/etc/s6-overlay/user-bundles.d/user/contents.d"
mkdir -p "${bundle_dir}"

register_service() {
  local service="$1"
  local notify_fd="$2"
  shift 2

  local svc_dir="/etc/s6-overlay/s6-rc.d/${service}"
  mkdir -p "${svc_dir}/dependencies.d"
  printf 'longrun\n' > "${svc_dir}/type"
  if [[ -n "${notify_fd}" ]]; then
    printf '%s\n' "${notify_fd}" > "${svc_dir}/notification-fd"
  fi
  ln -sfn "/usr/local/libexec/rc-wayland/${service}" "${svc_dir}/run"
  ln -sfn /usr/local/libexec/rc-wayland/finish "${svc_dir}/finish"
  for dep in "$@"; do
    touch "${svc_dir}/dependencies.d/${dep}"
  done
  touch "${bundle_dir}/${service}"
}

register_service dbus        3 base
register_service sway        3 base
register_service pipewire    3 dbus
register_service wireplumber 3 dbus pipewire
register_service portal-wlr  3 dbus pipewire wireplumber sway
register_service portal      3 dbus portal-wlr
register_service rc-kube     "" base

mkdir -p /run/user/1000 /run/rc-wayland/config /run/rc
cp -a /etc/rc-wayland/config/. /run/rc-wayland/config/
chown -R 1000:1000 /run /home/agent
chmod 0700 /run/user/1000
chmod 0755 /run
