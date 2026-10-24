package models

// AcquireRequest 获取并发槽位的请求。
type AcquireRequest struct {
	Domain string `json:"domain"`  // 实际请求域名，用于配置查找和计数。
	APIKey string `json:"api_key"` // 空值只匹配域名规则；域名规则始终优先。
	TTLMS  int64  `json:"ttl_ms"`
}

// AcquireResult Acquire 操作的返回结果（匹配设计文档）
type AcquireResult struct {
	Allowed       bool   `json:"allowed"`
	LeaseID       string `json:"lease_id,omitempty"`
	Reason        string `json:"reason,omitempty"` // 拒绝原因：limit_exceeded, config_not_found, api_key_disabled
	Message       string `json:"message,omitempty"`
	MaxConcurrent int    `json:"max_concurrent,omitempty"`
	CurrentCount  int    `json:"current_count,omitempty"`
	Tier          string `json:"tier,omitempty"` // 客户等级：basic, premium, enterprise
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

// RateLimitConfig 限流配置（用于配置管理 API）
type RateLimitConfig struct {
	Domain        string `json:"domain"`
	APIKey        string `json:"api_key"` // 空值表示该域名的总并发规则。
	APIKeyHash    string `json:"api_key_hash,omitempty"`
	MaxConcurrent int    `json:"max_concurrent"`
	Enabled       bool   `json:"enabled"`
	Tier          string `json:"tier,omitempty"`
	Description   string `json:"description,omitempty"`
	UpdatedAt     int64  `json:"updated_at,omitempty"`
}

// Validate 验证 AcquireRequest
func (r *AcquireRequest) Validate() error {
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
	if c.Domain == "" {
		return ErrEmptyDomain
	}
	if c.MaxConcurrent <= 0 {
		return ErrInvalidLimit
	}
	return nil
}
