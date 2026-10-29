package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
)

func TestConfigWriteValidationDoesNotMutateRedis(t *testing.T) {
	acquire, _, mr := setupTestHandler(t)
	configs := NewConfigHandler(acquire.redisClient)
	original := `{"service":"ai/inference","port":8000,"api_key":"known","max_concurrent":2,"enabled":true,"description":"original"}`
	require.Equal(t, 200, serviceRequest(configs, http.MethodPut, "/config", original).Code)
	key := testConfigKey("ai/inference", "known")
	before, err := acquire.redisClient.Rdb().HGetAll(context.Background(), key).Result()
	require.NoError(t, err)
	cases := map[string]any{
		"missing enabled": nil, "null enabled": nil, "string enabled": "true", "number enabled": 1,
		"bad hash": "wrong", "empty hash": "", "null hash": nil, "number hash": 1,
		"null key": nil, "number key": 1, "null description": nil, "removed tier": "premium",
		"null tier": nil, "fraction limit": 1.5,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			payload := map[string]any{"service": "ai/inference", "port": 8000, "api_key": "known", "max_concurrent": 3, "enabled": true}
			switch name {
			case "missing enabled":
				delete(payload, "enabled")
			case "null enabled", "string enabled", "number enabled":
				payload["enabled"] = value
			case "bad hash", "empty hash", "null hash", "number hash":
				payload["api_key_hash"] = value
			case "null key", "number key":
				payload["api_key"] = value
			case "null description":
				payload["description"] = value
			case "removed tier", "null tier":
				payload["tier"] = value
			case "fraction limit":
				payload["max_concurrent"] = value
			}
			body, err := json.Marshal(payload)
			require.NoError(t, err)
			result := serviceRequest(configs, http.MethodPut, "/config", string(body))
			require.Equal(t, 400, result.Code, result.Body.String())
			after, err := acquire.redisClient.Rdb().HGetAll(context.Background(), key).Result()
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, []string{key}, mr.Keys())
		})
	}
	for _, body := range []string{original + ` {}`, original + ` null`, original + ` garbage`} {
		require.Equal(t, 400, serviceRequest(configs, http.MethodPut, "/config", body).Code)
	}
	for _, suffix := range []string{
		`,"api_key_hash":"wrong","API_KEY_HASH":null}`,
		`,"api_key_hash":"wrong","API_KEY_HASH":"` + models.HashAPIKey("known") + `"}`,
		`,"Tier":null}`,
	} {
		body := strings.TrimSuffix(original, "}") + suffix
		require.Equal(t, 400, serviceRequest(configs, http.MethodPut, "/config", body).Code)
	}
}

