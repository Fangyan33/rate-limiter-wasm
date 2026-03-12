package models

import "errors"

var (
	// ErrEmptyAPIKey is returned when api_key is empty
	ErrEmptyAPIKey = errors.New("api_key cannot be empty")

	// ErrInvalidLimit is returned when limit is invalid
	ErrInvalidLimit = errors.New("limit must be greater than 0")

	// ErrInvalidTTL is returned when ttl_ms is invalid
	ErrInvalidTTL = errors.New("ttl_ms must be greater than 0")

	// ErrEmptyLeaseID is returned when lease_id is empty
	ErrEmptyLeaseID = errors.New("lease_id cannot be empty")
)
