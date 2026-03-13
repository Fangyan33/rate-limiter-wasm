package config_test

import (
	"reflect"
	"testing"

	"rate-limiter-wasm/internal/config"
)

func TestParseConfigParsesMinimalConfig(t *testing.T) {
	raw := []byte(`domains:
  - api.example.com
  - "*.service.example.com"
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
error_response:
  status_code: 429
  message: Rate limit exceeded for API key
`)

	cfg, err := config.Parse(raw)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if len(cfg.Domains) != 2 {
		t.Fatalf("expected 2 domains, got %d", len(cfg.Domains))
	}

	if len(cfg.RateLimits) != 1 {
		t.Fatalf("expected 1 rate limit, got %d", len(cfg.RateLimits))
	}

	if cfg.RateLimits[0].APIKey != "key_basic_001" {
		t.Fatalf("unexpected api_key: %q", cfg.RateLimits[0].APIKey)
	}

	if cfg.RateLimits[0].MaxConcurrent != 10 {
		t.Fatalf("expected max_concurrent=10, got %d", cfg.RateLimits[0].MaxConcurrent)
	}

	if cfg.ErrorResponse.StatusCode != 429 {
		t.Fatalf("expected status_code=429, got %d", cfg.ErrorResponse.StatusCode)
	}

	if cfg.ErrorResponse.Message != "Rate limit exceeded for API key" {
		t.Fatalf("unexpected error message: %q", cfg.ErrorResponse.Message)
	}
}

func TestParseConfigParsesRedisDistributedStore(t *testing.T) {
	cfg, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
distributed_store:
  backend: redis
  redis:
    address: redis.service:6379
    key_prefix: team-a
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if cfg.DistributedStore.Backend != "redis" {
		t.Fatalf("expected backend redis, got %q", cfg.DistributedStore.Backend)
	}
	if cfg.DistributedStore.Redis.Address != "redis.service:6379" {
		t.Fatalf("unexpected redis address: %q", cfg.DistributedStore.Redis.Address)
	}
	if cfg.DistributedStore.Redis.KeyPrefix != "team-a" {
		t.Fatalf("unexpected redis key prefix: %q", cfg.DistributedStore.Redis.KeyPrefix)
	}
}

func TestParseConfigRejectsRedisWithoutAddress(t *testing.T) {
	_, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
distributed_store:
  backend: redis
`))
	if err == nil {
		t.Fatal("expected Parse() to reject redis backend without address")
	}
}

func TestParseConfigRejectsUnsupportedDistributedBackend(t *testing.T) {
	_, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
distributed_store:
  backend: consul
`))
	if err == nil {
		t.Fatal("expected Parse() to reject unsupported distributed backend")
	}
}

func TestParseConfigDefaultsRedisKeyPrefixWhenOmitted(t *testing.T) {
	cfg, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
distributed_store:
  backend: redis
  redis:
    address: redis.service:6379
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if cfg.DistributedStore.Redis.KeyPrefix == "" {
		t.Fatal("expected default redis key_prefix to be populated")
	}
}

func TestParseDistributedStoreCounterServiceConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
distributed_limit:
  enabled: true
  backend: "counter_service"
  counter_service:
    cluster: "ratelimit-service"
    timeout_ms: 100
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if !cfg.DistributedLimit.Enabled {
		t.Fatal("expected distributed limit to be enabled")
	}
	if cfg.DistributedLimit.Backend != "counter_service" {
		t.Fatalf("expected backend counter_service, got %q", cfg.DistributedLimit.Backend)
	}
	if cfg.DistributedLimit.CounterService.Cluster != "ratelimit-service" {
		t.Fatalf("unexpected counter service cluster: %q", cfg.DistributedLimit.CounterService.Cluster)
	}
	if cfg.DistributedLimit.CounterService.TimeoutMS != 100 {
		t.Fatalf("expected timeout_ms=100, got %d", cfg.DistributedLimit.CounterService.TimeoutMS)
	}
}

func TestParseDistributedStoreCounterServiceDefaultsPathsAndLeaseTTL(t *testing.T) {
	cfg, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
distributed_limit:
  enabled: true
  backend: "counter_service"
  counter_service:
    cluster: "ratelimit-service"
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if cfg.DistributedLimit.CounterService.AcquirePath != "/acquire" {
		t.Fatalf("expected default acquire_path=/acquire, got %q", cfg.DistributedLimit.CounterService.AcquirePath)
	}
	if cfg.DistributedLimit.CounterService.ReleasePath != "/release" {
		t.Fatalf("expected default release_path=/release, got %q", cfg.DistributedLimit.CounterService.ReleasePath)
	}
	if cfg.DistributedLimit.CounterService.LeaseTTLMS != 30000 {
		t.Fatalf("expected default lease_ttl_ms=30000, got %d", cfg.DistributedLimit.CounterService.LeaseTTLMS)
	}
}

