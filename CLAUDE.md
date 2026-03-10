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

`build.sh` compiles the module root with:
- `GOOS=wasip1`
- `GOARCH=wasm`
- `CGO_ENABLED=0`

Default artifact path:
- `dist/rate-limiter.wasm`

### Run tests

- Full test suite:
  - `go test ./... -count=1`
- Single package:
  - `go test ./internal/plugin -count=1`
- Single test:
  - `go test ./internal/plugin -run TestPluginRejectsMissingAuthorizationHeader -count=1`

Useful focused packages:
- Config parsing tests: `go test ./internal/config -count=1`
- Limiter behavior tests: `go test ./internal/limiter -count=1`
- Store client tests: `go test ./internal/store -count=1`

### Linting

There is currently no repository-specific lint command, Makefile, or CI wrapper checked in. Prefer `go test ./... -count=1` as the main verification command unless a new lint step is added later.

## High-level architecture

This repository is a Go Proxy-WASM plugin for Envoy/Istio that enforces per-API-key concurrent request limits.

### Request flow

The runtime entrypoint is [main.go](main.go), which only registers the VM context with the Proxy-WASM SDK. The real behavior starts in [internal/plugin/root.go](internal/plugin/root.go):

1. `OnPluginStart` reads the plugin configuration supplied by Envoy/Istio.
2. `LoadConfiguration` parses YAML into the internal config model, builds the domain matcher, and constructs the request limiter.
3. For each HTTP stream, `OnHttpRequestHeaders`:
   - reads `:authority`
   - checks whether the host matches configured domains
   - reads `authorization`
   - extracts a Bearer token as the API key
   - tries to acquire a concurrency slot
4. If acquisition fails, the plugin sends a local HTTP rejection response.
5. When the stream ends, `OnHttpStreamDone` releases the acquired slot.

The plugin only enforces limits for matched domains. Unmatched hosts bypass all auth and limiter logic.

### Configuration model

The YAML schema is defined and validated in [internal/config/config.go](internal/config/config.go).

Important sections:
- `domains`: hostnames or wildcard host patterns to protect
- `rate_limits`: per-API-key concurrency limits
- `distributed_store`: legacy/validation path for Redis-shaped config still covered by tests
- `distributed_limit`: current switch for distributed limiting via `counter_service`
- `error_response`: local rejection status/message

Current distributed mode requires:
- `distributed_limit.enabled: true`
- `distributed_limit.backend: counter_service`
- `distributed_limit.counter_service.cluster` to be non-empty

### Matching and auth

- [internal/matcher/domain.go](internal/matcher/domain.go) normalizes and matches exact domains plus `*.` wildcard suffixes.
- [internal/auth/bearer.go](internal/auth/bearer.go) parses `Authorization: Bearer <api_key>` and rejects missing, malformed, or empty tokens.

These two packages together decide whether a request is subject to limiting and which API key bucket it uses.

### Limiter design

There are two limiter layers:

- [internal/limiter/local.go](internal/limiter/local.go): in-memory per-key concurrent limiter with idempotent release closures.
- [internal/limiter/distributed.go](internal/limiter/distributed.go): wrapper that prefers a distributed store when available, but falls back to the local limiter when the store is unavailable.

The important non-obvious behavior in `DistributedLimiter` is the fallback/recovery model:
- if the distributed store errors, the limiter switches to local fallback mode
- while fallback-era requests are still in flight, recovery back to distributed mode is intentionally blocked
- once fallback in-flight requests drain, successful store acquisition can switch the limiter back to distributed mode

This behavior is heavily specified by tests in [internal/limiter/distributed_test.go](internal/limiter/distributed_test.go). Preserve those semantics when modifying fallback logic.

### Distributed store boundary

The plugin does not talk to Redis directly anymore during request limiting.

[internal/store/client.go](internal/store/client.go) currently builds a `counter_service` client abstraction that satisfies the limiter’s `DistributedStore` interface. Right now it is a placeholder/no-op implementation that returns `limiter.ErrStoreUnavailable`, which deliberately exercises local fallback behavior.

That means:
- config and plugin wiring already support a distributed backend shape
- runtime behavior currently degrades to local limiting unless a real counter-service client is implemented

### Tests as executable spec

The repository uses package-level Go tests as the main specification for behavior:

- [internal/config/config_test.go](internal/config/config_test.go): YAML schema, defaults, and validation rules
- [internal/plugin/root_test.go](internal/plugin/root_test.go): plugin wiring, request handling, local rejection, and distributed fallback behavior at the Proxy-WASM host-emulator level
- [internal/plugin/reject_test.go](internal/plugin/reject_test.go): rejection-path edge cases
- [internal/limiter/distributed_test.go](internal/limiter/distributed_test.go): distributed/local fallback state transitions
- [internal/store/client_test.go](internal/store/client_test.go): counter-service client construction contract

For behavior changes, update tests first and use the narrowest package/test command possible before running the full suite.

## Deployment artifacts

Repository deployment examples live under [deploy/istio/](deploy/istio/):
- [deploy/istio/rate-limiter-envoyfilter.yaml](deploy/istio/rate-limiter-envoyfilter.yaml)
- [deploy/istio/rate-limiter-plugin-config.yaml](deploy/istio/rate-limiter-plugin-config.yaml)

These are examples for Istio `EnvoyFilter`-based deployment and inline plugin configuration. Keep them aligned with the schema in `internal/config/config.go` and with the build artifact path from `build.sh`.
