package models


// AcquireRequest 获取并发槽位的请求（新增 Domain 字段）
type AcquireRequest struct {
	Domain string `json:"domain"`           // 新增：域名，用于配置查找
	APIKey string `json:"api_key"`
	TTLMS  int64  `json:"ttl_ms"`
}

// AcquireResult Acquire 操作的返回结果（匹配设计文档）
type AcquireResult struct {
	Allowed       bool   `json:"allowed"`
	LeaseID       string `json:"lease_id,omitempty"`
	Reason        string `json:"reason,omitempty"`              // 拒绝原因：limit_exceeded, config_not_found, api_key_disabled
	Message       string `json:"message,omitempty"`
	MaxConcurrent int    `json:"max_concurrent,omitempty"`
	CurrentCount  int    `json:"current_count,omitempty"`
	Tier          string `json:"tier,omitempty"`                // 客户等级：basic, premium, enterprise
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

// RateLimitConfig 限流配置（用于配置管理 API）
type RateLimitConfig struct {
	Domain        string `json:"domain"`
	APIKey        string `json:"api_key"`
	MaxConcurrent int    `json:"max_concurrent"`
	Enabled       bool   `json:"enabled"`
	Tier          string `json:"tier,omitempty"`
	Description   string `json:"description,omitempty"`
	UpdatedAt     int64  `json:"updated_at,omitempty"`
}

// 错误定义
// Remove duplicate error definitions - they are already in errors.go
// var (
// 	ErrEmptyAPIKey   = errors.New("domain cannot be empty")
// 	ErrEmptyAPIKey   = errors.New("api_key cannot be empty")
// 	ErrInvalidTTL    = errors.New("ttl_ms must be positive")
// 	ErrEmptyLeaseID  = errors.New("lease_id cannot be empty")
// 	ErrInvalidLimit  = errors.New("max_concurrent must be positive")
// )

// Validate 验证 AcquireRequest
func (r *AcquireRequest) Validate() error {
	if r.Domain == "" {
		return ErrEmptyAPIKey // domain is required, reuse api_key error for simplicity
	}
	if r.APIKey == "" {
		return ErrEmptyAPIKey
	}
	if r.TTLMS <= 0 {
		return ErrInvalidTTL
	}
	return nil
}

// Validate 验证 ReleaseRequest
func (r *ReleaseRequest) Validate() error {
	if r.APIKey == "" {
		return ErrEmptyAPIKey
	}
	if r.LeaseID == "" {
		return ErrEmptyLeaseID
	}
	return nil
}

// Validate 验证 RateLimitConfig
func (c *RateLimitConfig) Validate() error {
	if c.Domain == "" {
		return ErrEmptyAPIKey // domain is required, reuse api_key error for simplicity
	}
	if c.APIKey == "" {
		return ErrEmptyAPIKey
	}
	if c.MaxConcurrent <= 0 {
		return ErrInvalidLimit
	}
	return nil
}
