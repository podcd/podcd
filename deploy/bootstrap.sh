#!/usr/bin/env bash
# Bootstrap a Debian/Ubuntu VM for podcd.
#
# Idempotent: run it as many times as you like.
# 
# It installs rootless podman, creates an unprivileged user to own the applications;
# writes the agent configuration and starts the agent as a systemd *user* service.
#
# usage:
#   sudo ./bootstrap.sh --repo-url https://github.com/podcd/podcd-gitops.git \
#                       [--revision main] [--repo-path clusters/prod] \
#                       [--user podcd] [--host prod-web-01] \
#                       [--binaries ./dist] [--release-version 1.0.0] [--interval 60s]
#
# If --user is omitted, the script defaults to the current user when run as a
# normal user, or to the invoking sudo user when run as root.

set -euo pipefail

REPO_URL=""
REVISION="main"
REPO_PATH=""
REPO_NAME="infrastructure"
RUN_USER=""
HOST_NAME=""
BINARY_DIR=""
RELEASE_VERSION=""
ALLOW_USER_LOGIN=false
INTERVAL="60s"

die() {
  echo "[podcd] bootstrap: $*" >&2
  exit 1
}

info() {
  echo "[podcd] bootstrap: $*"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo-url)         REPO_URL="$2"; shift 2 ;;
    --revision)         REVISION="$2"; shift 2 ;;
    --repo-path)        REPO_PATH="$2"; shift 2 ;;
    --repo-name)        REPO_NAME="$2"; shift 2 ;;
    --user)             RUN_USER="$2"; shift 2 ;;
    --host)             HOST_NAME="$2"; shift 2 ;;
    --binaries)         BINARY_DIR="$2"; shift 2 ;;
    --release-version)  RELEASE_VERSION="$2"; shift 2 ;;
    --allow-user-login) ALLOW_USER_LOGIN=true; shift ;;
    --interval)         INTERVAL="$2"; shift 2 ;;
    -h|--help)
      sed -n '2,25p' "$0"
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

[[ "$(id -u)" -eq 0 ]] || die "run this as root"
[[ -n "$REPO_URL" ]] || die "--repo-url is required"

# --------------------------------------------------------------------------- OS

[[ -r /etc/os-release ]] || die "cannot identify this distribution"
. /etc/os-release

case "${ID:-}${ID_LIKE:-}" in
  *debian*|*ubuntu*) ;;
  *) die "this script only supports Debian/Ubuntu" ;;
esac

# ------------------------------------------------------------------------- user

if [[ -z "$RUN_USER" ]]; then
  if [[ -n "${SUDO_USER:-}" ]]; then
    RUN_USER="$SUDO_USER"
  else
    die "--user is required when running as root"
  fi
fi

if id "$RUN_USER" >/dev/null 2>&1; then
  info "user $RUN_USER already exists"
else
  info "creating user $RUN_USER"

  if [[ "$ALLOW_USER_LOGIN" == true ]]; then
    useradd \
      --create-home \
      --shell /bin/bash \
      --comment "podcd agent" \
      "$RUN_USER"
  else
    useradd \
      --create-home \
      --shell /usr/sbin/nologin \
      --comment "podcd agent" \
      "$RUN_USER"
  fi
fi

if [[ "$ALLOW_USER_LOGIN" == true ]]; then
  usermod --shell /bin/bash "$RUN_USER"
else
  usermod --shell /usr/sbin/nologin "$RUN_USER"
fi

RUN_HOME="$(getent passwd "$RUN_USER" | cut -d: -f6)"
RUN_UID="$(id -u "$RUN_USER")"

[[ -n "$RUN_HOME" ]] || die "user $RUN_USER has no home directory"

# -------------------------------------------------------------------- run-as-user

export XDG_RUNTIME_DIR="/run/user/$RUN_UID"

as_user() {
  setpriv \
    --reuid "$RUN_UID" \
    --regid "$(id -g "$RUN_USER")" \
    --init-groups \
    env \
      HOME="$RUN_HOME" \
      USER="$RUN_USER" \
      LOGNAME="$RUN_USER" \
      XDG_RUNTIME_DIR="/run/user/$RUN_UID" \
      DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$RUN_UID/bus" \
      "$@"
}

# ---------------------------------------------------------------------- packages

PACKAGES=(
  podman
  uidmap
  dbus-user-session
  systemd-container
  util-linux
  git
  ca-certificates
)

MISSING=()

for pkg in "${PACKAGES[@]}"; do
  if ! dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null \
      | grep -q "ok installed"; then
    MISSING+=("$pkg")
  fi
done

