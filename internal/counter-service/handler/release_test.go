package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleaseHandler_Success(t *testing.T) {
	acquireHandler, releaseHandler, mr := setupTestHandler(t)

	mr.HSet("rl:config:api.example.com:test-key", "max_concurrent", "5")
	mr.HSet("rl:config:api.example.com:test-key", "enabled", "true")

	acquireBody, _ := json.Marshal(map[string]interface{}{
		"domain":  "api.example.com",
		"api_key": "test-key",
		"ttl_ms":  30000,
	})
	acquireReq := httptest.NewRequest(http.MethodPost, "/acquire", bytes.NewReader(acquireBody))
	acquireReq.Header.Set("Content-Type", "application/json")
	acquireResp := httptest.NewRecorder()
	acquireHandler.ServeHTTP(acquireResp, acquireReq)

	var acquireResult map[string]interface{}
	if err := json.NewDecoder(acquireResp.Body).Decode(&acquireResult); err != nil {
		t.Fatalf("failed to decode acquire response: %v", err)
	}

	releaseBody, _ := json.Marshal(map[string]interface{}{
		"lease_id": acquireResult["lease_id"],
	})
	releaseReq := httptest.NewRequest(http.MethodPost, "/release", bytes.NewReader(releaseBody))
	releaseReq.Header.Set("Content-Type", "application/json")
	releaseResp := httptest.NewRecorder()

	releaseHandler.ServeHTTP(releaseResp, releaseReq)

	if releaseResp.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", releaseResp.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(releaseResp.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if released, ok := resp["released"].(bool); !ok || !released {
		t.Error("expected released=true")
	}
}

func TestReleaseHandler_LeaseNotFound(t *testing.T) {
	_, releaseHandler, _ := setupTestHandler(t)

	body, _ := json.Marshal(map[string]interface{}{
		"lease_id": "non-existent",
	})
	req := httptest.NewRequest(http.MethodPost, "/release", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	releaseHandler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if released, ok := resp["released"].(bool); !ok || released {
		t.Error("expected released=false")
	}
}

func TestReleaseHandler_InvalidJSON(t *testing.T) {
	_, releaseHandler, _ := setupTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/release", bytes.NewReader([]byte("invalid json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	releaseHandler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

func TestReleaseHandler_ValidationError(t *testing.T) {
	_, releaseHandler, _ := setupTestHandler(t)

	tests := []struct {
		name string
		body map[string]interface{}
	}{
		{
			name: "empty lease_id",
			body: map[string]interface{}{
				"lease_id": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/release", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			releaseHandler.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected status 400, got %d", w.Code)
			}
		})
	}
}

func TestReleaseHandler_RedisNetworkError(t *testing.T) {
	_, releaseHandler, mr := setupTestHandler(t)
	mr.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"lease_id": "lease-1",
	})
	req := httptest.NewRequest(http.MethodPost, "/release", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	releaseHandler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", w.Code)
	}
}
