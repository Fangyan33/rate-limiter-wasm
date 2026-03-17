package plugin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"rate-limiter-wasm/internal/auth"
	"rate-limiter-wasm/internal/config"
	"rate-limiter-wasm/internal/limiter"
	"rate-limiter-wasm/internal/matcher"
	"rate-limiter-wasm/internal/store"

	proxywasm "github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm"
	types "github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

type requestLimiter interface {
	Acquire(apiKey string) (func(), bool)
}

type vmContext struct {
	types.DefaultVMContext
}

type rootContext struct {
	types.DefaultPluginContext
	cfg              config.Config
	matcher          *matcher.DomainMatcher
	limiter          requestLimiter
	limits           map[string]int
	counterService   config.CounterServiceConfig
	asyncDistributed bool

	metricPromptTokens     map[string]proxywasm.MetricCounter
	metricCompletionTokens map[string]proxywasm.MetricCounter
	metricParseErrors      map[string]proxywasm.MetricCounter
	metricKeys             map[string]struct{}
	metricKeyCount         int
	metricKeyLimit         int
}

type httpContext struct {
	types.DefaultHttpContext
	root               *rootContext
	release            func()
	pendingAcquire     bool
	distributedAPIKey  string
	distributedLeaseID string

	tokenStatsEnabled bool
	domain            string
	uid               string

	// Token statistics state.
	promptTokens        int
	completionTokens    int
	streamParseErrors   int
	responseContentType string

	// Buffer for SSE incremental parsing.
	sseBuf []byte
}

func NewVMContext() types.VMContext {
	return &vmContext{}
}

func NewRootContext() *rootContext {
	return &rootContext{}
}

func (*vmContext) NewPluginContext(contextID uint32) types.PluginContext {
	return &rootContext{}
}

func (r *rootContext) OnPluginStart(pluginConfigurationSize int) types.OnPluginStartStatus {
	data, err := proxywasm.GetPluginConfiguration()
	if err != nil {
		proxywasm.LogCriticalf("read plugin configuration: %v", err)
		return types.OnPluginStartStatusFailed
	}

	if err := r.LoadConfiguration(data); err != nil {
		proxywasm.LogCriticalf("parse plugin configuration: %v", err)
		return types.OnPluginStartStatusFailed
	}

	return types.OnPluginStartStatusOK
}

func (r *rootContext) LoadConfiguration(data []byte) error {
	cfg, err := config.Parse(data)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	domainMatcher, err := matcher.NewDomainMatcher(cfg.Domains)
	if err != nil {
		return fmt.Errorf("build domain matcher: %w", err)
	}

	limits := make(map[string]int, len(cfg.RateLimits))
	for _, limit := range cfg.RateLimits {
		limits[limit.APIKey] = limit.MaxConcurrent
	}

	r.cfg = cfg
	r.matcher = domainMatcher
	r.limits = limits
	r.counterService = cfg.DistributedLimit.CounterService
	r.asyncDistributed = cfg.DistributedLimit.Enabled &&
		cfg.DistributedLimit.Backend == "counter_service"

	// Token statistics metrics.
	r.metricPromptTokens = make(map[string]proxywasm.MetricCounter)
	r.metricCompletionTokens = make(map[string]proxywasm.MetricCounter)
	r.metricParseErrors = make(map[string]proxywasm.MetricCounter)
	r.metricKeys = make(map[string]struct{})
	r.metricKeyCount = 0
	r.metricKeyLimit = cfg.TokenStatistics.MetricKeyLimit

	requestLimiter, err := newRequestLimiter(cfg, limits)
	if err != nil {
		return fmt.Errorf("build request limiter: %w", err)
	}

	r.limiter = requestLimiter
	return nil
}

func (r *rootContext) Config() config.Config {
	return r.cfg
}

func (r *rootContext) NewHttpContext(contextID uint32) types.HttpContext {
	return &httpContext{root: r}
}

