package limiter

import "sync"

type LocalLimiter struct {
	mu       sync.Mutex
	limits   map[string]int
	inFlight map[string]int
}

func NewLocalLimiter(limits map[string]int) *LocalLimiter {
	cloned := make(map[string]int, len(limits))
	for apiKey, limit := range limits {
		cloned[apiKey] = limit
	}

	return &LocalLimiter{
		limits:   cloned,
		inFlight: make(map[string]int, len(cloned)),
	}
}

func (l *LocalLimiter) Acquire(apiKey string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	limit, ok := l.limits[apiKey]
	if !ok {
		return nil, false
	}
	if l.inFlight[apiKey] >= limit {
		return nil, false
	}

	l.inFlight[apiKey]++

	released := false
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()

		if released {
			return
		}
		released = true

		current := l.inFlight[apiKey]
		if current <= 1 {
			delete(l.inFlight, apiKey)
			return
		}
		l.inFlight[apiKey] = current - 1
	}, true
}

func (l *LocalLimiter) Limit(apiKey string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limits[apiKey]
}
