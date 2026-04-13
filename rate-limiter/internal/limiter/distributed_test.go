package limiter

import "testing"

func TestDistributedLimiterUsesStoreWhenHealthy(t *testing.T) {
	store := &fakeDistributedStore{
		acquireResults: []acquireResult{{ok: true}},
	}
	l := NewDistributedLimiter(map[string]int{
		"key_basic_001": 1,
	}, store)

	release, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected healthy distributed acquire to succeed")
	}
	if release == nil {
		t.Fatal("expected release function from distributed acquire")
	}
	if store.acquireCalls != 1 {
		t.Fatalf("expected 1 distributed acquire call, got %d", store.acquireCalls)
	}
	if store.releaseCalls != 0 {
		t.Fatalf("expected no release call before release, got %d", store.releaseCalls)
	}
	if l.Mode() != ModeDistributed {
		t.Fatalf("expected mode %q, got %q", ModeDistributed, l.Mode())
	}

	release()
	if store.releaseCalls != 1 {
		t.Fatalf("expected 1 distributed release call, got %d", store.releaseCalls)
	}
}

func TestDistributedLimiterFallsBackToLocalWhenStoreUnavailable(t *testing.T) {
	store := &fakeDistributedStore{
		acquireResults: []acquireResult{{err: ErrStoreUnavailable}},
	}
	l := NewDistributedLimiter(map[string]int{
		"key_basic_001": 1,
	}, store)

	firstRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected local fallback acquire to succeed")
	}
	if firstRelease == nil {
		t.Fatal("expected local fallback release function")
	}
	if store.acquireCalls != 1 {
		t.Fatalf("expected one distributed acquire attempt, got %d", store.acquireCalls)
	}
	if l.Mode() != ModeLocalFallback {
		t.Fatalf("expected mode %q, got %q", ModeLocalFallback, l.Mode())
	}

	if _, ok := l.Acquire("key_basic_001"); ok {
		t.Fatal("expected local fallback limiter to enforce local concurrency limit")
	}
	if store.acquireCalls != 2 {
		t.Fatalf("expected second distributed health probe while in fallback, got %d calls", store.acquireCalls)
	}
	if l.Mode() != ModeLocalFallback {
		t.Fatalf("expected to remain in fallback mode, got %q", l.Mode())
	}

	firstRelease()
}

func TestDistributedLimiterRecoversBackToStoreWhenHealthyAgain(t *testing.T) {
	store := &fakeDistributedStore{
		acquireResults: []acquireResult{
			{err: ErrStoreUnavailable},
			{ok: true},
		},
	}
	l := NewDistributedLimiter(map[string]int{
		"key_basic_001": 1,
	}, store)

	fallbackRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected fallback acquire to succeed")
	}
	if l.Mode() != ModeLocalFallback {
		t.Fatalf("expected fallback mode after store failure, got %q", l.Mode())
	}

	fallbackRelease()

	distributedRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected acquire to recover back to distributed store")
	}
	if distributedRelease == nil {
		t.Fatal("expected distributed release function after recovery")
	}
	if store.acquireCalls != 2 {
		t.Fatalf("expected second acquire to probe recovery, got %d calls", store.acquireCalls)
	}
	if l.Mode() != ModeDistributed {
		t.Fatalf("expected mode %q after recovery, got %q", ModeDistributed, l.Mode())
	}

	distributedRelease()
	if store.releaseCalls != 1 {
		t.Fatalf("expected distributed release after recovery, got %d", store.releaseCalls)
	}
}

