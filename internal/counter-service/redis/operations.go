package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"rate-limiter-wasm/internal/counter-service/models"
)

// AcquireResult 与 models.AcquireResult 保持一致
type AcquireResult struct {
	Allowed       bool   `json:"allowed"`
	LeaseID       string `json:"lease_id,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Message       string `json:"message,omitempty"`
	MaxConcurrent int    `json:"max_concurrent,omitempty"`
	CurrentCount  int    `json:"current_count,omitempty"`
	Tier          string `json:"tier,omitempty"`
}

type ReleaseResult struct {
	Released     bool   `json:"released"`
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message,omitempty"`
	CurrentCount int    `json:"current_count,omitempty"`
}

var (
	ErrConfigNotFound  = errors.New("config not found")
	ErrAPIKeyDisabled   = errors.New("api key disabled")
	ErrLimitExceeded    = errors.New("limit exceeded")
	ErrInvalidConfig    = errors.New("invalid config")
	ErrLeaseNotFound    = errors.New("lease not found")
	ErrRedisUnavailable = errors.New("redis unavailable")
)

// Acquire 执行原子获取并发槽位（调用 Lua acquire_with_config）
func (c *Client) Acquire(ctx context.Context, req models.AcquireRequest) (*AcquireResult, error) {
	leaseID := uuid.New().String()

	// 构造 Redis key（不含前缀，由 client.Key() 或直接拼接）
	configKey      := c.Key(fmt.Sprintf("config:%s:%s", req.Domain, req.APIKey))
	counterKey     := c.Key(fmt.Sprintf("counter:%s:%s", req.Domain, req.APIKey))
	leaseKey       := c.Key(fmt.Sprintf("lease:%s", leaseID))

	// 通配符 fallback
	wildcardConfigKey := ""
	if parts := strings.SplitN(req.Domain, ".", 2); len(parts) == 2 {
		wildcardConfigKey = c.Key(fmt.Sprintf("config:*.%s:%s", parts[1], req.APIKey))
	}
	globalConfigKey := c.Key(fmt.Sprintf("config:*:%s", req.APIKey))

	keys := []string{configKey, counterKey, leaseKey}
	if wildcardConfigKey != "" {
		keys = append(keys, wildcardConfigKey)
	} else {
		keys = append(keys, "") // 占位，避免脚本索引越界
	}
	keys = append(keys, globalConfigKey)

	args := []interface{}{req.TTLMS, leaseID}

	// 执行 Lua（建议生产环境先 SCRIPT LOAD 缓存 SHA，这里简化用 EVAL）
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
		case "config_not_found":   return &res, ErrConfigNotFound
		case "api_key_disabled":   return &res, ErrAPIKeyDisabled
		case "limit_exceeded":     return &res, ErrLimitExceeded
		case "invalid_config":     return &res, ErrInvalidConfig
		default:
			return &res, fmt.Errorf("unknown reason: %s", res.Reason)
		}
	}

	return &res, nil
}

// Release 执行释放并发槽位（调用 Lua release_with_lease）
func (c *Client) Release(ctx context.Context, leaseID string) (*ReleaseResult, error) {
	leaseKey := c.Key(fmt.Sprintf("lease:%s", leaseID))

	result, err := c.rdb.Eval(ctx, GetReleaseScript(), []string{leaseKey}).Result()
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
