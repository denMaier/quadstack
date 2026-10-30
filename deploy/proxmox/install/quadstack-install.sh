#!/usr/bin/env bash
# Runs inside the new LXC: the engine's base setup, then the Quadstack installer, which
# installs Podman, creates the account "quadlets" and asks for the sign-in settings.
# License: AGPL-3.0 | https://github.com/denMaier/quadstack/blob/main/LICENSE
# Source: https://github.com/denMaier/quadstack

source /dev/stdin <<<"$FUNCTIONS_FILE_PATH"
color
verb_ip6
catch_errors
setting_up_container
network_check
update_os

msg_info "Installing Podman"
$STD apt install -y podman passt uidmap dbus-user-session acl whiptail
msg_ok "Installed Podman"

QS_FRESH=1 QS_LISTEN="${QS_LISTEN:-0.0.0.0:8090}" \
  bash -c "$(curl -fsSL https://raw.githubusercontent.com/denMaier/quadstack/main/deploy/install.sh)"

motd_ssh
customize
cleanup_lxc
