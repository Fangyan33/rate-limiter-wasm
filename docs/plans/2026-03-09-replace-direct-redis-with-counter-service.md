# Replace direct Redis in WASM with counter service Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace the current in-plugin `go-redis` distributed limiter path with a deployable external counter-service client, while closing the fallback-to-recovery consistency gap with test-first changes.

**Architecture:** Keep the plugin responsible for request interception, API key parsing, and acquire/release lifecycle management. Replace the current `RedisStore` implementation with a transport-agnostic distributed store client that talks to an external counter service over Envoy-supported call patterns, and introduce an explicit recovery state so the plugin does not switch from local fallback back to distributed mode while fallback-era in-flight requests are still outstanding.

**Tech Stack:** Go 1.22, Proxy-WASM Go SDK, existing limiter/plugin packages, Go `testing`, fake distributed store test doubles.

---

### Task 1: Lock the recovery consistency bug with failing limiter tests

**Files:**
- Modify: `.claude/worktrees/agent-redis-fallback/internal/limiter/distributed_test.go`
- Modify: `.claude/worktrees/agent-redis-fallback/internal/limiter/distributed.go`

**Step 1: Write the failing test**

Add a focused test to `.claude/worktrees/agent-redis-fallback/internal/limiter/distributed_test.go` that proves the limiter must not return to `ModeDistributed` while a fallback-acquired request is still in flight.

Use this shape:

```go
func TestDistributedLimiterDoesNotRecoverWhileFallbackRequestIsStillInFlight(t *testing.T) {
	store := &fakeDistributedStore{
		acquireResults: []acquireResult{
			{err: ErrStoreUnavailable},
			{ok: true},
		},
	}
	l := NewDistributedLimiter(map[string]int{"key_basic_001": 1}, store)

	fallbackRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected fallback acquire to succeed")
	}
	if l.Mode() != ModeLocalFallback {
		t.Fatalf("expected mode %q after fallback acquire, got %q", ModeLocalFallback, l.Mode())
	}

	if _, ok := l.Acquire("key_basic_001"); ok {
		t.Fatal("expected limiter to reject recovery while fallback request remains in flight")
	}
	if l.Mode() != ModeLocalFallback {
		t.Fatalf("expected limiter to remain in fallback mode while fallback request remains in flight, got %q", l.Mode())
	}
	if store.releaseCalls != 0 {
		t.Fatalf("expected no distributed release calls before fallback request finishes, got %d", store.releaseCalls)
	}

	fallbackRelease()
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/limiter -run TestDistributedLimiterDoesNotRecoverWhileFallbackRequestIsStillInFlight -count=1`

Expected: FAIL because the current implementation switches back to `ModeDistributed` as soon as `store.Acquire` succeeds.

**Step 3: Write minimal implementation**

In `.claude/worktrees/agent-redis-fallback/internal/limiter/distributed.go`:
- Add an explicit recovery state, e.g. `ModeRecovering`.
- Track how many fallback-acquired requests are still in flight.
- When the store becomes reachable again, do **not** return to distributed mode until the fallback in-flight count reaches zero.
- Ensure fallback-acquired release functions decrement that counter exactly once.

Minimal implementation constraints:
- Do not redesign the whole store abstraction yet.
- Only add the state and bookkeeping required to make the test pass.
- Keep release functions idempotent.

**Step 4: Run test to verify it passes**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/limiter -run TestDistributedLimiterDoesNotRecoverWhileFallbackRequestIsStillInFlight -count=1`

Expected: PASS

**Step 5: Add one more failing test for post-drain recovery**

Add another test proving recovery is allowed after the fallback in-flight request finishes:

```go
func TestDistributedLimiterRecoversAfterFallbackInflightDrains(t *testing.T) {
	store := &fakeDistributedStore{
		acquireResults: []acquireResult{
			{err: ErrStoreUnavailable},
			{ok: true},
		},
	}
	l := NewDistributedLimiter(map[string]int{"key_basic_001": 1}, store)

	fallbackRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected fallback acquire to succeed")
	}

	fallbackRelease()

	distributedRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected limiter to recover after fallback inflight drains")
	}
	if l.Mode() != ModeDistributed {
		t.Fatalf("expected mode %q after drain, got %q", ModeDistributed, l.Mode())
	}
	distributedRelease()
}
```

**Step 6: Run both tests to verify red/green cycle**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/limiter -run 'TestDistributedLimiter(DoesNotRecoverWhileFallbackRequestIsStillInFlight|RecoversAfterFallbackInflightDrains)' -count=1`

Expected: PASS

