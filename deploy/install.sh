#!/usr/bin/env bash
# Quadstack installer:
#   bash -c "$(curl -fsSL https://raw.githubusercontent.com/denMaier/quadstack/main/deploy/install.sh)"
#
# On a Proxmox VE host it creates a new LXC with the community-scripts engine. Anywhere else
# (an existing LXC, VM or server) it installs Quadstack on that machine as an add-on for an
# account that owns rootless Quadlets, or updates or reconfigures an existing install.
#
# Every question can be answered in advance through the environment, for unattended runs:
#   QS_USER QS_PUBLIC_URL QS_ISSUER QS_CLIENT_ID QS_CLIENT_SECRET QS_ALLOWED_GROUPS
#   QS_ALLOWED_USERS QS_LISTEN QS_TERMINAL QS_MCP_MAX_HOURS QS_JOURNAL_ACL
#   QS_MODE=install|update|reconfigure  QS_VERSION=v0.2.0 (default: latest)  QS_YES=1
#   QS_PODLET=1 installs podlet (for Import) without asking; unattended runs skip it otherwise
set -Eeuo pipefail

REPO=denMaier/quadstack
RAW=${QS_RAW:-https://raw.githubusercontent.com/$REPO/main}
BT="Quadstack installer"

RD=$'\033[01;31m' GN=$'\033[1;92m' YW=$'\033[33m' BL=$'\033[36m' CL=$'\033[m'
msg_info() { printf ' %s⏳ %s%s\n' "$YW" "$1" "$CL"; }
msg_ok() { printf ' %s✔️  %s%s\n' "$GN" "$1" "$CL"; }
msg_warn() { printf ' %s⚠️  %s%s\n' "$YW" "$1" "$CL"; }
die() {
  printf ' %s✖️  %s%s\n' "$RD" "$1" "$CL" >&2
  exit 1
}
trap 'die "Failed at line $LINENO: $BASH_COMMAND"' ERR

# Questions use whiptail on a terminal, plain prompts otherwise, and the defaults when QS_YES=1
tty_ui() { [[ -t 0 && -t 1 ]] && command -v whiptail >/dev/null; }
cancelled() { die "Cancelled"; }

# ask VAR "question" [default]: keeps VAR when it is already set
ask() {
  local var=$1 prompt=$2 def=${3:-} val
  [[ -n "${!var:-}" ]] && return 0
  if [[ ${QS_YES:-} == 1 ]]; then
    val=$def
  elif tty_ui; then
    val=$(whiptail --backtitle "$BT" --title "Quadstack" --inputbox "$prompt" 14 76 "$def" 3>&1 1>&2 2>&3) || cancelled
  else
    read -r -p " $prompt [$def]: " val </dev/tty || cancelled
    val=${val:-$def}
  fi
  printf -v "$var" '%s' "$val"
}

# ask_valid CHECK VAR "question" [default]: asks again, with the answer filled in, until CHECK
# accepts VAR. CHECK may tidy VAR, and sets WHY when it refuses. With QS_YES=1 nobody can answer.
ask_valid() {
  local check=$1 var=$2 prompt=$3 def=${4:-} why=""
  while :; do
    ask "$var" "$why$prompt" "$def"
    "$check" && return 0
    [[ ${QS_YES:-} != 1 ]] || die "$WHY"
    def=${!var}
    printf -v "$var" '%s' ""
    why=$WHY$'\n\n'
  done
}

ask_secret() {
  local var=$1 prompt=$2 val
  [[ -n "${!var:-}" ]] && return 0
  if tty_ui; then
    val=$(whiptail --backtitle "$BT" --title "Quadstack" --passwordbox "$prompt" 12 76 3>&1 1>&2 2>&3) || cancelled
  else
    read -r -s -p " $prompt: " val </dev/tty || cancelled
    echo
  fi
  printf -v "$var" '%s' "$val"
}

# confirm "question" [yes|no]: the default applies with QS_YES=1
confirm() {
  local def=${2:-yes} a
  if [[ ${QS_YES:-} == 1 ]]; then
    [[ $def == yes ]]
  elif tty_ui; then
    local flag=()
    [[ $def == no ]] && flag=(--defaultno)
    whiptail --backtitle "$BT" --title "Quadstack" "${flag[@]}" --yesno "$1" 14 76
  else
    read -r -p " $1 [$([[ $def == yes ]] && echo Y/n || echo y/N)]: " a </dev/tty || cancelled
    a=${a:-$def}
    [[ ${a,,} =~ ^(y|yes)$ ]]
  fi
}

note() {
  if tty_ui && [[ ${QS_YES:-} != 1 ]]; then
    whiptail --backtitle "$BT" --title "Quadstack" --msgbox "$1" 16 76
  else
    printf ' %s%s%s\n' "$BL" "$1" "$CL"
  fi
}

# menu VAR "question" tag item [tag item…]
menu() {
  local var=$1 prompt=$2
  shift 2
  [[ -n "${!var:-}" ]] && return 0
  local val
  if [[ ${QS_YES:-} == 1 ]]; then
    val=$1
  elif tty_ui; then
    val=$(whiptail --backtitle "$BT" --title "Quadstack" --menu "$prompt" 18 76 8 "$@" 3>&1 1>&2 2>&3) || cancelled
  else
    local tags=() i=1
    echo " $prompt"
    while (($#)); do
      printf '   %d) %s  %s\n' "$i" "$1" "$2"
      tags+=("$1")
      shift 2
      i=$((i + 1))
    done
    while :; do
      read -r -p " Choice [1]: " i </dev/tty || cancelled
      i=${i:-1}
      [[ $i =~ ^[1-9][0-9]*$ ]] && ((i <= ${#tags[@]})) && break
      echo " Choose a number from 1 to ${#tags[@]}"
    done
    val=${tags[i - 1]}
  fi
  printf -v "$var" '%s' "$val"
}

### Proxmox VE host: a new LXC ##############################################################

if command -v pveversion >/dev/null 2>&1; then
  export COMMUNITY_SCRIPTS_URL=${COMMUNITY_SCRIPTS_URL:-$RAW/deploy/proxmox}
  exec bash -c "$(curl -fsSL "$COMMUNITY_SCRIPTS_URL/ct/quadstack.sh")"
fi

### Anywhere else: install on this machine #################################################

[[ $(id -u) == 0 ]] || die "Run this as root"
[[ -d /run/systemd/system ]] || die "Quadstack needs systemd"
case $(uname -m) in
x86_64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "Unsupported architecture $(uname -m)" ;;
esac
cd /

user_home() { getent passwd "$1" | cut -d: -f6; }
user_uid() { id -u "$1"; }

# as_user USER cmd…: runs in the account's systemd user session
as_user() {
  local u=$1 uid
  shift
  uid=$(user_uid "$u")
  runuser -u "$u" -- env XDG_RUNTIME_DIR="/run/user/$uid" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" "$@"
}

# userdir USER MODE PATH…: directories created as the account, so parents like ~/.config are its own
userdir() {
  local u=$1 mode=$2
  shift 2
  runuser -u "$u" -- mkdir -p "$@"
  chmod "$mode" "$@"
}

# envget KEY FILE [DEFAULT]: a value from an env file, without quotes
envget() {
  local v=""
  [[ -f $2 ]] && v=$(sed -n "s/^$1=//p" "$2" | tail -n1 | sed -e 's/^"\(.*\)"$/\1/' -e 's/\\"/"/g' -e 's/\\\\/\\/g')
  printf '%s' "${v:-${3:-}}"
}

# envline KEY VALUE: systemd EnvironmentFile syntax
envline() {
  local v=${2//\\/\\\\}
  printf '%s="%s"\n' "$1" "${v//\"/\\\"}"
}

resolve_version() {
  if [[ -n ${QS_VERSION:-} && $QS_VERSION != latest ]]; then
    echo "$QS_VERSION"
    return
  fi
  local url
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")
  [[ $url == */tag/* ]] || die "Could not find the latest release"
  echo "${url##*/}"
}

install_binary() {
  local u=$1 tag=$2 home tmp
  home=$(user_home "$u")
  tmp=$(mktemp -d)
  msg_info "Downloading Quadstack $tag ($ARCH)"
  curl -fsSL -o "$tmp/quadstack_linux_$ARCH" "https://github.com/$REPO/releases/download/$tag/quadstack_linux_$ARCH"
  curl -fsSL -o "$tmp/SHA256SUMS" "https://github.com/$REPO/releases/download/$tag/SHA256SUMS"
  (cd "$tmp" && grep " quadstack_linux_$ARCH\$" SHA256SUMS | sha256sum -c --quiet -) || die "Checksum mismatch"
  userdir "$u" 0755 "$home/.local/bin"
  install -o "$u" -g "$(id -gn "$u")" -m 0755 "$tmp/quadstack_linux_$ARCH" "$home/.local/bin/quadstack"
  rm -rf "$tmp"
  msg_ok "Installed $("$home/.local/bin/quadstack" -version) to $home/.local/bin/quadstack"
}

# podlet turns docker run commands and compose files into Quadlets (Import). It is optional:
# asked for, no by default, and kept up to date once this installer put it in /usr/local/bin.
# It never fails the install.
PODLET=/usr/local/bin/podlet
install_podlet() {
  local arch tag cur tmp url
  if [[ ! -x $PODLET ]]; then
    if command -v podlet >/dev/null 2>&1; then
      return 0 # installed some other way; leave it alone
    fi
    case ${QS_PODLET:-} in
    1 | yes) ;;
    0 | no) return 0 ;;
    *) confirm "Install podlet? It lets Quadstack import docker run commands and compose files (Import). Optional; you can run this installer again later." no || return 0 ;;
    esac
  fi
  case $(uname -m) in
  x86_64) arch=x86_64 ;;
  aarch64 | arm64) arch=aarch64 ;;
  *) msg_warn "No podlet build for $(uname -m); Import stays off" && return 0 ;;
  esac
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/containers/podlet/releases/latest) || return 1
  tag=${url##*/}
  [[ $tag == v* ]] || return 1
  cur=$("$PODLET" --version 2>/dev/null | awk '{print $2}' || true)
  if [[ v$cur == "$tag" ]]; then
    msg_ok "podlet $tag is up to date"
    return 0
  fi
  if ! command -v xz >/dev/null 2>&1; then
    command -v apt-get >/dev/null 2>&1 || return 1
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq xz-utils >/dev/null || return 1
  fi
  msg_info "Downloading podlet $tag ($arch)"
  tmp=$(mktemp -d) || return 1
  local f=podlet-$arch-unknown-linux-gnu.tar.xz
  url=https://github.com/containers/podlet/releases/download/$tag/$f
  if curl -fsSL -o "$tmp/$f" "$url" && curl -fsSL -o "$tmp/$f.sha256" "$url.sha256" &&
    (cd "$tmp" && grep . "$f.sha256" | sha256sum -c --quiet -) &&
    tar -xJf "$tmp/$f" -C "$tmp" && install -m 0755 "$tmp/${f%.tar.xz}/podlet" "$PODLET"; then
    rm -rf "$tmp"
    msg_ok "Installed podlet $tag to $PODLET"
  else
    rm -rf "$tmp"
    return 1
  fi
}

# The checks for ask_valid
# clean_url VALUE: no spaces or trailing slash; https:// when no scheme was typed
clean_url() {
  local v=${1//[[:space:]]/}
  v=${v%/}
  [[ -z $v || $v == *://* ]] || v=https://$v
  printf '%s' "$v"
}

check_public_url() {
  QS_PUBLIC_URL=$(clean_url "$QS_PUBLIC_URL")
  [[ $QS_PUBLIC_URL =~ ^https?://[^/]+$ ]] && return 0
  WHY="The public URL must look like https://quadstack.example.com"
  return 1
}

check_issuer() {
  QS_ISSUER=$(clean_url "$QS_ISSUER")
  if [[ ! $QS_ISSUER =~ ^https?://[^/]+ ]]; then
    WHY="The issuer must look like https://id.example.com"
    return 1
  fi
  curl -fsS -o /dev/null --max-time 10 "$QS_ISSUER/.well-known/openid-configuration" && return 0
  confirm "$QS_ISSUER/.well-known/openid-configuration could not be fetched from here. Continue anyway?" no && return 0
  WHY="$QS_ISSUER could not be reached. Check the issuer URL."
  return 1
}

check_client_id() {
  QS_CLIENT_ID=${QS_CLIENT_ID//[[:space:]]/}
  [[ -n $QS_CLIENT_ID ]] && return 0
  WHY="The client ID is required"
  return 1
}

check_account() {
  [[ $QS_USER =~ ^[a-z_][a-z0-9_-]*$ ]] && return 0
  WHY="An account name has lowercase letters, digits, - and _, and starts with a letter or _"
  return 1
}

check_listen() {
  QS_LISTEN=${QS_LISTEN//[[:space:]]/}
  local port=${QS_LISTEN##*:} cur
  if [[ ! $QS_LISTEN =~ :[1-9][0-9]{0,4}$ ]] || ((port > 65535)); then
    WHY="The listen address must look like 0.0.0.0:8090"
    return 1
  fi
  cur=$(envget QUADSTACK_LISTEN "$ENVF")
  [[ $port == "${cur##*:}" ]] && return 0 # Quadstack's own port
  ss -ltnH "sport = :$port" | grep -q . || return 0
  WHY="Port $port is already in use; choose another listen address"
  return 1
}

check_hours() {
  QS_MCP_MAX_HOURS=${QS_MCP_MAX_HOURS//[[:space:]]/}
  [[ $QS_MCP_MAX_HOURS =~ ^[0-9]+([.][0-9]+)?$ ]] && return 0
  WHY="Hours must be a number, such as 24"
  return 1
}

wait_active() {
  local u=$1 i
  for i in $(seq 1 20); do
    as_user "$u" systemctl --user is-active --quiet quadstack.service && return 0
    sleep 1
  done
  as_user "$u" systemctl --user --no-pager status quadstack.service | tail -n 15 || true
  die "Quadstack did not start; see: journalctl --user -u quadstack (as $u)"
}

# An existing install: the account whose user unit is quadstack.service
EXISTING=""
while IFS=: read -r name _ _ _ _ home _; do
  if [[ -f $home/.config/systemd/user/quadstack.service ]]; then
    EXISTING=$name
    break
  fi
done < <(getent passwd)

if [[ -n $EXISTING ]]; then
  ENVF=$(user_home "$EXISTING")/.config/quadstack/quadstack.env
  CUR=$("$(user_home "$EXISTING")/.local/bin/quadstack" -version 2>/dev/null || echo unknown)
  menu QS_MODE "Quadstack $CUR is installed for $EXISTING. What do you want to do?" \
    update "Install the latest release, keep the settings" \
    reconfigure "Change the settings (sign-in, address, …)"
  if [[ $QS_MODE == update ]]; then
    TAG=$(resolve_version)
    install_binary "$EXISTING" "$TAG"
    install_podlet || msg_warn "podlet could not be installed; Import stays off"
    msg_info "Restarting Quadstack"
    as_user "$EXISTING" systemctl --user restart quadstack.service
    wait_active "$EXISTING"
    msg_ok "Quadstack $TAG is running"
    exit 0
  fi
  QS_USER=$EXISTING
else
  # Nothing to update or change yet, e.g. /usr/bin/update after an install that did not finish
  QS_MODE=install
fi

### Podman ###################################################################################

if ! command -v podman >/dev/null 2>&1; then
  command -v apt-get >/dev/null 2>&1 || die "Install Podman 4.4 or newer first; this installer only installs it with apt"
  if [[ ${QS_FRESH:-} == 1 ]] || confirm "Podman is not installed. Install it now (podman, passt, uidmap, dbus-user-session, acl)?"; then
    msg_info "Installing Podman"
    DEBIAN_FRONTEND=noninteractive apt-get update -qq >/dev/null
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq podman passt uidmap dbus-user-session acl curl ca-certificates >/dev/null
    msg_ok "Installed $(podman --version)"
  else
    die "Podman is required"
  fi
fi
PV=$(podman version --format '{{.Client.Version}}' 2>/dev/null || podman --version | awk '{print $3}')
if [[ $(printf '%s\n' 4.4.0 "$PV" | sort -V | head -n1) != 4.4.0 ]]; then
  die "Podman $PV is too old: Quadlets need 4.4 or newer"
fi
if command -v apt-get >/dev/null 2>&1 && ! dpkg -s dbus-user-session >/dev/null 2>&1; then
  msg_info "Installing dbus-user-session, which systemctl --user needs"
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq dbus-user-session acl >/dev/null
  msg_ok "Installed dbus-user-session"
fi

### The Quadlet account ######################################################################

if [[ -z ${QS_USER:-} ]]; then
  CHOICES=()
  while IFS=: read -r name _ uid _ _ home _; do
    [[ $uid -ne 0 && -d $home/.config/containers/systemd ]] || continue
    n=$(find "$home/.config/containers/systemd" -name '*.container' 2>/dev/null | wc -l)
    CHOICES+=("$name" "existing account, $n container file(s)")
  done < <(getent passwd)
  if [[ ${QS_FRESH:-} == 1 ]]; then
    QS_USER=quadlets
  else
    CHOICES+=(new "Create a new account for Quadlets")
    menu QS_USER "Which account owns the Quadlets? Quadstack runs as that account and manages its ~/.config/containers/systemd." "${CHOICES[@]}"
    [[ $QS_USER == new ]] && QS_USER=""
  fi
fi
ask_valid check_account QS_USER "Name of the new account" quadlets

if ! id "$QS_USER" >/dev/null 2>&1; then
  msg_info "Creating account $QS_USER"
  # A regular UID (1000+) gets its own journal, so Quadstack can show its logs without ACLs
  useradd --create-home --shell /bin/bash --comment "Rootless Quadlets (Quadstack)" "$QS_USER"
  uid=$(user_uid "$QS_USER")
  # Subordinate IDs for rootless Podman must lie inside this machine's own ID range. In an
  # unprivileged LXC that range is only 65536 IDs, so the default 100000:65536 is unusable.
  for kind in uid gid; do
    top=$(awk '{e = $1 + $3; if (e > m) m = e} END {print m + 0}' "/proc/self/${kind}_map")
    if ((top >= 165536)); then start=100000 count=65536; else start=$((uid + 1)) count=$((top - uid - 1)); fi
    sed -i "/^$QS_USER:/d" "/etc/sub$kind"
    echo "$QS_USER:$start:$count" >>"/etc/sub$kind"
  done
  userdir "$QS_USER" 0755 "$(user_home "$QS_USER")/.config/containers/systemd"
  msg_ok "Created account $QS_USER (UID $uid, subordinate IDs $start-$((start + count - 1)))"
  if ((count < 65536)); then
    msg_warn "Containers can use UIDs 0-$count only; images with files owned by higher IDs (e.g. nobody, 65534) fail to pull"
  fi
fi
UIDN=$(user_uid "$QS_USER")
HOMEDIR=$(user_home "$QS_USER")
ENVF=$HOMEDIR/.config/quadstack/quadstack.env

loginctl enable-linger "$QS_USER"
for _ in $(seq 1 20); do [[ -S /run/user/$UIDN/bus ]] && break; sleep 1; done
[[ -S /run/user/$UIDN/bus ]] || die "The systemd user session of $QS_USER did not start (is dbus-user-session installed?)"
msg_ok "Lingering enabled: $QS_USER's units run without a login"

if ((UIDN < 1000)) && [[ ! -f /etc/tmpfiles.d/quadstack-journal.conf ]]; then
  if [[ ${QS_JOURNAL_ACL:-} == 1 ]] || { [[ -z ${QS_JOURNAL_ACL:-} ]] && confirm "$QS_USER is a system account (UID $UIDN), so its logs are in the system journal, which it cannot read.

Grant it read access with an ACL? Everyone who can sign in to Quadstack can then read this machine's whole journal."; }; then
    printf '%s\n' \
      "a+ /var/log/journal    - - - - d:user:$QS_USER:r-x,user:$QS_USER:r-x" \
      "A+ /var/log/journal/%m - - - - d:user:$QS_USER:r-x,user:$QS_USER:r-x" \
      >/etc/tmpfiles.d/quadstack-journal.conf
    systemd-tmpfiles --create /etc/tmpfiles.d/quadstack-journal.conf
    msg_ok "Journal readable for $QS_USER"
  fi
fi

### Settings #################################################################################

if [[ ! -f $ENVF || $QS_MODE == reconfigure ]] || ! confirm "$ENVF already exists. Keep these settings?"; then
  ask_valid check_public_url QS_PUBLIC_URL "The address people will open, behind your reverse proxy with TLS" "$(envget QUADSTACK_PUBLIC_URL "$ENVF")"

  note "Quadstack signs in with OpenID Connect only (Pocket ID, Authentik, Keycloak, …).

Create a confidential OIDC client in your provider with this redirect URI:

    $QS_PUBLIC_URL/auth/callback

and scopes openid, profile, email and groups. Then enter its details next."

  ask_valid check_issuer QS_ISSUER "OIDC issuer URL" "$(envget QUADSTACK_OIDC_ISSUER "$ENVF")"
  ask_valid check_client_id QS_CLIENT_ID "OIDC client ID" "$(envget QUADSTACK_OIDC_CLIENT_ID "$ENVF")"
  OLD_SECRET=$(envget QUADSTACK_OIDC_CLIENT_SECRET "$ENVF")
  if [[ -n $OLD_SECRET && -z ${QS_CLIENT_SECRET:-} ]] && confirm "Keep the current client secret?"; then
    QS_CLIENT_SECRET=$OLD_SECRET
  fi
  ask_secret QS_CLIENT_SECRET "OIDC client secret (empty for a public client)"

  while :; do
    ask QS_ALLOWED_GROUPS "Groups allowed to sign in, comma separated (empty for none)" "$(envget QUADSTACK_ALLOWED_GROUPS "$ENVF" admin)"
    ask QS_ALLOWED_USERS "Users allowed to sign in: sub, email or username, comma separated (empty for none)" "$(envget QUADSTACK_ALLOWED_USERS "$ENVF")"
    [[ -n ${QS_ALLOWED_GROUPS//[[:space:],]/}${QS_ALLOWED_USERS//[[:space:],]/} ]] && break
    [[ ${QS_YES:-} != 1 ]] || die "Allow at least one group or user; Quadstack refuses to start otherwise"
    note "Allow at least one group or user; Quadstack refuses to start otherwise."
    QS_ALLOWED_GROUPS="" QS_ALLOWED_USERS=""
  done

  ask_valid check_listen QS_LISTEN "Listen address for the reverse proxy to reach" "$(envget QUADSTACK_LISTEN "$ENVF" 0.0.0.0:8090)"

  if [[ -z ${QS_TERMINAL:-} ]]; then
    if confirm "Allow shells into containers from the browser (and the MCP exec tool)?" "$([[ $(envget QUADSTACK_TERMINAL "$ENVF") == false ]] && echo no || echo yes)"; then QS_TERMINAL=true; else QS_TERMINAL=false; fi
  fi
  ask_valid check_hours QS_MCP_MAX_HOURS "AI access (MCP) is off until you create a grant in the UI. Longest a grant may last, in hours (0 turns MCP off)" "$(envget QUADSTACK_MCP_MAX_HOURS "$ENVF" 24)"

  userdir "$QS_USER" 0700 "$HOMEDIR/.config/quadstack"
  tmp=$(mktemp)
  {
    echo "# Written by the Quadstack installer; see https://github.com/$REPO#configuration"
    envline QUADSTACK_LISTEN "$QS_LISTEN"
    envline QUADSTACK_PUBLIC_URL "$QS_PUBLIC_URL"
    envline QUADSTACK_OIDC_ISSUER "$QS_ISSUER"
    envline QUADSTACK_OIDC_CLIENT_ID "$QS_CLIENT_ID"
    envline QUADSTACK_OIDC_CLIENT_SECRET "$QS_CLIENT_SECRET"
    envline QUADSTACK_ALLOWED_GROUPS "$QS_ALLOWED_GROUPS"
    envline QUADSTACK_ALLOWED_USERS "$QS_ALLOWED_USERS"
    envline QUADSTACK_TERMINAL "$QS_TERMINAL"
    envline QUADSTACK_MCP_MAX_HOURS "$QS_MCP_MAX_HOURS"
    # Settings the installer does not ask for are kept
    [[ -f $ENVF ]] && grep -E '^QUADSTACK_' "$ENVF" | grep -vE '^QUADSTACK_(LISTEN|PUBLIC_URL|OIDC_ISSUER|OIDC_CLIENT_ID|OIDC_CLIENT_SECRET|ALLOWED_GROUPS|ALLOWED_USERS|TERMINAL|MCP_MAX_HOURS)=' || true
  } >"$tmp"
  install -o "$QS_USER" -g "$(id -gn "$QS_USER")" -m 0600 "$tmp" "$ENVF"
  rm -f "$tmp"
  msg_ok "Wrote $ENVF"
fi

### Binary and service #######################################################################

if [[ $QS_MODE == reconfigure ]]; then
  TAG=$CUR # only the settings change
else
  TAG=$(resolve_version)
  install_binary "$QS_USER" "$TAG"
  install_podlet || msg_warn "podlet could not be installed; Import stays off"

  UNIT=$HOMEDIR/.config/systemd/user/quadstack.service
  userdir "$QS_USER" 0755 "$HOMEDIR/.config/systemd/user"
  tmp=$(mktemp)
  curl -fsSL -o "$tmp" "https://raw.githubusercontent.com/$REPO/$TAG/deploy/quadstack.service"
  install -o "$QS_USER" -g "$(id -gn "$QS_USER")" -m 0644 "$tmp" "$UNIT"
  rm -f "$tmp"
fi

msg_info "Starting Quadstack"
as_user "$QS_USER" systemctl --user daemon-reload
as_user "$QS_USER" systemctl --user enable --quiet quadstack.service
as_user "$QS_USER" systemctl --user restart quadstack.service
wait_active "$QS_USER"
LISTEN=$(envget QUADSTACK_LISTEN "$ENVF")
HOSTPART=${LISTEN%:*}
[[ $HOSTPART == 0.0.0.0 || -z $HOSTPART ]] && HOSTPART=$(hostname -I | awk '{print $1}')
CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://$HOSTPART:${LISTEN##*:}/" || true)
[[ $CODE == 302 ]] && msg_ok "Quadstack $TAG is running on http://$HOSTPART:${LISTEN##*:}" ||
  msg_warn "Quadstack is running, but http://$HOSTPART:${LISTEN##*:}/ answered $CODE instead of a redirect to the sign-in"

cat <<EOF

 ${GN}Next steps${CL}
  1. Point your reverse proxy at ${BL}http://$HOSTPART:${LISTEN##*:}${CL} for ${BL}$(envget QUADSTACK_PUBLIC_URL "$ENVF")${CL},
     with websockets enabled (logs, pulls and shells) and the Authorization header passed through (MCP).
  2. Open $(envget QUADSTACK_PUBLIC_URL "$ENVF") and sign in.
  3. Quadlets go in $HOMEDIR/.config/containers/systemd, or create stacks in the UI.

  Update or change the settings by running this installer again$([[ -x /usr/bin/update ]] && echo ", or with: update").
  Settings: $ENVF
  Service:  as $QS_USER, systemctl --user status quadstack / journalctl --user -u quadstack
EOF