func (h *httpContext) OnHttpRequestHeaders(numHeaders int, endOfStream bool) types.Action {
	if h.root == nil || h.root.matcher == nil || h.root.limiter == nil {
		return types.ActionContinue
	}

	host, err := proxywasm.GetHttpRequestHeader(":authority")
	if err != nil || !h.root.matcher.Match(normalizeHost(host)) {
		return types.ActionContinue
	}

	h.domain = normalizeHost(host)

	authorization, err := proxywasm.GetHttpRequestHeader("authorization")
	if err != nil {
		return h.reject()
	}

	if h.root.cfg.TokenStatistics.Enabled {
		uid, err := parseUIDFromJWT(authorization)
		if err != nil {
			proxywasm.LogWarnf("token statistics disabled: parse uid from jwt: %v", err)
			h.tokenStatsEnabled = false
		} else {
			h.uid = uid
			h.tokenStatsEnabled = true
		}
	} else {
		h.tokenStatsEnabled = false
	}

	apiKey, err := auth.ParseBearerToken(authorization)
	if err != nil {
		return h.reject()
	}

	// If we might mutate the request body, remove content-length in advance.
	// (Required by proxy-wasm-go-sdk when the body size can change.)
	if h.root.cfg.TokenStatistics.Enabled && h.root.cfg.TokenStatistics.InjectStreamUsage && h.tokenStatsEnabled {
		_ = proxywasm.RemoveHttpRequestHeader("content-length")
	}

	// When counter_service async mode is enabled, dispatch an HTTP
	// callout and pause the request until the callback arrives.
	if h.root.asyncDistributed {
		h.pendingAcquire = true
		h.distributedAPIKey = apiKey

		cs := h.root.counterService

		body, _ := json.Marshal(struct {
			Domain string `json:"domain"`
			APIKey string `json:"api_key"`
			TTLMS  int64  `json:"ttl_ms"`
		}{
			Domain: h.domain,
			APIKey: apiKey,
			TTLMS:  int64(cs.LeaseTTLMS),
		})

		timeout := uint32(cs.TimeoutMS)
		if timeout == 0 {
			timeout = 5000
		}

		_, err := proxywasm.DispatchHttpCall(
			cs.Cluster,
			[][2]string{
				{":method", "POST"},
				{":path", cs.AcquirePath},
				{":authority", cs.Cluster},
				{"content-type", "application/json"},
			},
			body,
			nil,
			timeout,
			h.onAcquireResponse,
		)
		if err != nil {
			proxywasm.LogErrorf("dispatch acquire callout: %v", err)
			return h.reject()
		}

		return types.ActionPause
	}

	release, ok := h.root.limiter.Acquire(apiKey)
	if !ok {
		return h.reject()
	}

	h.release = release
	return types.ActionContinue
}

func (h *httpContext) OnHttpRequestBody(bodySize int, endOfStream bool) types.Action {
	if h.root == nil {
		return types.ActionContinue
	}
	cfg := h.root.cfg
	if !cfg.TokenStatistics.Enabled || !cfg.TokenStatistics.InjectStreamUsage || !h.tokenStatsEnabled {
		return types.ActionContinue
	}

	// Only mutate when the full request body is available.
	if !endOfStream {
		return types.ActionContinue
	}

	body, err := proxywasm.GetHttpRequestBody(0, bodySize)
	if err != nil || len(body) == 0 {
		return types.ActionContinue
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return types.ActionContinue
	}

	streamVal, ok := payload["stream"]
	if !ok {
		return types.ActionContinue
	}
	stream, ok := streamVal.(bool)
	if !ok || !stream {
		return types.ActionContinue
	}

	soRaw, hasSO := payload["stream_options"]
	if hasSO {
		if so, ok := soRaw.(map[string]any); ok {
			if v, ok := so["include_usage"].(bool); ok && v {
				return types.ActionContinue
			}
		}
	}

	payload["stream_options"] = map[string]any{"include_usage": true}
	mutated, err := json.Marshal(payload)
	if err != nil {
		return types.ActionContinue
	}

	if err := proxywasm.ReplaceHttpRequestBody(mutated); err != nil {
		return types.ActionContinue
	}
	return types.ActionContinue
}

func (h *httpContext) OnHttpResponseHeaders(numHeaders int, endOfStream bool) types.Action {
	if h.root == nil {
		return types.ActionContinue
	}
	if !h.root.cfg.TokenStatistics.Enabled || !h.tokenStatsEnabled {
		return types.ActionContinue
	}
	ct, err := proxywasm.GetHttpResponseHeader("content-type")
	if err == nil {
		h.responseContentType = strings.ToLower(strings.TrimSpace(ct))
	}
	return types.ActionContinue
}

