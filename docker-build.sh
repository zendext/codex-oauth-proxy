#!/usr/bin/env bash

set -euo pipefail

if [[ "${1:-}" != "" ]]; then
  echo "Usage: ./docker-build.sh"
  exit 1
fi

VERSION="$(git describe --tags --always --dirty)"
COMMIT="$(git rev-parse --short HEAD)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

export CODEX_OAUTH_PROXY_IMAGE="codex-oauth-proxy:latest"

docker compose build \
  --build-arg VERSION="${VERSION}" \
  --build-arg COMMIT="${COMMIT}" \
  --build-arg BUILD_DATE="${BUILD_DATE}"

docker compose up -d --remove-orphans --pull never
