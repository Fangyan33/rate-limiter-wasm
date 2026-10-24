package limiter

import "sync"

type LocalLimiter struct {
	mu             sync.Mutex
	limits         map[string]int
	inFlight       map[string]int
	domainInFlight map[string]int
}

func NewLocalLimiter(limits map[string]int) *LocalLimiter {
	cloned := make(map[string]int, len(limits))
	for apiKey, limit := range limits {
		cloned[apiKey] = limit
	}

	return &LocalLimiter{
		limits:         cloned,
		inFlight:       make(map[string]int, len(cloned)),
		domainInFlight: make(map[string]int),
	}
}

func (l *LocalLimiter) Acquire(apiKey string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	limit, ok := l.limits[apiKey]
	if !ok {
		return nil, false
	}
	return l.acquireLocked(l.inFlight, apiKey, limit)
}

// AcquireForDomain gives the empty-key rule priority over all API key rules.
// Domain counters are separate from legacy API key counters, even if names match.
func (l *LocalLimiter) AcquireForDomain(domain, apiKey string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if limit, ok := l.limits[""]; ok {
		return l.acquireLocked(l.domainInFlight, domain, limit)
	}
	limit, ok := l.limits[apiKey]
	if !ok {
		return nil, false
	}
	return l.acquireLocked(l.inFlight, apiKey, limit)
}

// acquireLocked requires l.mu to be held; the returned release locks it itself.
func (l *LocalLimiter) acquireLocked(inFlight map[string]int, key string, limit int) (func(), bool) {
	if inFlight[key] >= limit {
		return nil, false
	}

	inFlight[key]++

	released := false
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()

		if released {
			return
		}
		released = true

		current := inFlight[key]
		if current <= 1 {
			delete(inFlight, key)
			return
		}
		inFlight[key] = current - 1
	}, true
}

func (l *LocalLimiter) Limit(apiKey string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limits[apiKey]
}
