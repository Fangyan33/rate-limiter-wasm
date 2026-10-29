package models

import "fmt"

// AcquireRequest is the Wasm wire contract; service identity is resolved server-side.
type AcquireRequest struct {
	UpstreamCluster string `json:"upstream_cluster"`
	APIKey          string `json:"api_key"`
	TTLMS           int64  `json:"ttl_ms"`
}

// ServiceAcquireRequest is internal and cannot be supplied through the HTTP API.
type ServiceAcquireRequest struct {
	Service string
	Port    int
	APIKey  string
	TTLMS   int64
}

func (r *ServiceAcquireRequest) Validate() error {
	if err := ValidateService(r.Service, r.Port); err != nil {
		return err
	}
	if r.TTLMS <= 0 {
		return ErrInvalidTTL
	}
	return nil
}

// AcquireResult Acquire 操作的返回结果（匹配设计文档）
type AcquireResult struct {
	Allowed       bool   `json:"allowed"`
	LeaseID       string `json:"lease_id,omitempty"`
	Reason        string `json:"reason,omitempty"` // 仅 limit_exceeded 表示策略拒绝。
	Message       string `json:"message,omitempty"`
	MaxConcurrent int    `json:"max_concurrent,omitempty"`
	CurrentCount  int    `json:"current_count,omitempty"`
}

// ReleaseRequest 释放并发槽位的请求
type ReleaseRequest struct {
	APIKey  string `json:"api_key"`
	LeaseID string `json:"lease_id"`
}

// ReleaseResult Release 操作的返回结果
type ReleaseResult struct {
	Released     bool   `json:"released"`
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message,omitempty"`
	CurrentCount int    `json:"current_count,omitempty"`
}

// RenewRequest extends an existing lease without acquiring another slot.
type RenewRequest struct {
	LeaseID string `json:"lease_id"`
	TTLMS   int64  `json:"ttl_ms"`
}

func (r *RenewRequest) Validate() error {
	if r.LeaseID == "" {
		return ErrEmptyLeaseID
	}
	if r.TTLMS <= 0 {
		return ErrInvalidTTL
	}
	return nil
}

type RenewResult struct {
	Renewed bool   `json:"renewed"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// RateLimitConfig is an internal write model, never a query response.
type RateLimitConfig struct {
	Service        string
	Port           int
	APIKey         string // Empty selects the service-port default rule; never persisted in Redis.
	MaxConcurrent  int
	Enabled        bool
	Description    string
	DescriptionSet bool // An explicit empty description clears the stored value.
}

// ConfigView deliberately has no plaintext API key field.
type ConfigView struct {
	Service       string `json:"service"`
	Port          int    `json:"port"`
	APIKeyHash    string `json:"api_key_hash"`
	MaxConcurrent int    `json:"max_concurrent"`
	Enabled       bool   `json:"enabled"`
	Description   string `json:"description,omitempty"`
	UpdatedAt     int64  `json:"updated_at,omitempty"`
}

// Validate 验证 AcquireRequest
func (r *AcquireRequest) Validate() error {
	if r.UpstreamCluster == "" || len(r.UpstreamCluster) > 1024 {
		return fmt.Errorf("upstream_cluster must contain 1 to 1024 bytes")
	}
	if r.TTLMS <= 0 {
		return ErrInvalidTTL
	}
	return nil
}

// Validate 验证 ReleaseRequest
func (r *ReleaseRequest) Validate() error {
	if r.LeaseID == "" {
		return ErrEmptyLeaseID
	}
	return nil
}

// Validate 验证 RateLimitConfig
func (c *RateLimitConfig) Validate() error {
	if err := ValidateService(c.Service, c.Port); err != nil {
		return err
	}
	if c.MaxConcurrent <= 0 {
		return ErrInvalidLimit
	}
	return nil
}