func TestConfigViewsHashGuardAndNormalizedCRUD(t *testing.T) {
	acquire, _, mr := setupTestHandler(t)
	configs := NewConfigHandler(acquire.redisClient)
	payload := map[string]any{"service": "ai/inference", "port": 8000, "api_key": "Known key ", "api_key_hash": models.HashAPIKey("Known key "), "enabled": true, "max_concurrent": 2, "description": "保留描述"}
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	result := serviceRequest(configs, http.MethodPut, "/config", string(body))
	require.Equal(t, 200, result.Code, result.Body.String())
	key := testConfigKey("ai/inference", "Known key ")
	require.Equal(t, []string{key}, mr.Keys())
	fields, err := acquire.redisClient.Rdb().HGetAll(context.Background(), key).Result()
	require.NoError(t, err)
	require.NotContains(t, fields, "api_key")
	require.NotContains(t, fields, "api_key_hash")
	require.NotContains(t, fields, "tier")
	get := serviceRequest(configs, http.MethodGet, "/config?service=ai/inference&port=8000&api_key=Known+key+", "")
	require.Equal(t, 200, get.Code, get.Body.String())
	var view map[string]any
	require.NoError(t, json.Unmarshal(get.Body.Bytes(), &view))
	require.Equal(t, "ai/inference", view["service"])
	require.Equal(t, models.HashAPIKey("Known key "), view["api_key_hash"])
	require.NotContains(t, view, "api_key")
	require.NotContains(t, view, "tier")
	list := serviceRequest(configs, http.MethodGet, "/configs", "")
	require.Equal(t, 200, list.Code)
	var page struct {
		Configs []map[string]any `json:"configs"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &page))
	require.Len(t, page.Configs, 1)
	require.NotContains(t, page.Configs[0], "api_key")
	listed, err := json.Marshal(page.Configs[0])
	require.NoError(t, err)
	require.Equal(t, 400, serviceRequest(configs, http.MethodPut, "/config", string(listed)).Code)
	require.Equal(t, 400, serviceRequest(configs, http.MethodPut, "/config", get.Body.String()).Code)
	require.Equal(t, []string{key}, mr.Keys(), "query round-trip must not create a domain rule")
	require.Equal(t, 200, serviceRequest(configs, http.MethodDelete, "/config?service=ai/inference&port=8000&api_key=Known+key+", "").Code)
	require.Empty(t, mr.Keys())
	// Empty-key views can round-trip because their hash correctly identifies a domain rule.
	hash := models.HashAPIKey("")
	put := serviceRequest(configs, http.MethodPut, "/config", `{"service":"ai/inference","port":8000,"max_concurrent":1,"enabled":false,"api_key_hash":"`+hash+`"}`)
	require.Equal(t, 200, put.Code, put.Body.String())
	require.Equal(t, "false", mr.HGet(testConfigKey("ai/inference", ""), "enabled"))
}

func TestDescriptionOmittedPreservesAndExplicitEmptyClears(t *testing.T) {
	acquire, _, mr := setupTestHandler(t)
	configs := NewConfigHandler(acquire.redisClient)
	for _, tc := range []struct{ extra, want string }{
		{`,"description":"first"`, "first"},
		{"", "first"},
		{`,"description":"second"`, "second"},
		{`,"description":""`, ""},
	} {
		body := `{"service":"ai/inference","port":8000,"max_concurrent":1,"enabled":true` + tc.extra + `}`
		result := serviceRequest(configs, http.MethodPut, "/config", body)
		require.Equal(t, 200, result.Code, result.Body.String())
		require.Equal(t, tc.want, mr.HGet(testConfigKey("ai/inference", ""), "description"))
	}
	fields, err := acquire.redisClient.Rdb().HGetAll(context.Background(), testConfigKey("ai/inference", "")).Result()
	require.NoError(t, err)
	require.NotContains(t, fields, "description")
}

func TestCorruptRedisConfigIsNotReportedAsDisabled(t *testing.T) {
	for _, field := range []string{"enabled", "max_concurrent"} {
		for _, value := range []string{"<missing>", "garbage", "TRUE", "0", "1.5"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				acquire, _, mr := setupTestHandler(t)
				configs := NewConfigHandler(acquire.redisClient)
				setTestConfig(t, acquire.redisClient, "ai/inference", "known", 1, true)
				setTestConfig(t, acquire.redisClient, "ai/inference", "", 100, true)
				key := testConfigKey("ai/inference", "known")
				if value == "<missing>" {
					mr.HDel(key, field)
				} else {
					mr.HSet(key, field, value)
				}
				result := serviceRequest(acquire, http.MethodPost, "/acquire", `{"upstream_cluster":"outbound|8000||inference.ai.svc.cluster.local","api_key":"known","ttl_ms":1000}`)
				require.Equal(t, 500, result.Code, result.Body.String())
				require.Contains(t, result.Body.String(), `"reason":"invalid_config"`)
				for _, path := range []string{"/config?service=ai/inference&port=8000&api_key=known", "/configs"} {
					get := serviceRequest(configs, http.MethodGet, path, "")
					require.Equal(t, 500, get.Code, get.Body.String())
					require.NotContains(t, get.Body.String(), `"enabled":false`)
				}
				for _, key := range mr.Keys() {
					require.False(t, strings.HasPrefix(key, "rl:lease"), key)
				}
			})
		}
	}
}
