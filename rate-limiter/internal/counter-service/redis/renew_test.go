package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/redis"
)

func TestRenewKeepsSlotBeyondOriginalTTL(t *testing.T) {
	s, client := setupTestRedis(t)
	now := time.Unix(1700000000, 0)
	s.SetTime(now)
	advance := func(d time.Duration) {
		now = now.Add(d)
		s.SetTime(now)
		s.FastForward(d)
	}
	ctx := context.Background()
	require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{
		Domain: "api.example.com", APIKey: "key001", MaxConcurrent: 1, Enabled: true,
	}))
	req := models.AcquireRequest{Domain: "api.example.com", APIKey: "key001", TTLMS: 3000}
	lease, err := client.Acquire(ctx, req)
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		advance(time.Second)
		renewed, err := client.Renew(ctx, models.RenewRequest{LeaseID: lease.LeaseID, TTLMS: 3000})
		require.NoError(t, err)
		require.True(t, renewed.Renewed)
		_, err = client.Acquire(ctx, req)
		require.ErrorIs(t, err, redis.ErrLimitExceeded)
		members, err := s.ZMembers("rl:leases:api.example.com:" + sha256Hex("key001"))
		require.NoError(t, err)
		require.Equal(t, []string{lease.LeaseID}, members)
	}
	advance(4 * time.Second)
	_, err = client.Acquire(ctx, req)
	require.NoError(t, err, "slot must recover when renewal stops")
}

func TestRenewDoesNotResurrectLostLease(t *testing.T) {
	for _, loss := range []string{"released", "pointer expired", "score expired", "member missing", "never existed"} {
		t.Run(loss, func(t *testing.T) {
			s, client := setupTestRedis(t)
			ctx := context.Background()
			now := time.Unix(1700000000, 0)
			s.SetTime(now)
			require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{
				Domain: "api.example.com", APIKey: "key001", MaxConcurrent: 1, Enabled: true,
			}))
			lease, err := client.Acquire(ctx, models.AcquireRequest{Domain: "api.example.com", APIKey: "key001", TTLMS: 3000})
			require.NoError(t, err)
			leasesKey := "rl:leases:api.example.com:" + sha256Hex("key001")
			switch loss {
			case "released":
				_, err = client.Release(ctx, lease.LeaseID)
				require.NoError(t, err)
			case "pointer expired":
				s.FastForward(4 * time.Second)
			case "score expired":
				// Leave the pointer present to test the separate score check.
				s.SetTime(now.Add(4 * time.Second))
			case "member missing":
				require.NoError(t, client.Rdb().ZRem(ctx, leasesKey, lease.LeaseID).Err())
			case "never existed":
				lease.LeaseID = "unknown-lease"
			}
			result, err := client.Renew(ctx, models.RenewRequest{LeaseID: lease.LeaseID, TTLMS: 3000})
			require.ErrorIs(t, err, redis.ErrLeaseNotFound)
			require.False(t, result.Renewed)
			require.False(t, s.Exists("rl:lease:"+lease.LeaseID))
		})
	}
}

func TestRepeatedRenewNeverShortensLease(t *testing.T) {
	s, client := setupTestRedis(t)
	s.SetTime(time.Unix(1700000000, 0))
	ctx := context.Background()
	require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{
		Domain: "api.example.com", APIKey: "key001", MaxConcurrent: 1, Enabled: true,
	}))
	lease, err := client.Acquire(ctx, models.AcquireRequest{Domain: "api.example.com", APIKey: "key001", TTLMS: 3000})
	require.NoError(t, err)
	for _, ttl := range []int64{9000, 1000, 3000} {
		_, err = client.Renew(ctx, models.RenewRequest{LeaseID: lease.LeaseID, TTLMS: ttl})
		require.NoError(t, err)
		require.Equal(t, 9*time.Second, s.TTL("rl:lease:"+lease.LeaseID))
	}
}

func TestRenewReleaseRaceLeavesLeaseReleased(t *testing.T) {
	s, client := setupTestRedis(t)
	ctx := context.Background()
	require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{
		Domain: "api.example.com", APIKey: "key001", MaxConcurrent: 1, Enabled: true,
	}))
	for i := 0; i < 10; i++ {
		lease, err := client.Acquire(ctx, models.AcquireRequest{Domain: "api.example.com", APIKey: "key001", TTLMS: 30000})
		require.NoError(t, err)
		start := make(chan struct{})
		errors := make(chan error, 2)
		go func() {
			<-start
			_, err := client.Renew(ctx, models.RenewRequest{LeaseID: lease.LeaseID, TTLMS: 30000})
			if err == redis.ErrLeaseNotFound {
				err = nil
			}
			errors <- err
		}()
		go func() {
			<-start
			_, err := client.Release(ctx, lease.LeaseID)
			errors <- err
		}()
		close(start)
		require.NoError(t, <-errors)
		require.NoError(t, <-errors)
		require.False(t, s.Exists("rl:lease:"+lease.LeaseID))
		count, err := client.Rdb().ZCard(ctx, "rl:leases:api.example.com:"+sha256Hex("key001")).Result()
		require.NoError(t, err)
		require.Zero(t, count)
		_, err = client.Renew(ctx, models.RenewRequest{LeaseID: lease.LeaseID, TTLMS: 30000})
		require.ErrorIs(t, err, redis.ErrLeaseNotFound)
	}
}