func TestDistributedLimiterDoesNotRecoverWhileFallbackRequestIsStillInFlight(t *testing.T) {
	store := &fakeDistributedStore{
		acquireResults: []acquireResult{
			{err: ErrStoreUnavailable},
			{ok: true},
		},
	}
	l := NewDistributedLimiter(map[string]int{"key_basic_001": 1}, store)

	fallbackRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected fallback acquire to succeed")
	}
	if l.Mode() != ModeLocalFallback {
		t.Fatalf("expected mode %q after fallback acquire, got %q", ModeLocalFallback, l.Mode())
	}

	if _, ok := l.Acquire("key_basic_001"); ok {
		t.Fatal("expected limiter to reject recovery while fallback request remains in flight")
	}
	if l.Mode() != ModeLocalFallback {
		t.Fatalf("expected limiter to remain in fallback mode while fallback request remains in flight, got %q", l.Mode())
	}
	if store.releaseCalls != 1 {
		t.Fatalf("expected blocked distributed recovery probe to release its slot, got %d", store.releaseCalls)
	}

	fallbackRelease()
}

func TestDistributedLimiterFallsBackLocallyWhileRecoveryIsBlocked(t *testing.T) {
	store := &fakeDistributedStore{
		acquireResults: []acquireResult{
			{err: ErrStoreUnavailable},
			{ok: true},
		},
	}
	l := NewDistributedLimiter(map[string]int{"key_basic_001": 2}, store)

	firstRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected first fallback acquire to succeed")
	}

	secondRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected second acquire to keep using local fallback while recovery is blocked")
	}
	if secondRelease == nil {
		t.Fatal("expected local fallback release function while recovery is blocked")
	}
	if l.Mode() != ModeLocalFallback {
		t.Fatalf("expected mode %q while fallback request remains in flight, got %q", ModeLocalFallback, l.Mode())
	}
	if store.releaseCalls != 1 {
		t.Fatalf("expected blocked distributed recovery probe to release its slot, got %d", store.releaseCalls)
	}

	secondRelease()
	firstRelease()
}

func TestDistributedLimiterRecoversAfterFallbackInflightDrains(t *testing.T) {
	store := &fakeDistributedStore{
		acquireResults: []acquireResult{
			{err: ErrStoreUnavailable},
			{ok: true},
		},
	}
	l := NewDistributedLimiter(map[string]int{"key_basic_001": 1}, store)

	fallbackRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected fallback acquire to succeed")
	}

	fallbackRelease()

	distributedRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected limiter to recover after fallback inflight drains")
	}
	if l.Mode() != ModeDistributed {
		t.Fatalf("expected mode %q after drain, got %q", ModeDistributed, l.Mode())
	}
	distributedRelease()
}

func TestDistributedLimiterWithoutStoreUsesLocalFallbackOnly(t *testing.T) {
	l := NewDistributedLimiter(map[string]int{"key_basic_001": 1}, nil)

	firstRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected local acquire to succeed without distributed store")
	}
	if firstRelease == nil {
		t.Fatal("expected local release function without distributed store")
	}
	if l.Mode() != ModeLocalFallback {
		t.Fatalf("expected mode %q without store, got %q", ModeLocalFallback, l.Mode())
	}

	if _, ok := l.Acquire("key_basic_001"); ok {
		t.Fatal("expected local fallback to enforce limit without store")
	}

	firstRelease()

	secondRelease, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected local capacity to be restored after release")
	}
	secondRelease()
}

type fakeDistributedStore struct {
	acquireCalls   int
	releaseCalls   int
	acquireResults []acquireResult
}

type acquireResult struct {
	release func()
	ok      bool
	err     error
}

func (f *fakeDistributedStore) Acquire(apiKey string, limit int) (func(), bool, error) {
	f.acquireCalls++
	if len(f.acquireResults) == 0 {
		return nil, false, nil
	}

	result := f.acquireResults[0]
	f.acquireResults = f.acquireResults[1:]
	if result.err != nil {
		return nil, false, result.err
	}
	if !result.ok {
		return nil, false, nil
	}
	if result.release == nil {
		return func() {
			f.releaseCalls++
		}, true, nil
	}
	return result.release, true, nil
}

func (f *fakeDistributedStore) Name() string {
	return "fake"
}
