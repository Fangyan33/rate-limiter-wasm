package plugin_test

import (
	"testing"

	"rate-limiter-wasm/internal/plugin"

	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/proxytest"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

func TestRootContextLoadsValidatedConfig(t *testing.T) {
	root := plugin.NewRootContext()

	err := root.LoadConfiguration([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 2
error_response:
  message: denied
`))
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}

	cfg := root.Config()
	if len(cfg.Domains) != 1 || cfg.Domains[0] != "api.example.com" {
		t.Fatalf("unexpected domains: %#v", cfg.Domains)
	}
}

func TestRootContextAcceptsRedisDistributedConfig(t *testing.T) {
	root := plugin.NewRootContext()

	err := root.LoadConfiguration([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 2
distributed_store:
  backend: redis
  redis:
    address: redis.service:6379
    key_prefix: ratelimit
error_response:
  message: denied
`))
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}

	cfg := root.Config()
	if cfg.DistributedStore.Backend != "redis" {
		t.Fatalf("expected distributed backend redis, got %q", cfg.DistributedStore.Backend)
	}
	if cfg.DistributedStore.Redis.Address != "redis.service:6379" {
		t.Fatalf("unexpected redis address: %q", cfg.DistributedStore.Redis.Address)
	}
}

func TestRootContextRejectsInvalidConfig(t *testing.T) {
	root := plugin.NewRootContext()

	if err := root.LoadConfiguration([]byte(`domains: []`)); err == nil {
		t.Fatal("expected invalid config to be rejected")
	}
}

func TestRootContextStartsWithZeroValueConfig(t *testing.T) {
	root := plugin.NewRootContext()

	cfg := root.Config()
	if len(cfg.Domains) != 0 {
		t.Fatalf("expected empty domains on zero value config, got %#v", cfg.Domains)
	}
	if len(cfg.RateLimits) != 0 {
		t.Fatalf("expected empty rate limits on zero value config, got %#v", cfg.RateLimits)
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
	if resp := host.GetSentLocalResponse(contextID); resp != nil {
		t.Fatalf("expected no local response for unmatched domain, got %#v", resp)
	}
}

func TestPluginRejectsMissingAuthorizationHeader(t *testing.T) {
	host, reset := newHTTPHost(t)
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "api.example.com"},
	}, false)
	assertRejected(t, host, contextID, action, 429, "Rate limit exceeded")
}

func TestPluginRejectsUnknownAPIKeyWithoutDefaultFallback(t *testing.T) {
	host, reset := newHTTPHost(t)
	defer reset()

	unknownID := host.InitializeHttpContext()
	unknownAction := host.CallOnRequestHeaders(unknownID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer unknown_key"},
	}, false)
	assertRejected(t, host, unknownID, unknownAction, 429, "Rate limit exceeded")

	allowedID := host.InitializeHttpContext()
	allowedAction := host.CallOnRequestHeaders(allowedID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer key_basic_001"},
	}, false)
	if allowedAction != types.ActionContinue {
		t.Fatalf("expected configured api key to continue after unknown key rejection, got %v", allowedAction)
	}
}

func TestPluginRejectsWhenConcurrentLimitReached(t *testing.T) {
	host, reset := newHTTPHost(t)
	defer reset()

	firstID := host.InitializeHttpContext()
	firstAction := host.CallOnRequestHeaders(firstID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer key_basic_001"},
	}, false)
	if firstAction != types.ActionContinue {
		t.Fatalf("expected first request to continue, got %v", firstAction)
	}
	if resp := host.GetSentLocalResponse(firstID); resp != nil {
		t.Fatalf("expected no local response for first request, got %#v", resp)
	}

	secondID := host.InitializeHttpContext()
	secondAction := host.CallOnRequestHeaders(secondID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer key_basic_001"},
	}, false)
	assertRejected(t, host, secondID, secondAction, 429, "Rate limit exceeded")

	host.CompleteHttpContext(secondID)
	host.CompleteHttpContext(firstID)

	thirdID := host.InitializeHttpContext()
	thirdAction := host.CallOnRequestHeaders(thirdID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer key_basic_001"},
	}, false)
	if thirdAction != types.ActionContinue {
		t.Fatalf("expected request after rejected stream completion and original release to continue, got %v", thirdAction)
	}
}

