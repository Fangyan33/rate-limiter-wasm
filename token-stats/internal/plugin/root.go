package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"rate-limiter-wasm/shared/auth"
	sharedmatcher "rate-limiter-wasm/shared/matcher"
	"rate-limiter-wasm/token-stats/internal/config"

	proxywasm "github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm"
	types "github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm/types"
)

type vmContext struct {
	types.DefaultVMContext
}

type rootContext struct {
	types.DefaultPluginContext
	cfg                    config.Config
	matcher                *sharedmatcher.DomainMatcher
	metricPromptTokens     map[string]proxywasm.MetricCounter
	metricCompletionTokens map[string]proxywasm.MetricCounter
	metricParseErrors      map[string]proxywasm.MetricCounter
	metricKeys             map[string]struct{}
	metricKeyCount         int
	metricKeyLimit         int
}

type httpContext struct {
	types.DefaultHttpContext
	root *rootContext

	tokenStatsEnabled bool
	domain            string
	uid               string

	promptTokens        int
	completionTokens    int
	streamParseErrors   int
	responseContentType string
	sseBuf              []byte
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
	r.metricPromptTokens = make(map[string]proxywasm.MetricCounter)
	r.metricCompletionTokens = make(map[string]proxywasm.MetricCounter)
	r.metricParseErrors = make(map[string]proxywasm.MetricCounter)
	r.metricKeys = make(map[string]struct{})
	r.metricKeyCount = 0
	r.metricKeyLimit = cfg.TokenStatistics.MetricKeyLimit
	return nil
}

func (r *rootContext) Config() config.Config {
	return r.cfg
}

func (r *rootContext) NewHttpContext(contextID uint32) types.HttpContext {
	return &httpContext{root: r}
}

func (h *httpContext) OnHttpRequestHeaders(numHeaders int, endOfStream bool) types.Action {
	if h.root == nil || h.root.matcher == nil {
		return types.ActionContinue
	}

	host, err := proxywasm.GetHttpRequestHeader(":authority")
	if err != nil || !h.root.matcher.Match(normalizeHost(host)) {
		return types.ActionContinue
	}

	h.domain = normalizeHost(host)
	h.tokenStatsEnabled = h.root.cfg.TokenStatistics.Enabled
	if !h.tokenStatsEnabled {
		return types.ActionContinue
	}

	authorization, err := proxywasm.GetHttpRequestHeader("authorization")
	if err != nil {
		h.tokenStatsEnabled = false
		return types.ActionContinue
	}

	if _, err := auth.ParseBearerToken(authorization); err != nil {
		h.tokenStatsEnabled = false
		return types.ActionContinue
	}

	uid, err := auth.ParseUIDFromJWT(authorization)
	if err != nil {
		proxywasm.LogWarnf("token statistics disabled: parse uid from jwt: %v", err)
		h.tokenStatsEnabled = false
		return types.ActionContinue
	}

	h.uid = uid

	if h.root.cfg.TokenStatistics.InjectStreamUsage {
		_ = proxywasm.RemoveHttpRequestHeader("content-length")
	}

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
	if h.root == nil || !h.root.cfg.TokenStatistics.Enabled || !h.tokenStatsEnabled {
		return types.ActionContinue
	}
	ct, err := proxywasm.GetHttpResponseHeader("content-type")
	if err == nil {
		h.responseContentType = strings.ToLower(strings.TrimSpace(ct))
	}
	return types.ActionContinue
}

func (h *httpContext) OnHttpResponseBody(bodySize int, endOfStream bool) types.Action {
	if h.root == nil || !h.root.cfg.TokenStatistics.Enabled || !h.tokenStatsEnabled {
		return types.ActionContinue
	}

	if !isEventStream(h.responseContentType) {
		if !endOfStream {
			return types.ActionPause
		}
		body, err := proxywasm.GetHttpResponseBody(0, bodySize)
		if err != nil {
			h.streamParseErrors++
			return types.ActionContinue
		}
		prompt, completion, status := parseUsageFromJSON(body)
		switch status {
		case usageParseInvalidJSON:
			h.streamParseErrors++
			return types.ActionContinue
		case usageParseNoUsage:
			return types.ActionContinue
		}
		h.promptTokens += prompt
		h.completionTokens += completion
		return types.ActionContinue
	}

	chunk, err := proxywasm.GetHttpResponseBody(0, bodySize)
	if err != nil {
		h.streamParseErrors++
		return types.ActionContinue
	}
	h.parseSSEChunk(chunk)
	return types.ActionContinue
}

