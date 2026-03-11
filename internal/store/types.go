package store

type acquireRequest struct {
	APIKey string `json:"api_key"`
	Limit  int    `json:"limit"`
	TTLMS  int    `json:"ttl_ms"`
}

type acquireResponse struct {
	Allowed bool   `json:"allowed"`
	LeaseID string `json:"lease_id,omitempty"`
}

type releaseRequest struct {
	APIKey  string `json:"api_key"`
	LeaseID string `json:"lease_id"`
}

type releaseResponse struct {
	Released bool `json:"released"`
}
