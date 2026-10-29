package plugin_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

func domainHeaders(domain, authorization string) [][2]string {
	headers := [][2]string{{":authority", domain}}
	if authorization != "<missing>" {
		headers = append(headers, [2]string{"authorization", authorization})
	}
	return headers
}

func TestLocalDomainRuleHandlesAllCredentials(t *testing.T) {
	for _, authorization := range []string{"<missing>", "", "Bearer", "Bearer ", "Basic abc", "Bearer bad token", "Bearer known", "Bearer unknown"} {
		t.Run(authorization, func(t *testing.T) {
			host, reset := newHTTPHostWithConfig(t, []byte(`domains: ["*.example.com"]
rate_limits:
  - api_key: ""
    max_concurrent: 1
`))
			defer reset()
			first := host.InitializeHttpContext()
			require.Equal(t, types.ActionContinue, host.CallOnRequestHeaders(first, domainHeaders("API.EXAMPLE.COM:443", authorization), false))
			require.Nil(t, host.GetSentLocalResponse(first))
			second := host.InitializeHttpContext()
			action := host.CallOnRequestHeaders(second, domainHeaders("api.example.com", "Bearer known"), false)
			assertRejected(t, host, second, action, 429, "Rate limit exceeded")
			other := host.InitializeHttpContext()
			require.Equal(t, types.ActionContinue, host.CallOnRequestHeaders(other, domainHeaders("other.example.com", authorization), false))
			require.Nil(t, host.GetSentLocalResponse(other))
			host.CompleteHttpContext(second)
			host.CompleteHttpContext(first)
			third := host.InitializeHttpContext()
			require.Equal(t, types.ActionContinue, host.CallOnRequestHeaders(third, domainHeaders("api.example.com", "<missing>"), false))
			require.Nil(t, host.GetSentLocalResponse(third))
			host.CompleteHttpContext(third)
			host.CompleteHttpContext(other)
		})
	}
}

func TestLocalKeyRulesAllowUnmatchedCredentials(t *testing.T) {
	for _, authorization := range []string{"<missing>", "", "Bearer", "Bearer ", "Basic abc", "Bearer bad token"} {
		t.Run(authorization, func(t *testing.T) {
			host, reset := newHTTPHost(t)
			defer reset()
			id := host.InitializeHttpContext()
			action := host.CallOnRequestHeaders(id, domainHeaders("api.example.com", authorization), false)
			require.Equal(t, types.ActionContinue, action)
			require.Nil(t, host.GetSentLocalResponse(id))
		})
	}
}

func TestDistributedDomainRequestAndDecision(t *testing.T) {
	for _, authorization := range []string{"<missing>", "Bearer", "Basic abc", "Bearer bad token", "Bearer known"} {
		for _, decision := range []struct {
			name, status, body string
			reject, release    bool
		}{
			{"allowed", "200", `{"allowed":true,"lease_id":"domain-lease"}`, false, true},
			{"no rule", "200", `{"allowed":true}`, false, false},
			{"over limit", "200", `{"allowed":false,"reason":"limit_exceeded"}`, true, false},
			{"service unavailable", "503", `{}`, false, false},
			{"invalid response", "200", `{`, false, false},
		} {
			t.Run(authorization+"/"+decision.name, func(t *testing.T) {
				host, reset := newHTTPHostWithConfig(t, []byte(`domains: [api.example.com]
distributed_limit:
  enabled: true
  backend: counter_service
  counter_service:
    cluster: counter-service
`))
				defer reset()
				id := host.InitializeHttpContext()
				require.Equal(t, types.ActionPause, host.CallOnRequestHeaders(id, domainHeaders("API.EXAMPLE.COM:443", authorization), false))
				require.Nil(t, host.GetSentLocalResponse(id))
				calls := host.GetCalloutAttributesFromContext(id)
				require.Len(t, calls, 1)
				var req struct {
					Domain string `json:"domain"`
					APIKey string `json:"api_key"`
				}
				require.NoError(t, json.Unmarshal(calls[0].Body, &req))
				require.Equal(t, "api.example.com", req.Domain)
				wantKey := ""
				if authorization == "Bearer known" {
					wantKey = "known"
				}
				require.Equal(t, wantKey, req.APIKey)
				host.CallOnHttpCallResponse(calls[0].CalloutID, [][2]string{{":status", decision.status}}, nil, []byte(decision.body))
				if decision.reject {
					assertRejected(t, host, id, host.GetCurrentHttpStreamAction(id), 429, "Rate limit exceeded")
				} else {
					require.Equal(t, types.ActionContinue, host.GetCurrentHttpStreamAction(id))
					require.Nil(t, host.GetSentLocalResponse(id))
				}
				host.CompleteHttpContext(id)
				calls = host.GetCalloutAttributesFromContext(id)
				if decision.release {
					require.Len(t, calls, 2)
					var release struct {
						APIKey  string `json:"api_key"`
						LeaseID string `json:"lease_id"`
					}
					require.NoError(t, json.Unmarshal(calls[1].Body, &release))
					require.Equal(t, wantKey, release.APIKey)
					require.Equal(t, "domain-lease", release.LeaseID)
				} else {
					require.Len(t, calls, 1)
				}
			})
		}
	}
}
