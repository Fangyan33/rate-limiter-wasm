package plugin_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

func TestTokenStats_MetricNameFormat(t *testing.T) {
	jwt := mustJWTWithUID("sfe-platform")

	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - llm-svc.domain
rate_limits:
  - api_key: "`+jwt+`"
    max_concurrent: 1

token_statistics:
  enabled: true
  metric_key_limit: 5000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "llm-svc.domain"},
		{"authorization", "Bearer " + jwt},
	}, false)
	if action != types.ActionContinue {
		t.Fatalf("expected request to continue, got %v", action)
	}

	host.CallOnResponseBody(contextID, []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20}}`), true)
	host.CompleteHttpContext(contextID)

	prompt, err := host.GetCounterMetric("llm.prompt_tokens_totaldomain=.=llm-svc.domain;.;uid=.=sfe-platform;.;")
	if err != nil {
		t.Fatalf("GetCounterMetric(prompt): %v", err)
	}
	if prompt != 10 {
		t.Fatalf("unexpected prompt tokens: got %d want %d", prompt, 10)
	}

	completion, err := host.GetCounterMetric("llm.completion_tokens_totaldomain=.=llm-svc.domain;.;uid=.=sfe-platform;.;")
	if err != nil {
		t.Fatalf("GetCounterMetric(completion): %v", err)
	}
	if completion != 20 {
		t.Fatalf("unexpected completion tokens: got %d want %d", completion, 20)
	}
}

func TestTokenStats_MetricsIncrementedForUID(t *testing.T) {
	jwt := mustJWTWithUID("123")

	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
rate_limits:
  - api_key: "`+jwt+`"
    max_concurrent: 1

token_statistics:
  enabled: true
  metric_key_limit: 5000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer " + jwt},
	}, false)
	if action != types.ActionContinue {
		t.Fatalf("expected request to continue, got %v", action)
	}

	// Simulate response with usage.
	host.CallOnResponseBody(contextID, []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20}}`), true)

	// Stream done should flush metrics.
	host.CompleteHttpContext(contextID)

	prompt, err := host.GetCounterMetric("llm.prompt_tokens_totaldomain=.=api.example.com;.;uid=.=123;.;")
	if err != nil {
		t.Fatalf("GetCounterMetric(prompt): %v", err)
	}
	if prompt != 10 {
		t.Fatalf("unexpected prompt tokens: got %d want %d", prompt, 10)
	}

	completion, err := host.GetCounterMetric("llm.completion_tokens_totaldomain=.=api.example.com;.;uid=.=123;.;")
	if err != nil {
		t.Fatalf("GetCounterMetric(completion): %v", err)
	}
	if completion != 20 {
		t.Fatalf("unexpected completion tokens: got %d want %d", completion, 20)
	}
}

func TestTokenStats_MetricNamePreservesSpecialChars(t *testing.T) {
	jwt := mustJWTWithUID("sfe-platform")

	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - llm-svc.example.com
rate_limits:
  - api_key: "`+jwt+`"
    max_concurrent: 1

token_statistics:
  enabled: true
  metric_key_limit: 5000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "llm-svc.example.com"},
		{"authorization", "Bearer " + jwt},
	}, false)
	if action != types.ActionContinue {
		t.Fatalf("expected request to continue, got %v", action)
	}

	host.CallOnResponseBody(contextID, []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20}}`), true)
	host.CompleteHttpContext(contextID)

	prompt, err := host.GetCounterMetric("llm.prompt_tokens_totaldomain=.=llm-svc.example.com;.;uid=.=sfe-platform;.;")
	if err != nil {
		t.Fatalf("GetCounterMetric(prompt): %v", err)
	}
	if prompt != 10 {
		t.Fatalf("unexpected prompt tokens: got %d want %d", prompt, 10)
	}

	completion, err := host.GetCounterMetric("llm.completion_tokens_totaldomain=.=llm-svc.example.com;.;uid=.=sfe-platform;.;")
	if err != nil {
		t.Fatalf("GetCounterMetric(completion): %v", err)
	}
	if completion != 20 {
		t.Fatalf("unexpected completion tokens: got %d want %d", completion, 20)
	}
}

func TestTokenStats_MetricNameDoesNotLeaveTrailingSeparatorForMultiSegmentDomain(t *testing.T) {
	jwt := mustJWTWithUID("sfe-platform")

	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - vllm-083.default.paic.com.cn
rate_limits:
  - api_key: "`+jwt+`"
    max_concurrent: 1

token_statistics:
  enabled: true
  metric_key_limit: 5000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "vllm-083.default.paic.com.cn"},
		{"authorization", "Bearer " + jwt},
	}, false)
	if action != types.ActionContinue {
		t.Fatalf("expected request to continue, got %v", action)
	}

	host.CallOnResponseBody(contextID, []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20}}`), true)
	host.CompleteHttpContext(contextID)

	prompt, err := host.GetCounterMetric("llm.prompt_tokens_totaldomain=.=vllm-083.default.paic.com.cn;.;uid=.=sfe-platform;.;")
	if err != nil {
		t.Fatalf("GetCounterMetric(prompt): %v", err)
	}
	if prompt != 10 {
		t.Fatalf("unexpected prompt tokens: got %d want %d", prompt, 10)
	}
}