func (h *httpContext) OnHttpStreamDone() {
	if h.root != nil && h.root.cfg.TokenStatistics.Enabled && h.tokenStatsEnabled {
		h.updateTokenMetrics()
	}
}

func isEventStream(contentType string) bool {
	return strings.Contains(contentType, "text/event-stream")
}

type usageParseStatus int

const (
	usageParseInvalidJSON usageParseStatus = iota
	usageParseNoUsage
	usageParseFoundUsage
)

func parseUsageFromJSON(body []byte) (promptTokens int, completionTokens int, status usageParseStatus) {
	var payload struct {
		Usage *struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, 0, usageParseInvalidJSON
	}
	if payload.Usage == nil {
		return 0, 0, usageParseNoUsage
	}
	if payload.Usage.PromptTokens == nil && payload.Usage.CompletionTokens == nil {
		return 0, 0, usageParseNoUsage
	}

	if payload.Usage.PromptTokens != nil {
		promptTokens = *payload.Usage.PromptTokens
	}
	if payload.Usage.CompletionTokens != nil {
		completionTokens = *payload.Usage.CompletionTokens
	}
	return promptTokens, completionTokens, usageParseFoundUsage
}

func (h *httpContext) parseSSEChunk(chunk []byte) {
	h.sseBuf = append(h.sseBuf, chunk...)

	for {
		idx := bytes.IndexByte(h.sseBuf, '\n')
		if idx < 0 {
			if len(h.sseBuf) > 64*1024 {
				h.sseBuf = h.sseBuf[:0]
				h.streamParseErrors++
			}
			return
		}

		line := bytes.TrimSpace(h.sseBuf[:idx])
		h.sseBuf = h.sseBuf[idx+1:]
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		prompt, completion, status := parseUsageFromJSON(data)
		switch status {
		case usageParseInvalidJSON:
			h.streamParseErrors++
			continue
		case usageParseNoUsage:
			continue
		}
		h.promptTokens += prompt
		h.completionTokens += completion
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
	if r.metricKeyCount >= r.metricKeyLimit {
		finalUID = "__other__"
		key = domain + "|" + finalUID
		return finalUID, key
	}

	r.metricKeys[key] = struct{}{}
	r.metricKeyCount++
	return uid, key
}

func buildMetricName(metric, domain, uid string) string {
	return fmt.Sprintf("llm.%sdomain=.=%s;.;uid=.=%s;.;", metric, domain, uid)
}

func (r *rootContext) getPromptCounter(domain, uid, key string) proxywasm.MetricCounter {
	if c, ok := r.metricPromptTokens[key]; ok {
		return c
	}
	c := proxywasm.DefineCounterMetric(buildMetricName("prompt_tokens_total", domain, uid))
	r.metricPromptTokens[key] = c
	return c
}

func (r *rootContext) getCompletionCounter(domain, uid, key string) proxywasm.MetricCounter {
	if c, ok := r.metricCompletionTokens[key]; ok {
		return c
	}
	c := proxywasm.DefineCounterMetric(buildMetricName("completion_tokens_total", domain, uid))
	r.metricCompletionTokens[key] = c
	return c
}

func (r *rootContext) getParseErrorsCounter(domain, uid, key string) proxywasm.MetricCounter {
	if c, ok := r.metricParseErrors[key]; ok {
		return c
	}
	c := proxywasm.DefineCounterMetric(buildMetricName("stream_parse_errors_total", domain, uid))
	r.metricParseErrors[key] = c
	return c
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
