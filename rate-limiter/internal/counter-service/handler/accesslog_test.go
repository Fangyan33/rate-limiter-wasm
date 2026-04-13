package handler

import (
	"bytes"
	"encoding/hex"
	"hash/fnv"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAcquireHandler_WritesAccessLogWithMaskedAPIKey(t *testing.T) {
	t.Skip("Access logging not implemented yet")

	acquireHandler, _, mr := setupTestHandler(t)

	mr.HSet("rl:config:api.example.com:test-key", "max_concurrent", "5")
	mr.HSet("rl:config:api.example.com:test-key", "enabled", "true")

	// Capture standard logger output.
	var buf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	}()

	body := strings.NewReader(`{"domain":"api.example.com","api_key":"test-key","ttl_ms":30000}`)
	req := httptest.NewRequest(http.MethodPost, "/acquire", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	acquireHandler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	out := buf.String()
	if !strings.Contains(out, "path=/acquire") {
		t.Fatalf("expected access log to include path=/acquire, got: %s", out)
	}
	if !strings.Contains(out, "method=POST") {
		t.Fatalf("expected access log to include method=POST, got: %s", out)
	}
	if !strings.Contains(out, "status=200") {
		t.Fatalf("expected access log to include status=200, got: %s", out)
	}

	// Ensure api_key is not logged in plaintext.
	if strings.Contains(out, "api_key_raw=test-key") || strings.Contains(out, "test-key") {
		t.Fatalf("expected access log to NOT contain raw api_key, got: %s", out)
	}

	// But it should contain a stable masked/hash identifier.
	// We assert against our chosen short hash implementation (FNV-1a 64). This is
	// enough to correlate logs without leaking the full token.
	h := fnv.New64a()
	_, _ = h.Write([]byte("test-key"))
	expectedHash := hex.EncodeToString(h.Sum(nil))[:12]
	if !strings.Contains(out, "api_key_hash="+expectedHash) {
		t.Fatalf("expected access log to include api_key_hash=%s, got: %s", expectedHash, out)
	}
	if !strings.Contains(out, "api_key_mask=") {
		t.Fatalf("expected access log to include api_key_mask, got: %s", out)
	}
}
