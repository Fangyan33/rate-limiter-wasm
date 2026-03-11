package store

import (
	"testing"

	"rate-limiter-wasm/internal/config"
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

	c, ok := store.(*client)
	if !ok {
		t.Fatalf("expected *client, got %T", store)
	}
	if c.http.cluster != "ratelimit-service" {
		t.Fatalf("unexpected cluster: %q", c.http.cluster)
	}
	if c.http.acquirePath != "/acquire" {
		t.Fatalf("unexpected acquire path: %q", c.http.acquirePath)
	}
	if c.http.releasePath != "/release" {
		t.Fatalf("unexpected release path: %q", c.http.releasePath)
	}
	if c.http.timeoutMS != 100 {
		t.Fatalf("unexpected timeout: %d", c.http.timeoutMS)
	}
	if c.http.leaseTTLMS != 30000 {
		t.Fatalf("unexpected lease ttl: %d", c.http.leaseTTLMS)
	}
}
