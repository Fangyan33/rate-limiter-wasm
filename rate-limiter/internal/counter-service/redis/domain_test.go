package redis_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/redis"
)

func TestDomainRuleSharesQuota(t *testing.T) {
	for _, pattern := range []string{"api.example.com", "*.example.com", "*"} {
		t.Run(pattern, func(t *testing.T) {
			s, client := setupTestRedis(t)
			ctx := context.Background()
			require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Domain: pattern, MaxConcurrent: 2, Enabled: true}))
			require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Domain: "api.example.com", APIKey: "known", MaxConcurrent: 100, Enabled: true}))
			acquire := func(domain, key string) (*redis.AcquireResult, error) {
				return client.Acquire(ctx, models.AcquireRequest{Domain: domain, APIKey: key, TTLMS: 30000})
			}
			first, err := acquire("api.example.com", "")
			require.NoError(t, err)
			second, err := acquire("api.example.com", "known")
			require.NoError(t, err)
			require.Equal(t, 2, second.CurrentCount)
			for _, key := range []string{"", "known", "unknown"} {
				_, err := acquire("api.example.com", key)
				require.ErrorIs(t, err, redis.ErrLimitExceeded)
			}
			leasesKey := "rl:leases:api.example.com:" + sha256Hex("")
			pointer, err := s.Get("rl:lease:" + second.LeaseID)
			require.NoError(t, err)
			require.Equal(t, leasesKey, pointer)
			require.False(t, s.Exists("rl:leases:api.example.com:"+sha256Hex("known")))
			if pattern != "api.example.com" {
				other, err := acquire("other.example.com", "known")
				require.NoError(t, err)
				require.Equal(t, 1, other.CurrentCount)
			}
			_, err = client.Release(ctx, first.LeaseID)
			require.NoError(t, err)
			_, err = client.Release(ctx, first.LeaseID)
			require.ErrorIs(t, err, redis.ErrLeaseNotFound)
			_, err = acquire("api.example.com", "unknown")
			require.NoError(t, err)
			_, err = acquire("api.example.com", "")
			require.ErrorIs(t, err, redis.ErrLimitExceeded)
		})
	}
}

func TestDomainRulePrecedenceAndRejections(t *testing.T) {
	for _, tc := range []struct {
		name     string
		patterns []string
		limit    int
		enabled  bool
		invalid  bool
		wantErr  error
	}{
		{"exact first", []string{"api.example.com", "*.example.com", "*"}, 1, true, false, nil},
		{"parent before global and exact key", []string{"*.example.com", "*"}, 2, true, false, nil},
		{"global before exact key", []string{"*"}, 3, true, false, nil},
		{"disabled exact stops fallback", []string{"api.example.com", "*.example.com", "*"}, 1, false, false, redis.ErrAPIKeyDisabled},
		{"invalid parent stops fallback", []string{"*.example.com", "*"}, 2, true, true, redis.ErrInvalidConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, client := setupTestRedis(t)
			ctx := context.Background()
			for i, domain := range tc.patterns {
				require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Domain: domain, MaxConcurrent: tc.limit + i, Enabled: i > 0 || tc.enabled}))
			}
			if tc.invalid {
				s.HSet("rl:config:"+tc.patterns[0]+":"+sha256Hex(""), "max_concurrent", "bad")
			}
			require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Domain: "api.example.com", APIKey: "known", MaxConcurrent: 100, Enabled: true}))
			res, err := client.Acquire(ctx, models.AcquireRequest{Domain: "api.example.com", APIKey: "known", TTLMS: 30000})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.False(t, res.Allowed)
				require.False(t, s.Exists("rl:leases:api.example.com:"+sha256Hex("")))
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.limit, res.MaxConcurrent)
		})
	}
}

func TestDomainRuleConcurrentAcquire(t *testing.T) {
	_, client := setupTestRedis(t)
	ctx := context.Background()
	require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Domain: "api.example.com", MaxConcurrent: 5, Enabled: true}))
	type outcome struct {
		result *redis.AcquireResult
		err    error
	}
	outcomes := make(chan outcome, 40)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := client.Acquire(ctx, models.AcquireRequest{Domain: "api.example.com", APIKey: []string{"", "a", "b"}[i%3], TTLMS: 30000})
			outcomes <- outcome{res, err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(outcomes)
	var leases []string
	for outcome := range outcomes {
		if errors.Is(outcome.err, redis.ErrLimitExceeded) {
			continue
		}
		require.NoError(t, outcome.err)
		require.True(t, outcome.result.Allowed)
		leases = append(leases, outcome.result.LeaseID)
	}
	require.Len(t, leases, 5)
	for _, lease := range leases {
		_, err := client.Release(ctx, lease)
		require.NoError(t, err)
	}
}

func TestDomainLeaseRenewalAndRuleSwitch(t *testing.T) {
	s, client := setupTestRedis(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	s.SetTime(now)
	advance := func(d time.Duration) { now = now.Add(d); s.SetTime(now); s.FastForward(d) }
	require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Domain: "api.example.com", APIKey: "known", MaxConcurrent: 1, Enabled: true}))
	req := models.AcquireRequest{Domain: "api.example.com", APIKey: "known", TTLMS: 3000}
	legacy, err := client.Acquire(ctx, req)
	require.NoError(t, err)
	require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Domain: "api.example.com", MaxConcurrent: 1, Enabled: true}))
	domainLease, err := client.Acquire(ctx, req)
	require.NoError(t, err)
	for i := 0; i < 6; i++ {
		advance(time.Second)
		for _, lease := range []string{legacy.LeaseID, domainLease.LeaseID} {
			_, err := client.Renew(ctx, models.RenewRequest{LeaseID: lease, TTLMS: 3000})
			require.NoError(t, err)
		}
		_, err = client.Acquire(ctx, models.AcquireRequest{Domain: req.Domain, TTLMS: req.TTLMS})
		require.ErrorIs(t, err, redis.ErrLimitExceeded)
	}
	_, err = client.Release(ctx, legacy.LeaseID)
	require.NoError(t, err)
	_, err = client.Acquire(ctx, req)
	require.ErrorIs(t, err, redis.ErrLimitExceeded, "releasing legacy lease must not affect domain capacity")
	advance(4 * time.Second)
	_, err = client.Acquire(ctx, req)
	require.NoError(t, err, "expired domain lease must restore capacity")
	require.NoError(t, client.DeleteConfig(ctx, req.Domain, ""))
	fallback, err := client.Acquire(ctx, req)
	require.NoError(t, err, "deleting domain rule must restore existing key rule")
	pointer, err := s.Get("rl:lease:" + fallback.LeaseID)
	require.NoError(t, err)
	require.Equal(t, "rl:leases:api.example.com:"+sha256Hex("known"), pointer)
}
