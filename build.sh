#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_PATH="${1:-$ROOT_DIR/dist/rate-limiter.wasm}"

if ! command -v tinygo >/dev/null 2>&1; then
  echo "tinygo is required but was not found in PATH" >&2
  echo "Install: https://tinygo.org/getting-started/install/" >&2
  exit 1
fi

mkdir -p "$(dirname "$OUTPUT_PATH")"

tinygo build -o "$OUTPUT_PATH" -scheduler=none -target=wasi "$ROOT_DIR"

echo "built wasm artifact: $OUTPUT_PATH"
