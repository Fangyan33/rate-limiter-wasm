package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"rate-limiter-wasm/internal/counter-service/models"
	"rate-limiter-wasm/internal/counter-service/redis"
)

// ConfigHandler 处理配置管理相关 API
type ConfigHandler struct {
	redisClient *redis.Client
}

// NewConfigHandler 创建实例
func NewConfigHandler(client *redis.Client) *ConfigHandler {
	return &ConfigHandler{redisClient: client}
}

// ServeHTTP 实现 http.Handler，支持多种方法
func (h *ConfigHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		h.handlePutConfig(w, r)
	case http.MethodGet:
		if strings.HasPrefix(r.URL.Path, "/configs") {
			h.handleListConfigs(w, r)
		} else {
			h.handleGetConfig(w, r)
		}
	case http.MethodDelete:
		h.handleDeleteConfig(w, r)
	default:
		h.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handlePutConfig 处理 PUT /config
func (h *ConfigHandler) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var cfg models.RateLimitConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	if err := cfg.Validate(); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.redisClient.SetConfig(r.Context(), cfg); err != nil {
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleGetConfig 处理 GET /config?domain=...&api_key=...
func (h *ConfigHandler) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	apiKey := r.URL.Query().Get("api_key")

	if domain == "" || apiKey == "" {
		h.writeError(w, http.StatusBadRequest, "missing domain or api_key query param")
		return
	}

	cfg, err := h.redisClient.GetConfig(r.Context(), domain, apiKey)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfg == nil {
		h.writeError(w, http.StatusNotFound, "config not found")
		return
	}

	h.writeJSON(w, http.StatusOK, cfg)
}

// handleDeleteConfig 处理 DELETE /config?domain=...&api_key=...
func (h *ConfigHandler) handleDeleteConfig(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	apiKey := r.URL.Query().Get("api_key")

	if domain == "" || apiKey == "" {
		h.writeError(w, http.StatusBadRequest, "missing domain or api_key query param")
		return
	}

	if err := h.redisClient.DeleteConfig(r.Context(), domain, apiKey); err != nil {
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleListConfigs 处理 GET /configs?cursor=0&limit=100
func (h *ConfigHandler) handleListConfigs(w http.ResponseWriter, r *http.Request) {
	cursorStr := r.URL.Query().Get("cursor")
	limitStr := r.URL.Query().Get("limit")

	cursor, _ := strconv.ParseUint(cursorStr, 10, 64)
	limit, _ := strconv.ParseInt(limitStr, 10, 64)
	if limit <= 0 {
		limit = 100
	}

	// 默认全量扫描 pattern
	pattern := h.redisClient.Key("config:*")

	result, err := h.redisClient.ListConfigs(r.Context(), pattern, cursor, limit)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.writeJSON(w, http.StatusOK, result)
}

// writeError & writeJSON 复用 acquire.go 中的实现（或移动到公共 util）
func (h *ConfigHandler) writeError(w http.ResponseWriter, code int, msg string) {
	h.writeJSON(w, code, map[string]string{"error": msg})
}

func (h *ConfigHandler) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
