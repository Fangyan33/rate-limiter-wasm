package handler

import (
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
	"strings"
	"testing"
)

func serviceRequest(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}
func TestServiceConfigAndLeaseFlow(t *testing.T) {
	acquire, release, mr := setupTestHandler(t)
	configs := NewConfigHandler(acquire.redisClient)
	put := serviceRequest(configs, "PUT", "/config", `{"service":"ai/inference","port":8000,"max_concurrent":1,"enabled":true}`)
	require.Equal(t, 200, put.Code, put.Body.String())
	setTestConfig(t, acquire.redisClient, "ai/inference", "disabled", 100, false)
	for _, extra := range []string{"", "&api_key="} {
		get := serviceRequest(configs, "GET", "/config?service=ai/inference&port=8000"+extra, "")
		require.Equal(t, 200, get.Code)
		var cfg models.ConfigView
		require.NoError(t, json.Unmarshal(get.Body.Bytes(), &cfg))
		require.Equal(t, models.HashAPIKey(""), cfg.APIKeyHash)
		require.Equal(t, "ai/inference", cfg.Service)
		require.Equal(t, 8000, cfg.Port)
	}
	first := serviceRequest(acquire, "POST", "/acquire", `{"upstream_cluster":"outbound|8000||inference.ai.svc.cluster.local","ttl_ms":30000}`)
	require.Equal(t, 200, first.Code)
	var lease models.AcquireResult
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &lease))
	require.NotEmpty(t, lease.LeaseID)
	second := serviceRequest(acquire, "POST", "/acquire", `{"upstream_cluster":"outbound|8000|v2|inference.ai.svc.cluster.local","api_key":"disabled","ttl_ms":30000}`)
	require.Equal(t, 200, second.Code)
	require.Contains(t, second.Body.String(), `"reason":"limit_exceeded"`)
	released := serviceRequest(release, "POST", "/release", fmt.Sprintf(`{"lease_id":%q}`, lease.LeaseID))
	require.Equal(t, 200, released.Code)
	require.Contains(t, released.Body.String(), `"released":true`)
	require.Equal(t, 200, serviceRequest(configs, "DELETE", "/config?service=ai/inference&port=8000", "").Code)
	require.Equal(t, 404, serviceRequest(configs, "GET", "/config?service=ai/inference&port=8000", "").Code)
	unmetered := serviceRequest(acquire, "POST", "/acquire", `{"upstream_cluster":"outbound|8000||inference.ai.svc.cluster.local","ttl_ms":30000}`)
	require.JSONEq(t, `{"allowed":true}`, unmetered.Body.String())
	for _, key := range mr.Keys() {
		require.False(t, strings.HasPrefix(key, "rl:lease:"))
	}
}
func TestConfigRejectsInvalidIdentityAndUnknownParameters(t *testing.T) {
	acquire, _, mr := setupTestHandler(t)
	configs := NewConfigHandler(acquire.redisClient)
	for _, service := range []string{"inference", "inference.ai.svc.cluster.local", "*", "ai/*", "AI/inference", "ai/"} {
		body := fmt.Sprintf(`{"service":%q,"port":8000,"enabled":true,"max_concurrent":1}`, service)
		res := serviceRequest(configs, "PUT", "/config", body)
		require.Equal(t, 400, res.Code, body)
	}
	for _, port := range []string{"null", "0", "65536", "1.5", `"8000"`} {
		body := fmt.Sprintf(`{"service":"ai/inference","port":%s,"enabled":true,"max_concurrent":1}`, port)
		require.Equal(t, 400, serviceRequest(configs, "PUT", "/config", body).Code)
	}
	require.Equal(t, 400, serviceRequest(configs, "PUT", "/config", `{"service":"ai/inference","port":8000,"domain":"x","enabled":true,"max_concurrent":1}`).Code)
	for _, path := range []string{"/config?domain=x", "/config?service=ai/inference&port=8000&domain=x", "/config?service=ai/inference&port=8000&port=9000", "/config?service=ai/inference", "/configs?domain=x"} {
		require.Equal(t, 400, serviceRequest(configs, "GET", path, "").Code, path)
	}
	require.Empty(t, mr.Keys())
}
func TestUnrecognizedTargetsSkipRedisAndCustomClusterDomain(t *testing.T) {
	acquire, _, mr := setupTestHandler(t)
	setTestConfig(t, acquire.redisClient, "ai/inference", "", 1, true)
	custom := NewAcquireHandlerWithClusterDomain(acquire.redisClient, "corp.internal")
	req := `{"upstream_cluster":"outbound|8000||inference.ai.svc.corp.internal","ttl_ms":30000}`
	res := serviceRequest(custom, "POST", "/acquire", req)
	require.Equal(t, 200, res.Code)
	require.Contains(t, res.Body.String(), `"lease_id"`)
	mr.Close()
	for _, h := range []*AcquireHandler{acquire, NewAcquireHandlerWithClusterDomain(acquire.redisClient, "invalid.")} {
		for _, cluster := range []string{"PassthroughCluster", "outbound|8000||example.com", "outbound|8000||inference.ai.svc.corp.internal"} {
			res := serviceRequest(h, "POST", "/acquire", fmt.Sprintf(`{"upstream_cluster":%q,"ttl_ms":30000}`, cluster))
			require.Equal(t, 200, res.Code)
			require.JSONEq(t, `{"allowed":true}`, res.Body.String())
		}
	}
}
func TestAcquireRejectsUnknownFieldsAndMultipleObjects(t *testing.T) {
	acquire, _, _ := setupTestHandler(t)
	for _, body := range []string{`{"domain":"example.com","ttl_ms":30000}`, `{"upstream_cluster":"PassthroughCluster","ttl_ms":30000,"service":"ai/inference"}`, `{"upstream_cluster":"PassthroughCluster","ttl_ms":30000} {}`} {
		require.Equal(t, 400, serviceRequest(acquire, "POST", "/acquire", body).Code)
	}
}

func TestRuntimeEndpointsRejectUnknownQueryParameters(t *testing.T) {
	acquire, release, _ := setupTestHandler(t)
	renew := NewRenewHandler(acquire.redisClient)
	for path, h := range map[string]http.Handler{"/acquire": acquire, "/release": release, "/renew": renew} {
		result := serviceRequest(h, "POST", path+"?domain=example.com", `{}`)
		require.Equal(t, 400, result.Code)
		require.Contains(t, result.Body.String(), "query parameter")
	}
}
