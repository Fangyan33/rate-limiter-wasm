package store

import (
	"fmt"
	"strings"

	"rate-limiter-wasm/internal/config"
	"rate-limiter-wasm/internal/limiter"
)

type client struct{}

func (c *client) Acquire(apiKey string, limit int) (releaseFunc func(), allowed bool, err error) {
	return nil, false, limiter.ErrStoreUnavailable
}

func (c *client) Name() string {
	return "counter_service"
}

func NewClient(cfg config.CounterServiceConfig) (limiter.DistributedStore, error) {
	cluster := strings.TrimSpace(cfg.Cluster)
	if cluster == "" {
		return nil, fmt.Errorf("counter_service.cluster required")
	}

	if !strings.HasPrefix(cfg.AcquirePath, "/") {
		return nil, fmt.Errorf("acquire_path must start with /")
	}
	if cfg.ReleasePath == "" {
		return nil, fmt.Errorf("release_path cannot be empty")
	}
	if !strings.HasPrefix(cfg.ReleasePath, "/") {
		return nil, fmt.Errorf("release_path must start with /")
	}
	if cfg.LeaseTTLMS <= 0 {
		return nil, fmt.Errorf("lease_ttl_ms must be positive")
	}

	return &client{}, nil
}
