package plugin

import (
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
	}

	apiKey, err := auth.ParseBearerToken(authorization)
	if err != nil {
		return h.reject()
	}

	// When counter_service async mode is enabled, dispatch an HTTP
	// callout and pause the request until the callback arrives.
	if h.root.asyncDistributed {
		h.pendingAcquire = true
		h.distributedAPIKey = apiKey

		cs := h.root.counterService
		limit := h.root.limits[apiKey]

		body, _ := json.Marshal(struct {
			APIKey string `json:"api_key"`
			Limit  int    `json:"limit"`
			TTLMS  int    `json:"ttl_ms"`
		}{
			APIKey: apiKey,
			Limit:  limit,
			TTLMS:  cs.LeaseTTLMS,
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

func (h *httpContext) OnHttpStreamDone() {
	// Release local limiter slot if acquired
	if h.release != nil {
		h.release()
		h.release = nil
	}

	// Dispatch best-effort distributed release if we have a lease
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
				// Best-effort release, ignore response
			},
		)
		if err != nil {
			proxywasm.LogWarnf("dispatch release callout: %v", err)
		}

		h.distributedLeaseID = ""
	}
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
		Allowed bool   `json:"allowed"`
		LeaseID string `json:"lease_id,omitempty"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		proxywasm.LogErrorf("parse acquire response: %v", err)
		h.fallbackToLocalLimiter()
		return
	}

	if !resp.Allowed {
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
