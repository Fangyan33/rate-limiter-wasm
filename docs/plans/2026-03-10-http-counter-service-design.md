# HTTP Counter Service for Distributed Concurrency Limiting Design

> **Context:** This design preserves the existing Proxy-WASM SDK and distributed limiter abstraction while replacing the placeholder `counter_service` implementation with a real HTTP-based counter service backed by Redis.

**Goal:** Add a deployable distributed concurrency limiter that works in Envoy/Istio using standard Proxy-WASM HTTP call capabilities.

**Architecture:** The WASM plugin continues to handle request interception, domain matching, API key extraction, and request lifecycle management. A new `httpCounterService` implementation behind `internal/store` calls an external HTTP service for acquire/release operations. That service performs atomic Redis operations using a counter plus lease-TTL model, while the plugin preserves the existing fallback-to-local behavior when the distributed store is unavailable.

**Tech Stack:** Go 1.22, `github.com/tetratelabs/proxy-wasm-go-sdk`, Redis, external HTTP counter service, existing limiter/plugin/config test suites.

---

## 1. Overall architecture and request flow

Keep the current Proxy-WASM SDK and limiter structure unchanged. The only runtime behavior change inside the plugin is replacing the no-op implementation in `internal/store/client.go` with a real counter-service client.

Request flow:

1. The plugin receives an HTTP request and runs normal host matching and bearer-token parsing.
2. `DistributedLimiter.Acquire(apiKey)` calls the `DistributedStore` implementation.
3. The store implementation sends a standard Proxy-WASM HTTP call to a configured cluster, for example `POST /acquire`.
4. The external counter service executes an atomic Redis acquire operation.
5. If acquire succeeds, the plugin receives a `lease_id` and returns a release closure.
6. When the stream ends, the existing `OnHttpStreamDone` path invokes that release closure, which sends `POST /release`.
7. If the counter service is unavailable, the store returns `limiter.ErrStoreUnavailable`, and the existing distributed limiter falls back to local mode.

This keeps the plugin portable across Envoy/Istio deployments because the WASM side uses standard HTTP call capabilities rather than direct Redis access or vendor-specific host extensions.

---

## 2. Redis model and acquire/release semantics

Use a counter plus lease key design.

Per API key:

- `rl:concurrent:<api_key>:count`
- `rl:concurrent:<api_key>:lease:<lease_id>`

The service generates `lease_id` values; the WASM plugin only stores and returns them.

### Acquire

`POST /acquire`

Request body:

```json
{
  "api_key": "abc",
  "limit": 10,
  "ttl_ms": 30000
}
```

The service performs an atomic operation:

1. Read current count.
2. If `count >= limit`, return `allowed=false`.
3. Otherwise increment count, create the lease key with TTL, and return `allowed=true` plus `lease_id`.

### Release

`POST /release`

Request body:

```json
{
  "api_key": "abc",
  "lease_id": "lease-123"
}
```

The service atomically:

1. Checks whether the lease key exists.
2. If it exists, deletes the lease key and decrements the counter.
3. If it does not exist, returns success with `released=false`.

This preserves idempotent release behavior and prevents the counter from being decremented twice.

### Why lease TTL is required even without renewal

Even in the first version with no lease renewal:

- orphaned requests are eventually recovered if the plugin never sends release
- duplicate release attempts do not corrupt the counter

Trade-off: a very long-running request may outlive its TTL and free the slot early. This is acceptable for the first implementation because the design intentionally favors a simple, deployable version.

---

## 3. WASM-side client integration

The existing interface is already sufficient:

```go
Acquire(apiKey string, limit int) (func(), bool, error)
Name() string
```

So the plugin-side implementation only needs to translate HTTP responses into limiter semantics.

Recommended result mapping:

- HTTP 200 with `allowed=true` -> return `release func(), true, nil`
- HTTP 200 with `allowed=false` -> return `nil, false, nil`
- timeout, network error, malformed response, or 5xx -> return `nil, false, limiter.ErrStoreUnavailable`

Only a clear distributed rejection should be treated as a normal limit denial. Any transport or service failure should be treated as store unavailability so the existing fallback path stays intact.

### Release behavior

The release closure should issue a best-effort `POST /release` request.

Important constraint: in Proxy-WASM, end-of-stream cleanup is not a good place to depend on a guaranteed, synchronous remote confirmation. Therefore release failures should not affect the request outcome. The authoritative safety net is the lease TTL in Redis.

This aligns with the current project design, where availability is prioritized and the limiter already supports fallback to local mode.

---

## 4. Configuration and HTTP API contract

Keep the existing distributed config shape and only extend `counter_service` with optional fields:

```yaml
distributed_limit:
  enabled: true
  backend: counter_service
  counter_service:
    cluster: ratelimit-service
    timeout_ms: 100
    acquire_path: /acquire
    release_path: /release
    lease_ttl_ms: 30000
```

Suggested defaults:

- `acquire_path`: `/acquire`
- `release_path`: `/release`
- `lease_ttl_ms`: `30000`

### API contract

#### `POST /acquire`

Request:

```json
{
  "api_key": "abc",
  "limit": 10,
  "ttl_ms": 30000
}
```

Response when accepted:

```json
{
  "allowed": true,
  "lease_id": "lease-123"
}
```

Response when denied:

```json
{
  "allowed": false
}
```

#### `POST /release`

Request:

```json
{
  "api_key": "abc",
  "lease_id": "lease-123"
}
```

Response examples:

```json
{
  "released": true
}
```

or

```json
{
  "released": false
}
```

The WASM side should not depend on any richer protocol than this.

---

## 5. Testing strategy

### Store tests

Add tests for `internal/store` covering:

- successful acquire returns a release closure
- explicit distributed rejection returns `ok=false` without fallback error
- timeout, bad body, 5xx, or transport failure return `limiter.ErrStoreUnavailable`
- repeated release calls remain safe from the plugin’s perspective

### Limiter tests

Preserve current `DistributedLimiter` semantics:

- store failure triggers local fallback
- fallback-era in-flight requests block recovery to distributed mode
- recovery is only allowed after fallback in-flight requests drain

### Plugin tests

Extend plugin tests to verify:

- distributed rejection produces the configured local rejection response
- distributed store failure falls back to local limiter behavior
- stream completion invokes release behavior

---

## Recommended implementation direction

For this repository, the most practical path is:

1. keep `tetratelabs/proxy-wasm-go-sdk`
2. implement a real `httpCounterService` behind `internal/store`
3. call a configured Envoy cluster via standard HTTP call APIs
4. use Redis count + lease-TTL semantics in the external service
5. keep release best-effort and rely on TTL for recovery

This gives the project a deployable distributed concurrency limit mechanism without coupling the WASM plugin to Redis protocol details or Higress-specific host behavior.
