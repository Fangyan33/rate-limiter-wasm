# HTTP Counter Service for Distributed Concurrency Limiting Design

> **Context:** This design preserves the existing Proxy-WASM SDK and distributed limiter abstraction while replacing the placeholder `counter_service` implementation with a real HTTP-based counter service backed by Redis.
>
> **Implementation Note:** The final implementation uses **asynchronous HTTP callouts** in the plugin HTTP context lifecycle rather than synchronous `DistributedStore.Acquire`. Requests pause while the counter service acquire decision is pending, then resume or reject based on the callback response. This approach is required by Proxy-WASM's async-only HTTP call model.

**Goal:** Add a deployable distributed concurrency limiter that works in Envoy/Istio using standard Proxy-WASM HTTP call capabilities.

**Architecture:** The WASM plugin handles request interception, domain matching, API key extraction, and request lifecycle management. When `counter_service` mode is enabled, the plugin dispatches asynchronous HTTP callouts to an external counter service for acquire/release operations. That service performs atomic Redis operations using a counter plus lease-TTL model. The plugin preserves local fallback behavior when the distributed store is unavailable.

**Tech Stack:** Go 1.22, `github.com/tetratelabs/proxy-wasm-go-sdk`, Redis, external HTTP counter service, existing limiter/plugin/config test suites.

---

## 1. Overall architecture and request flow

The plugin uses Proxy-WASM's asynchronous HTTP callout model for distributed acquire/release operations.

Request flow:

1. The plugin receives an HTTP request and runs normal host matching and bearer-token parsing.
2. In `counter_service` mode, `OnHttpRequestHeaders` dispatches an async HTTP callout to `POST /acquire` and returns `ActionPause`.
3. The external counter service executes an atomic Redis acquire operation.
4. When the HTTP callback arrives, the plugin parses the response:
   - If `allowed=true`, the plugin stores the `lease_id` and calls `ResumeHttpRequest()`.
   - If `allowed=false`, the plugin sends a local rejection response.
   - If the HTTP call fails (non-200 status, parse error, timeout), the plugin falls back to the local limiter and resumes if a slot is available.
5. When the stream ends, `OnHttpStreamDone` dispatches a best-effort `POST /release` callout with the stored `lease_id`.
6. The plugin does not wait for the release response; it is fire-and-forget.

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

The plugin uses **asynchronous HTTP callouts** in the HTTP context lifecycle rather than a synchronous `DistributedStore.Acquire` interface.

### Acquire flow

In `OnHttpRequestHeaders`, when `counter_service` mode is enabled:

1. The plugin marshals an acquire request body with `api_key`, `limit`, and `ttl_ms`.
2. It dispatches `proxywasm.DispatchHttpCall` to the configured cluster and path.
3. It returns `types.ActionPause` to pause the request.
4. When the HTTP callback arrives, the plugin parses the response:
   - HTTP 200 with `allowed=true` → store `lease_id`, call `proxywasm.ResumeHttpRequest()`
   - HTTP 200 with `allowed=false` → send local rejection response
   - Non-200 status, timeout, parse error → fall back to local limiter, resume if slot available

### Release behavior

In `OnHttpStreamDone`, if a `lease_id` was stored from a successful acquire:

1. The plugin marshals a release request body with `api_key` and `lease_id`.
2. It dispatches a best-effort `POST /release` callout.
3. The plugin does not wait for or check the release response.
4. The authoritative safety net is the lease TTL in Redis.

This aligns with the current project design, where availability is prioritized and the limiter supports fallback to local mode.

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
