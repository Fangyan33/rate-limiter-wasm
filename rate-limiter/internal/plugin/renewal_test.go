package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/proxytest"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/handler"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/redis"
)

const renewalTestConfig = `distributed_limit:
  enabled: true
  backend: counter_service
  counter_service:
    cluster: counter-service
    timeout_ms: 5000
    lease_ttl_ms: 3000
`

type renewalTestVM struct {
	types.DefaultVMContext
	root *rootContext
}

func (vm *renewalTestVM) NewPluginContext(id uint32) types.PluginContext {
	vm.root.contextID = id
	return vm.root
}

func newRenewalHost(t *testing.T, prepare func(*rootContext)) (*rootContext, proxytest.HostEmulator, *time.Time) {
	t.Helper()
	now := time.Unix(1700000000, 0)
	root := &rootContext{now: func() time.Time { return now }}
	if prepare != nil {
		prepare(root)
	}
	host, reset := proxytest.NewHostEmulator(proxytest.NewEmulatorOption().
		WithProperty([]string{"cluster_name"}, []byte("outbound|8000||inference.ai.svc.cluster.local")).WithVMContext(&renewalTestVM{root: root}).WithPluginConfiguration([]byte(renewalTestConfig)))
	t.Cleanup(reset)
	require.Equal(t, types.OnPluginStartStatusOK, host.StartPlugin())
	return root, host, &now
}

func beginRenewalRequest(t *testing.T, host proxytest.HostEmulator) uint32 {
	t.Helper()
	id := host.InitializeHttpContext()
	require.Equal(t, types.ActionPause, host.CallOnRequestHeaders(id, [][2]string{
		{":authority", "api.example.com"}, {"authorization", "Bearer key001"},
	}, false))
	return id
}

func acquireTestLease(t *testing.T, host proxytest.HostEmulator) uint32 {
	t.Helper()
	id := beginRenewalRequest(t, host)
	callouts := host.GetCalloutAttributesFromContext(id)
	require.Len(t, callouts, 1)
	host.CallOnHttpCallResponse(callouts[0].CalloutID, [][2]string{{":status", "200"}}, nil,
		[]byte(`{"allowed":true,"lease_id":"lease-1"}`))
	require.Equal(t, types.ActionContinue, host.GetCurrentHttpStreamAction(id))
	return id
}

func TestRenewalRunsOncePerIntervalAndStopsWhenStreamFinishes(t *testing.T) {
	root, host, now := newRenewalHost(t, nil)
	id := acquireTestLease(t, host)
	require.Equal(t, uint32(1000), host.GetTickPeriod())
	host.Tick()
	require.Empty(t, host.GetCalloutAttributesFromContext(proxytest.PluginContextID))
	*now = now.Add(time.Second)
	host.Tick()
	callouts := host.GetCalloutAttributesFromContext(proxytest.PluginContextID)
	require.Len(t, callouts, 1)
	var req models.RenewRequest
	require.NoError(t, json.Unmarshal(callouts[0].Body, &req))
	require.Equal(t, models.RenewRequest{LeaseID: "lease-1", TTLMS: 3000}, req)
	*now = now.Add(time.Second)
	host.Tick()
	require.Len(t, host.GetCalloutAttributesFromContext(proxytest.PluginContextID), 1, "pending renewal must not be duplicated")
	host.CompleteHttpContext(id)
	require.Empty(t, root.activeLeases)
	host.CallOnHttpCallResponse(callouts[0].CalloutID, [][2]string{{":status", "200"}}, nil, []byte(`{"renewed":true}`))
	*now = now.Add(time.Second)
	host.Tick()
	require.Empty(t, root.activeLeases, "late callback must not restore a completed stream")
	require.Len(t, host.GetCalloutAttributesFromContext(proxytest.PluginContextID), 1)
	require.Len(t, host.GetCalloutAttributesFromContext(id), 2, "stream completion must send release")
}