func (h *httpContext) OnHttpResponseBody(bodySize int, endOfStream bool) types.Action {
	if h.root == nil {
		return types.ActionContinue
	}
	if !h.root.cfg.TokenStatistics.Enabled || !h.tokenStatsEnabled {
		return types.ActionContinue
	}

	// For non-SSE, we only parse when the full body is available.
	if !isEventStream(h.responseContentType) {
		if !endOfStream {
			return types.ActionContinue
		}
		body, err := proxywasm.GetHttpResponseBody(0, bodySize)
		if err != nil {
			h.streamParseErrors++
			return types.ActionContinue
		}
		prompt, completion, ok := parseUsageFromJSON(body)
		if !ok {
			h.streamParseErrors++
			return types.ActionContinue
		}
		h.promptTokens += prompt
		h.completionTokens += completion
		return types.ActionContinue
	}

	// SSE incremental parsing.
	chunk, err := proxywasm.GetHttpResponseBody(0, bodySize)
	if err != nil {
		h.streamParseErrors++
		return types.ActionContinue
	}
	h.parseSSEChunk(chunk)
	return types.ActionContinue
}

func isEventStream(contentType string) bool {
	return strings.Contains(contentType, "text/event-stream")
}

func parseUsageFromJSON(body []byte) (promptTokens int, completionTokens int, ok bool) {
	var payload struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, 0, false
	}
	if payload.Usage.PromptTokens == 0 && payload.Usage.CompletionTokens == 0 {
		return 0, 0, false
	}
	return payload.Usage.PromptTokens, payload.Usage.CompletionTokens, true
}

func (h *httpContext) parseSSEChunk(chunk []byte) {
	// Append and split by newlines.
	h.sseBuf = append(h.sseBuf, chunk...)

	for {
		idx := bytes.IndexByte(h.sseBuf, '\n')
		if idx < 0 {
			// Keep incomplete line.
			if len(h.sseBuf) > 64*1024 {
				// Prevent unbounded growth on malformed streams.
				h.sseBuf = h.sseBuf[:0]
				h.streamParseErrors++
			}
			return
		}

		line := bytes.TrimSpace(h.sseBuf[:idx])
		h.sseBuf = h.sseBuf[idx+1:]
		if len(line) == 0 {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		prompt, completion, ok := parseUsageFromJSON(data)
		if !ok {
			// Not all SSE frames carry usage; treat parse errors only when JSON is invalid.
			var tmp any
			if err := json.Unmarshal(data, &tmp); err != nil {
				h.streamParseErrors++
			}
			continue
		}
		h.promptTokens += prompt
		h.completionTokens += completion
	}
}

func (h *httpContext) OnHttpStreamDone() {
	// Release local limiter slot if acquired.
	if h.release != nil {
		h.release()
		h.release = nil
	}

	// Flush token statistics.
	if h.root != nil && h.root.cfg.TokenStatistics.Enabled && h.tokenStatsEnabled {
		h.updateTokenMetrics()
	}

	// Dispatch best-effort distributed release if we have a lease.
	if h.distributedLeaseID != "" && h.root != nil && h.root.asyncDistributed {
		cs := h.root.counterService
		body, _ := json.Marshal(struct {
			APIKey  string `json:"api_key"`
			LeaseID string `json:"lease_id"`
		}{
			APIKey:  h.distributedAPIKey,
			LeaseID: h.distributedLeaseID,
		})

		timeout := uint32(cs.TimeoutMS)
		if timeout == 0 {
			timeout = 5000
		}

		_, err := proxywasm.DispatchHttpCall(
			cs.Cluster,
			[][2]string{
				{":method", "POST"},
				{":path", cs.ReleasePath},
				{":authority", cs.Cluster},
				{"content-type", "application/json"},
			},
			body,
			nil,
			timeout,
			func(numHeaders, bodySize, numTrailers int) {
				// Best-effort release, ignore response.
			},
		)
		if err != nil {
			proxywasm.LogWarnf("dispatch release callout: %v", err)
		}

		h.distributedLeaseID = ""
	}
}

func (h *httpContext) updateTokenMetrics() {
	if h.root == nil {
		return
	}

	domain := sanitizeMetricValue(h.domain)
	uid := sanitizeMetricValue(h.uid)
	uid, key := h.root.ensureMetricKey(domain, uid)

	if h.promptTokens > 0 {
		c := h.root.getPromptCounter(domain, uid, key)
		c.Increment(uint64(h.promptTokens))
	}
	if h.completionTokens > 0 {
		c := h.root.getCompletionCounter(domain, uid, key)
		c.Increment(uint64(h.completionTokens))
	}
	if h.streamParseErrors > 0 {
		c := h.root.getParseErrorsCounter(domain, uid, key)
		c.Increment(uint64(h.streamParseErrors))
	}
}

func sanitizeMetricValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "_"
	}
	return strings.Map(func(r rune) rune {
		if r > 127 {
			return '_'
		}
		switch r {
		case ';', '=', '|', '\n', '\r', '\t', ' ':
			return '_'
		default:
			return r
		}
	}, v)
}

