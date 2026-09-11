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

die() { echo "[podcd] bootstrap: $*" >&2; exit 1; }
info() { echo "[podcd] bootstrap: $*"; }

while [ $# -gt 0 ]; do
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
    -h|--help)          sed -n '2,20p' "$0"; exit 0 ;;
    *)                  die "unknown argument: $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "run this as root (it installs packages and creates a user)"
[ -n "$REPO_URL" ] || die "--repo-url is required"
if [ -z "$RUN_USER" ]; then
  if [ -n "${SUDO_USER:-}" ] && [ "$(id -un)" = "root" ]; then
    RUN_USER="$SUDO_USER"
    info "no --user supplied; using sudo user $RUN_USER"
  elif [ "$(id -u)" -ne 0 ]; then
    RUN_USER="$(id -un)"
    info "no --user supplied; using current user $RUN_USER"
  else
    die "--user is required when running as root; use sudo to default to the invoking user or pass --user explicitly"
  fi
fi
if [ ! -r /etc/os-release ]; then
  die "cannot identify this distribution; this script targets Debian and Ubuntu"
fi
. /etc/os-release
case "${ID:-}${ID_LIKE:-}" in
  *debian*|*ubuntu*) ;;
  *) die "this script targets Debian/Ubuntu; ${PRETTY_NAME:-this system} is not supported yet" ;;
esac

# ---------------------------------------------------------------- packages ---
# uidmap gives rootless podman its subordinate uid range; dbus-user-session is
# what makes `systemctl --user` work for a user who is not logged in.
PACKAGES="podman uidmap dbus-user-session systemd-container git ca-certificates"
MISSING=""
for pkg in $PACKAGES; do
  dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null | grep -q "ok installed" || MISSING="$MISSING $pkg"
done
if [ -n "$MISSING" ]; then
  info "installing:$MISSING"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq $MISSING
else
  info "packages already installed"
fi

# Quadlet is what turns a .container file into a systemd service.
if [ ! -x /usr/libexec/podman/quadlet ] && [ ! -e /usr/lib/systemd/user-generators/podman-user-generator ]; then
  die "this podman has no quadlet generator (needs podman 4.4+); install a newer podman"
fi

# -------------------------------------------------------------------- user ---
if id "$RUN_USER" >/dev/null 2>&1; then
  info "user $RUN_USER already exists"
  current_shell="$(getent passwd "$RUN_USER" | cut -d: -f7)"
  if [ "$ALLOW_USER_LOGIN" = true ]; then
    if [ "$current_shell" != "/bin/bash" ]; then
      usermod --shell /bin/bash "$RUN_USER"
      info "user $RUN_USER shell updated to /bin/bash because --allow-user-login was set"
    fi
  else
    if [ "$current_shell" != "/usr/sbin/nologin" ]; then
      usermod --shell /usr/sbin/nologin "$RUN_USER"
      info "user $RUN_USER shell locked to /usr/sbin/nologin by default"
    fi
  fi
else
  info "creating user $RUN_USER"
  if [ "$ALLOW_USER_LOGIN" = true ]; then
    useradd --create-home --shell /bin/bash --comment "podcd agent" "$RUN_USER"
    info "user $RUN_USER can log in via /bin/bash because --allow-user-login was set"
  else
    useradd --create-home --shell /usr/sbin/nologin --comment "podcd agent" "$RUN_USER"
  fi
fi
RUN_HOME="$(getent passwd "$RUN_USER" | cut -d: -f6)"
RUN_UID="$(id -u "$RUN_USER")"
[ -n "$RUN_HOME" ] || die "user $RUN_USER has no home directory"

# Rootless podman needs a subordinate id range. useradd usually grants one;
# make sure, because the failure mode otherwise is a confusing runtime error.
if ! grep -q "^$RUN_USER:" /etc/subuid 2>/dev/null; then
  info "granting subuid/subgid range to $RUN_USER"
  usermod --add-subuids 200000-265535 --add-subgids 200000-265535 "$RUN_USER"
fi

# Lingering is what makes the user's services start at boot without a login.
# It is also what makes "reboot the VM and the app comes back" possible.
if [ "$(loginctl show-user "$RUN_USER" --property=Linger --value 2>/dev/null || echo no)" != "yes" ]; then
  info "enabling lingering for $RUN_USER"
  loginctl enable-linger "$RUN_USER"
fi

# ---------------------------------------------------------------- binaries ---
fetch_release_binaries() {
  # Download the published release tarball, matching the GitHub release asset
  # naming convention created by scripts/build-release.sh.
  local version="${RELEASE_VERSION:-latest}"
  local os arch tag_name url tmpdir

  case "$(uname -s)" in
    Linux) os="linux" ;;
    Darwin) os="darwin" ;;
    *) die "unsupported OS for release download: $(uname -s)" ;;
  esac

  case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) die "unsupported architecture for release download: $(uname -m)" ;;
  esac

  if [ "$version" = "latest" ]; then
    info "fetching latest podcd release metadata from GitHub"
    tag_name="$(curl -fsSL "https://api.github.com/repos/podcd/podcd/releases/latest" \
      | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
    [ -n "$tag_name" ] || die "unable to determine the latest podcd release"
    version="${tag_name#v}"
  elif [[ "$version" == v* ]]; then
    tag_name="$version"
    version="${version#v}"
  else
    tag_name="v$version"
  fi

  url="https://github.com/podcd/podcd/releases/download/${tag_name}/podcd_${version}_${os}_${arch}.tar.gz"
  tmpdir="$(mktemp -d)"

  info "downloading podcd release ${tag_name} from $url"
  curl -fsSL "$url" -o "$tmpdir/podcd.tar.gz" || {
    rm -rf "$tmpdir"
    die "failed to download podcd release ${tag_name}"
  }
  tar -xzf "$tmpdir/podcd.tar.gz" -C "$tmpdir"

  install -m 0755 "$tmpdir/podcd" /usr/local/bin/podcd
  install -m 0755 "$tmpdir/podcd-agent" /usr/local/bin/podcd-agent
  rm -rf "$tmpdir"
  info "installed /usr/local/bin/podcd and /usr/local/bin/podcd-agent from the GitHub release"
}

