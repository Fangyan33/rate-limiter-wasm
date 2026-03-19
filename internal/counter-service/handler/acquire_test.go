package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"rate-limiter-wasm/internal/counter-service/redis"
)

func setupTestHandler(t *testing.T) (*AcquireHandler, *ReleaseHandler, *miniredis.Miniredis) {
	t.Helper()

	mr := miniredis.RunT(t)
	client, err := redis.NewClient(redis.Config{
		Addr:      mr.Addr(),
		KeyPrefix: "rl:",
	})
	if err != nil {
		t.Fatalf("failed to create redis client: %v", err)
	}

	acquireHandler := NewAcquireHandler(client)
	releaseHandler := NewReleaseHandler(client)
	return acquireHandler, releaseHandler, mr
}

func TestAcquireHandler_Success(t *testing.T) {
	handler, _, mr := setupTestHandler(t)

	mr.HSet("rl:config:api.example.com:test-key", "max_concurrent", "5")
	mr.HSet("rl:config:api.example.com:test-key", "enabled", "true")

	reqBody := map[string]interface{}{
		"domain": "api.example.com",
		"api_key": "test-key",
		"ttl_ms":  30000,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

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
	handler, _, mr := setupTestHandler(t)

	mr.HSet("rl:config:api.example.com:test-key", "max_concurrent", "2")
	mr.HSet("rl:config:api.example.com:test-key", "enabled", "true")

	reqBody := map[string]interface{}{
		"domain": "api.example.com",
		"api_key": "test-key",
		"ttl_ms":  30000,
	}

	// Acquire twice to reach limit
	for i := 0; i < 2; i++ {
		body, _ := json.Marshal(reqBody)
		req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("acquire %d failed with status %d", i+1, w.Code)
		}
	}

	// Third acquire should be denied
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)

	if allowed, ok := resp["allowed"].(bool); !ok || allowed {
		t.Error("expected allowed=false when limit reached")
	}

	if reason, ok := resp["reason"].(string); !ok || reason != "limit_exceeded" {
		t.Errorf("expected reason=limit_exceeded, got %#v", resp["reason"])
	}

	if leaseID, ok := resp["lease_id"].(string); ok && leaseID != "" {
		t.Error("expected empty lease_id when denied")
	}
}

