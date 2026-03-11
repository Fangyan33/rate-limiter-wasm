package store

import (
	"fmt"
	"strings"

	"rate-limiter-wasm/internal/config"
	"rate-limiter-wasm/internal/limiter"
)

type counterService interface {
	Acquire(apiKey string, limit int) (func(), bool, error)
	Name() string
}

type client struct {
	service counterService
}

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

	return &client{service: newNoopCounterService(counterServiceOptions{
		cluster:     cluster,
		timeoutMS:   cfg.TimeoutMS,
		acquirePath: cfg.AcquirePath,
		releasePath: cfg.ReleasePath,
		leaseTTLMS:  cfg.LeaseTTLMS,
	})}, nil
}

func (c *client) Acquire(apiKey string, limit int) (func(), bool, error) {
	return c.service.Acquire(apiKey, limit)
}

func (c *client) Name() string {
	return "counter_service"
}

type counterServiceOptions struct {
	cluster     string
	timeoutMS   int
	acquirePath string
	releasePath string
	leaseTTLMS  int
}

type noopCounterService struct {
	options counterServiceOptions
}

func newNoopCounterService(options counterServiceOptions) counterService {
	return &noopCounterService{options: options}
}

func (s *noopCounterService) Acquire(apiKey string, limit int) (func(), bool, error) {
	return nil, false, limiter.ErrStoreUnavailable
}

func (s *noopCounterService) Name() string {
	return "counter_service"
}
