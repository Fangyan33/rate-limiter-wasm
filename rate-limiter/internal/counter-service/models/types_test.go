package models

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDomainOnlyValidation(t *testing.T) {
	require.NoError(t, (&AcquireRequest{Domain: "api.example.com", TTLMS: 1000}).Validate())
	require.NoError(t, (&RateLimitConfig{Domain: "api.example.com", MaxConcurrent: 1}).Validate())
	require.NoError(t, (&ReleaseRequest{LeaseID: "lease"}).Validate())
	require.ErrorIs(t, (&AcquireRequest{TTLMS: 1000}).Validate(), ErrEmptyDomain)
	require.ErrorIs(t, (&RateLimitConfig{MaxConcurrent: 1}).Validate(), ErrEmptyDomain)
	require.ErrorIs(t, (&AcquireRequest{Domain: "api.example.com"}).Validate(), ErrInvalidTTL)
	require.ErrorIs(t, (&RateLimitConfig{Domain: "api.example.com"}).Validate(), ErrInvalidLimit)
	require.ErrorIs(t, (&ReleaseRequest{}).Validate(), ErrEmptyLeaseID)
}
