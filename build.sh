#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_PATH="${1:-$ROOT_DIR/dist/rate-limiter.wasm}"

if ! command -v go >/dev/null 2>&1; then
  echo "go is required but was not found in PATH" >&2
  exit 1
fi

mkdir -p "$(dirname "$OUTPUT_PATH")"

GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -o "$OUTPUT_PATH" "$ROOT_DIR"

echo "built wasm artifact: $OUTPUT_PATH"