func TestParseDistributedStoreCounterServiceRejectsInvalidLeaseTTL(t *testing.T) {
	_, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
distributed_limit:
  enabled: true
  backend: "counter_service"
  counter_service:
    cluster: "ratelimit-service"
    lease_ttl_ms: 0
`))
	if err == nil {
		t.Fatal("expected Parse() to reject non-positive lease_ttl_ms")
	}
}

func TestParseDistributedStoreCounterServiceRejectsPathWithoutLeadingSlash(t *testing.T) {
	_, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
distributed_limit:
  enabled: true
  backend: "counter_service"
  counter_service:
    cluster: "ratelimit-service"
    acquire_path: acquire
`))
	if err == nil {
		t.Fatal("expected Parse() to reject acquire_path without leading slash")
	}
}

func TestParseDistributedStoreCounterServiceLeavesTimeoutUnsetWhenOmitted(t *testing.T) {
	cfg, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
distributed_limit:
  enabled: true
  backend: "counter_service"
  counter_service:
    cluster: "ratelimit-service"
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if cfg.DistributedLimit.CounterService.TimeoutMS != 0 {
		t.Fatalf("expected timeout_ms to remain unset, got %d", cfg.DistributedLimit.CounterService.TimeoutMS)
	}
}

func TestParseConfigRejectsEmptyDomains(t *testing.T) {
	_, err := config.Parse([]byte(`rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
error_response:
  status_code: 429
  message: denied
`))
	if err == nil {
		t.Fatal("expected Parse() to reject empty domains")
	}
}

func TestParseConfigRejectsInvalidRateLimit(t *testing.T) {
	_, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: ""
    max_concurrent: 0
error_response:
  status_code: 429
  message: denied
`))
	if err == nil {
		t.Fatal("expected Parse() to reject invalid rate limit entry")
	}
}

func TestParseConfigDefaultsErrorResponseWhenOmitted(t *testing.T) {
	cfg, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if cfg.ErrorResponse.StatusCode != 429 {
		t.Fatalf("expected default status_code=429, got %d", cfg.ErrorResponse.StatusCode)
	}

	if cfg.ErrorResponse.Message == "" {
		t.Fatal("expected default error_response.message to be populated")
	}
}

func TestParseConfigTokenStatisticsDefaults(t *testing.T) {
	cfg, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	tokenStats := reflect.ValueOf(cfg).FieldByName("TokenStatistics")
	if !tokenStats.IsValid() {
		t.Fatal("expected Config to define TokenStatistics")
	}

	enabled := tokenStats.FieldByName("Enabled")
	if !enabled.IsValid() || enabled.Kind() != reflect.Bool {
		t.Fatalf("expected TokenStatistics.Enabled to be bool, got %v", enabled.Kind())
	}
	if enabled.Bool() {
		t.Fatal("expected token_statistics.enabled default false")
	}

	metricKeyLimit := tokenStats.FieldByName("MetricKeyLimit")
	if !metricKeyLimit.IsValid() || metricKeyLimit.Kind() != reflect.Int {
		t.Fatalf("expected TokenStatistics.MetricKeyLimit to be int, got %v", metricKeyLimit.Kind())
	}
	if metricKeyLimit.Int() != 5000 {
		t.Fatalf("expected default metric_key_limit=5000, got %d", metricKeyLimit.Int())
	}
}

func TestParseConfigTokenStatisticsOverridesMetricKeyLimit(t *testing.T) {
	cfg, err := config.Parse([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 10
token_statistics:
  enabled: true
  metric_key_limit: 123
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	tokenStats := reflect.ValueOf(cfg).FieldByName("TokenStatistics")
	if !tokenStats.IsValid() {
		t.Fatal("expected Config to define TokenStatistics")
	}

	enabled := tokenStats.FieldByName("Enabled")
	if !enabled.IsValid() || enabled.Kind() != reflect.Bool {
		t.Fatalf("expected TokenStatistics.Enabled to be bool, got %v", enabled.Kind())
	}
	if !enabled.Bool() {
		t.Fatal("expected token_statistics.enabled true")
	}

	metricKeyLimit := tokenStats.FieldByName("MetricKeyLimit")
	if !metricKeyLimit.IsValid() || metricKeyLimit.Kind() != reflect.Int {
		t.Fatalf("expected TokenStatistics.MetricKeyLimit to be int, got %v", metricKeyLimit.Kind())
	}
	if metricKeyLimit.Int() != 123 {
		t.Fatalf("expected metric_key_limit=123, got %d", metricKeyLimit.Int())
	}
}
