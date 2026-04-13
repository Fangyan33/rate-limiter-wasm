package redis

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrNetworkTimeout = errors.New("redis network or timeout error")
)

// Config Redis 连接配置
type Config struct {
	Addr         string        `json:"addr" yaml:"addr"`
	Password     string        `json:"password" yaml:"password"`
	DB           int           `json:"db" yaml:"db"`
	PoolSize     int           `json:"pool_size" yaml:"pool_size"`
	MinIdle      int           `json:"min_idle" yaml:"min_idle"`
	MaxRetries   int           `json:"max_retries" yaml:"max_retries"`
	DialTimeout  time.Duration `json:"dial_timeout" yaml:"dial_timeout"`
	ReadTimeout  time.Duration `json:"read_timeout" yaml:"read_timeout"`
	WriteTimeout time.Duration `json:"write_timeout" yaml:"write_timeout"`
	TLS          bool          `json:"tls" yaml:"tls"`
	KeyPrefix    string        `json:"key_prefix" yaml:"key_prefix"` // 默认 "rl:"
}

// Client 封装 Redis 客户端
type Client struct {
	rdb    *redis.Client
	prefix string
}

// NewClient 创建 Redis 客户端（支持配置结构体）
func NewClient(cfg Config) (*Client, error) {
	opts := &redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		MinIdleConns: cfg.MinIdle,
		MaxRetries:   cfg.MaxRetries,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}

	if cfg.TLS {
		opts.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	}

	rdb := redis.NewClient(opts)

	// 验证连接
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}

	prefix := cfg.KeyPrefix
	if prefix == "" {
		prefix = "rl:" // 默认前缀
	}

	return &Client{
		rdb:    rdb,
		prefix: prefix,
	}, nil
}

// NewClientSimple 简化版构造函数（兼容旧代码）
func NewClientSimple(addr, password string, db, poolSize, maxRetries int) (*Client, error) {
	return NewClient(Config{
		Addr:       addr,
		Password:   password,
		DB:         db,
		PoolSize:   poolSize,
		MaxRetries: maxRetries,
		KeyPrefix:  "rl:",
	})
}

// Ping 检查连接
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close 关闭连接
func (c *Client) Close() error {
	return c.rdb.Close()
}

// Key 返回带前缀的完整 key
func (c *Client) Key(subKey string) string {
	return c.prefix + subKey
}

// Rdb 暴露底层 redis.Client（供 Lua 脚本、HSET 等直接使用）
func (c *Client) Rdb() *redis.Client {
	return c.rdb
}

// isNetworkError 判断是否为网络或超时错误
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

// wrapError 包装 Redis 错误
func wrapError(err error) error {
	if err == nil {
		return nil
	}
	if isNetworkError(err) {
		return ErrNetworkTimeout
	}
	return err
}
