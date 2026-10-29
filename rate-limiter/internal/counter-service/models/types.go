package models

import "strings"

// NormalizeDomain is shared by configuration CRUD and acquire lookups.
func NormalizeDomain(domain string) string {
	return strings.ToLower(strings.TrimSpace(domain))
}

// AcquireRequest 获取并发槽位的请求。
type AcquireRequest struct {
	Domain string `json:"domain"`  // 实际请求域名，用于配置查找和计数。
	APIKey string `json:"api_key"` // 先匹配具体 key，再回退空 key 的域名规则。
	TTLMS  int64  `json:"ttl_ms"`
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
	Domain         string
	APIKey         string // Empty selects a domain rule; never persisted in Redis.
	MaxConcurrent  int
	Enabled        bool
	Description    string
	DescriptionSet bool // An explicit empty description clears the stored value.
}

// ConfigView deliberately has no plaintext API key field.
type ConfigView struct {
	Domain        string `json:"domain"`
	APIKeyHash    string `json:"api_key_hash"`
	MaxConcurrent int    `json:"max_concurrent"`
	Enabled       bool   `json:"enabled"`
	Description   string `json:"description,omitempty"`
	UpdatedAt     int64  `json:"updated_at,omitempty"`
}

// Validate 验证 AcquireRequest
func (r *AcquireRequest) Validate() error {
	r.Domain = NormalizeDomain(r.Domain)
	if r.Domain == "" {
		return ErrEmptyDomain
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
	c.Domain = NormalizeDomain(c.Domain)
	if c.Domain == "" {
		return ErrEmptyDomain
	}
	if c.MaxConcurrent <= 0 {
		return ErrInvalidLimit
	}
	return nil
}