func TestAcquireHandler_ConfigMiss(t *testing.T) {
	handler, _, _ := setupTestHandler(t)

	reqBody := map[string]interface{}{
		"domain": "api.example.com",
		"api_key": "missing-config-key",
		"ttl_ms":  30000,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if allowed, ok := resp["allowed"].(bool); !ok || allowed {
		t.Errorf("expected allowed=false for config miss, got %#v", resp["allowed"])
	}
	if reason, ok := resp["reason"].(string); !ok || reason != "config_not_found" {
		t.Errorf("expected reason=config_not_found, got %#v", resp["reason"])
	}
}

func TestAcquireHandler_DisabledConfig(t *testing.T) {
	handler, _, mr := setupTestHandler(t)

	mr.HSet("rl:config:api.example.com:disabled-key", "max_concurrent", "5")
	mr.HSet("rl:config:api.example.com:disabled-key", "enabled", "false")

	reqBody := map[string]interface{}{
		"domain": "api.example.com",
		"api_key": "disabled-key",
		"ttl_ms":  30000,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if allowed, ok := resp["allowed"].(bool); !ok || allowed {
		t.Errorf("expected allowed=false for disabled config, got %#v", resp["allowed"])
	}
	if reason, ok := resp["reason"].(string); !ok || reason != "api_key_disabled" {
		t.Errorf("expected reason=api_key_disabled, got %#v", resp["reason"])
	}
}

func TestAcquireHandler_InvalidConfig(t *testing.T) {
	handler, _, mr := setupTestHandler(t)

	mr.HSet("rl:config:api.example.com:invalid-config-key", "max_concurrent", "0")
	mr.HSet("rl:config:api.example.com:invalid-config-key", "enabled", "true")

	reqBody := map[string]interface{}{
		"domain": "api.example.com",
		"api_key": "invalid-config-key",
		"ttl_ms":  30000,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if allowed, ok := resp["allowed"].(bool); !ok || allowed {
		t.Errorf("expected allowed=false for invalid config, got %#v", resp["allowed"])
	}
	if reason, ok := resp["reason"].(string); !ok || reason != "invalid_config" {
		t.Errorf("expected reason=invalid_config, got %#v", resp["reason"])
	}
}

func TestAcquireHandler_InvalidJSON(t *testing.T) {
	handler, _, _ := setupTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader([]byte("invalid json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

func TestAcquireHandler_ValidationError(t *testing.T) {
	handler, _, _ := setupTestHandler(t)

	tests := []struct {
		name    string
		reqBody map[string]interface{}
	}{
		{
			name: "empty domain",
			reqBody: map[string]interface{}{
				"domain": "",
				"api_key": "test-key",
				"ttl_ms":  30000,
			},
		},
		{
			name: "empty api_key",
			reqBody: map[string]interface{}{
				"domain": "api.example.com",
				"api_key": "",
				"ttl_ms":  30000,
			},
		},
		{
			name: "invalid ttl_ms",
			reqBody: map[string]interface{}{
				"domain": "api.example.com",
				"api_key": "test-key",
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

			handler.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected status 400, got %d", w.Code)
			}
		})
	}
}

func TestAcquireHandler_LeaseExpiredAutoRecovery(t *testing.T) {
	handler, _, mr := setupTestHandler(t)

	mr.HSet("rl:config:api.example.com:test-key", "max_concurrent", "1")
	mr.HSet("rl:config:api.example.com:test-key", "enabled", "true")

	reqBody := map[string]interface{}{
		"domain":  "api.example.com",
		"api_key": "test-key",
		"ttl_ms":  100,
	}

	body1, _ := json.Marshal(reqBody)
	req1 := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body1))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("expected first acquire status 200, got %d", w1.Code)
	}

	mr.FastForward(200 * time.Millisecond)
	time.Sleep(120 * time.Millisecond)

	body2, _ := json.Marshal(reqBody)
	req2 := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected second acquire status 200, got %d", w2.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w2.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if allowed, ok := resp["allowed"].(bool); !ok || !allowed {
		t.Fatalf("expected allowed=true after lease expiry, got %#v", resp["allowed"])
	}
}

func TestAcquireHandler_WildcardFallback(t *testing.T) {
	handler, _, mr := setupTestHandler(t)

	mr.HSet("rl:config:*.example.com:test-key", "max_concurrent", "3")
	mr.HSet("rl:config:*.example.com:test-key", "enabled", "true")

	reqBody := map[string]interface{}{
		"domain":  "api.example.com",
		"api_key": "test-key",
		"ttl_ms":  30000,
	}
	body, _ := json.Marshal(reqBody)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if allowed, ok := resp["allowed"].(bool); !ok || !allowed {
		t.Fatalf("expected allowed=true, got %#v", resp["allowed"])
	}
}

func TestAcquireHandler_GlobalFallback(t *testing.T) {
	handler, _, mr := setupTestHandler(t)

	mr.HSet("rl:config:*:test-key", "max_concurrent", "4")
	mr.HSet("rl:config:*:test-key", "enabled", "true")

	reqBody := map[string]interface{}{
		"domain":  "any.domain.com",
		"api_key": "test-key",
		"ttl_ms":  30000,
	}
	body, _ := json.Marshal(reqBody)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if allowed, ok := resp["allowed"].(bool); !ok || !allowed {
		t.Fatalf("expected allowed=true, got %#v", resp["allowed"])
	}
}

func TestAcquireHandler_RedisNetworkError(t *testing.T) {
	handler, _, mr := setupTestHandler(t)

	// Close miniredis to simulate network error
	mr.Close()

	reqBody := map[string]interface{}{
		"domain": "api.example.com",
		"api_key": "test-key",
		"ttl_ms":  30000,
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}
}