**Step 7: Run the package test suite**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/limiter -count=1`

Expected: PASS

**Step 8: Commit**

```bash
git -C /root/src/rate-limiter-wasm/.claude/worktrees/agent-redis-fallback add internal/limiter/distributed.go internal/limiter/distributed_test.go
git -C /root/src/rate-limiter-wasm/.claude/worktrees/agent-redis-fallback commit -m "fix: block recovery until fallback drains"
```

### Task 2: Remove direct Redis dependency from the limiter package

**Files:**
- Modify: `.claude/worktrees/agent-redis-fallback/internal/limiter/distributed.go`
- Modify: `.claude/worktrees/agent-redis-fallback/go.mod`
- Modify: `.claude/worktrees/agent-redis-fallback/go.sum`
- Modify: `.claude/worktrees/agent-redis-fallback/internal/limiter/distributed_test.go`

**Step 1: Write the failing test**

Add a transport-agnostic constructor test in `.claude/worktrees/agent-redis-fallback/internal/limiter/distributed_test.go` that exercises the limiter only through the `DistributedStore` interface and does not require a real Redis server.

Use this shape:

```go
func TestDistributedLimiterUsesGenericDistributedStoreContract(t *testing.T) {
	store := &fakeDistributedStore{
		acquireResults: []acquireResult{{ok: true}},
	}
	l := NewDistributedLimiter(map[string]int{"key_basic_001": 2}, store)

	release, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected generic distributed store acquire to succeed")
	}
	if store.acquireCalls != 1 {
		t.Fatalf("expected one acquire call, got %d", store.acquireCalls)
	}
	release()
}
```

This test may already pass; if so, keep it and move the red step to deleting the Redis-specific tests that assume `NewRedisStore` exists. The red condition for this task is: package tests fail once Redis-specific production code is removed.

**Step 2: Run package tests to capture current dependency on Redis-specific code**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/limiter -count=1`

Expected: PASS before refactor; this establishes the baseline.

**Step 3: Write minimal implementation**

Refactor `.claude/worktrees/agent-redis-fallback/internal/limiter/distributed.go` so that the limiter package contains only:
- `DistributedStore`
- `DistributedLimiter`
- fallback/recovery state tracking
- helper wrappers such as `once`

Delete the in-package `RedisStore` implementation entirely.

Also:
- Remove `github.com/redis/go-redis/v9` from `.claude/worktrees/agent-redis-fallback/go.mod`
- Remove `github.com/alicebob/miniredis/v2` from `.claude/worktrees/agent-redis-fallback/go.mod`
- Remove Redis/miniredis-specific tests from `.claude/worktrees/agent-redis-fallback/internal/limiter/distributed_test.go`

Do **not** add the new counter-service client in this task. This task only isolates the limiter core from Redis.

**Step 4: Run test to verify the refactor passes**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/limiter -count=1`

Expected: PASS with only interface-driven limiter tests remaining.

**Step 5: Run module tidy**

Run: `cd /root/src/rate-limiter-wasm/.claude/worktrees/agent-redis-fallback && go mod tidy`

Expected: `go.mod` / `go.sum` no longer contain Redis dependencies.

**Step 6: Re-run limiter package tests**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/limiter -count=1`

Expected: PASS

**Step 7: Commit**

```bash
git -C /root/src/rate-limiter-wasm/.claude/worktrees/agent-redis-fallback add go.mod go.sum internal/limiter/distributed.go internal/limiter/distributed_test.go
git -C /root/src/rate-limiter-wasm/.claude/worktrees/agent-redis-fallback commit -m "refactor: remove direct redis limiter implementation"
```

### Task 3: Introduce a deployable counter-service client abstraction in the plugin wiring

**Files:**
- Modify: `.claude/worktrees/agent-redis-fallback/internal/plugin/root.go`
- Create: `.claude/worktrees/agent-redis-fallback/internal/store/client.go`
- Create: `.claude/worktrees/agent-redis-fallback/internal/store/client_test.go`
- Modify: `.claude/worktrees/agent-redis-fallback/internal/config/config.go`
- Modify: `.claude/worktrees/agent-redis-fallback/internal/config/config_test.go`

**Step 1: Write the failing config test**

Add a config test proving distributed mode now requires counter-service client settings instead of direct Redis dial settings.

Example shape:

```go
func TestParseDistributedStoreCounterServiceConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
rate_limits:
  - api_key: "key_basic_001"
    max_concurrent: 1
domains:
  - "api.example.com"
distributed_limit:
  enabled: true
  backend: "counter_service"
  counter_service:
    cluster: "ratelimit-service"
    timeout_ms: 100
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.DistributedStore.Backend != "counter_service" {
		t.Fatalf("expected backend counter_service, got %q", cfg.DistributedStore.Backend)
	}
}
```

**Step 2: Run the targeted config test to verify it fails**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/config -run TestParseDistributedStoreCounterServiceConfig -count=1`

Expected: FAIL because the config model does not yet support the new backend.

**Step 3: Write the failing store client test**

Add `.claude/worktrees/agent-redis-fallback/internal/store/client_test.go` with a small constructor/contract test for a counter-service backed client. Do **not** require real network I/O. Test only constructor validation and interface behavior.

Example shape:

```go
func TestNewCounterServiceStoreRejectsEmptyCluster(t *testing.T) {
	_, err := NewCounterServiceStore(Config{Cluster: ""})
	if err == nil {
		t.Fatal("expected error for empty cluster")
	}
}
```

**Step 4: Run the targeted store test to verify it fails**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/store -run TestNewCounterServiceStoreRejectsEmptyCluster -count=1`

Expected: FAIL because the package/file does not exist yet.

