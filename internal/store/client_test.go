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

func TestNewClientRejectsInvalidLeaseTTL(t *testing.T) {
	_, err := NewClient(config.CounterServiceConfig{
		Cluster:    "ratelimit-service",
		LeaseTTLMS: 0,
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
