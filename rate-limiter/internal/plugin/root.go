package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"rate-limiter-wasm/rate-limiter/internal/config"
	"rate-limiter-wasm/rate-limiter/internal/limiter"
	"rate-limiter-wasm/shared/auth"
	sharedmatcher "rate-limiter-wasm/shared/matcher"

	proxywasm "github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm"
	types "github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

type requestLimiter interface {
	AcquireForDomain(domain, apiKey string) (func(), bool)
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
	activeLeases     map[string]*activeLease
	started          bool
	renewalReady     bool
	tickPeriod       uint32
	now              func() time.Time
	dispatchHTTPCall httpCallDispatcher
	setTickPeriod    func(uint32) error
	contextID        uint32
	pendingAcquires  map[uint32]*httpContext
	resumeRequest    func() error
	sendResponse     func(uint32, [][2]string, []byte, int32) error
}

type httpContext struct {
	types.DefaultHttpContext
	root               *rootContext
	release            func()
	distributedAPIKey  string
	distributedLeaseID string
	counterService     config.CounterServiceConfig
	acquireStartedAt   time.Time
	contextID          uint32
	acquirePending     bool
	acquireDeadline    time.Time
}

func NewVMContext() types.VMContext {
	return &vmContext{}
}

func NewRootContext() *rootContext {
	return &rootContext{}
}

func (*vmContext) NewPluginContext(contextID uint32) types.PluginContext {
	return &rootContext{contextID: contextID}
}

func (r *rootContext) OnPluginStart(pluginConfigurationSize int) (status types.OnPluginStartStatus) {
	// Returning failure here can reject xDS or prevent proxy startup even with fail_open.
	status = types.OnPluginStartStatusOK
	defer r.recoverRoot("initialize")
	data, err := proxywasm.GetPluginConfiguration()
	if err != nil {
		r.disable()
		safeLogCriticalf("read plugin configuration: %v; rate limiting disabled", err)
		return status
	}

	if err := r.LoadConfiguration(data); err != nil {
		safeLogCriticalf("parse plugin configuration: %v; rate limiting disabled", err)
		return status
	}
	r.started = true
	r.configureRenewalTimer()

	return types.OnPluginStartStatusOK
}

func (r *rootContext) LoadConfiguration(data []byte) error {
	valid := false
	defer func() {
		if !valid {
			r.disable()
		}
	}()
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
	if r.activeLeases == nil {
		r.activeLeases = make(map[string]*activeLease)
	}
	if r.started {
		r.configureRenewalTimer()
	}
	valid = true
	return nil
}

func (r *rootContext) disable() {
	r.cfg = config.Config{}
	r.matcher = nil
	r.limiter = nil
	r.asyncDistributed = false
}

func (r *rootContext) recoverRoot(operation string) {
	if recovered := recover(); recovered != nil {
		r.disable()
		safeLogCriticalf("%s panic: %v; rate limiting disabled", operation, recovered)
	}
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
	return &httpContext{root: r, contextID: contextID}
}

func (h *httpContext) OnHttpRequestHeaders(numHeaders int, endOfStream bool) (action types.Action) {
	defer func() {
		if recovered := recover(); recovered != nil {
			h.clearPendingAcquire()
			safeLogCriticalf("request headers panic: %v; failing open", recovered)
			action = types.ActionContinue
		}
	}()
	if h.root == nil || h.root.matcher == nil || h.root.limiter == nil {
		return types.ActionContinue
	}

	host, err := proxywasm.GetHttpRequestHeader(":authority")
	if err != nil || !h.root.matcher.Match(normalizeHost(host)) {
		return types.ActionContinue
	}

	// A domain-only rule also applies to missing or malformed credentials.
	// Leave authentication parsing unchanged for other plugins.
	var apiKey string
	if authorization, err := proxywasm.GetHttpRequestHeader("authorization"); err == nil {
		apiKey, _ = auth.ParseBearerToken(authorization)
	}

	if h.root.asyncDistributed {
		if !h.root.renewalReady {
			return types.ActionContinue
		}
		h.distributedAPIKey = apiKey
		return h.dispatchAcquire(normalizeHost(host), apiKey)
	}

	release, ok := h.root.limiter.AcquireForDomain(normalizeHost(host), apiKey)
	if !ok {
		return h.reject()
	}

	h.release = release
	return types.ActionContinue
}

