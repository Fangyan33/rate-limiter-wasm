package redis

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func setupTestRedis(t *testing.T) (*Client, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	client, err := NewClient(mr.Addr(), "", 0, 10, 3)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	return client, mr
}

func TestAcquire_Success(t *testing.T) {
	client, _ := setupTestRedis(t)
	defer client.Close()

	ctx := context.Background()
	req := AcquireRequest{
		APIKey: "test-key",
		Limit:  5,
		TTLMS:  30000,
	}

	result, err := client.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	if !result.Allowed {
		t.Error("expected allowed=true")
	}

	if result.LeaseID == "" {
		t.Error("expected non-empty lease_id")
	}
}

func TestAcquire_ReachLimit(t *testing.T) {
	client, _ := setupTestRedis(t)
	defer client.Close()

	ctx := context.Background()
	req := AcquireRequest{
		APIKey: "test-key",
		Limit:  3,
		TTLMS:  30000,
	}

	// Acquire 3 times (reach limit)
	for i := 0; i < 3; i++ {
		result, err := client.Acquire(ctx, req)
		if err != nil {
			t.Fatalf("Acquire %d failed: %v", i+1, err)
		}
		if !result.Allowed {
			t.Errorf("Acquire %d: expected allowed=true", i+1)
		}
	}

	// 4th acquire should be denied
	result, err := client.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("Acquire 4 failed: %v", err)
	}
	if result.Allowed {
		t.Error("expected allowed=false when limit reached")
	}
	if result.LeaseID != "" {
		t.Error("expected empty lease_id when denied")
	}
}

func TestRelease_Success(t *testing.T) {
	client, _ := setupTestRedis(t)
	defer client.Close()

	ctx := context.Background()

	// First acquire
	acqReq := AcquireRequest{
		APIKey: "test-key",
		Limit:  5,
		TTLMS:  30000,
	}
	acqResult, err := client.Acquire(ctx, acqReq)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	// Then release
	relReq := ReleaseRequest{
		APIKey:  "test-key",
		LeaseID: acqResult.LeaseID,
	}
	relResult, err := client.Release(ctx, relReq)
	if err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	if !relResult.Released {
		t.Error("expected released=true")
	}
}

func TestRelease_LeaseNotFound(t *testing.T) {
	client, _ := setupTestRedis(t)
	defer client.Close()

	ctx := context.Background()

	// Release non-existent lease
	relReq := ReleaseRequest{
		APIKey:  "test-key",
		LeaseID: "non-existent-lease",
	}
	relResult, err := client.Release(ctx, relReq)
	if err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	if relResult.Released {
		t.Error("expected released=false for non-existent lease")
	}
}

func TestRelease_DuplicateRelease(t *testing.T) {
	client, _ := setupTestRedis(t)
	defer client.Close()

	ctx := context.Background()

	// Acquire
	acqReq := AcquireRequest{
		APIKey: "test-key",
		Limit:  5,
		TTLMS:  30000,
	}
	acqResult, err := client.Acquire(ctx, acqReq)
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	// First release
	relReq := ReleaseRequest{
		APIKey:  "test-key",
		LeaseID: acqResult.LeaseID,
	}
	relResult1, err := client.Release(ctx, relReq)
	if err != nil {
		t.Fatalf("First release failed: %v", err)
	}
	if !relResult1.Released {
		t.Error("expected first release to succeed")
	}

	// Second release (duplicate)
	relResult2, err := client.Release(ctx, relReq)
	if err != nil {
		t.Fatalf("Second release failed: %v", err)
	}
	if relResult2.Released {
		t.Error("expected second release to return released=false")
	}
}

