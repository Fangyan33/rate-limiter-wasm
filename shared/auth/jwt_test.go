package auth_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"rate-limiter-wasm/shared/auth"
)

func TestParseUIDFromJWTStringUID(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"uid":"123"}`))
	jwt := "a." + payload + ".c"

	uid, err := auth.ParseUIDFromJWT("Bearer " + jwt)
	if err != nil {
		t.Fatalf("ParseUIDFromJWT() error = %v", err)
	}
	if uid != "123" {
		t.Fatalf("unexpected uid: got %q want %q", uid, "123")
	}
}

func TestParseUIDFromJWTNumericUID(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"uid":123}`))
	jwt := "a." + payload + ".c"

	uid, err := auth.ParseUIDFromJWT("Bearer " + jwt)
	if err != nil {
		t.Fatalf("ParseUIDFromJWT() error = %v", err)
	}
	if uid != "123" {
		t.Fatalf("unexpected uid: got %q want %q", uid, "123")
	}
}

func TestParseUIDFromJWTRejectsInvalidJWT(t *testing.T) {
	if _, err := auth.ParseUIDFromJWT("Bearer a.b"); err == nil {
		t.Fatal("expected invalid jwt error")
	}
}

func TestParseUIDFromJWTRejectsInvalidPayloadEncoding(t *testing.T) {
	if _, err := auth.ParseUIDFromJWT("Bearer a.!!!.c"); err == nil {
		t.Fatal("expected payload decode error")
	}
}

func TestParseUIDFromJWTRejectsMissingUID(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"abc"}`))
	jwt := "a." + payload + ".c"

	if _, err := auth.ParseUIDFromJWT("Bearer " + jwt); err == nil {
		t.Fatal("expected missing uid error")
	}
}

func TestParseUIDFromJWTRejectsUIDTooLong(t *testing.T) {
	payloadBytes, _ := json.Marshal(map[string]any{"uid": strings.Repeat("x", 65)})
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	jwt := "a." + payload + ".c"

	if _, err := auth.ParseUIDFromJWT("Bearer " + jwt); err == nil {
		t.Fatal("expected uid too long error")
	}
}

func TestParseUIDFromJWTRejectsOversizedHeader(t *testing.T) {
	if _, err := auth.ParseUIDFromJWT("Bearer " + strings.Repeat("a", 16*1024+1)); err == nil {
		t.Fatal("expected oversized header error")
	}
}
