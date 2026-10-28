package plugin_test

import (
	"bytes"
	"testing"

	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

func TestInvalidPluginConfigStartsInBypassMode(t *testing.T) {
	for _, cfg := range []string{"", "{", "domains: []", "domains: [api.example.com]\ntoken_statistics:\n  metric_key_limit: -1"} {
		t.Run(cfg, func(t *testing.T) {
			host, reset := newHTTPHostWithConfig(t, []byte(cfg))
			defer reset()
			id := host.InitializeHttpContext()
			if action := host.CallOnRequestHeaders(id, [][2]string{{":authority", "api.example.com"}}, false); action != types.ActionContinue {
				t.Fatalf("invalid config interrupted request: %v", action)
			}
			if response := host.GetSentLocalResponse(id); response != nil {
				t.Fatalf("invalid config generated response: %+v", response)
			}
		})
	}
}

func TestLargeOrMalformedJSONResponseNeverPausesBusinessStream(t *testing.T) {
	for _, chunks := range [][][]byte{
		{[]byte(`{"usage":`), []byte(`bad}`)},
		{bytes.Repeat([]byte("x"), 600*1024), bytes.Repeat([]byte("y"), 600*1024)},
		{bytes.Repeat([]byte("z"), 2*1024*1024)},
	} {
		host, reset := newHTTPHost(t)
		id := host.InitializeHttpContext()
		host.CallOnRequestHeaders(id, [][2]string{{":authority", "api.example.com"},
			{"authorization", "Bearer " + mustJWTWithUID("test")}}, false)
		host.CallOnResponseHeaders(id, [][2]string{{":status", "200"}, {"content-type", "application/json"}}, false)
		for i, chunk := range chunks {
			if action := host.CallOnResponseBody(id, chunk, i == len(chunks)-1); action != types.ActionContinue {
				t.Fatalf("statistics paused response chunk %d: %v", i, action)
			}
		}
		host.CompleteHttpContext(id)
		reset()
	}
}