func TestAcquireRelease_CounterAccuracy(t *testing.T) {
	client, _ := setupTestRedis(t)
	defer client.Close()

	ctx := context.Background()
	apiKey := "test-key"

	// Acquire 3 slots
	var leases []string
	for i := 0; i < 3; i++ {
		result, err := client.Acquire(ctx, AcquireRequest{
			APIKey: apiKey,
			Limit:  5,
			TTLMS:  30000,
		})
		if err != nil {
			t.Fatalf("Acquire %d failed: %v", i+1, err)
		}
		leases = append(leases, result.LeaseID)
	}

	// Check ZSET size
	leasesKey := buildLeasesKey(apiKey)
	size, err := client.rdb.ZCard(ctx, leasesKey).Result()
	if err != nil {
		t.Fatalf("ZCard failed: %v", err)
	}
	if size != 3 {
		t.Errorf("expected ZSET size=3, got %d", size)
	}

	// Release 2 slots
	for i := 0; i < 2; i++ {
		_, err := client.Release(ctx, ReleaseRequest{
			APIKey:  apiKey,
			LeaseID: leases[i],
		})
		if err != nil {
			t.Fatalf("Release %d failed: %v", i+1, err)
		}
	}

	// Check ZSET size after release
	size, err = client.rdb.ZCard(ctx, leasesKey).Result()
	if err != nil {
		t.Fatalf("ZCard failed: %v", err)
	}
	if size != 1 {
		t.Errorf("expected ZSET size=1 after releases, got %d", size)
	}
}

func TestAcquire_LeaseTTLExpiry(t *testing.T) {
	client, mr := setupTestRedis(t)
	defer client.Close()

	ctx := context.Background()
	apiKey := "test-key"

	// Acquire with short TTL
	result, err := client.Acquire(ctx, AcquireRequest{
		APIKey: apiKey,
		Limit:  5,
		TTLMS:  100, // 100ms TTL
	})
	if err != nil {
		t.Fatalf("Acquire failed: %v", err)
	}

	leaseKey := buildLeaseKey(apiKey, result.LeaseID)

	// Verify lease exists
	if !mr.Exists(leaseKey) {
		t.Error("lease key should exist immediately after acquire")
	}

	// Wait for lease to expire (real time, not miniredis time)
	time.Sleep(150 * time.Millisecond)

	// Fast-forward miniredis time to expire the lease key
	mr.FastForward(200 * time.Millisecond)

	// Verify lease expired
	if mr.Exists(leaseKey) {
		t.Error("lease key should have expired")
	}

	// ZSET should still contain the expired lease (not yet cleaned)
	leasesKey := buildLeasesKey(apiKey)
	size, err := client.rdb.ZCard(ctx, leasesKey).Result()
	if err != nil {
		t.Fatalf("ZCard failed: %v", err)
	}
	if size != 1 {
		t.Errorf("expected ZSET size=1 before cleanup, got %d", size)
	}

	// Next acquire should auto-clean expired leases
	result2, err := client.Acquire(ctx, AcquireRequest{
		APIKey: apiKey,
		Limit:  5,
		TTLMS:  30000,
	})
	if err != nil {
		t.Fatalf("Second acquire failed: %v", err)
	}
	if !result2.Allowed {
		t.Error("second acquire should succeed after expired lease cleanup")
	}

	// ZSET should now only contain the new lease (old one cleaned up)
	size, err = client.rdb.ZCard(ctx, leasesKey).Result()
	if err != nil {
		t.Fatalf("ZCard failed: %v", err)
	}
	if size != 1 {
		t.Errorf("expected ZSET size=1 after cleanup, got %d", size)
	}
}

func TestAcquire_ConcurrentRequests(t *testing.T) {
	client, _ := setupTestRedis(t)
	defer client.Close()

	ctx := context.Background()
	apiKey := "test-key"
	limit := int64(10)

	// Simulate concurrent acquires
	results := make(chan *AcquireResult, 15)
	errors := make(chan error, 15)

	for i := 0; i < 15; i++ {
		go func() {
			result, err := client.Acquire(ctx, AcquireRequest{
				APIKey: apiKey,
				Limit:  limit,
				TTLMS:  30000,
			})
			if err != nil {
				errors <- err
				return
			}
			results <- result
		}()
	}

	// Collect results
	var allowed, denied int
	for i := 0; i < 15; i++ {
		select {
		case err := <-errors:
			t.Fatalf("concurrent acquire failed: %v", err)
		case result := <-results:
			if result.Allowed {
				allowed++
			} else {
				denied++
			}
		}
	}

	// Should allow exactly 10, deny 5
	if allowed != 10 {
		t.Errorf("expected 10 allowed, got %d", allowed)
	}
	if denied != 5 {
		t.Errorf("expected 5 denied, got %d", denied)
	}
}
