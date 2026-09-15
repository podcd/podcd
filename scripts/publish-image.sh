#!/usr/bin/env bash
# Build and push the multi-arch release image.

# usage: VERSION=1.2.3 scripts/publish-image.sh

set -euo pipefail

VERSION="${VERSION:?VERSION must be set, e.g. VERSION=1.2.3 scripts/publish-image.sh}"
IMAGE="${IMAGE:-ghcr.io/podcd/podcd}"

docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --build-arg VERSION="$VERSION" \
  -f Containerfile \
  -t "$IMAGE:$VERSION" \
  -t "$IMAGE:latest" \
  --push \
  .

echo "pushed $IMAGE:$VERSION and $IMAGE:latest"
