package plugin

import (
	"encoding/json"
	"fmt"
	"math"
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
	cfg                config.Config
	matcher            *matcher.DomainMatcher
	limiter            requestLimiter
	limits             map[string]int
	counterService     config.CounterServiceConfig
	asyncDistributed   bool
}

type httpContext struct {
	types.DefaultHttpContext
	root                *rootContext
	release             func()
	pendingAcquire      bool
	distributedAPIKey   string
	distributedLeaseID  string
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

	authorization, err := proxywasm.GetHttpRequestHeader("authorization")
	if err != nil {
		return h.reject()
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
	if h.release != nil {
		h.release()
		h.release = nil
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
	if !cfg.DistributedLimit.Enabled {
		return limiter.NewLocalLimiter(limits), nil
	}

	distributedStore, err := store.NewClient(cfg.DistributedLimit.CounterService)
	if err != nil {
		return nil, err
	}

	return limiter.NewDistributedLimiter(limits, distributedStore), nil
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