install_binary() {
  local name="$1" src="$2"
  if [ -n "$src" ] && [ -x "$src" ]; then
    install -m 0755 "$src" "/usr/local/bin/$name"
    info "installed /usr/local/bin/$name"
  elif [ -x "/usr/local/bin/$name" ]; then
    info "/usr/local/bin/$name already installed"
  else
    info "no local $name binary found; downloading the latest GitHub release"
    fetch_release_binaries
  fi
}
install_binary podcd-agent "${BINARY_DIR:+$BINARY_DIR/podcd-agent}"
install_binary podcd "${BINARY_DIR:+$BINARY_DIR/podcd}"

# ------------------------------------------------------------------ config ---
CONFIG_DIR="$RUN_HOME/.config/podcd"
STATE_DIR="$RUN_HOME/.local/state/podcd"
UNIT_DIR="$RUN_HOME/.config/containers/systemd"
SERVICE_DIR="$RUN_HOME/.config/systemd/user"

install -d -o "$RUN_USER" -g "$RUN_USER" -m 0755 "$CONFIG_DIR" "$STATE_DIR" "$UNIT_DIR" "$SERVICE_DIR"

if [ -f "$CONFIG_DIR/agent.yaml" ] && grep -q '^\[podcd\]' "$CONFIG_DIR/agent.yaml" 2>/dev/null; then
  info "rewriting legacy invalid $CONFIG_DIR/agent.yaml"
  rm -f "$CONFIG_DIR/agent.yaml"
fi

if [ -f "$CONFIG_DIR/agent.yaml" ]; then
  info "keeping the existing $CONFIG_DIR/agent.yaml (delete it to regenerate)"
else
  info "writing $CONFIG_DIR/agent.yaml via podcd config create"
  as_user /usr/local/bin/podcd config create \
    --path "$CONFIG_DIR/agent.yaml" \
    --host "${HOST_NAME:-}" \
    --repo-url "$REPO_URL" \
    --repo-name "$REPO_NAME" \
    --repo-path "${REPO_PATH:-}" \
    --revision "$REVISION" \
    --interval "$INTERVAL"
  chown "$RUN_USER:$RUN_USER" "$CONFIG_DIR/agent.yaml"
  chmod 0644 "$CONFIG_DIR/agent.yaml"
fi

# Secrets are read from this file by the agent and never from Git. It is
# created empty and unreadable to anyone else.
if [ ! -f "$CONFIG_DIR/agent.env" ]; then
  printf '# Secrets for env: references, one KEY=value per line.\n' > "$CONFIG_DIR/agent.env"
  chown "$RUN_USER:$RUN_USER" "$CONFIG_DIR/agent.env"
  chmod 0600 "$CONFIG_DIR/agent.env"
fi

# ----------------------------------------------------------------- service ---
export XDG_RUNTIME_DIR="/run/user/$RUN_UID"
as_user() { setpriv --reuid "$RUN_UID" --regid "$(id -g "$RUN_USER")" --init-groups \
  env XDG_RUNTIME_DIR="/run/user/$RUN_UID" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$RUN_UID/bus" "$@"; }

if ! as_user /usr/local/bin/podcd install >/dev/null 2>&1; then
  # Older builds and some direct local runs may not yet have the install helper.
  # Fall back to the checked-in unit file when it is present in the repo checkout.
  SERVICE_SRC="$(dirname "$0")/podcd-agent.service"
  if [ -f "$SERVICE_SRC" ]; then
    install -o "$RUN_USER" -g "$RUN_USER" -m 0644 "$SERVICE_SRC" "$SERVICE_DIR/podcd-agent.service"
  else
    die "podcd-agent.service not found and the podcd install command failed"
  fi
fi

# The user manager may need a moment after enable-linger before it accepts
# commands; this is the one race in the whole bootstrap.
for _ in 1 2 3 4 5 6 7 8 9 10; do
  as_user systemctl --user is-system-running >/dev/null 2>&1 && break
  sleep 1
done

info "enabling podcd-agent.service for $RUN_USER"
as_user systemctl --user daemon-reload
as_user systemctl --user enable --now podcd-agent.service

cat <<EOF

podcd is bootstrapped.

  user:      $RUN_USER (lingering enabled, so it survives a reboot)
  config:    $CONFIG_DIR/agent.yaml
  units:     $UNIT_DIR
  state:     $STATE_DIR

next:
  sudo -u $RUN_USER XDG_RUNTIME_DIR=/run/user/$RUN_UID podcd status
  sudo -u $RUN_USER XDG_RUNTIME_DIR=/run/user/$RUN_UID podcd plan
  journalctl _UID=$RUN_UID -u podcd-agent --user -f

EOF