func (r *rootContext) ensureMetricKey(domain, uid string) (finalUID string, key string) {
	key = domain + "|" + uid
	if _, ok := r.metricKeys[key]; ok {
		return uid, key
	}

	// Existing key budget exhausted: overflow to __other__.
	if r.metricKeyCount >= r.metricKeyLimit {
		finalUID = "__other__"
		key = domain + "|" + finalUID
		return finalUID, key
	}

	r.metricKeys[key] = struct{}{}
	r.metricKeyCount++
	return uid, key
}

func (r *rootContext) getPromptCounter(domain, uid, key string) proxywasm.MetricCounter {
	if c, ok := r.metricPromptTokens[key]; ok {
		return c
	}
	c := proxywasm.DefineCounterMetric(fmt.Sprintf("llm_prompt_tokens_total;domain=%s;uid=%s;", domain, uid))
	r.metricPromptTokens[key] = c
	return c
}

func (r *rootContext) getCompletionCounter(domain, uid, key string) proxywasm.MetricCounter {
	if c, ok := r.metricCompletionTokens[key]; ok {
		return c
	}
	c := proxywasm.DefineCounterMetric(fmt.Sprintf("llm_completion_tokens_total;domain=%s;uid=%s;", domain, uid))
	r.metricCompletionTokens[key] = c
	return c
}

func (r *rootContext) getParseErrorsCounter(domain, uid, key string) proxywasm.MetricCounter {
	if c, ok := r.metricParseErrors[key]; ok {
		return c
	}
	c := proxywasm.DefineCounterMetric(fmt.Sprintf("llm_stream_parse_errors_total;domain=%s;uid=%s;", domain, uid))
	r.metricParseErrors[key] = c
	return c
}


