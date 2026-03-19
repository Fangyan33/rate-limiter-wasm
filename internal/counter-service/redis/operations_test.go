package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"rate-limiter-wasm/internal/counter-service/models"
	"rate-limiter-wasm/internal/counter-service/redis"
)

func setupTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
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

func TestAcquire_ExactMatch(t *testing.T) {
	s, client := setupTestRedis(t)

	// 准备精确配置
	s.HSet("rl:config:api.example.com:key001", "max_concurrent", "5")
	s.HSet("rl:config:api.example.com:key001", "enabled", "true")
	s.HSet("rl:config:api.example.com:key001", "tier", "premium")

	ctx := context.Background()
	result, err := client.Acquire(ctx, models.AcquireRequest{
		Domain: "api.example.com",
		APIKey: "key001",
		TTLMS:  30000,
	})

	assert.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.NotEmpty(t, result.LeaseID)
	assert.Equal(t, 5, result.MaxConcurrent)
	assert.Equal(t, 1, result.CurrentCount)
	assert.Equal(t, "premium", result.Tier)

	members, err := s.ZMembers("rl:leases:api.example.com:key001")
	assert.NoError(t, err)
	assert.Len(t, members, 1)
	assert.Equal(t, result.LeaseID, members[0])

	leaseVal, _ := s.Get("rl:lease:" + result.LeaseID)
	assert.Equal(t, "rl:leases:api.example.com:key001", leaseVal)
}

func TestAcquire_WildcardFallback(t *testing.T) {
	s, client := setupTestRedis(t)

	// 只设置通配符配置
	s.HSet("rl:config:*.example.com:key001", "max_concurrent", "3")
	s.HSet("rl:config:*.example.com:key001", "enabled", "true")

	result, err := client.Acquire(context.Background(), models.AcquireRequest{
		Domain: "api.example.com",
		APIKey: "key001",
		TTLMS:  30000,
	})

	assert.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Equal(t, 3, result.MaxConcurrent)

	leaseVal, _ := s.Get("rl:lease:" + result.LeaseID)
	assert.Equal(t, "rl:leases:api.example.com:key001", leaseVal)
}

func TestAcquire_GlobalFallback(t *testing.T) {
	s, client := setupTestRedis(t)

	// 只设置全局配置
	s.HSet("rl:config:*:key001", "max_concurrent", "1")
	s.HSet("rl:config:*:key001", "enabled", "true")

	result, err := client.Acquire(context.Background(), models.AcquireRequest{
		Domain: "any.domain.com",
		APIKey: "key001",
		TTLMS:  30000,
	})

	assert.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Equal(t, 1, result.MaxConcurrent)

	leaseVal, _ := s.Get("rl:lease:" + result.LeaseID)
	assert.Equal(t, "rl:leases:any.domain.com:key001", leaseVal)
}

func TestAcquire_ConfigNotFound(t *testing.T) {
	_, client := setupTestRedis(t)

	result, err := client.Acquire(context.Background(), models.AcquireRequest{
		Domain: "unknown.com",
		APIKey: "key999",
		TTLMS:  30000,
	})

	assert.Error(t, err)
	assert.False(t, result.Allowed)
	assert.Equal(t, "config_not_found", result.Reason)
}

func TestAcquire_APIKeyDisabled(t *testing.T) {
	s, client := setupTestRedis(t)

	s.HSet("rl:config:api.example.com:key001", "max_concurrent", "5")
	s.HSet("rl:config:api.example.com:key001", "enabled", "false")

	result, err := client.Acquire(context.Background(), models.AcquireRequest{
		Domain: "api.example.com",
		APIKey: "key001",
		TTLMS:  30000,
	})

	assert.Error(t, err)
	assert.False(t, result.Allowed)
	assert.Equal(t, "api_key_disabled", result.Reason)
}

func TestAcquire_LimitExceeded(t *testing.T) {
	s, client := setupTestRedis(t)

	s.HSet("rl:config:api.example.com:key001", "max_concurrent", "2")
	s.HSet("rl:config:api.example.com:key001", "enabled", "true")

	ctx := context.Background()

	// 先占满 2 个槽
	for i := 0; i < 2; i++ {
		_, err := client.Acquire(ctx, models.AcquireRequest{
			Domain: "api.example.com",
			APIKey: "key001",
			TTLMS:  30000,
		})
		assert.NoError(t, err)
	}

	// 第 3 次应该被拒绝
	result, err := client.Acquire(ctx, models.AcquireRequest{
		Domain: "api.example.com",
		APIKey: "key001",
		TTLMS:  30000,
	})

	assert.Error(t, err)
	assert.False(t, result.Allowed)
	assert.Equal(t, "limit_exceeded", result.Reason)
	assert.Equal(t, 2, result.MaxConcurrent)
	assert.Equal(t, 2, result.CurrentCount)
}

func TestRelease_Success(t *testing.T) {
	s, client := setupTestRedis(t)

	s.HSet("rl:config:api.example.com:key001", "max_concurrent", "5")
	s.HSet("rl:config:api.example.com:key001", "enabled", "true")

	// 先 acquire
	acqResult, err := client.Acquire(context.Background(), models.AcquireRequest{
		Domain: "api.example.com",
		APIKey: "key001",
		TTLMS:  30000,
	})
	require.NoError(t, err)
	require.True(t, acqResult.Allowed)

	// 再 release
	relResult, err := client.Release(context.Background(), acqResult.LeaseID)
	assert.NoError(t, err)
	assert.True(t, relResult.Released)
	assert.Equal(t, 0, relResult.CurrentCount)
}

func TestAcquire_LeaseExpiredAutoRecovery(t *testing.T) {
	s, client := setupTestRedis(t)

	s.HSet("rl:config:api.example.com:key001", "max_concurrent", "1")
	s.HSet("rl:config:api.example.com:key001", "enabled", "true")

	ctx := context.Background()

	r1, err := client.Acquire(ctx, models.AcquireRequest{
		Domain: "api.example.com",
		APIKey: "key001",
		TTLMS:  100,
	})
	require.NoError(t, err)
	require.True(t, r1.Allowed)

	s.FastForward(200 * time.Millisecond)
	time.Sleep(120 * time.Millisecond)

	r2, err := client.Acquire(ctx, models.AcquireRequest{
		Domain: "api.example.com",
		APIKey: "key001",
		TTLMS:  30000,
	})
	assert.NoError(t, err)
	assert.True(t, r2.Allowed, "acquire should succeed after lease expiry")
	assert.Equal(t, 1, r2.CurrentCount)
}

func TestRelease_Idempotent(t *testing.T) {
	s, client := setupTestRedis(t)

	s.HSet("rl:config:api.example.com:key001", "max_concurrent", "5")
	s.HSet("rl:config:api.example.com:key001", "enabled", "true")

	ctx := context.Background()
	acqResult, err := client.Acquire(ctx, models.AcquireRequest{
		Domain: "api.example.com",
		APIKey: "key001",
		TTLMS:  30000,
	})
	require.NoError(t, err)

	r1, err := client.Release(ctx, acqResult.LeaseID)
	assert.NoError(t, err)
	assert.True(t, r1.Released)
	assert.Equal(t, 0, r1.CurrentCount)

	r2, err := client.Release(ctx, acqResult.LeaseID)
	assert.ErrorIs(t, err, redis.ErrLeaseNotFound)
	require.NotNil(t, r2)
	assert.False(t, r2.Released)
	assert.Equal(t, "lease_not_found", r2.Reason)
}
