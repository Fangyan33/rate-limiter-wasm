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

	return &client{service: newNoopCounterService()}, nil
}

func (c *client) Acquire(apiKey string, limit int) (func(), bool, error) {
	return c.service.Acquire(apiKey, limit)
}

func (c *client) Name() string {
	return "counter_service"
}

type noopCounterService struct{}

func newNoopCounterService() counterService {
	return &noopCounterService{}
}

func (s *noopCounterService) Acquire(apiKey string, limit int) (func(), bool, error) {
	return nil, false, limiter.ErrStoreUnavailable
}

func (s *noopCounterService) Name() string {
	return "counter_service"
}
