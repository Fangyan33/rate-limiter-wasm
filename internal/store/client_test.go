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
		Cluster:   "ratelimit-service",
		TimeoutMS: 100,
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
