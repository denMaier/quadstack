#!/usr/bin/env bash
# Creates a Quadstack LXC on a Proxmox VE host with the community-scripts engine
# (https://github.com/community-scripts/core). Usually started through deploy/install.sh.
# The engine is pinned to a tested commit; the scripts come from this repository.
export COMMUNITY_SCRIPTS_URL="${COMMUNITY_SCRIPTS_URL:-https://raw.githubusercontent.com/denMaier/quadstack/main/deploy/proxmox}"
export COMMUNITY_SCRIPTS_CORE_URL="${COMMUNITY_SCRIPTS_CORE_URL:-https://raw.githubusercontent.com/community-scripts/core/5acac4483be88990354dd4e3778e2cdce4df428e}"
source <(curl -fsSL "${COMMUNITY_SCRIPTS_CORE_URL}/core/build.func")
# License: AGPL-3.0 | https://github.com/denMaier/quadstack/blob/main/LICENSE
# Source: https://github.com/denMaier/quadstack

APP="Quadstack"
var_tags="${var_tags:-podman;quadlet}"
var_cpu="${var_cpu:-2}"
var_ram="${var_ram:-2048}"
var_disk="${var_disk:-16}"
var_os="${var_os:-debian}"
var_version="${var_version:-13}"
var_arm64="${var_arm64:-yes}"
var_unprivileged="${var_unprivileged:-1}"
# Rootless Podman inside the container needs nesting and keyctl
var_nesting="${var_nesting:-1}"
var_keyctl="${var_keyctl:-1}"

header_info "$APP"
variables
color
catch_errors

function update_script() {
  header_info
  check_container_storage
  check_container_resources
  QS_MODE=update bash -c "$(curl -fsSL https://raw.githubusercontent.com/denMaier/quadstack/main/deploy/install.sh)"
  exit
}

start
build_container
description

msg_ok "Completed successfully!\n"
echo -e "${CREATING}${GN}${APP} setup has been successfully initialized!${CL}"
echo -e "${INFO}${YW} Point your reverse proxy (with websockets) at:${CL}"
echo -e "${GATEWAY}${BGN}http://${IP}:8090${CL}"
