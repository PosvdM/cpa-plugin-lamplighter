#!/bin/sh
# Builds the plugin library in CI for the runner's OS into dist/.
# Usage: sh scripts/ci-build.sh <goarch>
# The version comes from a v* tag, otherwise 0.0.0-dev.<short commit>.
set -eu
GOARCH=$1
case "${GITHUB_REF:-}" in
  refs/tags/v*) VERSION=${GITHUB_REF#refs/tags/v} ;;
  *) VERSION=0.0.0-dev.$(printf %.7s "${GITHUB_SHA:-unknown}") ;;
esac
GOOS=$(go env GOHOSTOS)
case $GOOS in
  linux) EXT=so ;;
  darwin)
    EXT=dylib
    # The macOS runner is arm64; clang builds the amd64 library with -arch.
    case $GOARCH in amd64) export CC="clang -arch x86_64" ;; *) export CC="clang -arch arm64" ;; esac
    ;;
  windows) EXT=dll ;;
  *) echo "unsupported OS $GOOS" >&2; exit 1 ;;
esac
export CGO_ENABLED=1 GOARCH
go build -buildvcs=false -trimpath -buildmode=c-shared \
  -ldflags "-s -w -X main.version=$VERSION" \
  -o "dist/lamplighter-v$VERSION.$EXT" .
rm -f dist/*.h
