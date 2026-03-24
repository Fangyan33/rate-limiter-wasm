# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Environment and common commands

- Go version: `go 1.22` ([go.mod](go.mod))
- Primary build target is a Proxy-WASM module compiled for WASI.

### Build the WASM artifact

- Default output:
  - `bash ./build.sh`
- Custom output path:
  - `bash ./build.sh ./dist/custom-name.wasm`

`build.sh` compiles the module root with TinyGo for Proxy-WASM compatibility:
- `tinygo build`
- `-target=wasi`
- `-scheduler=none`

Default artifact path:
- `dist/rate-limiter.wasm`

### Run tests

- Full test suite:
  - `go test ./... -count=1`
- Single package:
  - `go test ./internal/plugin -count=1`
- Single test:
  - `go test ./internal/plugin -run TestPluginRejectsMissingAuthorizationHeader -count=1`
