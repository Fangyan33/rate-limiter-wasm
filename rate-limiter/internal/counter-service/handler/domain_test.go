package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/redis"
)

func domainRequest(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	return w
}

func TestDomainConfigAndLeaseHTTPFlow(t *testing.T) {
	for _, keyField := range []string{`"api_key":"",`, ""} {
		t.Run("key field="+keyField, func(t *testing.T) {
			acquire, release, _ := setupTestHandler(t)
			configs := NewConfigHandler(acquire.redisClient)
			put := domainRequest(configs, http.MethodPut, "/config", `{"domain":"api.example.com",`+keyField+`"max_concurrent":1,"enabled":true}`)
			require.Equal(t, http.StatusOK, put.Code, put.Body.String())
			// A concrete key config must not bypass the domain rule.
			setTestConfig(t, acquire.redisClient, "api.example.com", "known", 100, true)
			for _, query := range []string{"", "&api_key="} {
				get := domainRequest(configs, http.MethodGet, "/config?domain=api.example.com"+query, "")
				require.Equal(t, http.StatusOK, get.Code)
				var cfg models.RateLimitConfig
				require.NoError(t, json.Unmarshal(get.Body.Bytes(), &cfg))
				require.Empty(t, cfg.APIKey)
				require.Equal(t, 1, cfg.MaxConcurrent)
				require.Equal(t, strings.TrimPrefix(testConfigKey(cfg.Domain, ""), "rl:config:"+cfg.Domain+":"), cfg.APIKeyHash)
			}
			cursor := "0"
			found := false
			for {
				list := domainRequest(configs, http.MethodGet, "/configs?limit=1&cursor="+cursor, "")
				require.Equal(t, http.StatusOK, list.Code)
				var page redis.ConfigListResult
				require.NoError(t, json.Unmarshal(list.Body.Bytes(), &page))
				for _, cfg := range page.Configs {
					require.Empty(t, cfg.APIKey)
					if "rl:config:"+cfg.Domain+":"+cfg.APIKeyHash == testConfigKey("api.example.com", "") {
						found = true
					}
				}
				cursor = page.Cursor
				if cursor == "0" {
					break
				}
			}
			require.True(t, found, "domain rule must be listed")
			first := domainRequest(acquire, http.MethodPost, "/acquire", `{"domain":"api.example.com",`+keyField+`"ttl_ms":30000}`)
			require.Equal(t, http.StatusOK, first.Code, first.Body.String())
			var lease models.AcquireResult
			require.NoError(t, json.Unmarshal(first.Body.Bytes(), &lease))
			require.True(t, lease.Allowed)
			require.NotEmpty(t, lease.LeaseID)
			second := domainRequest(acquire, http.MethodPost, "/acquire", `{"domain":"api.example.com","api_key":"known","ttl_ms":30000}`)
			require.Equal(t, http.StatusOK, second.Code)
			var denied models.AcquireResult
			require.NoError(t, json.Unmarshal(second.Body.Bytes(), &denied))
			require.False(t, denied.Allowed)
			require.Equal(t, "limit_exceeded", denied.Reason)
			released := domainRequest(release, http.MethodPost, "/release", `{"lease_id":"`+lease.LeaseID+`"}`)
			require.Equal(t, http.StatusOK, released.Code)
			var releaseResult models.ReleaseResult
			require.NoError(t, json.Unmarshal(released.Body.Bytes(), &releaseResult))
			require.True(t, releaseResult.Released)
			deleteQuery := ""
			if keyField != "" {
				deleteQuery = "&api_key="
			}
			deleted := domainRequest(configs, http.MethodDelete, "/config?domain=api.example.com"+deleteQuery, "")
			require.Equal(t, http.StatusOK, deleted.Code)
			get := domainRequest(configs, http.MethodGet, "/config?domain=api.example.com", "")
			require.Equal(t, http.StatusNotFound, get.Code)
			missing := domainRequest(acquire, http.MethodPost, "/acquire", `{"domain":"api.example.com","ttl_ms":30000}`)
			require.Equal(t, http.StatusOK, missing.Code)
			require.NoError(t, json.Unmarshal(missing.Body.Bytes(), &denied))
			require.False(t, denied.Allowed)
			require.Equal(t, "config_not_found", denied.Reason)
			known := domainRequest(configs, http.MethodGet, "/config?domain=api.example.com&api_key=known", "")
			require.Equal(t, http.StatusOK, known.Code, "deleting domain rule must preserve key rule")
		})
	}
}

func TestDomainConfigHTTPValidation(t *testing.T) {
	acquire, _, _ := setupTestHandler(t)
	configs := NewConfigHandler(acquire.redisClient)
	for _, method := range []string{http.MethodPut, http.MethodGet, http.MethodDelete} {
		res := domainRequest(configs, method, "/config", `{"max_concurrent":1}`)
		require.Equal(t, http.StatusBadRequest, res.Code)
		require.Contains(t, res.Body.String(), "domain")
	}
	res := domainRequest(configs, http.MethodPut, "/config", `{"domain":"api.example.com","max_concurrent":0}`)
	require.Equal(t, http.StatusBadRequest, res.Code)
}
