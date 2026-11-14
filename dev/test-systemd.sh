#!/usr/bin/env bash
# Run the real Podman/systemd test as a disposable account on a Linux systemd host.
# An optional precompiled test binary allows the same check inside a local test VM.
set -Eeuo pipefail
cd "$(dirname "$0")/.."
[[ -d /run/systemd/system ]] || { echo 'This test needs a Linux systemd host' >&2; exit 1; }
as_root() { if ((EUID == 0)); then "$@"; else sudo -n "$@"; fi; }
workspace=$(mktemp -d /tmp/quadstack-systemd.XXXXXX)
account=qs-ci-$$
created=false
dropin=""
cleanup() {
  if [[ $created == true ]]; then
    as_root loginctl disable-linger "$account" || true
    as_root loginctl terminate-user "$account" || true
    as_root systemctl stop "user@$uid.service" "user-runtime-dir@$uid.service" || true
    as_root userdel --remove "$account" || true
  fi
  if [[ -n $dropin ]]; then
    as_root rm -f "$dropin"
    as_root rmdir "$(dirname "$dropin")" 2>/dev/null || true
    as_root systemctl daemon-reload
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
account_home=$(getent passwd "$account" | cut -d: -f6)
# User managers cache HOME/XDG paths at startup. Set them after PAM has populated the
# service environment, so a hosted runner's imported paths cannot leak into this account.
cat >"$workspace/user-manager" <<'SH'
#!/bin/sh
set -eu
test_uid=$(id -u)
test_home=$(getent passwd "$test_uid" | cut -d: -f6)
exec /usr/bin/env HOME="$test_home" XDG_RUNTIME_DIR="/run/user/$test_uid" \
  XDG_CONFIG_HOME="$test_home/.config" XDG_DATA_HOME="$test_home/.local/share" \
  XDG_CACHE_HOME="$test_home/.cache" XDG_STATE_HOME="$test_home/.local/state" \
  DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$test_uid/bus" \
  /usr/lib/systemd/systemd --user
SH
chmod 0755 "$workspace/user-manager"
printf '[Service]\nExecStart=\nExecStart=%s/user-manager\n' "$workspace" >"$workspace/user-manager.conf"
dropin="/run/systemd/system/user@$uid.service.d/90-quadstack-ci.conf"
as_root install -D -m 0644 "$workspace/user-manager.conf" "$dropin"
as_root systemctl daemon-reload
as_test_user() {
  as_root runuser -u "$account" -- env HOME="$account_home" \
    XDG_RUNTIME_DIR="/run/user/$uid" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" "$@"
}
user_failure() {
  echo 'The disposable systemd user session did not start' >&2
  as_root systemctl --no-pager --full status "user@$uid.service" "user-runtime-dir@$uid.service" || true
  as_root journalctl --no-pager -b -u "user@$uid.service" -u "user-runtime-dir@$uid.service" -n 50 || true
  exit 1
}
# Debian's environment.d imports /etc/environment after that, and hosted runners set the
# runner's XDG paths there. Mask it for this account so the wrapper's paths stay in effect.
as_root runuser -u "$account" -- mkdir -p "$account_home/.config/environment.d"
as_root runuser -u "$account" -- touch "$account_home/.config/environment.d/99-environment.conf"
as_root loginctl enable-linger "$account"
# Hosted runners do not necessarily activate a fresh account's manager on enable-linger.
as_root systemctl start "user@$uid.service" || user_failure
for _ in {1..30}; do
  as_root test -S "/run/user/$uid/bus" && break
  sleep 1
done
as_root test -S "/run/user/$uid/bus" || user_failure
if ! as_test_user env TMPDIR=/tmp QS_TEST_SYSTEMD=1 "$workspace/quadstack.test" \
  -test.run '^TestSystemdAutoUpdateIntegration$' -test.timeout 90s -test.v; then
  # The test only sees unit results; the reasons are in the account's journal.
  as_root journalctl --no-pager -b _UID="$uid" -n 150 || true
  exit 1
fi