**Step 5: Write minimal implementation**

Implement:
- Config support for `backend: "counter_service"`
- A new `internal/store/client.go` package that exposes a constructor returning a `limiter.DistributedStore`
- For now, make the client a minimal skeleton suitable for plugin wiring and unit tests; keep all transport details behind an interface

Constraints:
- Do not add direct TCP or Redis code here.
- Keep the transport injectable for tests.
- The constructor must validate required config.

**Step 6: Update plugin wiring**

In `.claude/worktrees/agent-redis-fallback/internal/plugin/root.go`:
- Stop calling `limiter.NewRedisStore(...)`
- Instead, build the external store client when distributed backend is enabled
- If store construction fails, return an error from `LoadConfiguration`

**Step 7: Run targeted tests to verify they pass**

Run:
- `go test ./.claude/worktrees/agent-redis-fallback/internal/config -run TestParseDistributedStoreCounterServiceConfig -count=1`
- `go test ./.claude/worktrees/agent-redis-fallback/internal/store -count=1`

Expected: PASS

**Step 8: Run plugin package tests**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/plugin -count=1`

Expected: PASS

**Step 9: Commit**

```bash
git -C /root/src/rate-limiter-wasm/.claude/worktrees/agent-redis-fallback add internal/config/config.go internal/config/config_test.go internal/plugin/root.go internal/store/client.go internal/store/client_test.go
git -C /root/src/rate-limiter-wasm/.claude/worktrees/agent-redis-fallback commit -m "feat: wire distributed limiter through counter service client"
```

### Task 4: Add plugin-level regression tests for distributed wiring and fallback behavior

**Files:**
- Modify: `.claude/worktrees/agent-redis-fallback/internal/plugin/root_test.go`
- Modify: `.claude/worktrees/agent-redis-fallback/internal/plugin/root.go`

**Step 1: Write the failing plugin test**

Add a plugin-level test proving distributed backend wiring does not bypass the limiter lifecycle.

Suggested shape:

```go
func TestLoadConfigurationBuildsDistributedLimiterForCounterServiceBackend(t *testing.T) {
	r := NewRootContext()
	err := r.LoadConfiguration([]byte(`
domains:
  - "api.example.com"
rate_limits:
  - api_key: "key_basic_001"
    max_concurrent: 1
distributed_limit:
  enabled: true
  backend: "counter_service"
  counter_service:
    cluster: "ratelimit-service"
`))
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}
	if r.limiter == nil {
		t.Fatal("expected limiter to be configured")
	}
}
```

Add a second plugin test proving invalid distributed config is rejected.

**Step 2: Run targeted plugin tests to verify they fail**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/plugin -run 'TestLoadConfiguration(BuildsDistributedLimiterForCounterServiceBackend|RejectsInvalidCounterServiceConfig)' -count=1`

Expected: FAIL before wiring is complete.

**Step 3: Write minimal implementation**

Make only the minimal plugin changes needed for these tests to pass.

**Step 4: Run targeted plugin tests to verify they pass**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/plugin -run 'TestLoadConfiguration(BuildsDistributedLimiterForCounterServiceBackend|RejectsInvalidCounterServiceConfig)' -count=1`

Expected: PASS

**Step 5: Run full plugin package tests**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/plugin -count=1`

Expected: PASS

**Step 6: Commit**

```bash
git -C /root/src/rate-limiter-wasm/.claude/worktrees/agent-redis-fallback add internal/plugin/root.go internal/plugin/root_test.go
git -C /root/src/rate-limiter-wasm/.claude/worktrees/agent-redis-fallback commit -m "test: cover counter service plugin wiring"
```

### Task 5: Run full verification for the worktree

**Files:**
- Verify only: `.claude/worktrees/agent-redis-fallback/...`

**Step 1: Run limiter tests**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/limiter -count=1`

Expected: PASS

**Step 2: Run config tests**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/config -count=1`

Expected: PASS

**Step 3: Run store tests**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/store -count=1`

Expected: PASS

**Step 4: Run plugin tests**

Run: `go test ./.claude/worktrees/agent-redis-fallback/internal/plugin -count=1`

Expected: PASS

**Step 5: Run full worktree test suite**

Run: `cd /root/src/rate-limiter-wasm/.claude/worktrees/agent-redis-fallback && go test ./... -count=1`

Expected: PASS

**Step 6: Review against requirements**

Check manually that:
- No direct `go-redis` usage remains in the worktree
- Recovery no longer switches to distributed while fallback-era requests are still in flight
- Plugin wiring uses an external counter-service abstraction rather than direct Redis access
- Existing local limiter and auth/domain behavior still pass tests

**Step 7: Commit final verification-only changes if needed**

If verification required any small test-only adjustments, create a final commit. Otherwise skip commit.

---

Plan complete and saved to `docs/plans/2026-03-09-replace-direct-redis-with-counter-service.md`. Two execution options:

**1. Subagent-Driven (this session)** - I dispatch fresh subagent per task, review between tasks, fast iteration

**2. Parallel Session (separate)** - Open new session with executing-plans, batch execution with checkpoints

Which approach?
