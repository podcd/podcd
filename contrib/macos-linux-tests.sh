#!/usr/bin/env bash
#
# Run podcd's Linux-only test targets from a Mac.
#
# Usage:
#   contrib/macos-linux-tests.sh            # make test
#   contrib/macos-linux-tests.sh test-e2e   # the real end-to-end suite
#   contrib/macos-linux-tests.sh vet lint   # any number of make targets
#
# Undo everything it installs with:
#   podman machine ssh 'rm -rf ~/.local/go ~/go && sudo rpm-ostree uninstall make'
# or just throw the machine away.

set -euo pipefail

TARGETS=("${@:-test}")
REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

die() { printf '%s\n' "$*" >&2; exit 1; }
say() { printf '\033[1m==>\033[0m %s\n' "$*"; }

[[ $(uname -s) == Darwin ]] || die "this script is for macOS; elsewhere just run: make ${TARGETS[*]}"
command -v podman >/dev/null || die "podman is not installed (brew install podman)"

# The VM only sees paths it mounts, and by default that is your home directory.
case "$REPO_ROOT" in
  "$HOME"/*) ;;
  *) die "the checkout must live under $HOME for the podman machine to see it; this one is at $REPO_ROOT" ;;
esac

if ! podman machine inspect >/dev/null 2>&1; then
  say "creating a podman machine"
  podman machine init
fi
if ! podman machine ssh true >/dev/null 2>&1; then
  say "starting the podman machine"
  podman machine start
fi

# A long-lived machine can end up with a gvproxy that has stopped answering DNS
# while routing still works, which otherwise surfaces much later as a confusing
# "could not resolve host" from curl or from `podman pull`.
if ! podman machine ssh 'getent hosts go.dev >/dev/null 2>&1'; then
  die "the VM cannot resolve DNS (routing is probably fine, the resolver is not).
Restart it to rebuild the network, then run this again:
    podman machine stop && podman machine start"
fi

# The toolchain version the module asks for, so the VM matches the Mac.
GO_VERSION=$(awk '/^go /{print $2; exit}' "$REPO_ROOT/go.mod")
[[ -n $GO_VERSION ]] || die "could not read the go version from go.mod"

# Fedora CoreOS has no package manager for this; a user-local tarball needs no
# root and leaves the image untouched.
say "ensuring Go $GO_VERSION is in the VM"
podman machine ssh "
  set -euo pipefail
  want=go$GO_VERSION
  if [ \"\$(~/.local/go/bin/go version 2>/dev/null | awk '{print \$3}')\" = \"\$want\" ]; then
    echo \"\$want already installed\"
  else
    arch=\$(uname -m); case \"\$arch\" in aarch64) arch=arm64;; x86_64) arch=amd64;; esac
    echo \"downloading \$want for linux-\$arch\"
    curl -fsSL \"https://go.dev/dl/\$want.linux-\$arch.tar.gz\" -o /tmp/go.tgz
    rm -rf ~/.local/go && mkdir -p ~/.local
    tar -C ~/.local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz
  fi
"

if ! podman machine ssh 'command -v make >/dev/null 2>&1'; then
  say "installing make in the VM (rpm-ostree, no reboot)"
  podman machine ssh 'sudo rpm-ostree install --apply-live --allow-inactive -y make' >/dev/null ||
    die "could not install make in the VM; try: podman machine ssh 'sudo rpm-ostree install --apply-live make'"
fi

if ! podman machine ssh 'systemctl is-active --quiet network-online.target'; then
  say "activating network-online.target in the VM (else every unit start stalls)"
  podman machine ssh '
    sudo systemctl start network-online.target
    systemctl --user reset-failed podman-user-wait-network-online.service 2>/dev/null || true
  '
fi

podman machine ssh "loginctl enable-linger \$(id -un) 2>/dev/null || true"

if podman machine ssh 'systemctl --user is-active --quiet podcd-agent.service'; then
  say "WARNING: podcd-agent.service is running in the VM and shares the unit
    directory with the tests. Stop it first: podman machine ssh 'systemctl --user stop podcd-agent.service'"
fi

say "running: make ${TARGETS[*]}"
# CGO_ENABLED=0 because the image has no C compiler, and the Makefile turns the
# race detector on only when cgo is available. The Mac and CI still run it.
podman machine ssh "
  set -euo pipefail
  export PATH=\$HOME/.local/go/bin:\$PATH CGO_ENABLED=0
  cd '$REPO_ROOT'
  make ${TARGETS[*]}
"
