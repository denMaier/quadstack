#!/usr/bin/env bash
# Run the real Podman/systemd test as a disposable account on a Linux systemd host.
# An optional precompiled test binary allows the same check inside a local test VM.
set -Eeuo pipefail
cd "$(dirname "$0")/.."
[[ -d /run/systemd/system ]] || { echo 'This test needs a Linux systemd host' >&2; exit 1; }
as_root() { if ((EUID == 0)); then "$@"; else sudo -n "$@"; fi; }
workspace=$(mktemp -d)
account=qs-ci-$$
created=false
cleanup() {
  if [[ $created == true ]]; then
    as_root loginctl disable-linger "$account" || true
    as_root loginctl terminate-user "$account" || true
    as_root systemctl stop "user@$uid.service" "user-runtime-dir@$uid.service" || true
    as_root userdel --remove "$account" || true
  fi
  rm -rf "$workspace"
}
trap cleanup EXIT
if [[ $# == 1 ]]; then
  cp "$1" "$workspace/quadstack.test"
else
  go test -c -o "$workspace/quadstack.test" .
fi
chmod 0755 "$workspace" "$workspace/quadstack.test"
as_root useradd --create-home --shell /bin/bash "$account"
created=true
uid=$(id -u "$account")
user_failure() {
  echo 'The disposable systemd user session did not start' >&2
  as_root systemctl --no-pager --full status "user@$uid.service" "user-runtime-dir@$uid.service" || true
  as_root journalctl --no-pager -b -u "user@$uid.service" -u "user-runtime-dir@$uid.service" -n 50 || true
  exit 1
}
as_root loginctl enable-linger "$account"
# Hosted runners do not necessarily activate a fresh account's manager on enable-linger.
as_root systemctl start "user@$uid.service" || user_failure
for _ in {1..30}; do
  [[ -S /run/user/$uid/bus ]] && break
  sleep 1
done
[[ -S /run/user/$uid/bus ]] || user_failure
as_root runuser -u "$account" -- env \
  XDG_RUNTIME_DIR="/run/user/$uid" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
  TMPDIR=/tmp QS_TEST_SYSTEMD=1 "$workspace/quadstack.test" \
  -test.run '^TestSystemdAutoUpdateIntegration$' -test.timeout 90s -test.v
