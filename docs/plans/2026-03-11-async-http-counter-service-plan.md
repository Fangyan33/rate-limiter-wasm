# Async HTTP Counter Service Integration Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace the current synchronous placeholder distributed store path with a real Proxy-WASM-compatible asynchronous HTTP counter-service flow.

**Architecture:** Keep configuration parsing and local limiter behavior intact, but move distributed acquire/release decisions out of the synchronous `DistributedStore` interface and into the HTTP context lifecycle. The plugin will dispatch an HTTP call to the configured counter service, pause the request, and resume or reject it from the callback. Local fallback semantics remain available when the counter service is unavailable.

**Tech Stack:** Go 1.22, Proxy-WASM Go SDK, proxytest host emulator, existing config/plugin/limiter/store packages.

---

### Task 1: Lock in the async request-flow shape with a failing plugin test

**Files:**
- Modify: `internal/plugin/root_test.go`
- Modify: `internal/plugin/root.go`

**Step 1: Write the failing test**

Add a plugin test proving that when distributed limiting is enabled, the request pauses while the counter-service acquire decision is pending.

Use this shape in `internal/plugin/root_test.go`:

```go
func TestPluginPausesRequestWhileCounterServiceAcquireIsPending(t *testing.T) {
	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 1
distributed_limit:
  enabled: true
  backend: counter_service
  counter_service:
    cluster: ratelimit-service
    acquire_path: /acquire
    release_path: /release
    lease_ttl_ms: 30000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer key_basic_001"},
	}, false)

	if action != types.ActionPause {
		t.Fatalf("expected distributed acquire to pause request, got %v", action)
	}
	if resp := host.GetSentLocalResponse(contextID); resp != nil {
		t.Fatalf("expected no local response while acquire decision is pending, got %#v", resp)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/plugin -run TestPluginPausesRequestWhileCounterServiceAcquireIsPending -count=1`

Expected: FAIL because the current implementation still routes through the synchronous limiter path and does not intentionally pause for asynchronous distributed acquire.

**Step 3: Write minimal implementation**

In `internal/plugin/root.go`:
- Introduce a dedicated distributed HTTP path in `OnHttpRequestHeaders` before calling the existing local limiter path.
- Add minimal request-state tracking to `httpContext` so distributed-enabled requests can pause before a result exists.
- Do not implement the full callback yet; only add the smallest code needed for this test.

**Step 4: Run test to verify it passes**

Run: `go test ./internal/plugin -run TestPluginPausesRequestWhileCounterServiceAcquireIsPending -count=1`

Expected: PASS

**Step 5: Commit**

```bash
git add internal/plugin/root.go internal/plugin/root_test.go
git commit -m "test: pause distributed requests for async acquire"
```

### Task 2: Introduce a dedicated async counter-service client contract

**Files:**
- Modify: `internal/store/client.go`
- Create: `internal/store/types.go`
- Create: `internal/store/http_client.go`
- Modify: `internal/store/client_test.go`

**Step 1: Write the failing test**

Add a store test that asserts the new client exposes the configured HTTP request details needed for async acquire.

Use this shape in `internal/store/client_test.go`:

```go
func TestNewClientBuildsAsyncCounterServiceClient(t *testing.T) {
	store, err := NewClient(config.CounterServiceConfig{
		Cluster:     "ratelimit-service",
		TimeoutMS:   100,
		AcquirePath: "/acquire",
		ReleasePath: "/release",
		LeaseTTLMS:  30000,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	client, ok := store.(*client)
	if !ok {
		t.Fatalf("expected *client, got %T", store)
	}
	if client.http.cluster != "ratelimit-service" {
		t.Fatalf("unexpected cluster: %q", client.http.cluster)
	}
	if client.http.acquirePath != "/acquire" {
		t.Fatalf("unexpected acquire path: %q", client.http.acquirePath)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/store -run TestNewClientBuildsAsyncCounterServiceClient -count=1`

Expected: FAIL because the current client still wraps only the no-op synchronous service abstraction.

**Step 3: Write minimal implementation**

Create:
- `internal/store/types.go` with `acquireRequest`, `acquireResponse`, `releaseRequest`, `releaseResponse`
- `internal/store/http_client.go` with a minimal `httpCounterServiceClient` struct holding cluster, timeout, paths, and lease TTL