func (h *httpContext) onAcquireResponse(numHeaders, bodySize, numTrailers int) {
	h.pendingAcquire = false

	// Check HTTP status first
	headers, err := proxywasm.GetHttpCallResponseHeaders()
	if err != nil {
		proxywasm.LogErrorf("read acquire response headers: %v", err)
		h.fallbackToLocalLimiter()
		return
	}

	status := ""
	for _, header := range headers {
		if header[0] == ":status" {
			status = header[1]
			break
		}
	}

	if status != "200" {
		proxywasm.LogWarnf("counter service returned status %s, falling back to local limiter", status)
		h.fallbackToLocalLimiter()
		return
	}

	body, err := proxywasm.GetHttpCallResponseBody(0, math.MaxInt32)
	if err != nil {
		proxywasm.LogErrorf("read acquire response body: %v", err)
		h.fallbackToLocalLimiter()
		return
	}

	var resp struct {
		Allowed       bool   `json:"allowed"`
		LeaseID       string `json:"lease_id,omitempty"`
		Reason        string `json:"reason,omitempty"`
		Message       string `json:"message,omitempty"`
		MaxConcurrent int    `json:"max_concurrent,omitempty"`
		CurrentCount  int    `json:"current_count,omitempty"`
		Tier          string `json:"tier,omitempty"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		proxywasm.LogErrorf("parse acquire response: %v", err)
		h.fallbackToLocalLimiter()
		return
	}

	if !resp.Allowed {
		proxywasm.LogWarnf("counter service denied: reason=%s message=%s", resp.Reason, resp.Message)
		h.reject()
		return
	}

	h.distributedLeaseID = resp.LeaseID
	if err := proxywasm.ResumeHttpRequest(); err != nil {
		proxywasm.LogErrorf("resume http request: %v", err)
	}
}

func (h *httpContext) fallbackToLocalLimiter() {
	if h.root == nil || h.root.limiter == nil {
		h.reject()
		return
	}

	release, ok := h.root.limiter.Acquire(h.distributedAPIKey)
	if !ok {
		h.reject()
		return
	}

	h.release = release
	if err := proxywasm.ResumeHttpRequest(); err != nil {
		proxywasm.LogErrorf("resume http request after fallback: %v", err)
	}
}

func (h *httpContext) reject() (action types.Action) {
	if h.root == nil {
		return types.ActionContinue
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			safeLogCriticalf("send local response panic: %v", recovered)
			action = types.ActionContinue
		}
	}()

	if err := proxywasm.SendHttpResponse(
		uint32(h.root.cfg.ErrorResponse.StatusCode),
		[][2]string{{"content-type", "text/plain; charset=utf-8"}},
		[]byte(h.root.cfg.ErrorResponse.Message),
		-1,
	); err != nil {
		safeLogCriticalf("send local response: %v", err)
		return types.ActionContinue
	}
	return types.ActionPause
}

func safeLogCriticalf(format string, args ...any) {
	defer func() {
		_ = recover()
	}()
	proxywasm.LogCriticalf(format, args...)
}

func newRequestLimiter(cfg config.Config, limits map[string]int) (requestLimiter, error) {
	// Counter service mode uses async HTTP callouts in the plugin layer,
	// so we only need a local limiter for fallback.
	//
	// The async HTTP flow is implemented in OnHttpRequestHeaders (acquire)
	// and OnHttpStreamDone (release) using proxywasm.DispatchHttpCall().
	// This is required because Proxy-WASM SDK only supports async HTTP calls.
	//
	// See internal/store/client.go for details on why the DistributedStore
	// interface is not used for counter_service mode.
	if cfg.DistributedLimit.Enabled && cfg.DistributedLimit.Backend == "counter_service" {
		return limiter.NewLocalLimiter(limits), nil
	}

	// Non-counter-service distributed modes (if any) would use the
	// synchronous distributed limiter path.
	//
	// NOTE: Currently no other distributed backends are implemented.
	// This path exists for potential future synchronous backends
	// (e.g., direct Redis protocol, gRPC services).
	if cfg.DistributedLimit.Enabled {
		distributedStore, err := store.NewClient(cfg.DistributedLimit.CounterService)
		if err != nil {
			return nil, err
		}
		return limiter.NewDistributedLimiter(limits, distributedStore), nil
	}

	return limiter.NewLocalLimiter(limits), nil
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return ""
	}
	if idx := strings.Index(host, ":"); idx >= 0 {
		return host[:idx]
	}
	return host
}

const (
	maxAuthorizationHeaderBytes = 16 * 1024
	maxUIDBytes                 = 64
)

func parseUIDFromJWT(authorizationHeader string) (string, error) {
	if len(authorizationHeader) > maxAuthorizationHeaderBytes {
		return "", fmt.Errorf("authorization header too large")
	}

	jwt, err := auth.ParseBearerToken(authorizationHeader)
	if err != nil {
		return "", err
	}

	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid jwt format")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode jwt payload: %w", err)
	}

	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return "", fmt.Errorf("parse jwt payload: %w", err)
	}

	rawUID, ok := claims["uid"]
	if !ok {
		return "", fmt.Errorf("missing uid")
	}

	var uid string
	switch v := rawUID.(type) {
	case string:
		uid = v
	case float64:
		// json.Unmarshal uses float64 for all JSON numbers.
		if v != math.Trunc(v) {
			return "", fmt.Errorf("uid must be an integer")
		}
		uid = strconv.FormatInt(int64(v), 10)
	default:
		return "", fmt.Errorf("unsupported uid type")
	}

	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "", fmt.Errorf("uid must not be empty")
	}
	if len(uid) > maxUIDBytes {
		return "", fmt.Errorf("uid too long")
	}

	if strings.ContainsAny(uid, "\n\r\t") {
		return "", fmt.Errorf("uid contains invalid whitespace")
	}

	return uid, nil
}
