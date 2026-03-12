#!/usr/bin/env bash
set -euo pipefail

OUTPUT=${1:-dist/counter-service}

mkdir -p "$(dirname "$OUTPUT")"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -o "$OUTPUT" ./cmd/counter-service

echo "built $OUTPUT"