Refactor `internal/store/client.go` so:
- `client` holds `http *httpCounterServiceClient`
- existing constructor validation remains
- synchronous `Acquire` can stay as fallback/unavailable for now, but the async-capable HTTP client config must exist

**Step 4: Run test to verify it passes**

Run: `go test ./internal/store -run TestNewClientBuildsAsyncCounterServiceClient -count=1`

Expected: PASS

**Step 5: Commit**

```bash
git add internal/store/client.go internal/store/client_test.go internal/store/http_client.go internal/store/types.go
git commit -m "refactor: add async counter service client config"
```

### Task 3: Add an acquire callback path with explicit success and deny behavior

**Files:**
- Modify: `internal/plugin/root.go`
- Modify: `internal/plugin/root_test.go`
- Modify: `internal/store/http_client.go`

**Step 1: Write the failing success test**

Add a test proving that a successful distributed acquire resumes the paused request and stores release state.

Use this shape:

```go
func TestPluginResumesRequestWhenCounterServiceAcquireSucceeds(t *testing.T) {
	// Arrange host and fake counter-service callback success.
	// Assert initial OnHttpRequestHeaders returns ActionPause.
	// Trigger the acquire callback with allowed=true and lease_id set.
	// Assert the request resumes and no local response is sent.
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/plugin -run TestPluginResumesRequestWhenCounterServiceAcquireSucceeds -count=1`

Expected: FAIL because no acquire callback handling exists yet.

**Step 3: Write the failing deny test**

Add a second test proving that `allowed=false` results in the configured local reject response.

Use this shape:

```go
func TestPluginRejectsRequestWhenCounterServiceAcquireDenies(t *testing.T) {
	// Arrange paused request.
	// Trigger callback with allowed=false.
	// Assert ActionPause remains associated with a sent local response.
}
```

**Step 4: Run both tests to verify they fail for the right reason**

Run: `go test ./internal/plugin -run 'TestPlugin(ResumesRequestWhenCounterServiceAcquireSucceeds|RejectsRequestWhenCounterServiceAcquireDenies)' -count=1`

Expected: FAIL because callback handling is missing.

**Step 5: Write minimal implementation**

In `internal/plugin/root.go` and `internal/store/http_client.go`:
- Add an async acquire dispatch helper
- Parse the acquire response body
- On `allowed=true`, store release metadata in `httpContext` and resume the request
- On `allowed=false`, send the configured reject response

Keep the implementation narrow:
- Do not add release dispatch yet
- Do not refactor unrelated local limiter paths

**Step 6: Run tests to verify they pass**

Run: `go test ./internal/plugin -run 'TestPlugin(ResumesRequestWhenCounterServiceAcquireSucceeds|RejectsRequestWhenCounterServiceAcquireDenies)' -count=1`

Expected: PASS

**Step 7: Commit**

```bash
git add internal/plugin/root.go internal/plugin/root_test.go internal/store/http_client.go
git commit -m "feat: handle async counter service acquire responses"
```

### Task 4: Add fallback behavior for acquire transport/service failures

**Files:**
- Modify: `internal/plugin/root.go`
- Modify: `internal/plugin/root_test.go`
- Modify: `internal/store/http_client.go`

**Step 1: Write the failing fallback test**

Add a plugin test proving that when the acquire HTTP call fails, the plugin uses local fallback semantics instead of permanently pausing or failing open.

Use this shape:

```go
func TestPluginFallsBackToLocalLimitWhenAsyncAcquireFails(t *testing.T) {
	// First request pauses then acquires local fallback on callback failure.
	// Second request should be rejected by the local fallback limit.
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/plugin -run TestPluginFallsBackToLocalLimitWhenAsyncAcquireFails -count=1`

Expected: FAIL because async failure fallback has not been implemented.

**Step 3: Write minimal implementation**

Implement callback failure handling so that:
- a transport/parsing/service failure invokes the local limiter path
- success is only treated as distributed success when the response is valid and explicit
- fallback state is stored in the same `httpContext` lifecycle so release stays possible later

**Step 4: Run test to verify it passes**

