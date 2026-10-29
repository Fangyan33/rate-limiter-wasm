package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
func (c *Client) Acquire(ctx context.Context, req models.AcquireRequest) (*AcquireResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	leaseID := uuid.New().String()
	apiKeyHash := hashAPIKey(req.APIKey)

	var keyConfigs [3]string
	if req.APIKey != "" {
		keyConfigs = c.configKeys(req.Domain, apiKeyHash)
	}
	domainHash := hashAPIKey("")
	domainConfigs := c.configKeys(req.Domain, domainHash)
	leasesKey := c.Key(fmt.Sprintf("leases:%s:%s", req.Domain, apiKeyHash))
	domainLeasesKey := c.Key(fmt.Sprintf("leases:%s:%s", req.Domain, domainHash))
	leaseKey := c.Key(fmt.Sprintf("lease:%s", leaseID))

	keys := []string{
		keyConfigs[0], leasesKey, leaseKey, keyConfigs[1], keyConfigs[2],
		domainConfigs[0], domainConfigs[1], domainConfigs[2], domainLeasesKey,
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

// configKeys preserves the exact, immediate-parent wildcard, global order.
func (c *Client) configKeys(domain, apiKeyHash string) [3]string {
	wildcard := ""
	if parts := strings.SplitN(domain, ".", 2); len(parts) == 2 {
		wildcard = c.Key(fmt.Sprintf("config:*.%s:%s", parts[1], apiKeyHash))
	}
	return [3]string{
		c.Key(fmt.Sprintf("config:%s:%s", domain, apiKeyHash)),
		wildcard,
		c.Key(fmt.Sprintf("config:*:%s", apiKeyHash)),
	}
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
