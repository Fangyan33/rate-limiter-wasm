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

func TestCorruptCandidateStopsFallback(t *testing.T) {
	for _, field := range []string{"enabled", "max_concurrent"} {
		for _, value := range []string{"<missing>", "garbage", "0", "1.5", "TRUE"} {
			for _, key := range []string{"known", ""} {
				t.Run(field+"/"+value+"/"+key, func(t *testing.T) {
					s, client := setupTestRedis(t)
					ctx := context.Background()
					cfg := models.RateLimitConfig{Port: 8000, Service: "ai/inference", APIKey: key, MaxConcurrent: 1, Enabled: true}
					require.NoError(t, client.SetConfig(ctx, cfg))
					require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Port: 9000, Service: "ai/inference", MaxConcurrent: 100, Enabled: true}))
					configKey := "rl:service:config:ai/inference:8000:" + sha256Hex(key)
					if value == "<missing>" {
						s.HDel(configKey, field)
					} else {
						s.HSet(configKey, field, value)
					}
					_, err := client.Acquire(ctx, models.ServiceAcquireRequest{Port: 8000, Service: cfg.Service, APIKey: "known", TTLMS: 30000})
					require.ErrorIs(t, err, redis.ErrInvalidConfig)
					for _, stored := range s.Keys() {
						require.NotContains(t, stored, "rl:lease")
					}
				})
			}
		}
	}
}

func TestServiceDefaultRuleConcurrentAcquire(t *testing.T) {
	_, client := setupTestRedis(t)
	ctx := context.Background()
	require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Port: 8000, Service: "ai/inference", MaxConcurrent: 5, Enabled: true}))
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
			res, err := client.Acquire(ctx, models.ServiceAcquireRequest{Port: 8000, Service: "ai/inference", APIKey: []string{"", "a", "b"}[i%3], TTLMS: 30000})
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

func TestServiceDefaultLeaseRenewalAndRuleSwitch(t *testing.T) {
	s, client := setupTestRedis(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	s.SetTime(now)
	advance := func(d time.Duration) { now = now.Add(d); s.SetTime(now); s.FastForward(d) }
	require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Port: 8000, Service: "ai/inference", APIKey: "known", MaxConcurrent: 1, Enabled: true}))
	req := models.ServiceAcquireRequest{Port: 8000, Service: "ai/inference", APIKey: "known", TTLMS: 3000}
	specific, err := client.Acquire(ctx, req)
	require.NoError(t, err)
	require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Port: 8000, Service: "ai/inference", MaxConcurrent: 1, Enabled: true}))
	require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Port: 8000, Service: req.Service, APIKey: req.APIKey, MaxConcurrent: 1, Enabled: false}))
	defaultLease, err := client.Acquire(ctx, req)
	require.NoError(t, err)
	for i := 0; i < 6; i++ {
		advance(time.Second)
		for _, lease := range []string{specific.LeaseID, defaultLease.LeaseID} {
			_, err := client.Renew(ctx, models.RenewRequest{LeaseID: lease, TTLMS: 3000})
			require.NoError(t, err)
		}
		_, err = client.Acquire(ctx, models.ServiceAcquireRequest{Port: 8000, Service: req.Service, TTLMS: req.TTLMS})
		require.ErrorIs(t, err, redis.ErrLimitExceeded)
	}
	_, err = client.Release(ctx, specific.LeaseID)
	require.NoError(t, err)
	_, err = client.Acquire(ctx, req)
	require.ErrorIs(t, err, redis.ErrLimitExceeded, "releasing specific lease must not affect default capacity")
	advance(4 * time.Second)
	_, err = client.Acquire(ctx, req)
	require.NoError(t, err, "expired default lease must restore capacity")
	require.NoError(t, client.DeleteConfig(ctx, req.Service, req.Port, ""))
	require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Port: 8000, Service: req.Service, APIKey: req.APIKey, MaxConcurrent: 1, Enabled: true}))
	fallback, err := client.Acquire(ctx, req)
	require.NoError(t, err, "deleting default rule must restore existing key rule")
	pointer, err := s.Get("rl:lease:" + fallback.LeaseID)
	require.NoError(t, err)
	require.Equal(t, "rl:service:leases:ai/inference:8000:"+sha256Hex("known"), pointer)
}

func TestServiceRulePriorityDefaultsAndIdentityIsolation(t *testing.T) {
	for _, specific := range []string{"enabled", "disabled", "absent"} {
		t.Run(specific, func(t *testing.T) {
			s, client := setupTestRedis(t)
			ctx := context.Background()
			require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Service: "ai/inference", Port: 8000, MaxConcurrent: 2, Enabled: true}))
			if specific != "absent" {
				require.NoError(t, client.SetConfig(ctx, models.RateLimitConfig{Service: "ai/inference", Port: 8000, APIKey: "known", MaxConcurrent: 1, Enabled: specific == "enabled"}))
			}
			acquire := func(service string, port int, key string) (*redis.AcquireResult, error) {
				return client.Acquire(ctx, models.ServiceAcquireRequest{Service: service, Port: port, APIKey: key, TTLMS: 30000})
			}
			first, err := acquire("ai/inference", 8000, "known")
			require.NoError(t, err)
			wantKey := ""
			if specific == "enabled" {
				wantKey = "known"
			}
			pointer, err := s.Get("rl:lease:" + first.LeaseID)
			require.NoError(t, err)
			require.Equal(t, "rl:service:leases:ai/inference:8000:"+sha256Hex(wantKey), pointer)
			if specific == "enabled" {
				_, err = acquire("ai/inference", 8000, "known")
				require.ErrorIs(t, err, redis.ErrLimitExceeded)
				_, err = acquire("ai/inference", 8000, "unknown")
				require.NoError(t, err)
			} else {
				_, err = acquire("ai/inference", 8000, "")
				require.NoError(t, err)
				_, err = acquire("ai/inference", 8000, "other")
				require.ErrorIs(t, err, redis.ErrLimitExceeded)
			}
			for _, other := range []models.RateLimitConfig{{Service: "ai/inference", Port: 9000, MaxConcurrent: 1, Enabled: true}, {Service: "other/inference", Port: 8000, MaxConcurrent: 1, Enabled: true}, {Service: "ai/another", Port: 8000, MaxConcurrent: 1, Enabled: true}} {
				require.NoError(t, client.SetConfig(ctx, other))
				res, err := acquire(other.Service, other.Port, "")
				require.NoError(t, err)
				require.NotEmpty(t, res.LeaseID)
			}
			unmetered, err := acquire("ai/unconfigured", 8000, "")
			require.NoError(t, err)
			require.Empty(t, unmetered.LeaseID)
		})
	}
}