func TestRenewalFailuresKeepRequestOpenAndRetryUntilDeadline(t *testing.T) {
	for _, response := range []struct{ status, body string }{
		{"503", `{"renewed":false}`},
		{"", ""},
		{"200", "{"},
		{"200", `{}`},
		{"200", `{"renewed":false,"reason":"internal_error"}`},
	} {
		t.Run(response.status+response.body, func(t *testing.T) {
			root, host, now := newRenewalHost(t, nil)
			id := acquireTestLease(t, host)
			*now = now.Add(time.Second)
			host.Tick()
			callouts := host.GetCalloutAttributesFromContext(proxytest.PluginContextID)
			host.CallOnHttpCallResponse(callouts[0].CalloutID, [][2]string{{":status", response.status}}, nil, []byte(response.body))
			*now = now.Add(time.Second)
			host.Tick()
			require.Len(t, host.GetCalloutAttributesFromContext(proxytest.PluginContextID), 2)
			*now = now.Add(time.Second)
			host.Tick()
			require.Empty(t, root.activeLeases)
			require.Nil(t, host.GetSentLocalResponse(id))
			require.Equal(t, types.ActionContinue, host.GetCurrentHttpStreamAction(id))
		})
	}
}

func TestRenewalLostLeaseIsNotReacquired(t *testing.T) {
	root, host, now := newRenewalHost(t, nil)
	id := acquireTestLease(t, host)
	*now = now.Add(time.Second)
	host.Tick()
	callouts := host.GetCalloutAttributesFromContext(proxytest.PluginContextID)
	host.CallOnHttpCallResponse(callouts[0].CalloutID, [][2]string{{":status", "200"}}, nil,
		[]byte(`{"renewed":false,"reason":"lease_not_found"}`))
	*now = now.Add(time.Second)
	host.Tick()
	require.Empty(t, root.activeLeases)
	require.Len(t, host.GetCalloutAttributesFromContext(id), 1)
	require.Len(t, host.GetCalloutAttributesFromContext(proxytest.PluginContextID), 1)
	require.Nil(t, host.GetSentLocalResponse(id))
}

func TestRenewalUsesAcquiredConfigAfterConfigUpdate(t *testing.T) {
	root, host, now := newRenewalHost(t, nil)
	id := acquireTestLease(t, host)
	updated := strings.ReplaceAll(renewalTestConfig, "counter-service\n", "new-counter-service\n")
	updated = strings.ReplaceAll(updated, "lease_ttl_ms: 3000", "lease_ttl_ms: 6000\n    renew_path: /new-renew\n    release_path: /new-release")
	require.NoError(t, root.LoadConfiguration([]byte(updated)))
	*now = now.Add(time.Second)
	host.Tick()
	callouts := host.GetCalloutAttributesFromContext(proxytest.PluginContextID)
	require.Len(t, callouts, 1)
	require.Equal(t, "counter-service", callouts[0].Upstream)
	require.Contains(t, callouts[0].Headers, [2]string{":path", "/renew"})
	require.Contains(t, string(callouts[0].Body), `"ttl_ms":3000`)
	host.CompleteHttpContext(id)
	release := host.GetCalloutAttributesFromContext(id)[1]
	require.Equal(t, "counter-service", release.Upstream)
	require.Contains(t, release.Headers, [2]string{":path", "/release"})
}

