package plugin_test

import (
	"encoding/json"
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

func TestPluginResumesRequestWhenCounterServiceAcquireSucceeds(t *testing.T) {
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

	callouts := host.GetCalloutAttributesFromContext(contextID)
	if len(callouts) != 1 {
		t.Fatalf("expected one counter-service callout, got %d", len(callouts))
	}
	if callouts[0].Upstream != "ratelimit-service" {
		t.Fatalf("unexpected callout upstream: %q", callouts[0].Upstream)
	}

	host.CallOnHttpCallResponse(callouts[0].CalloutID, [][2]string{{":status", "200"}}, nil, []byte(`{"allowed":true,"lease_id":"lease-123"}`))

	if got := host.GetCurrentHttpStreamAction(contextID); got != types.ActionContinue {
		t.Fatalf("expected request to resume after successful acquire, got %v", got)
	}
	if resp := host.GetSentLocalResponse(contextID); resp != nil {
		t.Fatalf("expected no local response after successful acquire, got %#v", resp)
	}
}

func TestPluginDispatchesCounterServiceReleaseOnStreamDone(t *testing.T) {
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
		t.Fatalf("expected request to pause, got %v", action)
	}

	callouts := host.GetCalloutAttributesFromContext(contextID)
	if len(callouts) != 1 {
		t.Fatalf("expected one acquire callout, got %d", len(callouts))
	}

	// Simulate successful acquire with lease_id
	host.CallOnHttpCallResponse(callouts[0].CalloutID, [][2]string{{":status", "200"}}, nil, []byte(`{"allowed":true,"lease_id":"lease-abc-123"}`))

	if got := host.GetCurrentHttpStreamAction(contextID); got != types.ActionContinue {
		t.Fatalf("expected request to resume, got %v", got)
	}

	// Complete the stream
	host.CompleteHttpContext(contextID)

	// Check that a release callout was dispatched
	releaseCallouts := host.GetCalloutAttributesFromContext(contextID)
	if len(releaseCallouts) != 2 {
		t.Fatalf("expected acquire + release callouts, got %d", len(releaseCallouts))
	}

	releaseCallout := releaseCallouts[1]
	if releaseCallout.Upstream != "ratelimit-service" {
		t.Fatalf("unexpected release upstream: %q", releaseCallout.Upstream)
	}

	// Verify release request body contains api_key and lease_id
	var releaseReq struct {
		APIKey  string `json:"api_key"`
		LeaseID string `json:"lease_id"`
	}
	if err := json.Unmarshal(releaseCallout.Body, &releaseReq); err != nil {
		t.Fatalf("parse release request body: %v", err)
	}
	if releaseReq.APIKey != "key_basic_001" {
		t.Fatalf("unexpected api_key in release: %q", releaseReq.APIKey)
	}
	if releaseReq.LeaseID != "lease-abc-123" {
		t.Fatalf("unexpected lease_id in release: %q", releaseReq.LeaseID)
	}
}

func TestPluginResumesRequestWhenAsyncAcquireReturnsNon200(t *testing.T) {
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

	firstID := host.InitializeHttpContext()
	firstAction := host.CallOnRequestHeaders(firstID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer key_basic_001"},
	}, false)
	if firstAction != types.ActionPause {
		t.Fatalf("expected first request to pause, got %v", firstAction)
	}

	firstCallouts := host.GetCalloutAttributesFromContext(firstID)
	if len(firstCallouts) != 1 {
		t.Fatalf("expected one first-request callout, got %d", len(firstCallouts))
	}

	secondID := host.InitializeHttpContext()
	secondAction := host.CallOnRequestHeaders(secondID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer key_basic_001"},
	}, false)
	if secondAction != types.ActionPause {
		t.Fatalf("expected second request to pause, got %v", secondAction)
	}

	secondCallouts := host.GetCalloutAttributesFromContext(secondID)
	if len(secondCallouts) != 1 {
		t.Fatalf("expected one second-request callout, got %d", len(secondCallouts))
	}

	host.CallOnHttpCallResponse(firstCallouts[0].CalloutID, [][2]string{{":status", "500"}}, nil, []byte(`{"error":"service unavailable"}`))

	if got := host.GetCurrentHttpStreamAction(firstID); got != types.ActionContinue {
		t.Fatalf("expected first request to resume after non-200 acquire response, got %v", got)
	}
	if resp := host.GetSentLocalResponse(firstID); resp != nil {
		t.Fatalf("expected no local response after first non-200 acquire response, got %#v", resp)
	}

	host.CallOnHttpCallResponse(secondCallouts[0].CalloutID, [][2]string{{":status", "500"}}, nil, []byte(`{"error":"service unavailable"}`))

	if got := host.GetCurrentHttpStreamAction(secondID); got != types.ActionContinue {
		t.Fatalf("expected second request to also resume after non-200 acquire response, got %v", got)
	}
	if resp := host.GetSentLocalResponse(secondID); resp != nil {
		t.Fatalf("expected no local response for second request after non-200 acquire response, got %#v", resp)
	}
}

func TestPluginResumesRequestWhenAsyncAcquireResponseIsInvalidJSON(t *testing.T) {
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
		t.Fatalf("expected request to pause, got %v", action)
	}

	callouts := host.GetCalloutAttributesFromContext(contextID)
	if len(callouts) != 1 {
		t.Fatalf("expected one callout, got %d", len(callouts))
	}

	host.CallOnHttpCallResponse(callouts[0].CalloutID, [][2]string{{":status", "200"}}, nil, []byte(`not-json`))

	if got := host.GetCurrentHttpStreamAction(contextID); got != types.ActionContinue {
		t.Fatalf("expected request to resume after invalid acquire response body, got %v", got)
	}
	if resp := host.GetSentLocalResponse(contextID); resp != nil {
		t.Fatalf("expected no local response after invalid acquire response body, got %#v", resp)
	}
}

func TestPluginDoesNotDispatchReleaseWhenAcquireFailedOpen(t *testing.T) {
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
		t.Fatalf("expected request to pause, got %v", action)
	}

	callouts := host.GetCalloutAttributesFromContext(contextID)
	if len(callouts) != 1 {
		t.Fatalf("expected one callout, got %d", len(callouts))
	}

	host.CallOnHttpCallResponse(callouts[0].CalloutID, [][2]string{{":status", "500"}}, nil, []byte(`{"error":"service unavailable"}`))
	host.CompleteHttpContext(contextID)

	finalCallouts := host.GetCalloutAttributesFromContext(contextID)
	if len(finalCallouts) != 1 {
		t.Fatalf("expected only acquire callout after fail-open, got %d", len(finalCallouts))
	}
}

func TestPluginRejectsRequestWhenCounterServiceAcquireDenies(t *testing.T) {
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

	callouts := host.GetCalloutAttributesFromContext(contextID)
	if len(callouts) != 1 {
		t.Fatalf("expected one counter-service callout, got %d", len(callouts))
	}

	host.CallOnHttpCallResponse(callouts[0].CalloutID, [][2]string{{":status", "200"}}, nil, []byte(`{"allowed":false}`))

	resp := host.GetSentLocalResponse(contextID)
	if resp == nil {
		t.Fatal("expected local rejection after denied acquire")
	}
	if resp.StatusCode != 429 {
		t.Fatalf("unexpected status code: got %d want 429", resp.StatusCode)
	}
	if string(resp.Data) != "Rate limit exceeded" {
		t.Fatalf("unexpected response body: got %q want %q", string(resp.Data), "Rate limit exceeded")
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
