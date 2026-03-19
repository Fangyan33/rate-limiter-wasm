package redis_test

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"rate-limiter-wasm/internal/counter-service/models"
	"rate-limiter-wasm/internal/counter-service/redis"
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

func TestSetGetConfig(t *testing.T) {
	_, client := setupConfigTest(t)

	cfg := models.RateLimitConfig{
		Domain:        "api.example.com",
		APIKey:        "key001",
		MaxConcurrent: 10,
		Enabled:       true,
		Tier:          "premium",
		Description:   "Premium tier",
	}

	// Set
	err := client.SetConfig(context.Background(), cfg)
	assert.NoError(t, err)

	// Get
	got, err := client.GetConfig(context.Background(), cfg.Domain, cfg.APIKey)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, cfg.Domain, got.Domain)
	assert.Equal(t, cfg.APIKey, got.APIKey)
	assert.Equal(t, cfg.MaxConcurrent, got.MaxConcurrent)
	assert.Equal(t, cfg.Enabled, got.Enabled)
	assert.Equal(t, cfg.Tier, got.Tier)
	assert.Equal(t, cfg.Description, got.Description)
	assert.Greater(t, got.UpdatedAt, int64(0))
}

func TestDeleteConfig(t *testing.T) {
	_, client := setupConfigTest(t)

	cfg := models.RateLimitConfig{
		Domain:        "api.example.com",
		APIKey:        "key001",
		MaxConcurrent: 5,
		Enabled:       true,
	}

	_ = client.SetConfig(context.Background(), cfg)

	err := client.DeleteConfig(context.Background(), cfg.Domain, cfg.APIKey)
	assert.NoError(t, err)

	got, err := client.GetConfig(context.Background(), cfg.Domain, cfg.APIKey)
	assert.NoError(t, err)
	assert.Nil(t, got)
}

func TestListConfigs(t *testing.T) {
	_, client := setupConfigTest(t)

	// 插入 3 个配置
	cfgs := []models.RateLimitConfig{
		{Domain: "api1.com", APIKey: "k1", MaxConcurrent: 2, Enabled: true},
		{Domain: "api2.com", APIKey: "k2", MaxConcurrent: 5, Enabled: true},
		{Domain: "*.example.com", APIKey: "k3", MaxConcurrent: 10, Enabled: true},
	}

	for _, cfg := range cfgs {
		_ = client.SetConfig(context.Background(), cfg)
	}

	result, err := client.ListConfigs(context.Background(), "rl:config:*", 0, 100)
	require.NoError(t, err)
	assert.Len(t, result.Configs, 3)

	// 验证内容
	found := 0
	for _, c := range result.Configs {
		if c.APIKey == "k1" || c.APIKey == "k2" || c.APIKey == "k3" {
			found++
		}
	}
	assert.Equal(t, 3, found)

	// 测试分页（模拟 cursor）
	// miniredis SCAN 支持有限，这里简单验证 cursor 格式
	assert.NotEmpty(t, result.Cursor)
}

func TestSetConfig_ValidationFail(t *testing.T) {
	_, client := setupConfigTest(t)

	invalid := models.RateLimitConfig{
		Domain:        "",
		APIKey:        "key001",
		MaxConcurrent: 0,
		Enabled:       true,
	}

	err := client.SetConfig(context.Background(), invalid)
	assert.Error(t, err)
}
