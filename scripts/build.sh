#!/bin/sh
# Builds the plugin for the CPU architecture of the Docker host.
# Usage: sh scripts/build.sh [version]
set -eu
VERSION=${1:-0.0.0-dev}
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
mkdir -p "$ROOT/dist"
docker run --rm -v "$ROOT:/src" -w /src -v lamplighter-gomod:/go/pkg/mod golang:1.26-bookworm \
  sh -c "go build -buildvcs=false -trimpath -buildmode=c-shared -ldflags '-s -w -X main.version=$VERSION' -o dist/lamplighter-v$VERSION.so . && rm -f dist/lamplighter-v$VERSION.h"
echo "$ROOT/dist/lamplighter-v$VERSION.so"
