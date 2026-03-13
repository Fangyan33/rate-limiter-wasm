package plugin

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseUIDFromJWT_ValidStringUID(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"uid":"123"}`))
	jwt := header + "." + payload + ".sig"

	uid, err := parseUIDFromJWT("Bearer " + jwt)
	if err != nil {
		t.Fatalf("parseUIDFromJWT() error = %v", err)
	}
	if uid != "123" {
		t.Fatalf("unexpected uid: got %q want %q", uid, "123")
	}
}

func TestParseUIDFromJWT_ValidNumericUID(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"uid":123}`))
	jwt := header + "." + payload + ".sig"

	uid, err := parseUIDFromJWT("Bearer " + jwt)
	if err != nil {
		t.Fatalf("parseUIDFromJWT() error = %v", err)
	}
	if uid != "123" {
		t.Fatalf("unexpected uid: got %q want %q", uid, "123")
	}
}

func TestParseUIDFromJWT_Errors(t *testing.T) {
	t.Parallel()

	t.Run("not three parts", func(t *testing.T) {
		_, err := parseUIDFromJWT("Bearer a.b")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("invalid base64 payload", func(t *testing.T) {
		_, err := parseUIDFromJWT("Bearer a.!!!.c")
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("missing uid", func(t *testing.T) {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`))
		jwt := header + "." + payload + ".sig"
		_, err := parseUIDFromJWT("Bearer " + jwt)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("uid too long", func(t *testing.T) {
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
		payloadBytes, _ := json.Marshal(map[string]any{"uid": strings.Repeat("x", 65)})
		payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
		jwt := header + "." + payload + ".sig"
		_, err := parseUIDFromJWT("Bearer " + jwt)
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("authorization header too large", func(t *testing.T) {
		// 16KB hard cap in parseUIDFromJWT.
		_, err := parseUIDFromJWT("Bearer " + strings.Repeat("a", 16*1024+1))
		if err == nil {
			t.Fatal("expected error")
		}
	})
}