func TestTokenStats_MetricNameSupportsOtherDomainShapes(t *testing.T) {
	testCases := []struct {
		name           string
		configDomain   string
		authority      string
		expectedDomain string
	}{
		{
			name:           "带连字符和多级子域",
			configDomain:   "api-gw-01.prod.example.co.uk",
			authority:      "api-gw-01.prod.example.co.uk",
			expectedDomain: "api-gw-01.prod.example.co.uk",
		},
		{
			name:           "单标签主机",
			configDomain:   "model-gateway",
			authority:      "model-gateway",
			expectedDomain: "model-gateway",
		},
		{
			name:           "host带端口会先归一化",
			configDomain:   "api.example.com",
			authority:      "api.example.com:8443",
			expectedDomain: "api.example.com",
		},
		{
			name:           "大写host会先转小写",
			configDomain:   "mixed.case.example.com",
			authority:      "MIXED.CASE.EXAMPLE.COM",
			expectedDomain: "mixed.case.example.com",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			jwt := mustJWTWithUID("sfe-platform")

			host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - `+tc.configDomain+`
rate_limits:
  - api_key: "`+jwt+`"
    max_concurrent: 1

token_statistics:
  enabled: true
  metric_key_limit: 5000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
			defer reset()

			contextID := host.InitializeHttpContext()
			action := host.CallOnRequestHeaders(contextID, [][2]string{
				{":authority", tc.authority},
				{"authorization", "Bearer " + jwt},
			}, false)
			if action != types.ActionContinue {
				t.Fatalf("expected request to continue, got %v", action)
			}

			host.CallOnResponseBody(contextID, []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20}}`), true)
			host.CompleteHttpContext(contextID)

			prompt, err := host.GetCounterMetric("llm.prompt_tokens_totaldomain=.="+tc.expectedDomain+";.;uid=.=sfe-platform;.;")
			if err != nil {
				t.Fatalf("GetCounterMetric(prompt): %v", err)
			}
			if prompt != 10 {
				t.Fatalf("unexpected prompt tokens: got %d want %d", prompt, 10)
			}
		})
	}
}

func TestTokenStats_MetricNamePreservesDotsInUID(t *testing.T) {
	jwt := mustJWTWithUID("user.name")

	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
rate_limits:
  - api_key: "`+jwt+`"
    max_concurrent: 1

token_statistics:
  enabled: true
  metric_key_limit: 5000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer " + jwt},
	}, false)
	if action != types.ActionContinue {
		t.Fatalf("expected request to continue, got %v", action)
	}

	host.CallOnResponseBody(contextID, []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":20}}`), true)
	host.CompleteHttpContext(contextID)

	prompt, err := host.GetCounterMetric("llm.prompt_tokens_totaldomain=.=api.example.com;.;uid=.=user.name;.;")
	if err != nil {
		t.Fatalf("GetCounterMetric(prompt): %v", err)
	}
	if prompt != 10 {
		t.Fatalf("unexpected prompt tokens: got %d want %d", prompt, 10)
	}

	completion, err := host.GetCounterMetric("llm.completion_tokens_totaldomain=.=api.example.com;.;uid=.=user.name;.;")
	if err != nil {
		t.Fatalf("GetCounterMetric(completion): %v", err)
	}
	if completion != 20 {
		t.Fatalf("unexpected completion tokens: got %d want %d", completion, 20)
	}
}

