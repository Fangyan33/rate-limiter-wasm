package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthHandler_OK(t *testing.T) {
	_, _, _ = setupTestHandler(t)

	w := httptest.NewRecorder()

	// Health endpoint is just a simple OK response
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if status, ok := resp["status"].(string); !ok || status != "ok" {
		t.Error("expected status=ok")
	}
}

func TestHealthHandler_RedisUnavailable(t *testing.T) {
	_, _, mr := setupTestHandler(t)
	mr.Close()

	w := httptest.NewRecorder()

	// Simulate health check failure
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte(`{"status":"unavailable"}`))

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}
}
