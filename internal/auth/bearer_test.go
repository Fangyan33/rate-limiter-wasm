package auth_test

import (
	"errors"
	"testing"

	"rate-limiter-wasm/internal/auth"
)

func TestParseBearerTokenReturnsAPIKey(t *testing.T) {
	apiKey, err := auth.ParseBearerToken("Bearer key_basic_001")
	if err != nil {
		t.Fatalf("ParseBearerToken() error = %v", err)
	}

	if apiKey != "key_basic_001" {
		t.Fatalf("expected api key %q, got %q", "key_basic_001", apiKey)
	}
}

func TestParseBearerTokenRejectsMissingHeader(t *testing.T) {
	if _, err := auth.ParseBearerToken(""); !errors.Is(err, auth.ErrMissingAuthorizationHeader) {
		t.Fatalf("expected ErrMissingAuthorizationHeader, got %v", err)
	}
}

func TestParseBearerTokenRejectsWhitespaceOnlyHeader(t *testing.T) {
	if _, err := auth.ParseBearerToken("   "); !errors.Is(err, auth.ErrMissingAuthorizationHeader) {
		t.Fatalf("expected ErrMissingAuthorizationHeader, got %v", err)
	}
}

func TestParseBearerTokenRejectsInvalidScheme(t *testing.T) {
	if _, err := auth.ParseBearerToken("Basic abc123"); !errors.Is(err, auth.ErrInvalidAuthorizationFormat) {
		t.Fatalf("expected ErrInvalidAuthorizationFormat, got %v", err)
	}
}

func TestParseBearerTokenRejectsMissingToken(t *testing.T) {
	if _, err := auth.ParseBearerToken("Bearer "); !errors.Is(err, auth.ErrEmptyBearerToken) {
		t.Fatalf("expected ErrEmptyBearerToken, got %v", err)
	}
}

func TestParseBearerTokenRejectsBareBearerKeyword(t *testing.T) {
	if _, err := auth.ParseBearerToken("Bearer"); !errors.Is(err, auth.ErrEmptyBearerToken) {
		t.Fatalf("expected ErrEmptyBearerToken, got %v", err)
	}
}

func TestParseBearerTokenRejectsMultipleSpacesBeforeToken(t *testing.T) {
	if _, err := auth.ParseBearerToken("Bearer   key_basic_001"); !errors.Is(err, auth.ErrInvalidAuthorizationFormat) {
		t.Fatalf("expected ErrInvalidAuthorizationFormat, got %v", err)
	}
}

func TestParseBearerTokenRejectsLeadingWhitespace(t *testing.T) {
	if _, err := auth.ParseBearerToken(" Bearer key_basic_001"); !errors.Is(err, auth.ErrInvalidAuthorizationFormat) {
		t.Fatalf("expected ErrInvalidAuthorizationFormat, got %v", err)
	}
}

func TestParseBearerTokenRejectsTrailingWhitespace(t *testing.T) {
	if _, err := auth.ParseBearerToken("Bearer key_basic_001 "); !errors.Is(err, auth.ErrInvalidAuthorizationFormat) {
		t.Fatalf("expected ErrInvalidAuthorizationFormat, got %v", err)
	}
}

func TestParseBearerTokenRejectsTabSeparator(t *testing.T) {
	if _, err := auth.ParseBearerToken("Bearer\tkey_basic_001"); !errors.Is(err, auth.ErrInvalidAuthorizationFormat) {
		t.Fatalf("expected ErrInvalidAuthorizationFormat, got %v", err)
	}
}

func TestParseBearerTokenRejectsMalformedValue(t *testing.T) {
	if _, err := auth.ParseBearerToken("Bearer key extra"); !errors.Is(err, auth.ErrInvalidAuthorizationFormat) {
		t.Fatalf("expected ErrInvalidAuthorizationFormat, got %v", err)
	}
}
