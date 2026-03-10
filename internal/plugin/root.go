package plugin

import (
	"fmt"
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
	cfg     config.Config
	matcher *matcher.DomainMatcher
	limiter requestLimiter
}

type httpContext struct {
	types.DefaultHttpContext
	root    *rootContext
	release func()
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
