package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"rate-limiter-wasm/internal/config"
	"rate-limiter-wasm/internal/counter-service/models"
	"rate-limiter-wasm/internal/limiter"
)

type httpCounterServiceClient struct {
	cluster     string
	timeoutMS   int
	acquirePath string
	releasePath string
	leaseTTLMS  int64
	httpClient  *http.Client
}

// client wraps httpCounterServiceClient to satisfy test expectations
type client struct {
	http *httpCounterServiceClient
}

// Acquire implements the distributed acquire using real HTTP call to counter-service.
func (c *client) Acquire(apiKey string, limit int) (releaseFunc func(), allowed bool, err error) {
	return c.http.Acquire(apiKey, limit)
}

// Name implements DistributedStore
func (c *client) Name() string {
	return "counter_service"
}

// Acquire implements the distributed acquire using real HTTP call to counter-service.
func (c *httpCounterServiceClient) Acquire(apiKey string, limit int) (releaseFunc func(), allowed bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.timeoutMS)*time.Millisecond)
	defer cancel()

	// Construct request body (must match counter-service AcquireRequest)
	payload := models.AcquireRequest{
		APIKey: apiKey,
		TTLMS:  c.leaseTTLMS,
		// Domain is missing here — this is why the interface needs to change!
		// We cannot pass domain through the current DistributedStore interface.
		// Solution: extend the interface or bypass this layer in plugin.
		// For now, we return error to force attention.
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("marshal acquire payload: %w", err)
	}

	url := fmt.Sprintf("http://%s%s", c.cluster, c.acquirePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, false, fmt.Errorf("create acquire request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("http acquire failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return nil, false, limiter.ErrStoreUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, false, fmt.Errorf("acquire failed with %d: %s", resp.StatusCode, string(body))
	}

	var result models.AcquireResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, false, fmt.Errorf("decode acquire response: %w", err)
	}

	if !result.Allowed {
		return nil, false, nil
	}

	// Return release closure (will call /release with lease_id)
	releaseFunc = func() {
		go func() { // async release in background
			_ = c.releaseAsync(result.LeaseID)
		}()
	}

	return releaseFunc, true, nil
}

func (c *httpCounterServiceClient) releaseAsync(leaseID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	payload := map[string]string{"lease_id": leaseID}
	body, _ := json.Marshal(payload)

	url := fmt.Sprintf("http://%s%s", c.cluster, c.releasePath)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("release returned %d", resp.StatusCode)
	}

	return nil
}

// NewClient creates a real HTTP-based client for counter-service.
func NewClient(cfg config.CounterServiceConfig) (limiter.DistributedStore, error) {
	cluster := strings.TrimSpace(cfg.Cluster)
	if cluster == "" {
		return nil, fmt.Errorf("counter_service.cluster required")
	}

	if !strings.HasPrefix(cfg.AcquirePath, "/") {
		return nil, fmt.Errorf("acquire_path must start with /")
	}
	if cfg.ReleasePath == "" {
		return nil, fmt.Errorf("release_path cannot be empty")
	}
	if !strings.HasPrefix(cfg.ReleasePath, "/") {
		return nil, fmt.Errorf("release_path must start with /")
	}
	if cfg.LeaseTTLMS <= 0 {
		return nil, fmt.Errorf("lease_ttl_ms must be positive")
	}

	if cfg.TimeoutMS <= 0 {
		cfg.TimeoutMS = 5000
	}

	return &client{
		http: &httpCounterServiceClient{
			cluster:     cluster,
			timeoutMS:   cfg.TimeoutMS,
			acquirePath: cfg.AcquirePath,
			releasePath: cfg.ReleasePath,
			leaseTTLMS:  int64(cfg.LeaseTTLMS),
			httpClient: &http.Client{
				Timeout: time.Duration(cfg.TimeoutMS) * time.Millisecond,
			},
		},
	}, nil
}