func (h *httpContext) dispatchAcquire(domain, apiKey string) types.Action {
	cs := h.root.counterService
	h.counterService = cs
	h.acquireStartedAt = h.root.currentTime()
	body, _ := json.Marshal(struct {
		Domain string `json:"domain"`
		APIKey string `json:"api_key"`
		TTLMS  int64  `json:"ttl_ms"`
	}{
		Domain: domain,
		APIKey: apiKey,
		TTLMS:  int64(cs.LeaseTTLMS),
	})

	timeout := calloutTimeout(cs.TimeoutMS)
	// An independent root timer also resumes streams if a callout callback is lost.
	h.acquireDeadline = h.acquireStartedAt.Add(time.Duration(timeout) * time.Millisecond)
	h.acquirePending = true
	if h.root.pendingAcquires == nil {
		h.root.pendingAcquires = make(map[uint32]*httpContext)
	}
	h.root.pendingAcquires[h.contextID] = h
	h.root.configureRenewalTimer()
	if !h.root.renewalReady {
		h.clearPendingAcquire()
		return types.ActionContinue
	}

	_, err := h.root.dispatchCall(
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
		h.clearPendingAcquire()
		proxywasm.LogErrorf("dispatch acquire callout: %v", err)
		return types.ActionContinue
	}

	return types.ActionPause
}

func (h *httpContext) OnHttpStreamDone() {
	defer func() {
		if recovered := recover(); recovered != nil {
			safeLogCriticalf("release panic: %v; business response remains unchanged", recovered)
		}
	}()
	h.clearPendingAcquire()
	if h.release != nil {
		h.release()
		h.release = nil
	}

	if h.distributedLeaseID == "" || h.root == nil {
		return
	}
	delete(h.root.activeLeases, h.distributedLeaseID)

	cs := h.counterService
	body, _ := json.Marshal(struct {
		APIKey  string `json:"api_key"`
		LeaseID string `json:"lease_id"`
	}{
		APIKey:  h.distributedAPIKey,
		LeaseID: h.distributedLeaseID,
	})

	timeout := calloutTimeout(cs.TimeoutMS)

	_, err := h.root.dispatchCall(
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
	defer func() {
		if recovered := recover(); recovered != nil {
			safeLogCriticalf("acquire callback panic: %v; failing open", recovered)
			h.resumeAfterFailedOpen()
		}
	}()
	// A watchdog may already have resumed this request. A late denial must not reject it.
	if !h.acquirePending {
		return
	}
	if !h.root.currentTime().Before(h.acquireDeadline) {
		h.resumeAfterFailedOpen()
		return
	}
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

	if bodySize <= 0 || bodySize > 64*1024 {
		proxywasm.LogWarn("invalid acquire response size; failing open")
		h.resumeAfterFailedOpen()
		return
	}
	body, err := proxywasm.GetHttpCallResponseBody(0, bodySize)
	if err != nil {
		proxywasm.LogErrorf("read acquire response body: %v", err)
		h.resumeAfterFailedOpen()
		return
	}

	var resp struct {
		Allowed *bool  `json:"allowed"`
		LeaseID string `json:"lease_id,omitempty"`
		Reason  string `json:"reason,omitempty"`
		Message string `json:"message,omitempty"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		proxywasm.LogErrorf("parse acquire response: %v", err)
		h.resumeAfterFailedOpen()
		return
	}
	if resp.Allowed == nil {
		proxywasm.LogWarn("acquire response missing boolean allowed; failing open")
		h.resumeAfterFailedOpen()
		return
	}

	if !*resp.Allowed {
		switch resp.Reason {
		case "limit_exceeded":
		default:
			proxywasm.LogWarnf("invalid acquire denial: reason=%s; failing open", resp.Reason)
			h.resumeAfterFailedOpen()
			return
		}
		proxywasm.LogWarnf("counter service denied: reason=%s message=%s", resp.Reason, resp.Message)
		if h.reject() == types.ActionContinue {
			h.resumeAfterFailedOpen()
		} else {
			h.clearPendingAcquire()
		}
		return
	}

	h.distributedLeaseID = resp.LeaseID
	if resp.LeaseID != "" {
		h.root.trackLease(resp.LeaseID, h.counterService, h.acquireStartedAt)
	}
	h.resumeAfterFailedOpen()
}

func (h *httpContext) resumeAfterFailedOpen() {
	defer func() {
		if recovered := recover(); recovered != nil {
			safeLogCriticalf("resume request panic: %v; watchdog will retry", recovered)
		}
	}()
	resume := proxywasm.ResumeHttpRequest
	if h.root != nil && h.root.resumeRequest != nil {
		resume = h.root.resumeRequest
	}
	if err := resume(); err != nil {
		proxywasm.LogErrorf("resume http request after failed-open acquire: %v", err)
		return // Leave it pending so the root watchdog can retry.
	}
	h.clearPendingAcquire()
}

func (h *httpContext) clearPendingAcquire() {
	wasPending := h.acquirePending
	h.acquirePending = false
	if h.root != nil {
		defer h.root.recoverRoot("clear acquire")
		delete(h.root.pendingAcquires, h.contextID)
		if wasPending {
			h.root.configureRenewalTimer()
		}
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

	send := proxywasm.SendHttpResponse
	if h.root.sendResponse != nil {
		send = h.root.sendResponse
	}
	if err := send(
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

func calloutTimeout(configuredMS int) uint32 {
	if configuredMS <= 0 || configuredMS > 5000 {
		return 5000
	}
	return uint32(configuredMS)
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
