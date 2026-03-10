package limiter

import "testing"

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