func TestTokenStats_MetricKeyLimitOverflowsToOther(t *testing.T) {
	jwt1 := mustJWTWithUID("u1")
	jwt2 := mustJWTWithUID("u2")
	jwt3 := mustJWTWithUID("u3")

	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
rate_limits:
  - api_key: "`+jwt1+`"
    max_concurrent: 100
  - api_key: "`+jwt2+`"
    max_concurrent: 100
  - api_key: "`+jwt3+`"
    max_concurrent: 100

token_statistics:
  enabled: true
  metric_key_limit: 2
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	for _, jwt := range []string{jwt1, jwt2, jwt3} {
		contextID := host.InitializeHttpContext()
		action := host.CallOnRequestHeaders(contextID, [][2]string{
			{":authority", "api.example.com"},
			{"authorization", "Bearer " + jwt},
		}, false)
		if action != types.ActionContinue {
			t.Fatalf("expected continue for jwt=%s, got %v", jwt, action)
		}
		host.CallOnResponseBody(contextID, []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`), true)
		host.CompleteHttpContext(contextID)
	}

	// First two should have their own series.
	if v, err := host.GetCounterMetric("llm.prompt_tokens_totaldomain=.=api.example.com;.;uid=.=u1;.;"); err != nil || v != 1 {
		t.Fatalf("uid=u1 prompt got (%d,%v)", v, err)
	}
	if v, err := host.GetCounterMetric("llm.prompt_tokens_totaldomain=.=api.example.com;.;uid=.=u2;.;"); err != nil || v != 1 {
		t.Fatalf("uid=u2 prompt got (%d,%v)", v, err)
	}

	// Third should overflow to __other__.
	if v, err := host.GetCounterMetric("llm.prompt_tokens_totaldomain=.=api.example.com;.;uid=.=__other__;.;"); err != nil || v != 1 {
		t.Fatalf("uid=__other__ prompt got (%d,%v)", v, err)
	}
}

func TestTokenStats_DisabledWhenJWTUIDMissing(t *testing.T) {
	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
rate_limits:
  - api_key: abc
    max_concurrent: 1

token_statistics:
  enabled: true
  metric_key_limit: 5000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "api.example.com"},
		// Not a JWT; should still pass limiting (apiKey is "abc"), but stats disabled.
		{"authorization", "Bearer abc"},
	}, false)
	if action != types.ActionContinue {
		t.Fatalf("expected continue, got %v", action)
	}
	if resp := host.GetSentLocalResponse(contextID); resp != nil {
		t.Fatalf("expected no local response, got %#v", resp)
	}

	host.CallOnResponseBody(contextID, []byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`), true)
	host.CompleteHttpContext(contextID)

	// Any token stats metric should not exist.
	if _, err := host.GetCounterMetric("llm.prompt_tokens_totaldomain=.=api.example.com;.;uid=.=__other__;.;"); err == nil {
		t.Fatal("expected no token stats metric when uid parsing fails")
	}
}

func TestTokenStats_InjectStreamUsage_AddsStreamOptionsIncludeUsage(t *testing.T) {
	jwt := mustJWTWithUID("123")

	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
rate_limits:
  - api_key: "`+jwt+`"
    max_concurrent: 1

token_statistics:
  enabled: true
  inject_stream_usage: true
  metric_key_limit: 5000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer " + jwt},
		{"content-type", "application/json"},
		{"content-length", "13"},
	}, false)
	if action != types.ActionContinue {
		t.Fatalf("expected continue, got %v", action)
	}

	// Request body: stream=true, no stream_options.
	host.CallOnRequestBody(contextID, []byte(`{"stream":true}`), true)

	got := host.GetCurrentRequestBody(contextID)
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("unmarshal mutated request body: %v", err)
	}
	so, ok := payload["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("expected stream_options to be object, got %#v", payload["stream_options"])
	}
	if v, ok := so["include_usage"].(bool); !ok || !v {
		t.Fatalf("expected stream_options.include_usage=true, got %#v", so["include_usage"])
	}

	// content-length should have been removed (SDK requirement when body size changes).
	headers := host.GetCurrentRequestHeaders(contextID)
	for _, h := range headers {
		if h[0] == "content-length" {
			t.Fatalf("expected content-length to be removed, got headers=%v", headers)
		}
	}

	// The mutated JSON should be longer than the original, so this also guards that we actually replaced the body.
	if len(got) <= len(`{"stream":true}`) {
		t.Fatalf("expected mutated request body to be larger than original, got len=%d", len(got))
	}
}

