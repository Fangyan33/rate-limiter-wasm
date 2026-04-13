package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Domains         []string              `yaml:"domains"`
	TokenStatistics TokenStatisticsConfig `yaml:"token_statistics"`
}

type TokenStatisticsConfig struct {
	Enabled           bool `yaml:"enabled"`
	InjectStreamUsage bool `yaml:"inject_stream_usage"`
	MetricKeyLimit    int  `yaml:"metric_key_limit"`
}

func Parse(data []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}

	if cfg.TokenStatistics.MetricKeyLimit == 0 {
		cfg.TokenStatistics.MetricKeyLimit = 5000
	}

	if len(cfg.Domains) == 0 {
		return Config{}, fmt.Errorf("domains must not be empty")
	}
	for _, domain := range cfg.Domains {
		if strings.TrimSpace(domain) == "" {
			return Config{}, fmt.Errorf("domains must not contain empty values")
		}
	}
	if cfg.TokenStatistics.MetricKeyLimit <= 0 {
		return Config{}, fmt.Errorf("token_statistics.metric_key_limit must be > 0")
	}

	return cfg, nil
}
