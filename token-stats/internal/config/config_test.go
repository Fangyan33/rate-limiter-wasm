package config_test

import (
	"testing"

	"rate-limiter-wasm/token-stats/internal/config"
)

func TestParseConfigDefaultsMetricKeyLimit(t *testing.T) {
	cfg, err := config.Parse([]byte(`domains:
  - api.example.com
token_statistics:
  enabled: true
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if cfg.TokenStatistics.MetricKeyLimit != 5000 {
		t.Fatalf("unexpected metric key limit: %d", cfg.TokenStatistics.MetricKeyLimit)
	}
}

func TestParseConfigRejectsEmptyDomains(t *testing.T) {
	if _, err := config.Parse([]byte(`domains: []`)); err == nil {
		t.Fatal("expected empty domains to be rejected")
	}
}
