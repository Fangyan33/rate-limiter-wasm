package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/redis"
)

const acquireTimeout = 500 * time.Millisecond

// AcquireHandler 处理 POST /acquire 请求
type AcquireHandler struct {
	redisClient   *redis.Client
	clusterDomain string
}

// NewAcquireHandler 创建 handler 实例
func NewAcquireHandler(client *redis.Client) *AcquireHandler {
	return NewAcquireHandlerWithClusterDomain(client, "cluster.local")
}

func NewAcquireHandlerWithClusterDomain(client *redis.Client, clusterDomain string) *AcquireHandler {
	return &AcquireHandler{redisClient: client, clusterDomain: clusterDomain}
}

// ServeHTTP 实现 http.Handler 接口
func (h *AcquireHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := validateQuery(r, nil); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req models.AcquireRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	if err := req.Validate(); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	service, port, ok := models.ParseUpstreamCluster(req.UpstreamCluster, h.clusterDomain)
	if !ok {
		h.writeJSON(w, http.StatusOK, models.AcquireResult{Allowed: true})
		return
	}

	// 所有连接等待、命令执行和重试共用一个预算，预留 HTTP 响应时间。
	ctx, cancel := context.WithTimeout(r.Context(), acquireTimeout)
	defer cancel()
	result, err := h.redisClient.Acquire(ctx, models.ServiceAcquireRequest{Service: service, Port: port, APIKey: req.APIKey, TTLMS: req.TTLMS})
	if err != nil {
		status := http.StatusInternalServerError
		reason := "internal_error"

		switch {
		case errors.Is(err, redis.ErrRedisUnavailable):
			status = http.StatusServiceUnavailable
			reason = "redis_unavailable"
		case errors.Is(err, redis.ErrLimitExceeded):
			status = http.StatusOK
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
