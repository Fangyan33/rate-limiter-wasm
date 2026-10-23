package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/redis"
)

type RenewHandler struct {
	redisClient *redis.Client
}

func NewRenewHandler(client *redis.Client) *RenewHandler {
	return &RenewHandler{redisClient: client}
}

func (h *RenewHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	writeError := func(status int, message string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(errorResp{Error: message})
	}
	if r.Method != http.MethodPost {
		writeError(http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req models.RenewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(http.StatusBadRequest, err.Error())
		return
	}
	result, err := h.redisClient.Renew(r.Context(), req)
	status := http.StatusOK
	if err != nil && !errors.Is(err, redis.ErrLeaseNotFound) {
		status = http.StatusInternalServerError
		reason := "internal_error"
		if errors.Is(err, redis.ErrRedisUnavailable) {
			status = http.StatusServiceUnavailable
			reason = "redis_unavailable"
		}
		result = &models.RenewResult{Reason: reason, Message: err.Error()}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(result)
}