func TestDelayedAcquireReschedulesRenewalAfterConfigUpdate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		config     string
		tickPeriod uint32
	}{
		{"longer TTL", renewalTestConfig, 100},
		{"distributed disabled", "distributed_limit:\n  enabled: false\n", 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, host, now := newRenewalHost(t, nil)
			oldConfig := strings.ReplaceAll(renewalTestConfig, "lease_ttl_ms: 3000", "lease_ttl_ms: 300")
			require.NoError(t, root.LoadConfiguration([]byte(oldConfig)))
			id := beginRenewalRequest(t, host)
			acquire := host.GetCalloutAttributesFromContext(id)[0]
			require.Empty(t, root.activeLeases)

			*now = now.Add(25 * time.Millisecond)
			require.NoError(t, root.LoadConfiguration([]byte(tc.config)))
			require.Equal(t, tc.tickPeriod, host.GetTickPeriod())
			host.CallOnHttpCallResponse(acquire.CalloutID, [][2]string{{":status", "200"}}, nil,
				[]byte(`{"allowed":true,"lease_id":"delayed-lease"}`))
			require.Equal(t, uint32(100), host.GetTickPeriod(), "the delayed lease must restore its shorter renewal timer")

			// Advance by the actual timer period, beyond the original 300ms TTL.
			for i := 0; i < 3; i++ {
				*now = now.Add(time.Duration(host.GetTickPeriod()) * time.Millisecond)
				host.Tick()
				calls := host.GetCalloutAttributesFromContext(proxytest.PluginContextID)
				require.Len(t, calls, i+1)
				var req models.RenewRequest
				require.NoError(t, json.Unmarshal(calls[i].Body, &req))
				require.Equal(t, models.RenewRequest{LeaseID: "delayed-lease", TTLMS: 300}, req)
				host.CallOnHttpCallResponse(calls[i].CalloutID, [][2]string{{":status", "200"}}, nil,
					[]byte(`{"renewed":true}`))
			}
			require.Len(t, root.activeLeases, 1)
			require.Nil(t, host.GetSentLocalResponse(id))
			require.Equal(t, types.ActionContinue, host.GetCurrentHttpStreamAction(id))
			host.CompleteHttpContext(id)
			require.Empty(t, root.activeLeases)
		})
	}
}

func TestAcquireDispatchFailureFailsOpen(t *testing.T) {
	root, host, _ := newRenewalHost(t, func(root *rootContext) {
		root.dispatchHTTPCall = func(string, [][2]string, []byte, [][2]string, uint32, func(int, int, int)) (uint32, error) {
			return 0, errors.New("unknown cluster")
		}
	})
	id := host.InitializeHttpContext()
	require.Equal(t, types.ActionContinue, host.CallOnRequestHeaders(id, [][2]string{
		{":authority", "api.example.com"}, {"authorization", "Bearer key001"},
	}, false))
	require.Nil(t, host.GetSentLocalResponse(id))
	host.CompleteHttpContext(id)
	require.Empty(t, root.activeLeases)
	require.Empty(t, host.GetCalloutAttributesFromContext(id))
}

func TestRenewalTimerFailureFailsOpenWithoutAcquiring(t *testing.T) {
	root, host, _ := newRenewalHost(t, func(root *rootContext) {
		root.setTickPeriod = func(uint32) error { return errors.New("timer unavailable") }
	})
	id := host.InitializeHttpContext()
	require.Equal(t, types.ActionContinue, host.CallOnRequestHeaders(id, [][2]string{
		{":authority", "api.example.com"}, {"authorization", "Bearer key001"},
	}, false))
	require.Nil(t, host.GetSentLocalResponse(id))
	require.Empty(t, root.activeLeases)
	require.Empty(t, host.GetCalloutAttributesFromContext(id))
}

func TestRenewalDispatchFailureRetriesAndBoundsTimeout(t *testing.T) {
	root, host, now := newRenewalHost(t, nil)
	id := acquireTestLease(t, host)
	var timeouts []uint32
	root.dispatchHTTPCall = func(_ string, _ [][2]string, _ []byte, _ [][2]string, timeout uint32, _ func(int, int, int)) (uint32, error) {
		timeouts = append(timeouts, timeout)
		return 0, errors.New("dispatch failed")
	}
	*now = now.Add(time.Second)
	host.Tick()
	*now = now.Add(1750 * time.Millisecond)
	host.Tick()
	require.Equal(t, []uint32{1000, 250}, timeouts)
	require.Nil(t, host.GetSentLocalResponse(id))
	*now = now.Add(time.Second)
	host.Tick()
	require.Empty(t, root.activeLeases)
}

