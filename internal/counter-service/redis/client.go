package redis

import (
	"context"
	"errors"
	"net"

	"github.com/redis/go-redis/v9"
)

var (
	// ErrNetworkTimeout indicates a network or timeout error
	ErrNetworkTimeout = errors.New("redis network or timeout error")
)

// Client wraps the Redis client
type Client struct {
	rdb *redis.Client
}

// NewClient creates a new Redis client
func NewClient(addr, password string, db, poolSize, maxRetries int) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		PoolSize:     poolSize,
		MaxRetries:   maxRetries,
		MinIdleConns: 2,
	})

	return &Client{rdb: rdb}, nil
}

// Ping checks the Redis connection
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close closes the Redis connection
func (c *Client) Close() error {
	return c.rdb.Close()
}

// isNetworkError checks if the error is a network or timeout error
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}

	return false
}

// wrapError wraps Redis errors for proper classification
func wrapError(err error) error {
	if err == nil {
		return nil
	}
	if isNetworkError(err) {
		return ErrNetworkTimeout
	}
	return err
}
