#!/usr/bin/env bash
# Cross-compile podcd and podcd-agent for release and package them for
# GitHub Releases. Invoked by semantic-release (see .releaserc.json) with
# VERSION set to the version being released.
#
# usage: VERSION=1.2.3 scripts/build-release.sh

set -euo pipefail

VERSION="${VERSION:?VERSION must be set, e.g. VERSION=1.2.3 scripts/build-release.sh}"
PLATFORMS="linux/amd64 linux/arm64"
OUT="dist/release"
LDFLAGS="-X github.com/podcd/podcd/internal/cli.Version=$VERSION"

rm -rf "$OUT"
mkdir -p "$OUT"

for platform in $PLATFORMS; do
  GOOS="${platform%/*}"
  GOARCH="${platform#*/}"
  workdir="$(mktemp -d)"
  echo "building $GOOS/$GOARCH"
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o "$workdir/podcd" ./cmd/cli
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o "$workdir/podcd-agent" ./cmd/agent
  tar -C "$workdir" -czf "$OUT/podcd_${VERSION}_${GOOS}_${GOARCH}.tar.gz" podcd podcd-agent
  rm -rf "$workdir"
done

( cd "$OUT" && sha256sum -- *.tar.gz > checksums.txt )

echo "release artifacts:"
ls -la "$OUT"
