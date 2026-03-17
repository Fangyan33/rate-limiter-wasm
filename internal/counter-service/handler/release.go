package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"rate-limiter-wasm/internal/counter-service/redis"
)

// ReleaseHandler 处理 POST /release 请求
type ReleaseHandler struct {
	redisClient *redis.Client
}

// NewReleaseHandler 创建 handler 实例
func NewReleaseHandler(client *redis.Client) *ReleaseHandler {
	return &ReleaseHandler{redisClient: client}
}

// ServeHTTP 实现 http.Handler 接口
func (h *ReleaseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req struct {
		LeaseID string `json:"lease_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	if req.LeaseID == "" {
		h.writeError(w, http.StatusBadRequest, "lease_id required")
		return
	}

	result, err := h.redisClient.Release(r.Context(), req.LeaseID)
	if err != nil {
		status := http.StatusInternalServerError
		reason := "internal error"

		if errors.Is(err, redis.ErrRedisUnavailable) {
			status = http.StatusServiceUnavailable
			reason = "redis unavailable"
		} else if errors.Is(err, redis.ErrLeaseNotFound) {
			status = http.StatusOK // 租约不存在也返回 200，但 released=false
		}

		h.writeJSON(w, status, map[string]interface{}{
			"released": false,
			"reason":   reason,
			"message":  err.Error(),
		})
		return
	}

	h.writeJSON(w, http.StatusOK, result)
}

// writeError 统一错误响应
func (h *ReleaseHandler) writeError(w http.ResponseWriter, code int, msg string) {
	h.writeJSON(w, code, errorResp{Error: msg})
}

// writeJSON 统一 JSON 响应
func (h *ReleaseHandler) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
