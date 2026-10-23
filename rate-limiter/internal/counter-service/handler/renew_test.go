package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
)

func TestRenewHandler(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		status int
		reason string
	}{
		{"active", 200, ""},
		{"released", 200, "lease_not_found"},
		{"expired", 200, "lease_not_found"},
		{"unknown", 200, "lease_not_found"},
		{"redis unavailable", 503, "redis_unavailable"},
		{"redis script error", 500, "internal_error"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			acquire, _, mr := setupTestHandler(t)
			setTestConfig(t, acquire.redisClient, "api.example.com", "key001", 1, true)
			lease, err := acquire.redisClient.Acquire(context.Background(), models.AcquireRequest{
				Domain: "api.example.com", APIKey: "key001", TTLMS: 3000,
			})
			require.NoError(t, err)
			switch scenario.name {
			case "released":
				_, err = acquire.redisClient.Release(context.Background(), lease.LeaseID)
				require.NoError(t, err)
			case "expired":
				mr.FastForward(4 * time.Second)
			case "unknown":
				lease.LeaseID = "unknown"
			case "redis unavailable":
				mr.Close()
			case "redis script error":
				mr.SetError("ERR injected failure")
			}
			body, err := json.Marshal(models.RenewRequest{LeaseID: lease.LeaseID, TTLMS: 3000})
			require.NoError(t, err)
			w := httptest.NewRecorder()
			NewRenewHandler(acquire.redisClient).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/renew", bytes.NewReader(body)))
			require.Equal(t, scenario.status, w.Code)
			var result models.RenewResult
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
			require.Equal(t, scenario.reason, result.Reason)
			require.Equal(t, scenario.name == "active", result.Renewed)
		})
	}
}

func TestRenewHandlerRejectsInvalidRequests(t *testing.T) {
	acquire, _, _ := setupTestHandler(t)
	h := NewRenewHandler(acquire.redisClient)
	for _, tc := range []struct {
		method string
		body   string
		status int
	}{
		{"GET", `{}`, 405},
		{"POST", `{`, 400},
		{"POST", `{"ttl_ms":3000}`, 400},
		{"POST", `{"lease_id":"lease"}`, 400},
		{"POST", `{"lease_id":"lease","ttl_ms":-1}`, 400},
	} {
		t.Run(tc.method+tc.body, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(tc.method, "/renew", bytes.NewBufferString(tc.body)))
			require.Equal(t, tc.status, w.Code)
		})
	}
}