Run: `go test ./internal/plugin -run TestPluginFallsBackToLocalLimitWhenAsyncAcquireFails -count=1`

Expected: PASS

**Step 5: Commit**

```bash
git add internal/plugin/root.go internal/plugin/root_test.go internal/store/http_client.go
git commit -m "feat: fallback to local limiter on async acquire failure"
```

### Task 5: Implement async release dispatch on stream completion

**Files:**
- Modify: `internal/plugin/root.go`
- Modify: `internal/plugin/root_test.go`
- Modify: `internal/store/http_client.go`

**Step 1: Write the failing release test**

Add a test proving that a distributed acquire success causes `OnHttpStreamDone` to dispatch a best-effort release request.

Use this shape:

```go
func TestPluginDispatchesCounterServiceReleaseOnStreamDone(t *testing.T) {
	// Arrange successful acquire with lease_id.
	// Complete the stream.
	// Assert release dispatch was attempted exactly once.
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/plugin -run TestPluginDispatchesCounterServiceReleaseOnStreamDone -count=1`

Expected: FAIL because release dispatch is not implemented.

**Step 3: Write minimal implementation**

Implement best-effort release dispatch:
- capture `lease_id` on successful acquire
- on `OnHttpStreamDone`, dispatch `POST /release`
- ignore release errors other than optional logging
- ensure the release path is idempotent inside `httpContext`

**Step 4: Run test to verify it passes**

Run: `go test ./internal/plugin -run TestPluginDispatchesCounterServiceReleaseOnStreamDone -count=1`

Expected: PASS

**Step 5: Commit**

```bash
git add internal/plugin/root.go internal/plugin/root_test.go internal/store/http_client.go
git commit -m "feat: dispatch async counter service release"
```

### Task 6: Remove or isolate the obsolete synchronous distributed store path

**Files:**
- Modify: `internal/limiter/distributed.go`
- Modify: `internal/limiter/distributed_test.go`
- Modify: `internal/plugin/root.go`
- Modify: `internal/store/client.go`

**Step 1: Write the failing cleanup test**

Add or update a plugin/limiter test proving that distributed counter-service mode no longer depends on the synchronous `DistributedStore.Acquire` path.

Use this shape:

```go
func TestCounterServiceModeDoesNotUseSynchronousDistributedLimiterPath(t *testing.T) {
	// Assert counter-service-enabled config bypasses synchronous distributed Acquire.
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/plugin -run TestCounterServiceModeDoesNotUseSynchronousDistributedLimiterPath -count=1`

Expected: FAIL because the old coupling still exists.

**Step 3: Write minimal implementation**

Refactor so:
- `counter_service` mode uses the new async plugin path
- the synchronous distributed limiter remains only for any retained generic store use case, or is removed if now unused
- no dead code remains in the request path

Keep changes DRY and YAGNI.

**Step 4: Run focused package tests**

Run:
- `go test ./internal/plugin -count=1`
- `go test ./internal/store -count=1`
- `go test ./internal/config -count=1`
- `go test ./internal/limiter -count=1`

Expected: PASS

**Step 5: Commit**

```bash
git add internal/plugin/root.go internal/plugin/root_test.go internal/store/client.go internal/limiter/distributed.go internal/limiter/distributed_test.go
git commit -m "refactor: route counter service mode through async plugin flow"
```

### Task 7: Run full verification and update the design doc if needed

**Files:**
- Modify: `docs/plans/2026-03-10-http-counter-service-design.md`

**Step 1: Run full test suite**

Run: `go test ./... -count=1`

Expected: PASS

**Step 2: Update the design doc**

Amend `docs/plans/2026-03-10-http-counter-service-design.md` so it explicitly states that Proxy-WASM counter-service acquire is asynchronous and request pausing/resume is handled in the plugin HTTP context rather than via a synchronous `DistributedStore.Acquire` API.

**Step 3: Re-run any affected tests**

Run: `go test ./internal/plugin ./internal/store ./internal/config -count=1`

Expected: PASS

**Step 4: Commit**

```bash
git add docs/plans/2026-03-10-http-counter-service-design.md
git commit -m "docs: clarify async counter service request flow"
```
