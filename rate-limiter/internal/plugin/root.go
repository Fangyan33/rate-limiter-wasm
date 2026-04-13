package plugin

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"rate-limiter-wasm/rate-limiter/internal/config"
	"rate-limiter-wasm/rate-limiter/internal/limiter"
	"rate-limiter-wasm/shared/auth"
	sharedmatcher "rate-limiter-wasm/shared/matcher"

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
	matcher          *sharedmatcher.DomainMatcher
	limiter          requestLimiter
	counterService   config.CounterServiceConfig
	asyncDistributed bool
}

type httpContext struct {
	types.DefaultHttpContext
	root               *rootContext
	release            func()
	distributedAPIKey  string
	distributedLeaseID string
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

	domainMatcher, err := sharedmatcher.NewDomainMatcher(cfg.Domains)
	if err != nil {
		return fmt.Errorf("build domain matcher: %w", err)
	}

	r.cfg = cfg
	r.matcher = domainMatcher
	r.counterService = cfg.DistributedLimit.CounterService
	r.asyncDistributed = cfg.DistributedLimit.Enabled &&
		cfg.DistributedLimit.Backend == "counter_service"
	r.limiter = limiter.NewLocalLimiter(buildLimits(cfg.RateLimits))
	return nil
}

func buildLimits(rateLimits []config.RateLimit) map[string]int {
	limits := make(map[string]int, len(rateLimits))
	for _, limit := range rateLimits {
		limits[limit.APIKey] = limit.MaxConcurrent
	}
	return limits
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

	if h.root.asyncDistributed {
		h.distributedAPIKey = apiKey
		return h.dispatchAcquire(normalizeHost(host), apiKey)
	}

	release, ok := h.root.limiter.Acquire(apiKey)
	if !ok {
		return h.reject()
	}

	h.release = release
	return types.ActionContinue
}

func (h *httpContext) dispatchAcquire(domain, apiKey string) types.Action {
	cs := h.root.counterService
	body, _ := json.Marshal(struct {
		Domain string `json:"domain"`
		APIKey string `json:"api_key"`
		TTLMS  int64  `json:"ttl_ms"`
	}{
		Domain: domain,
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

func (h *httpContext) OnHttpStreamDone() {
	if h.release != nil {
		h.release()
		h.release = nil
	}

	if h.distributedLeaseID == "" || h.root == nil || !h.root.asyncDistributed {
		return
	}

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
		func(numHeaders, bodySize, numTrailers int) {},
	)
	if err != nil {
		proxywasm.LogWarnf("dispatch release callout: %v", err)
	}

	h.distributedLeaseID = ""
}

func (h *httpContext) onAcquireResponse(numHeaders, bodySize, numTrailers int) {
	headers, err := proxywasm.GetHttpCallResponseHeaders()
	if err != nil {
		proxywasm.LogErrorf("read acquire response headers: %v", err)
		h.resumeAfterFailedOpen()
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
		proxywasm.LogWarnf("counter service returned status %s, failing open", status)
		h.resumeAfterFailedOpen()
		return
	}

	body, err := proxywasm.GetHttpCallResponseBody(0, math.MaxInt32)
	if err != nil {
		proxywasm.LogErrorf("read acquire response body: %v", err)
		h.resumeAfterFailedOpen()
		return
	}

	var resp struct {
		Allowed bool   `json:"allowed"`
		LeaseID string `json:"lease_id,omitempty"`
		Reason  string `json:"reason,omitempty"`
		Message string `json:"message,omitempty"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		proxywasm.LogErrorf("parse acquire response: %v", err)
		h.resumeAfterFailedOpen()
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

func (h *httpContext) resumeAfterFailedOpen() {
	if err := proxywasm.ResumeHttpRequest(); err != nil {
		proxywasm.LogErrorf("resume http request after failed-open acquire: %v", err)
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
