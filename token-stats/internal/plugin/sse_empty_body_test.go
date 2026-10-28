package plugin_test

import (
	"testing"

	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

func TestTokenStats_SSEEmptyBodyCallbacks(t *testing.T) {
	type chunk struct {
		body string
		end  bool
	}
	for _, tc := range []struct {
		name               string
		chunks             []chunk
		prompt, completion uint64
		parseErrors        uint64
	}{
		{name: "empty stream", chunks: []chunk{{end: true}}},
		{
			name: "fragmented usage with empty intermediate and final callbacks",
			chunks: []chunk{
				{body: "data: {\"usage\":{\"prompt_tokens\":"},
				{},
				{body: "3,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n"},
				{end: true},
			},
			prompt: 3, completion: 4,
		},
		{
			name: "invalid frame counted once before empty final callback",
			chunks: []chunk{
				{body: "data: {not json}\n\n"},
				{body: "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n"},
				{end: true},
			},
			prompt: 3, completion: 4, parseErrors: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, reset := newHTTPHost(t)
			defer reset()
			id := host.InitializeHttpContext()
			host.CallOnRequestHeaders(id, [][2]string{
				{":authority", "api.example.com"},
				{"authorization", "Bearer " + mustJWTWithUID("empty_sse")},
			}, false)
			host.CallOnResponseHeaders(id, [][2]string{{"content-type", "text/event-stream"}}, false)
			for _, part := range tc.chunks {
				if action := host.CallOnResponseBody(id, []byte(part.body), part.end); action != types.ActionContinue {
					t.Fatalf("SSE 回调不应阻塞转发: %v", action)
				}
			}
			host.CompleteHttpContext(id)
			for name, want := range map[string]uint64{
				"prompt_tokens_total": tc.prompt, "completion_tokens_total": tc.completion,
				"stream_parse_errors_total": tc.parseErrors,
			} {
				got, err := host.GetCounterMetric("llm." + name + "domain=.=api.example.com;.;uid=.=empty_sse;.;")
				if want > 0 && err != nil {
					t.Fatalf("读取指标 %s: %v", name, err)
				}
				if got != want {
					t.Errorf("%s = %d，期望 %d", name, got, want)
				}
			}
		})
	}
}