func TestLongRequestAcrossMultipleTTLsThroughCounterService(t *testing.T) {
	t.Run("key rule", func(t *testing.T) { testLongRequestThroughCounterService(t, false) })
	t.Run("service default rule without credentials", func(t *testing.T) { testLongRequestThroughCounterService(t, true) })
}

func testLongRequestThroughCounterService(t *testing.T, defaultOnly bool) {
	_, host, now := newRenewalHost(t, nil)
	mr := miniredis.RunT(t)
	mr.SetTime(*now)
	client, err := redis.NewClient(redis.Config{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	apiKey := "key001"
	if defaultOnly {
		apiKey = ""
	}
	require.NoError(t, client.SetConfig(context.Background(), models.RateLimitConfig{
		Service: "ai/inference", Port: 8000, APIKey: apiKey, MaxConcurrent: 1, Enabled: true,
	}))
	mux := http.NewServeMux()
	mux.Handle("/acquire", handler.NewAcquireHandler(client))
	mux.Handle("/renew", handler.NewRenewHandler(client))
	mux.Handle("/release", handler.NewReleaseHandler(client))
	respond := func(call proxytest.HttpCalloutAttribute) {
		path := ""
		for _, header := range call.Headers {
			if header[0] == ":path" {
				path = header[1]
			}
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(call.Body)))
		host.CallOnHttpCallResponse(call.CalloutID, [][2]string{{":status", strconv.Itoa(w.Code)}}, nil, w.Body.Bytes())
	}
	first := host.InitializeHttpContext()
	headers := [][2]string{{":authority", "api.example.com"}}
	if !defaultOnly {
		headers = append(headers, [2]string{"authorization", "Bearer key001"})
	}
	require.Equal(t, types.ActionPause, host.CallOnRequestHeaders(first, headers, false))
	respond(host.GetCalloutAttributesFromContext(first)[0])
	require.Equal(t, types.ActionContinue, host.GetCurrentHttpStreamAction(first))
	for i := 0; i < 10; i++ {
		*now = now.Add(time.Second)
		mr.SetTime(*now)
		mr.FastForward(time.Second)
		host.Tick()
		calls := host.GetCalloutAttributesFromContext(proxytest.PluginContextID)
		require.Len(t, calls, i+1)
		respond(calls[i])
		second := beginRenewalRequest(t, host)
		respond(host.GetCalloutAttributesFromContext(second)[0])
		response := host.GetSentLocalResponse(second)
		require.NotNil(t, response, "second request must remain denied beyond the original TTL")
		require.Equal(t, uint32(429), response.StatusCode)
		host.CompleteHttpContext(second)
	}
	host.CompleteHttpContext(first)
	respond(host.GetCalloutAttributesFromContext(first)[1])
	third := beginRenewalRequest(t, host)
	respond(host.GetCalloutAttributesFromContext(third)[0])
	require.Nil(t, host.GetSentLocalResponse(third))
	require.Equal(t, types.ActionContinue, host.GetCurrentHttpStreamAction(third))
}

func TestMissingCredentialsFailOpenOnDispatchOrTimerFailure(t *testing.T) {
	for _, failure := range []string{"dispatch", "timer"} {
		t.Run(failure, func(t *testing.T) {
			root, host, _ := newRenewalHost(t, func(root *rootContext) {
				if failure == "timer" {
					root.setTickPeriod = func(uint32) error { return errors.New("timer unavailable") }
				} else {
					root.dispatchHTTPCall = func(string, [][2]string, []byte, [][2]string, uint32, func(int, int, int)) (uint32, error) {
						return 0, errors.New("dispatch unavailable")
					}
				}
			})
			id := host.InitializeHttpContext()
			require.Equal(t, types.ActionContinue, host.CallOnRequestHeaders(id, [][2]string{{":authority", "api.example.com"}}, false))
			require.Nil(t, host.GetSentLocalResponse(id))
			host.CompleteHttpContext(id)
			require.Empty(t, root.activeLeases)
			require.Empty(t, host.GetCalloutAttributesFromContext(id))
		})
	}
}
