package plugin

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/proxytest"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
	"strings"
	"testing"
)

func TestRouteTargetPassedUnmodifiedAcrossAuthorities(t *testing.T) {
	for _, auth := range []string{"", "malformed", "Bearer known"} {
		t.Run(auth, func(t *testing.T) {
			_, host, _ := newRenewalHost(t, nil)
			for _, authority := range []string{"first.example.com", "second.example.com"} {
				id := host.InitializeHttpContext()
				require.Equal(t, types.ActionPause, host.CallOnRequestHeaders(id, [][2]string{{":authority", authority}, {"authorization", auth}}, false))
				calls := host.GetCalloutAttributesFromContext(id)
				require.Len(t, calls, 1)
				var req models.AcquireRequest
				require.NoError(t, json.Unmarshal(calls[0].Body, &req))
				require.Equal(t, "outbound|8000||inference.ai.svc.cluster.local", req.UpstreamCluster)
				if auth == "Bearer known" {
					require.Equal(t, "known", req.APIKey)
				} else {
					require.Empty(t, req.APIKey)
				}
				var fields map[string]any
				require.NoError(t, json.Unmarshal(calls[0].Body, &fields))
				require.Len(t, fields, 3)
				require.NotContains(t, fields, "domain")
				require.NotContains(t, fields, "service")
				require.NotContains(t, fields, "port")
			}
		})
	}
}
func TestMissingOversizedAndDisabledTargetsPassThrough(t *testing.T) {
	for _, cluster := range []string{"", strings.Repeat("x", 1025)} {
		t.Run(cluster[:min(len(cluster), 10)], func(t *testing.T) {
			host, reset := proxytest.NewHostEmulator(proxytest.NewEmulatorOption().WithVMContext(NewVMContext()).WithPluginConfiguration([]byte(renewalTestConfig)).WithProperty([]string{"cluster_name"}, []byte(cluster)))
			defer reset()
			require.Equal(t, types.OnPluginStartStatusOK, host.StartPlugin())
			id := host.InitializeHttpContext()
			require.Equal(t, types.ActionContinue, host.CallOnRequestHeaders(id, nil, false))
			require.Empty(t, host.GetCalloutAttributesFromContext(id))
		})
	}
	root, host, _ := newRenewalHost(t, nil)
	require.NoError(t, root.LoadConfiguration([]byte("distributed_limit:\n  enabled: false\n")))
	id := host.InitializeHttpContext()
	require.Equal(t, types.ActionContinue, host.CallOnRequestHeaders(id, nil, false))
	require.Empty(t, host.GetCalloutAttributesFromContext(id))
}
func TestNon200DenialFailsOpen(t *testing.T) {
	for _, status := range []string{"400", "500", "503"} {
		t.Run(status, func(t *testing.T) {
			_, host, _ := newRenewalHost(t, nil)
			id := beginRenewalRequest(t, host)
			call := host.GetCalloutAttributesFromContext(id)[0]
			host.CallOnHttpCallResponse(call.CalloutID, [][2]string{{":status", status}}, nil, []byte(`{"allowed":false,"reason":"limit_exceeded"}`))
			require.Nil(t, host.GetSentLocalResponse(id))
			require.Equal(t, types.ActionContinue, host.GetCurrentHttpStreamAction(id))
		})
	}
}
