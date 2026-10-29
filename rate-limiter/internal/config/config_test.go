package config_test

import (
	"github.com/stretchr/testify/require"
	"rate-limiter-wasm/rate-limiter/internal/config"
	"testing"
)

const enabled = `distributed_limit:
  enabled: true
  backend: counter_service
  counter_service:
    cluster: counter-service
`

func TestConfigDefaultsAndDisabledMode(t *testing.T) {
	cfg, err := config.Parse([]byte(enabled))
	require.NoError(t, err)
	require.True(t, cfg.DistributedLimit.Enabled)
	cs := cfg.DistributedLimit.CounterService
	require.Equal(t, "/acquire", cs.AcquirePath)
	require.Equal(t, "/renew", cs.RenewPath)
	require.Equal(t, "/release", cs.ReleasePath)
	require.Equal(t, 30000, cs.LeaseTTLMS)
	require.Equal(t, 429, cfg.ErrorResponse.StatusCode)
	cfg, err = config.Parse([]byte("distributed_limit:\n  enabled: false\n"))
	require.NoError(t, err)
	require.False(t, cfg.DistributedLimit.Enabled)
}
func TestInvalidAndRemovedConfiguration(t *testing.T) {
	for _, data := range []string{"", "{", "domains: [api.example.com]", "rate_limits: []", "---\n{}\n---\n{}", "distributed_limit:\n  enabled: true", enabled + "    lease_ttl_ms: 0\n", enabled + "    renew_path: relative\n", enabled + "    timeout_ms: -1\n", enabled + "    timeout_ms: null\n", enabled + "    typo: bad\n"} {
		_, err := config.Parse([]byte(data))
		require.Error(t, err, data)
	}
}
