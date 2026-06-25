#!/usr/bin/env bash
set -euo pipefail

# Build script for Hyperstack agent
# Supports: amd64, arm64; static builds; version embedding

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)
BIN_DIR="$ROOT_DIR/bin"
BINARY_NAME="hyperstack-agent"

mkdir -p "$BIN_DIR"

VERSION=${VERSION:-$(git -C "$ROOT_DIR" describe --tags --always 2>/dev/null || echo "dev")}
DATE=${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}

ldflags="-s -w -X main.version=$VERSION -X main.date=$DATE"

pushd "$ROOT_DIR" >/dev/null

echo "Building for linux/amd64..."
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$ldflags" -o "$BIN_DIR/${BINARY_NAME}-linux-amd64" ./cmd/agent

echo "Building for linux/arm64..."
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$ldflags" -o "$BIN_DIR/${BINARY_NAME}-linux-arm64" ./cmd/agent

echo "Building static (amd64)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -a -ldflags "$ldflags -extldflags '-static'" -o "$BIN_DIR/${BINARY_NAME}-static-linux-amd64" ./cmd/agent

popd >/dev/null

echo "Done. Artifacts in $BIN_DIR"

for artifact in "$BIN_DIR"/"$BINARY_NAME"-*; do
  [ -f "$artifact" ] || continue
  sha256sum "$artifact" > "$artifact.sha256"
  echo "Checksum written: $artifact.sha256"
done
