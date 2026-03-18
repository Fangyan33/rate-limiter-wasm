package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"rate-limiter-wasm/internal/counter-service/models"
	"rate-limiter-wasm/internal/counter-service/redis"
)

// AcquireHandler 处理 POST /acquire 请求
type AcquireHandler struct {
	redisClient *redis.Client
}

// NewAcquireHandler 创建 handler 实例
func NewAcquireHandler(client *redis.Client) *AcquireHandler {
	return &AcquireHandler{redisClient: client}
}

// ServeHTTP 实现 http.Handler 接口
func (h *AcquireHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req models.AcquireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	if err := req.Validate(); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := h.redisClient.Acquire(r.Context(), req)
	if err != nil {
		status := http.StatusOK
		reason := "internal_error"

		switch {
		case errors.Is(err, redis.ErrRedisUnavailable):
			status = http.StatusServiceUnavailable
			reason = "redis_unavailable"
		case errors.Is(err, redis.ErrConfigNotFound):
			reason = "config_not_found"
		case errors.Is(err, redis.ErrAPIKeyDisabled):
			reason = "api_key_disabled"
		case errors.Is(err, redis.ErrLimitExceeded):
			reason = "limit_exceeded"
		case errors.Is(err, redis.ErrInvalidConfig):
			reason = "invalid_config"
		}

		h.writeJSON(w, status, map[string]interface{}{
			"allowed": false,
			"reason":  reason,
			"message": err.Error(),
		})
		return
	}

	h.writeJSON(w, http.StatusOK, result)
}

// writeError 统一错误响应
type errorResp struct {
	Error string `json:"error"`
}

func (h *AcquireHandler) writeError(w http.ResponseWriter, code int, msg string) {
	h.writeJSON(w, code, errorResp{Error: msg})
}

// writeJSON 统一 JSON 响应
func (h *AcquireHandler) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
