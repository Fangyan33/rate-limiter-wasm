package store

import (
	"errors"
	"testing"

	"rate-limiter-wasm/internal/config"
	"rate-limiter-wasm/internal/limiter"
)

func TestNewClientRejectsEmptyCluster(t *testing.T) {
	_, err := NewClient(config.CounterServiceConfig{})
	if err == nil {
		t.Fatal("expected NewClient() to reject empty cluster")
	}
}

func TestNewClientReturnsDistributedStore(t *testing.T) {
	client, err := NewClient(config.CounterServiceConfig{
		Cluster:     "ratelimit-service",
		TimeoutMS:   100,
		AcquirePath: "/acquire",
		ReleasePath: "/release",
		LeaseTTLMS:  30000,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if client == nil {
		t.Fatal("expected NewClient() to return a distributed store")
	}
	if client.Name() != "counter_service" {
		t.Fatalf("unexpected store name: %q", client.Name())
	}
}

func TestNewClientRejectsInvalidLeaseTTL(t *testing.T) {
	_, err := NewClient(config.CounterServiceConfig{
		Cluster:     "ratelimit-service",
		AcquirePath: "/acquire",
		ReleasePath: "/release",
		LeaseTTLMS:  0,
	})
	if err == nil {
		t.Fatal("expected NewClient() to reject non-positive lease_ttl_ms")
	}
}

func TestNewClientRejectsAcquirePathWithoutLeadingSlash(t *testing.T) {
	_, err := NewClient(config.CounterServiceConfig{
		Cluster:     "ratelimit-service",
		AcquirePath: "acquire",
		ReleasePath: "/release",
		LeaseTTLMS:  30000,
	})
	if err == nil {
		t.Fatal("expected NewClient() to reject acquire_path without leading slash")
	}
}

func TestNewClientRejectsEmptyReleasePath(t *testing.T) {
	_, err := NewClient(config.CounterServiceConfig{
		Cluster:     "ratelimit-service",
		AcquirePath: "/acquire",
		ReleasePath: "",
		LeaseTTLMS:  30000,
	})
	if err == nil {
		t.Fatal("expected NewClient() to reject empty release_path")
	}
}

func TestNewClientRejectsReleasePathWithoutLeadingSlash(t *testing.T) {
	_, err := NewClient(config.CounterServiceConfig{
		Cluster:     "ratelimit-service",
		AcquirePath: "/acquire",
		ReleasePath: "release",
		LeaseTTLMS:  30000,
	})
	if err == nil {
		t.Fatal("expected NewClient() to reject release_path without leading slash")
	}
}

func TestNewClientReturnsPlaceholderStoreThatReportsUnavailable(t *testing.T) {
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

	release, allowed, err := store.Acquire("key_basic_001", 1)
	if !errors.Is(err, limiter.ErrStoreUnavailable) {
		t.Fatalf("expected ErrStoreUnavailable, got %v", err)
	}
	if allowed {
		t.Fatal("expected placeholder store acquire to deny distributed acquire")
	}
	if release != nil {
		t.Fatal("expected nil release func from placeholder store")
	}
}
