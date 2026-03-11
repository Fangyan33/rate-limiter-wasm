package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultErrorStatusCode           = 429
	defaultErrorMessage              = "Rate limit exceeded"
	defaultCounterServiceAcquirePath = "/acquire"
	defaultCounterServiceReleasePath = "/release"
	defaultCounterServiceLeaseTTLMS  = 30000
)

type Config struct {
	Domains          []string               `yaml:"domains"`
	RateLimits       []RateLimit            `yaml:"rate_limits"`
	DistributedStore DistributedStoreConfig `yaml:"distributed_store"`
	DistributedLimit DistributedLimitConfig `yaml:"distributed_limit"`
	ErrorResponse    ErrorResponse          `yaml:"error_response"`
}

type RateLimit struct {
	APIKey        string `yaml:"api_key"`
	MaxConcurrent int    `yaml:"max_concurrent"`
}

type DistributedStoreConfig struct {
	Backend string           `yaml:"backend"`
	Redis   RedisStoreConfig `yaml:"redis"`
}

type DistributedLimitConfig struct {
	Enabled        bool                 `yaml:"enabled"`
	Backend        string               `yaml:"backend"`
	CounterService CounterServiceConfig `yaml:"counter_service"`
}

type CounterServiceConfig struct {
	Cluster     string `yaml:"cluster"`
	TimeoutMS   int    `yaml:"timeout_ms"`
	AcquirePath string `yaml:"acquire_path"`
	ReleasePath string `yaml:"release_path"`
	LeaseTTLMS  int    `yaml:"lease_ttl_ms"`

	leaseTTLMSSet bool `yaml:"-"`
}

func (c *CounterServiceConfig) UnmarshalYAML(value *yaml.Node) error {
	type rawCounterServiceConfig struct {
		Cluster     string `yaml:"cluster"`
		TimeoutMS   int    `yaml:"timeout_ms"`
		AcquirePath string `yaml:"acquire_path"`
		ReleasePath string `yaml:"release_path"`
		LeaseTTLMS  *int   `yaml:"lease_ttl_ms"`
	}

	var raw rawCounterServiceConfig
	if err := value.Decode(&raw); err != nil {
		return err
	}

	c.Cluster = raw.Cluster
	c.TimeoutMS = raw.TimeoutMS
	c.AcquirePath = raw.AcquirePath
	c.ReleasePath = raw.ReleasePath
	if raw.LeaseTTLMS != nil {
		c.LeaseTTLMS = *raw.LeaseTTLMS
		c.leaseTTLMSSet = true
	}

	return nil
}

type RedisStoreConfig struct {
	Address   string `yaml:"address"`
	KeyPrefix string `yaml:"key_prefix"`
}

type ErrorResponse struct {
	StatusCode int    `yaml:"status_code"`
	Message    string `yaml:"message"`
}

func Parse(data []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.ErrorResponse.StatusCode == 0 {
		c.ErrorResponse.StatusCode = defaultErrorStatusCode
	}
	if strings.TrimSpace(c.ErrorResponse.Message) == "" {
		c.ErrorResponse.Message = defaultErrorMessage
	}
	if strings.TrimSpace(c.DistributedStore.Redis.KeyPrefix) == "" {
		c.DistributedStore.Redis.KeyPrefix = "rate-limiter"
	}
	if strings.TrimSpace(c.DistributedLimit.CounterService.AcquirePath) == "" {
		c.DistributedLimit.CounterService.AcquirePath = defaultCounterServiceAcquirePath
	}
	if strings.TrimSpace(c.DistributedLimit.CounterService.ReleasePath) == "" {
		c.DistributedLimit.CounterService.ReleasePath = defaultCounterServiceReleasePath
	}
	if !c.DistributedLimit.CounterService.leaseTTLMSSet {
		c.DistributedLimit.CounterService.LeaseTTLMS = defaultCounterServiceLeaseTTLMS
	}
}

func (c *Config) Validate() error {
	if len(c.Domains) == 0 {
		return fmt.Errorf("domains must not be empty")
	}

	for _, domain := range c.Domains {
		if strings.TrimSpace(domain) == "" {
			return fmt.Errorf("domains must not contain empty values")
		}
	}

	if len(c.RateLimits) == 0 {
		return fmt.Errorf("rate_limits must not be empty")
	}

	seen := make(map[string]struct{}, len(c.RateLimits))
	for _, limit := range c.RateLimits {
		if strings.TrimSpace(limit.APIKey) == "" {
			return fmt.Errorf("rate_limits.api_key must not be empty")
		}
		if limit.MaxConcurrent <= 0 {
			return fmt.Errorf("rate_limits.max_concurrent must be greater than zero")
		}
		if _, ok := seen[limit.APIKey]; ok {
			return fmt.Errorf("duplicate api_key: %s", limit.APIKey)
		}
		seen[limit.APIKey] = struct{}{}
	}

	switch backend := strings.TrimSpace(c.DistributedStore.Backend); backend {
	case "", "redis":
		if backend == "redis" && strings.TrimSpace(c.DistributedStore.Redis.Address) == "" {
			return fmt.Errorf("distributed_store.redis.address must not be empty when backend is redis")
		}
	default:
		return fmt.Errorf("unsupported distributed_store.backend: %s", backend)
	}

	if c.DistributedLimit.Enabled {
		if strings.TrimSpace(c.DistributedLimit.Backend) != "counter_service" {
			return fmt.Errorf("distributed_limit.backend must be counter_service when enabled")
		}
		if strings.TrimSpace(c.DistributedLimit.CounterService.Cluster) == "" {
			return fmt.Errorf("distributed_limit.counter_service.cluster must not be empty when enabled")
		}
		if c.DistributedLimit.CounterService.TimeoutMS < 0 {
			return fmt.Errorf("distributed_limit.counter_service.timeout_ms must be >= 0")
		}
		if c.DistributedLimit.CounterService.LeaseTTLMS <= 0 {
			return fmt.Errorf("distributed_limit.counter_service.lease_ttl_ms must be > 0")
		}
		if err := validateCounterServicePath("acquire_path", c.DistributedLimit.CounterService.AcquirePath); err != nil {
			return err
		}
		if err := validateCounterServicePath("release_path", c.DistributedLimit.CounterService.ReleasePath); err != nil {
			return err
		}
	}

	if c.ErrorResponse.StatusCode < 400 {
		return fmt.Errorf("error_response.status_code must be >= 400")
	}

	return nil
}

func validateCounterServicePath(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("distributed_limit.counter_service.%s must not be empty", name)
	}
	if !strings.HasPrefix(value, "/") {
		return fmt.Errorf("distributed_limit.counter_service.%s must start with /", name)
	}
	return nil
}
