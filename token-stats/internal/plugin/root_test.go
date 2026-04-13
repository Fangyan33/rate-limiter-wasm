package plugin_test

import (
	"testing"

	"rate-limiter-wasm/token-stats/internal/plugin"

	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/proxytest"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

func TestRootContextLoadsValidatedConfig(t *testing.T) {
	root := plugin.NewRootContext()

	err := root.LoadConfiguration([]byte(`domains:
  - api.example.com
token_statistics:
  enabled: true
  inject_stream_usage: true
`))
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}

	cfg := root.Config()
	if len(cfg.Domains) != 1 || cfg.Domains[0] != "api.example.com" {
		t.Fatalf("unexpected domains: %#v", cfg.Domains)
	}
	if !cfg.TokenStatistics.Enabled {
		t.Fatal("expected token statistics to be enabled")
	}
}

func TestRootContextRejectsInvalidConfig(t *testing.T) {
	root := plugin.NewRootContext()

	if err := root.LoadConfiguration([]byte(`domains: []`)); err == nil {
		t.Fatal("expected invalid config to be rejected")
	}
}

func TestPluginBypassesUnmatchedDomain(t *testing.T) {
	host, reset := newHTTPHost(t)
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "other.example.com"},
	}, false)
	if action != types.ActionContinue {
		t.Fatalf("expected continue for unmatched domain, got %v", action)
	}
}

func TestPluginDoesNotRejectMissingAuthorizationHeader(t *testing.T) {
	host, reset := newHTTPHost(t)
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "api.example.com"},
	}, false)
	if action != types.ActionContinue {
		t.Fatalf("expected continue when authorization header is missing, got %v", action)
	}
	if resp := host.GetSentLocalResponse(contextID); resp != nil {
		t.Fatalf("expected no local response, got %#v", resp)
	}
}

func newHTTPHost(t *testing.T) (proxytest.HostEmulator, func()) {
	t.Helper()

	return newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
token_statistics:
  enabled: true
  inject_stream_usage: true
  metric_key_limit: 5000
`))
}

func newHTTPHostWithConfig(t *testing.T, cfg []byte) (proxytest.HostEmulator, func()) {
	t.Helper()

	opt := proxytest.NewEmulatorOption().
		WithVMContext(plugin.NewVMContext()).
		WithPluginConfiguration(cfg)

	host, reset := proxytest.NewHostEmulator(opt)
	if status := host.StartPlugin(); status != types.OnPluginStartStatusOK {
		reset()
		t.Fatalf("StartPlugin() status = %v", status)
	}

	return host, reset
}
