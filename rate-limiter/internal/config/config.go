package config

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultErrorStatusCode           = 429
	defaultErrorMessage              = "Rate limit exceeded"
	defaultCounterServiceAcquirePath = "/acquire"
	defaultCounterServiceReleasePath = "/release"
	defaultCounterServiceRenewPath   = "/renew"
	defaultCounterServiceLeaseTTLMS  = 30000
)

type Config struct {
	DistributedLimit DistributedLimitConfig `yaml:"distributed_limit"`
	ErrorResponse    ErrorResponse          `yaml:"error_response"`
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
	RenewPath   string `yaml:"renew_path"`
	LeaseTTLMS  int    `yaml:"lease_ttl_ms"`

	leaseTTLMSSet bool `yaml:"-"`
}

func (c *CounterServiceConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.MappingNode {
		for i := 0; i < len(value.Content); i += 2 {
			name := value.Content[i].Value
			switch name {
			case "cluster", "timeout_ms", "acquire_path", "release_path", "renew_path", "lease_ttl_ms":
			default:
				return fmt.Errorf("unknown counter_service field %q", name)
			}
			if value.Content[i+1].Tag == "!!null" {
				return fmt.Errorf("counter_service.%s must not be null", name)
			}
		}
	}
	type rawCounterServiceConfig struct {
		Cluster     string `yaml:"cluster"`
		TimeoutMS   int    `yaml:"timeout_ms"`
		AcquirePath string `yaml:"acquire_path"`
		ReleasePath string `yaml:"release_path"`
		RenewPath   string `yaml:"renew_path"`
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
	c.RenewPath = raw.RenewPath
	if raw.LeaseTTLMS != nil {
		c.LeaseTTLMS = *raw.LeaseTTLMS
		c.leaseTTLMSSet = true
	}

	return nil
}

type ErrorResponse struct {
	StatusCode int    `yaml:"status_code"`
	Message    string `yaml:"message"`
}

func Parse(data []byte) (Config, error) {
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := decoder.Decode(new(any)); err != io.EOF {
		return Config{}, fmt.Errorf("configuration must contain one YAML document")
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
	if strings.TrimSpace(c.DistributedLimit.CounterService.AcquirePath) == "" {
		c.DistributedLimit.CounterService.AcquirePath = defaultCounterServiceAcquirePath
	}
	if strings.TrimSpace(c.DistributedLimit.CounterService.ReleasePath) == "" {
		c.DistributedLimit.CounterService.ReleasePath = defaultCounterServiceReleasePath
	}
	if strings.TrimSpace(c.DistributedLimit.CounterService.RenewPath) == "" {
		c.DistributedLimit.CounterService.RenewPath = defaultCounterServiceRenewPath
	}
	if !c.DistributedLimit.CounterService.leaseTTLMSSet {
		c.DistributedLimit.CounterService.LeaseTTLMS = defaultCounterServiceLeaseTTLMS
	}
}

func (c *Config) Validate() error {
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
		if err := validateCounterServicePath("renew_path", c.DistributedLimit.CounterService.RenewPath); err != nil {
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
