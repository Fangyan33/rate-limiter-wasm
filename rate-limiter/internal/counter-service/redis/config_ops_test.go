package redis_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/redis"
)

func setupConfigTest(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })

	client, err := redis.NewClient(redis.Config{
		Addr:      s.Addr(),
		KeyPrefix: "rl:",
	})
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })

	return s, client
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestSetGetConfig(t *testing.T) {
	s, client := setupConfigTest(t)

	cfg := models.RateLimitConfig{
		Port: 8000, Service: "ai/inference",
		APIKey:        "key001",
		MaxConcurrent: 10,
		Enabled:       true,
		Description:   "Premium tier",
	}

	// Set
	err := client.SetConfig(context.Background(), cfg)
	assert.NoError(t, err)
	assert.True(t, s.Exists("rl:service:config:ai/inference:8000:"+sha256Hex(cfg.APIKey)))
	assert.False(t, s.Exists("rl:service:config:ai/inference:8000:"+cfg.APIKey))

	// Get
	got, err := client.GetConfig(context.Background(), cfg.Service, cfg.Port, cfg.APIKey)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, cfg.Service, got.Service)
	assert.Equal(t, sha256Hex(cfg.APIKey), got.APIKeyHash)
	assert.Equal(t, cfg.MaxConcurrent, got.MaxConcurrent)
	assert.Equal(t, cfg.Enabled, got.Enabled)
	assert.Equal(t, cfg.Description, got.Description)
	assert.Greater(t, got.UpdatedAt, int64(0))
}

func TestDeleteConfig(t *testing.T) {
	s, client := setupConfigTest(t)

	cfg := models.RateLimitConfig{
		Port: 8000, Service: "ai/inference",
		APIKey:        "key001",
		MaxConcurrent: 5,
		Enabled:       true,
	}

	_ = client.SetConfig(context.Background(), cfg)

	err := client.DeleteConfig(context.Background(), cfg.Service, cfg.Port, cfg.APIKey)
	assert.NoError(t, err)
	assert.False(t, s.Exists("rl:service:config:ai/inference:8000:"+sha256Hex(cfg.APIKey)))

	got, err := client.GetConfig(context.Background(), cfg.Service, cfg.Port, cfg.APIKey)
	assert.NoError(t, err)
	assert.Nil(t, got)
}

func TestListConfigs(t *testing.T) {
	_, client := setupConfigTest(t)

	// 插入 3 个配置
	cfgs := []models.RateLimitConfig{
		{Port: 8000, Service: "ai/one", APIKey: "k1", MaxConcurrent: 2, Enabled: true},
		{Port: 8000, Service: "ai/two", APIKey: "k2", MaxConcurrent: 5, Enabled: true},
		{Port: 8000, Service: "other/three", APIKey: "k3", MaxConcurrent: 10, Enabled: true},
	}

	for _, cfg := range cfgs {
		_ = client.SetConfig(context.Background(), cfg)
	}

	result, err := client.ListConfigs(context.Background(), "rl:service:config:*", 0, 100)
	require.NoError(t, err)
	assert.Len(t, result.Configs, 3)

	// 验证内容
	found := 0
	for _, c := range result.Configs {
		if c.APIKeyHash == sha256Hex("k1") || c.APIKeyHash == sha256Hex("k2") || c.APIKeyHash == sha256Hex("k3") {
			found++
		}
	}
	assert.Equal(t, 3, found)

	// 测试分页（模拟 cursor）
	// miniredis SCAN 支持有限，这里简单验证 cursor 格式
	assert.NotEmpty(t, result.Cursor)
}

func TestListConfigs_EmptyPage(t *testing.T) {
	s, client := setupConfigTest(t)
	// SCAN 可以遍历到与 pattern 不匹配的键，并返回空配置页。
	s.Set("unrelated", "value")
	result, err := client.ListConfigs(context.Background(), "rl:service:config:*", 0, 100)
	require.NoError(t, err)
	assert.Equal(t, "0", result.Cursor)
	assert.NotNil(t, result.Configs)
	assert.Empty(t, result.Configs)
}

func TestListConfigs_RedisEmptyObjectPage(t *testing.T) {
	s, client := setupConfigTest(t)
	// 真实 Redis cjson 将空 Lua table 编码为 {}，miniredis 则编码为 []。
	// 空页也可能有非零 cursor，调用方必须能够继续遍历。
	s.Server().SetPreHook(func(peer *server.Peer, command string, _ ...string) bool {
		if command != "EVAL" {
			return false
		}
		peer.WriteBulk(`{"cursor":"17","configs":{}}`)
		return true
	})
	result, err := client.ListConfigs(context.Background(), "rl:service:config:*", 0, 2)
	require.NoError(t, err)
	assert.Equal(t, "17", result.Cursor)
	assert.NotNil(t, result.Configs)
	assert.Empty(t, result.Configs)
}

func TestSetConfig_ValidationFail(t *testing.T) {
	_, client := setupConfigTest(t)

	invalid := models.RateLimitConfig{
		Port: 8000, Service: "",
		APIKey:        "key001",
		MaxConcurrent: 0,
		Enabled:       true,
	}

	err := client.SetConfig(context.Background(), invalid)
	assert.Error(t, err)
}
