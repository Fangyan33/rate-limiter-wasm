package limiter

import "sync"

type domainKey struct {
	domain string
	apiKey string
}

type LocalLimiter struct {
	mu             sync.Mutex
	limits         map[string]int
	inFlight       map[string]int
	domainInFlight map[domainKey]int
}

func NewLocalLimiter(limits map[string]int) *LocalLimiter {
	cloned := make(map[string]int, len(limits))
	for apiKey, limit := range limits {
		cloned[apiKey] = limit
	}

	return &LocalLimiter{
		limits:         cloned,
		inFlight:       make(map[string]int, len(cloned)),
		domainInFlight: make(map[domainKey]int),
	}
}

func (l *LocalLimiter) Acquire(apiKey string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	limit, ok := l.limits[apiKey]
	if !ok {
		return nil, false
	}
	return acquireLocked(l, l.inFlight, apiKey, limit)
}

// AcquireForDomain selects a specific key before a domain rule. No rule is unlimited.
func (l *LocalLimiter) AcquireForDomain(domain, apiKey string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if apiKey != "" {
		if limit, ok := l.limits[apiKey]; ok {
			return acquireLocked(l, l.domainInFlight, domainKey{domain, apiKey}, limit)
		}
	}
	if limit, ok := l.limits[""]; ok {
		return acquireLocked(l, l.domainInFlight, domainKey{domain, ""}, limit)
	}
	return nil, true
}

// acquireLocked requires l.mu to be held; the returned release locks it itself.
func acquireLocked[K comparable](l *LocalLimiter, inFlight map[K]int, key K, limit int) (func(), bool) {
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