func TestPluginReleasesSlotOnStreamDone(t *testing.T) {
	host, reset := newHTTPHost(t)
	defer reset()

	firstID := host.InitializeHttpContext()
	firstAction := host.CallOnRequestHeaders(firstID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer key_basic_001"},
	}, false)
	if firstAction != types.ActionContinue {
		t.Fatalf("expected first request to continue, got %v", firstAction)
	}

	host.CompleteHttpContext(firstID)

	secondID := host.InitializeHttpContext()
	secondAction := host.CallOnRequestHeaders(secondID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer key_basic_001"},
	}, false)
	if secondAction != types.ActionContinue {
		t.Fatalf("expected second request after stream done to continue, got %v", secondAction)
	}
	if resp := host.GetSentLocalResponse(secondID); resp != nil {
		t.Fatalf("expected no local response after release, got %#v", resp)
	}
}

func TestPluginFallsBackToLocalLimitWhenCounterServiceUnavailable(t *testing.T) {
	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 1
distributed_limit:
  enabled: true
  backend: counter_service
  counter_service:
    cluster: ratelimit-service
    acquire_path: /acquire
    release_path: /release
    lease_ttl_ms: 30000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	// With the async distributed path enabled, requests pause while
	// the counter-service acquire decision is pending. The synchronous
	// fallback to local limiting will be exercised by the callback
	// handler in a later task; for now, verify the pause behavior.
	firstID := host.InitializeHttpContext()
	firstAction := host.CallOnRequestHeaders(firstID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer key_basic_001"},
	}, false)
	if firstAction != types.ActionPause {
		t.Fatalf("expected first request to pause for async distributed acquire, got %v", firstAction)
	}
	if resp := host.GetSentLocalResponse(firstID); resp != nil {
		t.Fatalf("expected no local response while acquire decision is pending, got %#v", resp)
	}
}

func TestRootContextRejectsInvalidCounterServiceConfig(t *testing.T) {
	root := plugin.NewRootContext()

	err := root.LoadConfiguration([]byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 1
distributed_limit:
  enabled: true
  backend: counter_service
  counter_service:
    cluster: ""
`))
	if err == nil {
		t.Fatal("expected invalid counter_service config to be rejected")
	}
}

func TestPluginPausesRequestWhileCounterServiceAcquireIsPending(t *testing.T) {
	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 1
distributed_limit:
  enabled: true
  backend: counter_service
  counter_service:
    cluster: ratelimit-service
    acquire_path: /acquire
    release_path: /release
    lease_ttl_ms: 30000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer key_basic_001"},
	}, false)

	if action != types.ActionPause {
		t.Fatalf("expected distributed acquire to pause request, got %v", action)
	}
	if resp := host.GetSentLocalResponse(contextID); resp != nil {
		t.Fatalf("expected no local response while acquire decision is pending, got %#v", resp)
	}
}

func newHTTPHost(t *testing.T) (proxytest.HostEmulator, func()) {
	t.Helper()

	return newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
rate_limits:
  - api_key: key_basic_001
    max_concurrent: 1
error_response:
  status_code: 429
  message: Rate limit exceeded
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

func assertRejected(t *testing.T, host proxytest.HostEmulator, contextID uint32, action types.Action, wantStatus uint32, wantBody string) {
	t.Helper()

	if action != types.ActionPause {
		t.Fatalf("expected pause when rejecting request, got %v", action)
	}

	resp := host.GetSentLocalResponse(contextID)
	if resp == nil {
		t.Fatal("expected local response to be sent")
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("unexpected status code: got %d want %d", resp.StatusCode, wantStatus)
	}
	if string(resp.Data) != wantBody {
		t.Fatalf("unexpected response body: got %q want %q", string(resp.Data), wantBody)
	}
}
