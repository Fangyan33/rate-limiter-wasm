package limiter

import (
	"sync"
	"testing"
)

func TestDomainLimitSharesQuotaAndIsolatesDomains(t *testing.T) {
	l := NewLocalLimiter(map[string]int{"": 2})
	first, ok := l.AcquireForDomain("a.example.com", "")
	if !ok {
		t.Fatal("empty key should acquire domain slot")
	}
	second, ok := l.AcquireForDomain("a.example.com", "known")
	if !ok {
		t.Fatal("known key should acquire domain slot")
	}
	for _, key := range []string{"", "known", "unknown"} {
		if _, ok := l.AcquireForDomain("a.example.com", key); ok {
			t.Fatalf("%q bypassed domain limit", key)
		}
	}
	other, ok := l.AcquireForDomain("b.example.com", "known")
	if !ok {
		t.Fatal("domains must have independent counters")
	}
	defer other()
	first()
	first()
	third, ok := l.AcquireForDomain("a.example.com", "unknown")
	if !ok {
		t.Fatal("release must restore domain capacity")
	}
	if _, ok := l.AcquireForDomain("a.example.com", ""); ok {
		t.Fatal("double release restored extra capacity")
	}
	second()
	third()
}

func TestSpecificKeyPriorityAndDomainIsolation(t *testing.T) {
	l := NewLocalLimiter(map[string]int{"known": 1, "": 1})
	keyRelease, ok := l.AcquireForDomain("a.example.com", "known")
	if !ok {
		t.Fatal("key rule should acquire")
	}
	if _, ok = l.AcquireForDomain("a.example.com", "known"); ok {
		t.Fatal("full key rule must not fall back to domain")
	}
	domainRelease, ok := l.AcquireForDomain("a.example.com", "unknown")
	if !ok {
		t.Fatal("key pool must not consume domain quota")
	}
	otherRelease, ok := l.AcquireForDomain("b.example.com", "known")
	if !ok {
		t.Fatal("same key must have independent per-domain capacity")
	}
	keyRelease()
	if _, ok = l.AcquireForDomain("a.example.com", ""); ok {
		t.Fatal("key release must not free domain capacity")
	}
	domainRelease()
	otherRelease()
	if len(l.domainInFlight) != 0 {
		t.Fatal("released counters must be removed")
	}
	for _, key := range []string{"", "unknown"} {
		release, allowed := NewLocalLimiter(map[string]int{"known": 1}).AcquireForDomain("a.example.com", key)
		if !allowed || release != nil {
			t.Fatalf("unmatched key must be unlimited: %q", key)
		}
	}
}

func TestDomainAndLegacyCountersDoNotCollide(t *testing.T) {
	l := NewLocalLimiter(map[string]int{"": 1, "api.example.com": 1})
	domainRelease, ok := l.AcquireForDomain("api.example.com", "")
	if !ok {
		t.Fatal("domain acquire failed")
	}
	defer domainRelease()
	keyRelease, ok := l.Acquire("api.example.com")
	if !ok {
		t.Fatal("domain name collided with API key")
	}
	defer keyRelease()
}

func TestDomainLimitConcurrentAcquire(t *testing.T) {
	l := NewLocalLimiter(map[string]int{"": 5})
	var wg sync.WaitGroup
	releases := make(chan func(), 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if release, ok := l.AcquireForDomain("api.example.com", []string{"", "a", "b"}[i%3]); ok {
				releases <- release
			}
		}(i)
	}
	wg.Wait()
	close(releases)
	if len(releases) != 5 {
		t.Fatalf("got %d concurrent admissions, want 5", len(releases))
	}
	for release := range releases {
		release()
	}
}

func TestLocalLimiterAcquireAndRelease(t *testing.T) {
	l := NewLocalLimiter(map[string]int{
		"key_basic_001": 1,
	})

	release, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected first acquire to succeed")
	}

	if _, ok := l.Acquire("key_basic_001"); ok {
		t.Fatal("expected second acquire to be rejected at limit")
	}

	release()

	if _, ok := l.Acquire("key_basic_001"); !ok {
		t.Fatal("expected acquire to succeed again after release")
	}
}

func TestLocalLimiterReleaseIsIdempotent(t *testing.T) {
	l := NewLocalLimiter(map[string]int{
		"key_basic_001": 1,
	})

	release, ok := l.Acquire("key_basic_001")
	if !ok {
		t.Fatal("expected acquire to succeed")
	}

	release()
	release()

	if _, ok := l.Acquire("key_basic_001"); !ok {
		t.Fatal("expected limiter state to stay consistent after double release")
	}
}

func TestLocalLimiterRejectsUnknownAPIKey(t *testing.T) {
	l := NewLocalLimiter(map[string]int{
		"key_basic_001": 1,
	})

	if _, ok := l.Acquire("missing"); ok {
		t.Fatal("expected unknown api key to be rejected")
	}
}
