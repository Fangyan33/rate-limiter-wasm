package plugin

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/proxytest"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

func TestInvalidPluginConfigurationStartsInBypassMode(t *testing.T) {
	for _, cfg := range []string{"", "{", "domains: []"} {
		t.Run(cfg, func(t *testing.T) {
			host, reset := proxytest.NewHostEmulator(proxytest.NewEmulatorOption().
				WithVMContext(NewVMContext()).WithPluginConfiguration([]byte(cfg)))
			defer reset()
			require.Equal(t, types.OnPluginStartStatusOK, host.StartPlugin())
			id := host.InitializeHttpContext()
			require.Equal(t, types.ActionContinue, host.CallOnRequestHeaders(id, [][2]string{{":authority", "api.example.com"}}, false))
			require.Nil(t, host.GetSentLocalResponse(id))
		})
	}
}

func TestInvalidConfigurationDisablesPreviousRules(t *testing.T) {
	root, host, _ := newRenewalHost(t, nil)
	require.Error(t, root.LoadConfiguration([]byte("domains: []")))
	id := host.InitializeHttpContext()
	require.Equal(t, types.ActionContinue, host.CallOnRequestHeaders(id, [][2]string{{":authority", "api.example.com"}}, false))
	require.Empty(t, host.GetCalloutAttributesFromContext(id))
	require.Nil(t, host.GetSentLocalResponse(id))
}

func TestAcquireOnlyRejectsExplicitPolicyDecisions(t *testing.T) {
	for _, body := range []string{`{"allowed":true}`, "", "{", "{}", "null", `{"allowed":null}`, `{"allowed":"false"}`,
		`{"error":"backend failed"}`, `{"allowed":false}`, `{"allowed":false,"reason":"internal_error"}`,
		`{"allowed":false,"reason":"invalid_config"}`, `{"allowed":false,"reason":"redis_unavailable"}`,
		`{"allowed":false,"reason":"unknown"}`, `{"allowed":false,"reason":"api_key_disabled"}`, `{"allowed":false,"reason":"config_not_found"}`, strings.Repeat("x", 64*1024+1)} {
		t.Run(body[:min(len(body), 80)], func(t *testing.T) {
			root, host, _ := newRenewalHost(t, nil)
			id := beginRenewalRequest(t, host)
			call := host.GetCalloutAttributesFromContext(id)[0]
			host.CallOnHttpCallResponse(call.CalloutID, [][2]string{{":status", "200"}}, nil, []byte(body))
			require.Equal(t, types.ActionContinue, host.GetCurrentHttpStreamAction(id))
			require.Nil(t, host.GetSentLocalResponse(id))
			require.Empty(t, root.pendingAcquires)
			require.Empty(t, root.activeLeases)
		})
	}
	for _, reason := range []string{"limit_exceeded"} {
		t.Run(reason, func(t *testing.T) {
			_, host, _ := newRenewalHost(t, nil)
			id := beginRenewalRequest(t, host)
			call := host.GetCalloutAttributesFromContext(id)[0]
			host.CallOnHttpCallResponse(call.CalloutID, [][2]string{{":status", "200"}}, nil,
				[]byte(`{"allowed":false,"reason":"`+reason+`"}`))
			require.NotNil(t, host.GetSentLocalResponse(id))
			require.Equal(t, uint32(429), host.GetSentLocalResponse(id).StatusCode)
		})
	}
}

func TestFailedAsyncLocalResponseResumesRequest(t *testing.T) {
	for _, shouldPanic := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "panic"}[shouldPanic], func(t *testing.T) {
			root, host, _ := newRenewalHost(t, func(root *rootContext) {
				root.sendResponse = func(uint32, [][2]string, []byte, int32) error {
					if shouldPanic {
						panic("host response failed")
					}
					return errors.New("host response failed")
				}
			})
			id := beginRenewalRequest(t, host)
			call := host.GetCalloutAttributesFromContext(id)[0]
			host.CallOnHttpCallResponse(call.CalloutID, [][2]string{{":status", "200"}}, nil,
				[]byte(`{"allowed":false,"reason":"limit_exceeded"}`))
			require.Equal(t, types.ActionContinue, host.GetCurrentHttpStreamAction(id))
			require.Nil(t, host.GetSentLocalResponse(id))
			require.Empty(t, root.pendingAcquires)
		})
	}
}

func TestAcquireWatchdogResumesMissingCallbackAndIgnoresLateDenial(t *testing.T) {
	root, host, now := newRenewalHost(t, nil)
	id := beginRenewalRequest(t, host)
	call := host.GetCalloutAttributesFromContext(id)[0]
	*now = now.Add(5 * time.Second)
	host.Tick()
	require.Equal(t, types.ActionContinue, host.GetCurrentHttpStreamAction(id))
	require.Empty(t, root.pendingAcquires)
	host.CallOnHttpCallResponse(call.CalloutID, [][2]string{{":status", "200"}}, nil,
		[]byte(`{"allowed":false,"reason":"limit_exceeded"}`))
	require.Nil(t, host.GetSentLocalResponse(id))
	require.Empty(t, root.activeLeases)
}

func TestAcquireWatchdogRetriesFailedResume(t *testing.T) {
	root, host, now := newRenewalHost(t, nil)
	resumes := 0
	root.resumeRequest = func() error {
		resumes++
		if resumes == 1 {
			return errors.New("temporary host failure")
		}
		return proxywasm.ResumeHttpRequest()
	}
	id := beginRenewalRequest(t, host)
	call := host.GetCalloutAttributesFromContext(id)[0]
	host.CallOnHttpCallResponse(call.CalloutID, [][2]string{{":status", "503"}}, nil, nil)
	require.Len(t, root.pendingAcquires, 1)
	*now = now.Add(5 * time.Second)
	host.Tick()
	require.Equal(t, types.ActionContinue, host.GetCurrentHttpStreamAction(id))
	require.Empty(t, root.pendingAcquires)
}

func TestCompletedStreamIsRemovedFromAcquireWatchdog(t *testing.T) {
	root, host, now := newRenewalHost(t, nil)
	id := beginRenewalRequest(t, host)
	host.CompleteHttpContext(id)
	*now = now.Add(5 * time.Second)
	require.NotPanics(t, host.Tick)
	require.Empty(t, root.pendingAcquires)
}

func TestInitializationPanicDisablesPluginWithoutFailingStartup(t *testing.T) {
	root, host, _ := newRenewalHost(t, func(root *rootContext) {
		root.setTickPeriod = func(uint32) error { panic("timer hostcall failed") }
	})
	require.False(t, root.asyncDistributed)
	id := host.InitializeHttpContext()
	require.Equal(t, types.ActionContinue, host.CallOnRequestHeaders(id, [][2]string{{":authority", "api.example.com"}}, false))
	require.Nil(t, host.GetSentLocalResponse(id))
}