func TestTokenStats_InjectStreamUsage_DoesNotChangeWhenAlreadyIncludesUsage(t *testing.T) {
	jwt := mustJWTWithUID("123")

	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
rate_limits:
  - api_key: "`+jwt+`"
    max_concurrent: 1

token_statistics:
  enabled: true
  inject_stream_usage: true
  metric_key_limit: 5000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer " + jwt},
		{"content-type", "application/json"},
		{"content-length", "51"},
	}, false)
	if action != types.ActionContinue {
		t.Fatalf("expected continue, got %v", action)
	}

	orig := []byte(`{"stream":true,"stream_options":{"include_usage":true}}`)
	host.CallOnRequestBody(contextID, orig, true)

	got := host.GetCurrentRequestBody(contextID)

	// payload order after json.Unmarshal+Marshal is not stable, so compare semantically.
	var gotPayload map[string]any
	if err := json.Unmarshal(got, &gotPayload); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	var wantPayload map[string]any
	if err := json.Unmarshal(orig, &wantPayload); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	gotBytes, _ := json.Marshal(gotPayload)
	wantBytes, _ := json.Marshal(wantPayload)
	if string(gotBytes) != string(wantBytes) {
		t.Fatalf("expected request body semantically unchanged, got=%s want=%s", string(gotBytes), string(wantBytes))
	}
}

func TestTokenStats_SSEIncrementalParsing(t *testing.T) {
	jwt := mustJWTWithUID("123")

	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
rate_limits:
  - api_key: "`+jwt+`"
    max_concurrent: 1

token_statistics:
  enabled: true
  metric_key_limit: 5000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer " + jwt},
	}, false)
	if action != types.ActionContinue {
		t.Fatalf("expected continue, got %v", action)
	}

	// Mark response as event-stream.
	host.CallOnResponseHeaders(contextID, [][2]string{{"content-type", "text/event-stream"}}, false)

	// Feed two chunks; the second completes the line.
	host.CallOnResponseBody(contextID, []byte("data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n"), false)
	host.CallOnResponseBody(contextID, []byte("data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4}}\n"), true)

	host.CompleteHttpContext(contextID)

	prompt, err := host.GetCounterMetric("llm.prompt_tokens_totaldomain=.=api.example.com;.;uid=.=123;.;")
	if err != nil {
		t.Fatalf("GetCounterMetric(prompt): %v", err)
	}
	if prompt != 4 {
		t.Fatalf("unexpected prompt tokens: got %d want %d", prompt, 4)
	}

	completion, err := host.GetCounterMetric("llm.completion_tokens_totaldomain=.=api.example.com;.;uid=.=123;.;")
	if err != nil {
		t.Fatalf("GetCounterMetric(completion): %v", err)
	}
	if completion != 6 {
		t.Fatalf("unexpected completion tokens: got %d want %d", completion, 6)
	}
}

func TestTokenStats_SSEParseErrorsIncrementedForInvalidJSONFrame(t *testing.T) {
	jwt := mustJWTWithUID("123")

	host, reset := newHTTPHostWithConfig(t, []byte(`domains:
  - api.example.com
rate_limits:
  - api_key: "`+jwt+`"
    max_concurrent: 1

token_statistics:
  enabled: true
  metric_key_limit: 5000
error_response:
  status_code: 429
  message: Rate limit exceeded
`))
	defer reset()

	contextID := host.InitializeHttpContext()
	action := host.CallOnRequestHeaders(contextID, [][2]string{
		{":authority", "api.example.com"},
		{"authorization", "Bearer " + jwt},
	}, false)
	if action != types.ActionContinue {
		t.Fatalf("expected continue, got %v", action)
	}

	host.CallOnResponseHeaders(contextID, [][2]string{{"content-type", "text/event-stream"}}, false)

	// Invalid JSON frame should bump parse errors.
	host.CallOnResponseBody(contextID, []byte("data: {not json}\n"), true)
	host.CompleteHttpContext(contextID)

	errs, err := host.GetCounterMetric("llm.stream_parse_errors_totaldomain=.=api.example.com;.;uid=.=123;.;")
	if err != nil {
		t.Fatalf("GetCounterMetric(parse_errors): %v", err)
	}
	if errs != 1 {
		t.Fatalf("unexpected parse errors: got %d want %d", errs, 1)
	}
}

func mustJWTWithUID(uid string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"uid":"` + uid + `"}`))
	return header + "." + payload + ".sig"
}
