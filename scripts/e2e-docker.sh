#!/usr/bin/env bash
#
# Run the docker runtime's end-to-end tests (test/e2e, the Docker* tests).
#
# With a real Docker daemon and Compose v2 on this machine, the tests run
# here. Without one - podman's docker shim does not count - they run inside
# a privileged container that starts its own dockerd (docker-in-docker), so
# any Linux box with rootful podman or docker can run them. The container
# gets the checkout and the Go module cache mounted, and keeps its images
# in a named volume so the second run does not pull again.
#
# Usage:
#   scripts/e2e-docker.sh            # make test-e2e-docker
#   E2E_ENGINE=docker scripts/e2e-docker.sh   # force the engine for the container

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
GO_TEST=(go test ./test/e2e/ -run 'Docker' -v -timeout 15m)

die() { printf '%s\n' "$*" >&2; exit 1; }
say() { printf '\033[1m==>\033[0m %s\n' "$*"; }

real_docker() {
  command -v docker >/dev/null || return 1
  local v
  v=$(docker version 2>/dev/null) || return 1
  [[ ${v,,} != *podman* ]] || return 1
  docker compose version --short 2>/dev/null | grep -qv '^1\.' || return 1
}

if real_docker; then
  say "docker daemon found; running the tests here"
  cd "$REPO_ROOT"
  PODCD_E2E=1 "${GO_TEST[@]}"
  exit
fi

# No daemon: docker-in-docker in a privileged container. The daemon inside
# needs real root for its cgroups, iptables and overlay, so a rootless podman
# will not do; go through sudo when not root already.
ENGINE=${E2E_ENGINE:-}
if [[ -z $ENGINE ]]; then
  if command -v podman >/dev/null; then ENGINE=podman
  elif command -v docker >/dev/null; then ENGINE=docker
  else die "neither podman nor docker is installed; nothing can run the docker-in-docker container"
  fi
fi
SUDO=()
if [[ $(id -u) != 0 ]]; then
  if [[ $ENGINE == podman ]] || ! docker info >/dev/null 2>&1; then
    SUDO=(sudo)
  fi
fi

GO_VERSION=$(awk '/^go /{print $2; exit}' "$REPO_ROOT/go.mod")
[[ -n $GO_VERSION ]] || die "could not read the go version from go.mod"
IMAGE="docker.io/library/golang:${GO_VERSION%.*}-alpine"
MODCACHE=$(go env GOMODCACHE 2>/dev/null || echo "$HOME/go/pkg/mod")
mkdir -p "$MODCACHE"

say "no docker daemon here; running docker-in-docker with ${SUDO[*]:-} $ENGINE ($IMAGE)"
"${SUDO[@]}" "$ENGINE" run --rm --privileged \
  --volume "$REPO_ROOT:/src" \
  --volume "$MODCACHE:/go/pkg/mod" \
  --volume podcd-e2e-docker:/var/lib/docker \
  --env PODCD_E2E=1 \
  --env GOFLAGS=-buildvcs=false \
  --workdir /src \
  "$IMAGE" sh -euc '
    apk add --no-cache docker docker-cli-compose git >/dev/null
    dockerd >/tmp/dockerd.log 2>&1 &
    for i in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
    docker info >/dev/null 2>&1 || { cat /tmp/dockerd.log; echo "dockerd did not come up" >&2; exit 1; }
    '"${GO_TEST[*]}"'
  '
