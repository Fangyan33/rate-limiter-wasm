package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"rate-limiter-wasm/internal/counter-service/models"
	redisclient "rate-limiter-wasm/internal/counter-service/redis"
)

// Handler handles counter-service HTTP requests.
type Handler struct {
	redis *redisclient.Client
}

// NewHandler creates a new Handler.
func NewHandler(client *redisclient.Client) *Handler {
	return &Handler{redis: client}
}

// Acquire handles POST /acquire.
func (h *Handler) Acquire(w http.ResponseWriter, r *http.Request) {
	var req models.AcquireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
		return
	}

	if err := req.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
		return
	}

	result, err := h.redis.Acquire(r.Context(), redisclient.AcquireRequest{
		APIKey: req.APIKey,
		Limit:  req.Limit,
		TTLMS:  req.TTLMS,
	})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, redisclient.ErrNetworkTimeout) {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, models.ErrorResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, models.AcquireResponse{
		Allowed: result.Allowed,
		LeaseID: result.LeaseID,
	})
}

// Release handles POST /release.
func (h *Handler) Release(w http.ResponseWriter, r *http.Request) {
	var req models.ReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
		return
	}

	if err := req.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, models.ErrorResponse{Error: err.Error()})
		return
	}

	result, err := h.redis.Release(r.Context(), redisclient.ReleaseRequest{
		APIKey:  req.APIKey,
		LeaseID: req.LeaseID,
	})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, redisclient.ErrNetworkTimeout) {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, models.ErrorResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, models.ReleaseResponse{
		Released: result.Released,
	})
}

// Health handles GET /health.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if err := h.redis.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
