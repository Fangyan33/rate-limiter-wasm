#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
DIST_DIR="${1:-$ROOT_DIR/dist}"

if ! command -v tinygo >/dev/null 2>&1; then
  echo "tinygo is required but was not found in PATH" >&2
  echo "Install: https://tinygo.org/getting-started/install/" >&2
  exit 1
fi

mkdir -p "$DIST_DIR"

GOFLAGS="${GOFLAGS:-} -buildvcs=false" \
  tinygo build -o "$DIST_DIR/rate-limiter.wasm" -scheduler=none -target=wasi "$ROOT_DIR/rate-limiter/cmd/wasm"

GOFLAGS="${GOFLAGS:-} -buildvcs=false" \
  tinygo build -o "$DIST_DIR/token-stats.wasm" -scheduler=none -target=wasi "$ROOT_DIR/token-stats/cmd/wasm"

echo "built wasm artifacts:"
echo "  $DIST_DIR/rate-limiter.wasm"
echo "  $DIST_DIR/token-stats.wasm"

sha256sum "$DIST_DIR/rate-limiter.wasm" "$DIST_DIR/token-stats.wasm"
