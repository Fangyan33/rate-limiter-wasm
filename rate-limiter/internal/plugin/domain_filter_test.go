package plugin

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/proxytest"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

const filteredTestConfig = "domains: ['api.example.com', '*.example.org']\n" + renewalTestConfig

func TestDomainFilterBeforeAcquire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers [][2]string
		match   bool
	}{
		{"exact", [][2]string{{":authority", "api.example.com"}}, true},
		{"normalized", [][2]string{{":authority", "API.EXAMPLE.COM.:443"}}, true},
		{"wildcard", [][2]string{{":authority", "chat.example.org"}}, true},
		{"nested", [][2]string{{":authority", "a.b.example.org:80"}}, true},
		{"host fallback", [][2]string{{"host", "api.example.com"}}, true},
		{"empty authority fallback", [][2]string{{":authority", ""}, {"host", "api.example.com"}}, true},
		{"authority wins", [][2]string{{":authority", "other.test"}, {"host", "api.example.com"}}, false},
		{"root excluded", [][2]string{{":authority", "example.org"}}, false},
		{"suffix boundary", [][2]string{{":authority", "badexample.org"}}, false},
		{"suffix attack", [][2]string{{":authority", "chat.example.org.attacker.test"}}, false},
		{"unmatched", [][2]string{{":authority", "other.test"}}, false},
		{"missing", nil, false},
		{"invalid", [][2]string{{":authority", "api.example.com:bad"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, host, now := newRenewalHost(t, nil)
			require.NoError(t, root.LoadConfiguration([]byte(filteredTestConfig)))
			id := host.InitializeHttpContext()
			action := host.CallOnRequestHeaders(id, tc.headers, false)
			if tc.match {
				require.Equal(t, types.ActionPause, action)
				calls := host.GetCalloutAttributesFromContext(id)
				require.Len(t, calls, 1)
				var body map[string]any
				require.NoError(t, json.Unmarshal(calls[0].Body, &body))
				require.Equal(t, map[string]any{"upstream_cluster": "outbound|8000||inference.ai.svc.cluster.local", "api_key": "", "ttl_ms": float64(3000)}, body)
				return
			}
			require.Equal(t, types.ActionContinue, action)
			require.Empty(t, root.pendingAcquires)
			host.CompleteHttpContext(id)
			*now = now.Add(time.Second)
			host.Tick()
			require.Empty(t, root.activeLeases)
			require.Empty(t, host.GetCalloutAttributesFromContext(id))
			require.Empty(t, host.GetCalloutAttributesFromContext(proxytest.PluginContextID))
			require.Nil(t, host.GetSentLocalResponse(id))
		})
	}
}

func TestRemovingDomainFilterRestoresCallsWithoutHost(t *testing.T) {
	for _, domains := range []string{"", "domains: []\n"} {
		t.Run(domains, func(t *testing.T) {
			root, host, _ := newRenewalHost(t, nil)
			require.NoError(t, root.LoadConfiguration([]byte(filteredTestConfig)))
			require.NoError(t, root.LoadConfiguration([]byte(domains+renewalTestConfig)))
			id := host.InitializeHttpContext()
			require.Equal(t, types.ActionPause, host.CallOnRequestHeaders(id, nil, false))
			require.Len(t, host.GetCalloutAttributesFromContext(id), 1)
		})
	}
}

func TestDomainChangeKeepsExistingLease(t *testing.T) {
	root, host, now := newRenewalHost(t, nil)
	require.NoError(t, root.LoadConfiguration([]byte(filteredTestConfig)))
	id := acquireTestLease(t, host)
	require.NoError(t, root.LoadConfiguration([]byte("domains: ['other.test']\n"+renewalTestConfig)))
	bypass := host.InitializeHttpContext()
	require.Equal(t, types.ActionContinue, host.CallOnRequestHeaders(bypass, [][2]string{{":authority", "api.example.com"}}, false))
	host.CompleteHttpContext(bypass)
	*now = now.Add(time.Second)
	host.Tick()
	calls := host.GetCalloutAttributesFromContext(proxytest.PluginContextID)
	require.Len(t, calls, 1)
	require.JSONEq(t, `{"lease_id":"lease-1","ttl_ms":3000}`, string(calls[0].Body))
	host.CompleteHttpContext(id)
	require.Empty(t, root.activeLeases)
	require.Len(t, host.GetCalloutAttributesFromContext(id), 2)
	require.Empty(t, host.GetCalloutAttributesFromContext(bypass))
}

func TestFilteredAcquirePolicyAndFailure(t *testing.T) {
	for _, tc := range []struct {
		status, body string
		deny         bool
	}{
		{"200", `{"allowed":false,"reason":"limit_exceeded"}`, true},
		{"503", `{"allowed":false,"reason":"limit_exceeded"}`, false},
		{"200", `{"allowed":false,"reason":"internal_error"}`, false},
		{"200", `{}`, false}, {"200", `broken`, false},
	} {
		t.Run(tc.status+tc.body, func(t *testing.T) {
			root, host, _ := newRenewalHost(t, nil)
			require.NoError(t, root.LoadConfiguration([]byte(filteredTestConfig)))
			id := beginRenewalRequest(t, host)
			call := host.GetCalloutAttributesFromContext(id)[0]
			host.CallOnHttpCallResponse(call.CalloutID, [][2]string{{":status", tc.status}}, nil, []byte(tc.body))
			if tc.deny {
				require.NotNil(t, host.GetSentLocalResponse(id))
				require.Equal(t, uint32(429), host.GetSentLocalResponse(id).StatusCode)
			} else {
				require.Nil(t, host.GetSentLocalResponse(id))
				require.Equal(t, types.ActionContinue, host.GetCurrentHttpStreamAction(id))
			}
		})
	}
}
