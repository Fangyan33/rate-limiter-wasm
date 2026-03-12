package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"rate-limiter-wasm/internal/counter-service/redis"
)

func setupTestHandler(t *testing.T) (*Handler, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	client, err := redis.NewClient(mr.Addr(), "", 0, 10, 3)
	if err != nil {
		t.Fatalf("failed to create redis client: %v", err)
	}

	handler := NewHandler(client)
	return handler, mr
}

func TestAcquireHandler_Success(t *testing.T) {
	handler, _ := setupTestHandler(t)

	reqBody := map[string]interface{}{
		"api_key": "test-key",
		"limit":   5,
		"ttl_ms":  30000,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Acquire(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if allowed, ok := resp["allowed"].(bool); !ok || !allowed {
		t.Error("expected allowed=true")
	}

	if leaseID, ok := resp["lease_id"].(string); !ok || leaseID == "" {
		t.Error("expected non-empty lease_id")
	}
}

func TestAcquireHandler_LimitReached(t *testing.T) {
	handler, _ := setupTestHandler(t)

	reqBody := map[string]interface{}{
		"api_key": "test-key",
		"limit":   2,
		"ttl_ms":  30000,
	}

	// Acquire twice to reach limit
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.Acquire(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("acquire %d failed with status %d", i+1, w.Code)
		}
	}

	// Third acquire should be denied
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Acquire(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)

	if allowed, ok := resp["allowed"].(bool); !ok || allowed {
		t.Error("expected allowed=false when limit reached")
	}

	if leaseID, ok := resp["lease_id"].(string); ok && leaseID != "" {
		t.Error("expected empty lease_id when denied")
	}
}

func TestAcquireHandler_InvalidJSON(t *testing.T) {
	handler, _ := setupTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader([]byte("invalid json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Acquire(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

func TestAcquireHandler_ValidationError(t *testing.T) {
	handler, _ := setupTestHandler(t)

	tests := []struct {
		name    string
		reqBody map[string]interface{}
	}{
		{
			name: "empty api_key",
			reqBody: map[string]interface{}{
				"api_key": "",
				"limit":   5,
				"ttl_ms":  30000,
			},
		},
		{
			name: "invalid limit",
			reqBody: map[string]interface{}{
				"api_key": "test-key",
				"limit":   0,
				"ttl_ms":  30000,
			},
		},
		{
			name: "invalid ttl_ms",
			reqBody: map[string]interface{}{
				"api_key": "test-key",
				"limit":   5,
				"ttl_ms":  0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.reqBody)
			req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handler.Acquire(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected status 400, got %d", w.Code)
			}
		})
	}
}

func TestAcquireHandler_RedisNetworkError(t *testing.T) {
	handler, mr := setupTestHandler(t)

	// Close miniredis to simulate network error
	mr.Close()

	reqBody := map[string]interface{}{
		"api_key": "test-key",
		"limit":   5,
		"ttl_ms":  30000,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.Acquire(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}
}
