package models

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// ConfigWriteRequest preserves optional-field presence independently of values.
type ConfigWriteRequest struct {
	Domain        string  `json:"domain"`
	APIKey        string  `json:"api_key"`
	APIKeyHash    *string `json:"api_key_hash"`
	MaxConcurrent int     `json:"max_concurrent"`
	Enabled       *bool   `json:"enabled"`
	Description   *string `json:"description"`
}

func (r *ConfigWriteRequest) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	// encoding/json accepts case-insensitive field aliases. Validate them too so
	// an alias cannot overwrite a supplied hash with null and bypass the guard.
	for name, value := range fields {
		switch strings.ToLower(name) {
		case "tier":
			return fmt.Errorf("tier is no longer supported")
		case "domain", "api_key", "api_key_hash", "max_concurrent", "enabled", "description":
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return fmt.Errorf("%s must not be null", name)
			}
		}
	}
	type wire ConfigWriteRequest
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	for name, value := range fields {
		if strings.EqualFold(name, "api_key_hash") {
			var hash string
			if err := json.Unmarshal(value, &hash); err != nil {
				return err
			}
			if hash != HashAPIKey(decoded.APIKey) {
				return fmt.Errorf("api_key_hash must equal SHA256(api_key)")
			}
		}
	}
	*r = ConfigWriteRequest(decoded)
	return nil
}

func HashAPIKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

// Config validates the entire write before any Redis command can be issued.
func (r *ConfigWriteRequest) Config() (RateLimitConfig, error) {
	if r.Enabled == nil {
		return RateLimitConfig{}, fmt.Errorf("enabled is required and must be a boolean")
	}
	if r.APIKeyHash != nil && *r.APIKeyHash != HashAPIKey(r.APIKey) {
		return RateLimitConfig{}, fmt.Errorf("api_key_hash must equal SHA256(api_key)")
	}
	cfg := RateLimitConfig{
		Domain: r.Domain, APIKey: r.APIKey, MaxConcurrent: r.MaxConcurrent, Enabled: *r.Enabled,
	}
	if r.Description != nil {
		cfg.Description, cfg.DescriptionSet = *r.Description, true
	}
	err := cfg.Validate()
	return cfg, err
}
