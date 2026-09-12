#!/usr/bin/env bash
# Cross-compile podcd for release and package it for
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
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o "$workdir/podcd" ./cmd/podcd
  archive="podcd_${VERSION}_${GOOS}_${GOARCH}.tar.gz"
  tar -C "$workdir" -czf "$OUT/$archive" podcd
  # Basename only, so `sha256sum -c` works wherever the archive is downloaded to.
  ( cd "$OUT" && sha256sum "$archive" > "$archive.sha256" )
  rm -rf "$workdir"
done

echo "release artifacts:"
ls -la "$OUT"