if ((${#MISSING[@]})); then
  info "installing packages: ${MISSING[*]}"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq "${MISSING[@]}"
else
  info "packages already installed"
fi

# Quadlet is required for rootless container services.
if [[ ! -x /usr/libexec/podman/quadlet ]] &&
   [[ ! -e /usr/lib/systemd/user-generators/podman-user-generator ]]; then
  die "Podman does not provide Quadlet; install Podman 4.4+"
fi

# ------------------------------------------------------------------------- podman

install -d \
  -o "$RUN_USER" \
  -g "$RUN_USER" \
  -m 0755 \
  "$RUN_HOME/.local" \
  "$RUN_HOME/.local/share" \
  "$RUN_HOME/.local/share/containers" \
  "$RUN_HOME/.config"

if ! grep -q "^$RUN_USER:" /etc/subuid 2>/dev/null; then
  info "adding subordinate UID/GID ranges for $RUN_USER"
  usermod \
    --add-subuids 200000-265535 \
    --add-subgids 200000-265535 \
    "$RUN_USER"
fi

loginctl enable-linger "$RUN_USER"

# ----------------------------------------------------------------------- binaries

download_release() {
  local version="${RELEASE_VERSION:-latest}"
  local tag_name os arch url tmpdir

  case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac

  case "$version" in
    latest)
      info "fetching latest podcd release"
      tag_name="$(
        curl -fsSL \
          https://api.github.com/repos/podcd/podcd/releases/latest |
        sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' |
        head -n1
      )"
      [[ -n "$tag_name" ]] || die "unable to determine latest podcd release"
      version="${tag_name#v}"
      ;;
    v*)
      tag_name="$version"
      version="${version#v}"
      ;;
    *)
      tag_name="v$version"
      ;;
  esac

  url="https://github.com/podcd/podcd/releases/download/$tag_name/podcd_${version}_linux_${arch}.tar.gz"

  tmpdir="$(mktemp -d)"
  trap 'rm -rf "$tmpdir"' RETURN

  info "downloading podcd $tag_name"
  curl -fsSL "$url" -o "$tmpdir/podcd.tar.gz"
  tar -xzf "$tmpdir/podcd.tar.gz" -C "$tmpdir"

  install -m 0755 "$tmpdir/podcd" /usr/local/bin/podcd
  install -m 0755 "$tmpdir/podcd-agent" /usr/local/bin/podcd-agent
}

if [[ -n "$BINARY_DIR" ]]; then
  install -m 0755 "$BINARY_DIR/podcd" /usr/local/bin/podcd
  install -m 0755 "$BINARY_DIR/podcd-agent" /usr/local/bin/podcd-agent
  info "installed podcd binaries from $BINARY_DIR"
else
  # The bootstrap depends on `podcd install`, so an old CLI is not usable.
  if [[ ! -x /usr/local/bin/podcd ]] ||
     ! /usr/local/bin/podcd install --help >/dev/null 2>&1; then
    download_release
  else
    info "podcd binaries already installed"
  fi
fi

# Make the dependency explicit rather than falling back to a local service file.
if ! /usr/local/bin/podcd install --help >/dev/null 2>&1; then
  die "installed podcd CLI does not support 'podcd install'; install a newer release"
fi

# ------------------------------------------------------------------------- config

CONFIG_DIR="$RUN_HOME/.config/podcd"
STATE_DIR="$RUN_HOME/.local/state/podcd"
UNIT_DIR="$RUN_HOME/.config/containers/systemd"

install -d \
  -o "$RUN_USER" \
  -g "$RUN_USER" \
  -m 0755 \
  "$CONFIG_DIR" \
  "$STATE_DIR" \
  "$UNIT_DIR"

if [[ -f "$CONFIG_DIR/agent.yaml" ]]; then
  info "keeping existing $CONFIG_DIR/agent.yaml"
else
  info "creating $CONFIG_DIR/agent.yaml"

  as_user /usr/local/bin/podcd config create \
    --path "$CONFIG_DIR/agent.yaml" \
    --host "${HOST_NAME:-}" \
    --repo-url "$REPO_URL" \
    --repo-name "$REPO_NAME" \
    --repo-path "${REPO_PATH:-}" \
    --revision "$REVISION" \
    --interval "$INTERVAL"
fi

chown "$RUN_USER:$RUN_USER" "$CONFIG_DIR/agent.yaml"
chmod 0644 "$CONFIG_DIR/agent.yaml"

if [[ ! -f "$CONFIG_DIR/agent.env" ]]; then
  cat > "$CONFIG_DIR/agent.env" <<'EOF'
# Secrets for env: references.
# One KEY=value per line.
EOF

  chown "$RUN_USER:$RUN_USER" "$CONFIG_DIR/agent.env"
  chmod 0600 "$CONFIG_DIR/agent.env"
fi

# ----------------------------------------------------------------------- service

info "installing podcd-agent.service"

as_user /usr/local/bin/podcd install

# enable-linger can take a moment to bring up the user manager.
for _ in {1..10}; do
  if as_user systemctl --user is-system-running >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

info "starting podcd-agent.service"

as_user systemctl --user daemon-reload
as_user systemctl --user enable --now podcd-agent.service

# ------------------------------------------------------------------------- done

cat <<EOF

podcd is bootstrapped.

  user:      $RUN_USER
  config:    $CONFIG_DIR/agent.yaml
  units:     $UNIT_DIR
  state:     $STATE_DIR

next:
  sudo -u $RUN_USER XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR podcd status
  sudo -u $RUN_USER XDG_RUNTIME_DIR=$XDG_RUNTIME_DIR podcd plan
  journalctl _UID=$RUN_UID -u podcd-agent --user -f

EOF
