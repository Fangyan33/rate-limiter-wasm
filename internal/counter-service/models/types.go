package models

// AcquireRequest represents the request to acquire a concurrency slot
type AcquireRequest struct {
	APIKey string `json:"api_key"`
	Limit  int64  `json:"limit"`
	TTLMS  int64  `json:"ttl_ms"`
}

// AcquireResponse represents the response from acquire operation
type AcquireResponse struct {
	Allowed bool   `json:"allowed"`
	LeaseID string `json:"lease_id,omitempty"`
}

// ReleaseRequest represents the request to release a concurrency slot
type ReleaseRequest struct {
	APIKey  string `json:"api_key"`
	LeaseID string `json:"lease_id"`
}

// ReleaseResponse represents the response from release operation
type ReleaseResponse struct {
	Released bool `json:"released"`
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error string `json:"error"`
}

// Validate validates the AcquireRequest
func (r *AcquireRequest) Validate() error {
	if r.APIKey == "" {
		return ErrEmptyAPIKey
	}
	if r.Limit <= 0 {
		return ErrInvalidLimit
	}
	if r.TTLMS <= 0 {
		return ErrInvalidTTL
	}
	return nil
}

// Validate validates the ReleaseRequest
func (r *ReleaseRequest) Validate() error {
	if r.APIKey == "" {
		return ErrEmptyAPIKey
	}
	if r.LeaseID == "" {
		return ErrEmptyLeaseID
	}
	return nil
}
