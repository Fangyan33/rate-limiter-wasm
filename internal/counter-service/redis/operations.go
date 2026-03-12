package redis

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// AcquireRequest represents a request to acquire a concurrency slot
type AcquireRequest struct {
	APIKey string
	Limit  int64
	TTLMS  int64
}

// AcquireResult represents the result of an acquire operation
type AcquireResult struct {
	Allowed bool
	LeaseID string
}

// ReleaseRequest represents a request to release a concurrency slot
type ReleaseRequest struct {
	APIKey  string
	LeaseID string
}

// ReleaseResult represents the result of a release operation
type ReleaseResult struct {
	Released bool
}

// Acquire attempts to acquire a concurrency slot for the given API key
func (c *Client) Acquire(ctx context.Context, req AcquireRequest) (*AcquireResult, error) {
	// Generate unique lease ID
	leaseID := uuid.New().String()

	// Build Redis keys
	counterKey := buildCounterKey(req.APIKey)
	leaseKey := buildLeaseKey(req.APIKey, leaseID)

	// Execute Lua script
	result, err := c.rdb.Eval(ctx, acquireScript, []string{counterKey, leaseKey}, req.Limit, req.TTLMS).Result()
	if err != nil {
		return nil, wrapError(err)
	}

	// Parse result
	resultSlice, ok := result.([]interface{})
	if !ok || len(resultSlice) != 2 {
		return nil, fmt.Errorf("unexpected script result format: %v", result)
	}

	allowed, ok := resultSlice[0].(int64)
	if !ok {
		return nil, fmt.Errorf("unexpected allowed value type: %T", resultSlice[0])
	}

	if allowed == 0 {
		return &AcquireResult{Allowed: false}, nil
	}

	// Script returns the full lease key, but we only need the lease_id
	// Since we generated the lease_id ourselves, just return it
	return &AcquireResult{
		Allowed: true,
		LeaseID: leaseID,
	}, nil
}

// Release releases a previously acquired concurrency slot
func (c *Client) Release(ctx context.Context, req ReleaseRequest) (*ReleaseResult, error) {
	// Build Redis keys
	counterKey := buildCounterKey(req.APIKey)
	leaseKey := buildLeaseKey(req.APIKey, req.LeaseID)

	// Execute Lua script
	result, err := c.rdb.Eval(ctx, releaseScript, []string{counterKey, leaseKey}).Result()
	if err != nil {
		return nil, wrapError(err)
	}

	// Parse result
	released, ok := result.(int64)
	if !ok {
		return nil, fmt.Errorf("unexpected script result type: %T", result)
	}

	return &ReleaseResult{
		Released: released == 1,
	}, nil
}
