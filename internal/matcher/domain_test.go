package matcher_test

import (
	"testing"

	"rate-limiter-wasm/internal/matcher"
)

func TestDomainMatcherMatchesExactDomain(t *testing.T) {
	m, err := matcher.NewDomainMatcher([]string{"api.example.com"})
	if err != nil {
		t.Fatalf("NewDomainMatcher() error = %v", err)
	}

	if !m.Match("api.example.com") {
		t.Fatal("expected exact domain to match")
	}
}

func TestDomainMatcherMatchesWildcardSubdomain(t *testing.T) {
	m, err := matcher.NewDomainMatcher([]string{"*.service.example.com"})
	if err != nil {
		t.Fatalf("NewDomainMatcher() error = %v", err)
	}

	if !m.Match("chat.service.example.com") {
		t.Fatal("expected wildcard subdomain to match")
	}
}

func TestDomainMatcherDoesNotMatchWildcardBaseDomain(t *testing.T) {
	m, err := matcher.NewDomainMatcher([]string{"*.service.example.com"})
	if err != nil {
		t.Fatalf("NewDomainMatcher() error = %v", err)
	}

	if m.Match("service.example.com") {
		t.Fatal("expected base domain not to match wildcard pattern")
	}
}

func TestDomainMatcherReturnsFalseWhenNoPatternMatches(t *testing.T) {
	m, err := matcher.NewDomainMatcher([]string{"api.example.com", "*.service.example.com"})
	if err != nil {
		t.Fatalf("NewDomainMatcher() error = %v", err)
	}

	if m.Match("other.example.com") {
		t.Fatal("expected unmatched domain to return false")
	}
}

func TestDomainMatcherRejectsEmptyPattern(t *testing.T) {
	if _, err := matcher.NewDomainMatcher([]string{""}); err == nil {
		t.Fatal("expected empty pattern to be rejected")
	}
}
