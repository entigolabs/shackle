#!/bin/sh
# Builds the release binaries into dist/ with their checksums:
#
#   scripts/build.sh [version]
#
# The binaries are static (no cgo) and built without paths or VCS state, so
# the same source and Go version give the same bytes.
set -eu
version=${1:-dev}
cd "$(dirname "$0")/.."
rm -rf dist
mkdir -p dist
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X main.version=$version" -o "dist/shackle-linux-$arch" .
done
(cd dist && sha256sum shackle-linux-* > SHA256SUMS)
cat dist/SHA256SUMS
