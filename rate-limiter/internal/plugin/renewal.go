package plugin

import (
	"encoding/json"
	"math"
	"time"

	"rate-limiter-wasm/rate-limiter/internal/config"

	"github.com/tetratelabs/proxy-wasm-go-sdk/proxywasm"
)

type httpCallDispatcher func(string, [][2]string, []byte, [][2]string, uint32, func(int, int, int)) (uint32, error)

type activeLease struct {
	config      config.CounterServiceConfig
	expiresAt   time.Time
	nextRenewAt time.Time
	pending     bool
}

func (r *rootContext) currentTime() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *rootContext) dispatchCall(cluster string, headers [][2]string, body []byte, trailers [][2]string, timeout uint32, callback func(int, int, int)) (uint32, error) {
	if r.dispatchHTTPCall != nil {
		return r.dispatchHTTPCall(cluster, headers, body, trailers, timeout, callback)
	}
	return proxywasm.DispatchHttpCall(cluster, headers, body, trailers, timeout, callback)
}

func renewalInterval(ttlMS int) time.Duration {
	intervalMS := ttlMS / 3
	if intervalMS < 1 {
		intervalMS = 1
	}
	return time.Duration(intervalMS) * time.Millisecond
}

func (r *rootContext) configureRenewalTimer() {
	period := time.Second
	needed := r.asyncDistributed || len(r.activeLeases) > 0
	if r.asyncDistributed {
		if interval := renewalInterval(r.counterService.LeaseTTLMS); interval < period {
			period = interval
		}
	}
	for _, lease := range r.activeLeases {
		if interval := renewalInterval(lease.config.LeaseTTLMS); interval < period {
			period = interval
		}
	}
	if !needed {
		period = 0
	}
	periodMS := uint32(period / time.Millisecond)
	if r.tickPeriod == periodMS && (!needed || r.renewalReady) {
		return
	}
	setPeriod := r.setTickPeriod
	if setPeriod == nil {
		setPeriod = proxywasm.SetTickPeriodMilliSeconds
	}
	if err := setPeriod(periodMS); err != nil {
		r.renewalReady = false
		proxywasm.LogErrorf("configure lease renewal timer: %v; distributed requests will fail open", err)
		return
	}
	r.tickPeriod = periodMS
	r.renewalReady = needed
}

func (r *rootContext) trackLease(id string, cs config.CounterServiceConfig, startedAt time.Time) {
	lease := &activeLease{
		config:      cs,
		expiresAt:   startedAt.Add(time.Duration(cs.LeaseTTLMS) * time.Millisecond),
		nextRenewAt: startedAt.Add(renewalInterval(cs.LeaseTTLMS)),
	}
	if !r.currentTime().Before(lease.expiresAt) {
		proxywasm.LogWarn("acquire response arrived after the conservative lease deadline; failing open without renewal")
		return
	}
	r.activeLeases[id] = lease
	// Configuration may have changed while acquire was pending. Include this
	// lease's original TTL before the next tick can miss its renewal deadline.
	r.configureRenewalTimer()
}

func (r *rootContext) OnTick() {
	r.configureRenewalTimer()
	now := r.currentTime()
	for id, lease := range r.activeLeases {
		if !now.Before(lease.expiresAt) {
			r.loseLease(id, "lease expired before renewal was confirmed")
			continue
		}
		if lease.pending || now.Before(lease.nextRenewAt) {
			continue
		}
		r.dispatchRenew(id, lease, now)
	}
}

func (r *rootContext) dispatchRenew(id string, lease *activeLease, startedAt time.Time) {
	interval := renewalInterval(lease.config.LeaseTTLMS)
	timeoutMS := int64(lease.config.TimeoutMS)
	if timeoutMS == 0 {
		timeoutMS = 5000
	}
	if limit := interval.Milliseconds(); timeoutMS > limit {
		timeoutMS = limit
	}
	if remaining := lease.expiresAt.Sub(startedAt).Milliseconds(); timeoutMS > remaining {
		timeoutMS = remaining
	}
	if timeoutMS < 1 {
		r.loseLease(id, "insufficient time remaining for renewal")
		return
	}
	if timeoutMS > math.MaxUint32 {
		timeoutMS = math.MaxUint32
	}
	body, _ := json.Marshal(struct {
		LeaseID string `json:"lease_id"`
		TTLMS   int    `json:"ttl_ms"`
	}{id, lease.config.LeaseTTLMS})
	lease.pending = true
	lease.nextRenewAt = startedAt.Add(interval)
	_, err := r.dispatchCall(lease.config.Cluster, [][2]string{
		{":method", "POST"},
		{":path", lease.config.RenewPath},
		{":authority", lease.config.Cluster},
		{"content-type", "application/json"},
	}, body, nil, uint32(timeoutMS), func(numHeaders, bodySize, numTrailers int) {
		r.onRenewResponse(id, lease, startedAt)
	})
	if err != nil {
		lease.pending = false
		proxywasm.LogWarnf("dispatch lease renewal: %v; keeping business request open", err)
	}
}

func (r *rootContext) onRenewResponse(id string, lease *activeLease, startedAt time.Time) {
	// A stream may have finished while the root-context callout was in flight.
	if r.activeLeases[id] != lease {
		return
	}
	lease.pending = false
	if !r.currentTime().Before(lease.expiresAt) {
		r.loseLease(id, "renewal response arrived after the conservative lease deadline")
		return
	}
	headers, err := proxywasm.GetHttpCallResponseHeaders()
	if err != nil {
		proxywasm.LogWarnf("read lease renewal headers: %v", err)
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
		proxywasm.LogWarnf("lease renewal returned status %s; keeping business request open", status)
		return
	}
	body, err := proxywasm.GetHttpCallResponseBody(0, math.MaxInt32)
	if err != nil {
		proxywasm.LogWarnf("read lease renewal body: %v", err)
		return
	}
	var result struct {
		Renewed *bool  `json:"renewed"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Renewed == nil {
		proxywasm.LogWarn("invalid lease renewal response; keeping business request open")
		return
	}
	if !*result.Renewed {
		if result.Reason == "lease_not_found" {
			r.loseLease(id, "counter service no longer holds the lease")
		} else {
			proxywasm.LogWarnf("lease renewal denied: reason=%s; keeping business request open", result.Reason)
		}
		return
	}
	lease.expiresAt = startedAt.Add(time.Duration(lease.config.LeaseTTLMS) * time.Millisecond)
}

func (r *rootContext) loseLease(id, reason string) {
	delete(r.activeLeases, id)
	proxywasm.LogWarnf("lease renewal stopped: %s; business request remains open and concurrency may exceed the limit", reason)
}
