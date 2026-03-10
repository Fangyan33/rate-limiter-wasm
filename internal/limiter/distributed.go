package limiter

import (
	"errors"
	"sync"
)

var ErrStoreUnavailable = errors.New("distributed store unavailable")

type Mode string

const (
	ModeDistributed   Mode = "distributed"
	ModeLocalFallback Mode = "local-fallback"
)

type DistributedStore interface {
	Acquire(apiKey string, limit int) (func(), bool, error)
	Name() string
}

type DistributedLimiter struct {
	mu               sync.Mutex
	local            *LocalLimiter
	store            DistributedStore
	mode             Mode
	fallbackInflight int
}

func NewDistributedLimiter(limits map[string]int, store DistributedStore) *DistributedLimiter {
	mode := ModeLocalFallback
	if store != nil {
		mode = ModeDistributed
	}

	return &DistributedLimiter{
		local: NewLocalLimiter(limits),
		store: store,
		mode:  mode,
	}
}

func (l *DistributedLimiter) Acquire(apiKey string) (func(), bool) {
	limit := l.local.Limit(apiKey)
	if limit <= 0 {
		return nil, false
	}

	store := l.currentStore()
	if store == nil {
		l.setMode(ModeLocalFallback)
		return l.acquireLocalFallback(apiKey)
	}

	release, ok, err := store.Acquire(apiKey, limit)
	if err != nil {
		l.setMode(ModeLocalFallback)
		return l.acquireLocalFallback(apiKey)
	}
	if ok {
		if l.shouldRecoverToDistributed() {
			l.setMode(ModeDistributed)
			return once(release), true
		}

		once(release)()
		l.setMode(ModeLocalFallback)
		return l.acquireLocalFallback(apiKey)
	}

	return nil, false
}

func (l *DistributedLimiter) Mode() Mode {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mode
}

func (l *DistributedLimiter) currentStore() DistributedStore {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.store
}

func (l *DistributedLimiter) setMode(mode Mode) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.mode = mode
}

func (l *DistributedLimiter) acquireLocalFallback(apiKey string) (func(), bool) {
	release, ok := l.local.Acquire(apiKey)
	if !ok {
		return nil, false
	}

	l.incrementFallbackInflight()
	onceRelease := once(func() {
		release()
		l.decrementFallbackInflight()
	})
	return onceRelease, true
}

func (l *DistributedLimiter) shouldRecoverToDistributed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fallbackInflight == 0
}

func (l *DistributedLimiter) incrementFallbackInflight() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fallbackInflight++
}

func (l *DistributedLimiter) decrementFallbackInflight() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fallbackInflight > 0 {
		l.fallbackInflight--
	}
}

func once(release func()) func() {
	if release == nil {
		return func() {}
	}

	var once sync.Once
	return func() {
		once.Do(release)
	}
}
