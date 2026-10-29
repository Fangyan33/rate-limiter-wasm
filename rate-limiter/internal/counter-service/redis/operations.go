package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
)

// AcquireResult 与 models.AcquireResult 保持一致
type AcquireResult struct {
	Allowed       bool   `json:"allowed"`
	LeaseID       string `json:"lease_id,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Message       string `json:"message,omitempty"`
	MaxConcurrent int    `json:"max_concurrent,omitempty"`
	CurrentCount  int    `json:"current_count,omitempty"`
}

type ReleaseResult struct {
	Released     bool   `json:"released"`
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message,omitempty"`
	CurrentCount int    `json:"current_count,omitempty"`
}

var (
	ErrLimitExceeded    = errors.New("limit exceeded")
	ErrInvalidConfig    = errors.New("invalid config")
	ErrLeaseNotFound    = errors.New("lease not found")
	ErrRedisUnavailable = errors.New("redis unavailable")
)

// Acquire 执行原子获取并发槽位（调用 Lua acquire_with_config）
func (c *Client) Acquire(ctx context.Context, req models.ServiceAcquireRequest) (*AcquireResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	leaseID := uuid.New().String()
	apiKeyHash := hashAPIKey(req.APIKey)

	specificConfig := ""
	if req.APIKey != "" {
		specificConfig = c.configKey(req.Service, req.Port, apiKeyHash)
	}
	defaultHash := hashAPIKey("")
	keys := []string{
		specificConfig,
		c.Key(fmt.Sprintf("service:leases:%s:%d:%s", req.Service, req.Port, apiKeyHash)),
		c.Key("lease:" + leaseID),
		c.configKey(req.Service, req.Port, defaultHash),
		c.Key(fmt.Sprintf("service:leases:%s:%d:%s", req.Service, req.Port, defaultHash)),
	}

	args := []interface{}{req.TTLMS, leaseID}

	result, err := c.rdb.Eval(ctx, GetAcquireScript(), keys, args...).Result()
	if err != nil {
		if isNetworkError(err) {
			return nil, ErrRedisUnavailable
		}
		return nil, fmt.Errorf("eval acquire script: %w", err)
	}

	jsonStr, ok := result.(string)
	if !ok {
		return nil, fmt.Errorf("script returned non-string: %T", result)
	}

	var res AcquireResult
	if err := json.Unmarshal([]byte(jsonStr), &res); err != nil {
		return nil, fmt.Errorf("unmarshal acquire result: %w", err)
	}

	if !res.Allowed {
		switch res.Reason {
		case "limit_exceeded":
			return &res, ErrLimitExceeded
		case "invalid_config":
			return &res, ErrInvalidConfig
		default:
			return &res, fmt.Errorf("unknown reason: %s", res.Reason)
		}
	}

	return &res, nil
}

func (c *Client) configKey(service string, port int, hash string) string {
	return c.Key(fmt.Sprintf("service:config:%s:%d:%s", service, port, hash))
}

// Release 执行释放并发槽位（调用 Lua release_with_lease）
func (c *Client) Release(ctx context.Context, leaseID string) (*ReleaseResult, error) {
	leaseKey := c.Key(fmt.Sprintf("lease:%s", leaseID))

	result, err := c.rdb.Eval(ctx, GetReleaseScript(), []string{leaseKey}, leaseID).Result()
	if err != nil {
		if isNetworkError(err) {
			return nil, ErrRedisUnavailable
		}
		return nil, fmt.Errorf("eval release script: %w", err)
	}

	jsonStr, ok := result.(string)
	if !ok {
		return nil, fmt.Errorf("script returned non-string: %T", result)
	}

	var res ReleaseResult
	if err := json.Unmarshal([]byte(jsonStr), &res); err != nil {
		return nil, fmt.Errorf("unmarshal release result: %w", err)
	}

	if !res.Released {
		return &res, ErrLeaseNotFound
	}

	return &res, nil
}

// Renew atomically extends both the lease pointer and its existing ZSET member.
func (c *Client) Renew(ctx context.Context, req models.RenewRequest) (*models.RenewResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	leaseKey := c.Key(fmt.Sprintf("lease:%s", req.LeaseID))
	result, err := c.rdb.Eval(ctx, GetRenewScript(), []string{leaseKey}, req.LeaseID, req.TTLMS).Text()
	if err != nil {
		if isNetworkError(err) {
			return nil, ErrRedisUnavailable
		}
		return nil, fmt.Errorf("eval renew script: %w", err)
	}
	var res models.RenewResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		return nil, fmt.Errorf("unmarshal renew result: %w", err)
	}
	if !res.Renewed {
		return &res, ErrLeaseNotFound
	}
	return &res, nil
}
