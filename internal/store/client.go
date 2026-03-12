// Package store provides distributed store client implementations for rate limiting.
//
// IMPORTANT: Current Status and Usage
//
// This package is currently a PLACEHOLDER for future synchronous distributed backends.
// The counter_service mode (the primary distributed backend) does NOT use this interface.
//
// Architecture Context:
//   - counter_service mode uses ASYNCHRONOUS HTTP callouts directly in the plugin layer
//     (see internal/plugin/root.go OnHttpRequestHeaders and onAcquireResponse)
//   - This is required because Proxy-WASM SDK only supports async HTTP calls
//   - The limiter.DistributedStore interface was designed for synchronous backends
//
// Current Behavior:
//   - NewClient() validates counter_service configuration but returns a placeholder
//   - The Acquire() method always returns limiter.ErrStoreUnavailable
//   - This client is NEVER called when counter_service mode is enabled
//   - See internal/plugin/root.go:336-340 for the routing logic
//
// Future Use Cases:
//   - This interface is preserved for potential future synchronous distributed backends
//   - Examples: direct Redis protocol, gRPC services, or other sync-capable stores
//   - If such backends are added, they would use this DistributedStore abstraction
//
// Related Files:
//   - internal/plugin/root.go:133-176 - async counter_service acquire flow
//   - internal/plugin/root.go:187-231 - async counter_service release flow
//   - internal/plugin/root.go:335-353 - limiter construction routing logic
//   - docs/plans/2026-03-10-http-counter-service-design.md - design rationale
package store

import (
	"fmt"
	"strings"

	"rate-limiter-wasm/internal/config"
	"rate-limiter-wasm/internal/limiter"
)

type client struct {
	http *httpCounterServiceClient
}

// NewClient validates counter_service configuration and returns a placeholder client.
//
// IMPORTANT: This client is NOT used in counter_service mode.
// When counter_service is enabled, the plugin uses async HTTP callouts directly
// (see internal/plugin/root.go:336-340). This function only validates configuration
// for potential future synchronous backends.
//
// The returned client's Acquire() method always returns limiter.ErrStoreUnavailable.
func NewClient(cfg config.CounterServiceConfig) (limiter.DistributedStore, error) {
	cluster := strings.TrimSpace(cfg.Cluster)
	if cluster == "" {
		return nil, fmt.Errorf("counter_service.cluster must not be empty")
	}
	if cfg.TimeoutMS < 0 {
		return nil, fmt.Errorf("counter_service.timeout_ms must be >= 0")
	}
	if strings.TrimSpace(cfg.AcquirePath) == "" {
		return nil, fmt.Errorf("counter_service.acquire_path must not be empty")
	}
	if !strings.HasPrefix(cfg.AcquirePath, "/") {
		return nil, fmt.Errorf("counter_service.acquire_path must start with /")
	}
	if strings.TrimSpace(cfg.ReleasePath) == "" {
		return nil, fmt.Errorf("counter_service.release_path must not be empty")
	}
	if !strings.HasPrefix(cfg.ReleasePath, "/") {
		return nil, fmt.Errorf("counter_service.release_path must start with /")
	}
	if cfg.LeaseTTLMS <= 0 {
		return nil, fmt.Errorf("counter_service.lease_ttl_ms must be > 0")
	}

	return &client{
		http: &httpCounterServiceClient{
			cluster:     cluster,
			timeoutMS:   cfg.TimeoutMS,
			acquirePath: cfg.AcquirePath,
			releasePath: cfg.ReleasePath,
			leaseTTLMS:  cfg.LeaseTTLMS,
		},
	}, nil
}

// Acquire is a placeholder that always returns limiter.ErrStoreUnavailable.
//
// This method is NOT called in counter_service mode. The actual distributed
// acquire logic uses async HTTP callouts in internal/plugin/root.go.
func (c *client) Acquire(apiKey string, limit int) (func(), bool, error) {
	return nil, false, limiter.ErrStoreUnavailable
}

func (c *client) Name() string {
	return "counter_service"
}
